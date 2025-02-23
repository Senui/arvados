// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package crunchrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"git.arvados.org/arvados.git/sdk/go/arvados"
)

type singularityExecutor struct {
	logf          func(string, ...interface{})
	fakeroot      bool // use --fakeroot flag, allow --network=bridge when non-root (currently only used by tests)
	spec          containerSpec
	tmpdir        string
	child         *exec.Cmd
	imageFilename string // "sif" image
}

func newSingularityExecutor(logf func(string, ...interface{})) (*singularityExecutor, error) {
	tmpdir, err := ioutil.TempDir("", "crunch-run-singularity-")
	if err != nil {
		return nil, err
	}
	return &singularityExecutor{
		logf:   logf,
		tmpdir: tmpdir,
	}, nil
}

func (e *singularityExecutor) Runtime() string {
	buf, err := exec.Command("singularity", "--version").CombinedOutput()
	if err != nil {
		return "singularity (unknown version)"
	}
	return strings.TrimSuffix(string(buf), "\n")
}

func (e *singularityExecutor) getOrCreateProject(ownerUuid string, name string, containerClient *arvados.Client) (*arvados.Group, error) {
	var gp arvados.GroupList
	err := containerClient.RequestAndDecode(&gp,
		arvados.EndpointGroupList.Method,
		arvados.EndpointGroupList.Path,
		nil, arvados.ListOptions{Filters: []arvados.Filter{
			arvados.Filter{"owner_uuid", "=", ownerUuid},
			arvados.Filter{"name", "=", name},
			arvados.Filter{"group_class", "=", "project"},
		},
			Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(gp.Items) == 1 {
		return &gp.Items[0], nil
	}

	var rgroup arvados.Group
	err = containerClient.RequestAndDecode(&rgroup,
		arvados.EndpointGroupCreate.Method,
		arvados.EndpointGroupCreate.Path,
		nil, map[string]interface{}{
			"group": map[string]string{
				"owner_uuid":  ownerUuid,
				"name":        name,
				"group_class": "project",
			},
		})
	if err != nil {
		return nil, err
	}
	return &rgroup, nil
}

func (e *singularityExecutor) checkImageCache(dockerImageID string, container arvados.Container, arvMountPoint string,
	containerClient *arvados.Client) (collection *arvados.Collection, err error) {

	// Cache the image to keep
	cacheGroup, err := e.getOrCreateProject(container.RuntimeUserUUID, ".cache", containerClient)
	if err != nil {
		return nil, fmt.Errorf("error getting '.cache' project: %v", err)
	}
	imageGroup, err := e.getOrCreateProject(cacheGroup.UUID, "auto-generated singularity images", containerClient)
	if err != nil {
		return nil, fmt.Errorf("error getting 'auto-generated singularity images' project: %s", err)
	}

	collectionName := fmt.Sprintf("singularity image for %v", dockerImageID)
	var cl arvados.CollectionList
	err = containerClient.RequestAndDecode(&cl,
		arvados.EndpointCollectionList.Method,
		arvados.EndpointCollectionList.Path,
		nil, arvados.ListOptions{Filters: []arvados.Filter{
			arvados.Filter{"owner_uuid", "=", imageGroup.UUID},
			arvados.Filter{"name", "=", collectionName},
		},
			Limit: 1})
	if err != nil {
		return nil, fmt.Errorf("error querying for collection '%v': %v", collectionName, err)
	}
	var imageCollection arvados.Collection
	if len(cl.Items) == 1 {
		imageCollection = cl.Items[0]
	} else {
		collectionName := "converting " + collectionName
		exp := time.Now().Add(24 * 7 * 2 * time.Hour)
		err = containerClient.RequestAndDecode(&imageCollection,
			arvados.EndpointCollectionCreate.Method,
			arvados.EndpointCollectionCreate.Path,
			nil, map[string]interface{}{
				"collection": map[string]string{
					"owner_uuid": imageGroup.UUID,
					"name":       collectionName,
					"trash_at":   exp.UTC().Format(time.RFC3339),
				},
				"ensure_unique_name": true,
			})
		if err != nil {
			return nil, fmt.Errorf("error creating '%v' collection: %s", collectionName, err)
		}

	}

	return &imageCollection, nil
}

// LoadImage will satisfy ContainerExecuter interface transforming
// containerImage into a sif file for later use.
func (e *singularityExecutor) LoadImage(dockerImageID string, imageTarballPath string, container arvados.Container, arvMountPoint string,
	containerClient *arvados.Client) error {

	var imageFilename string
	var sifCollection *arvados.Collection
	var err error
	if containerClient != nil {
		sifCollection, err = e.checkImageCache(dockerImageID, container, arvMountPoint, containerClient)
		if err != nil {
			return err
		}
		imageFilename = fmt.Sprintf("%s/by_uuid/%s/image.sif", arvMountPoint, sifCollection.UUID)
	} else {
		imageFilename = e.tmpdir + "/image.sif"
	}

	if _, err := os.Stat(imageFilename); os.IsNotExist(err) {
		// Make sure the docker image is readable, and error
		// out if not.
		if _, err := os.Stat(imageTarballPath); err != nil {
			return err
		}

		e.logf("building singularity image")
		// "singularity build" does not accept a
		// docker-archive://... filename containing a ":" character,
		// as in "/path/to/sha256:abcd...1234.tar". Workaround: make a
		// symlink that doesn't have ":" chars.
		err := os.Symlink(imageTarballPath, e.tmpdir+"/image.tar")
		if err != nil {
			return err
		}

		// Set up a cache and tmp dir for singularity build
		err = os.Mkdir(e.tmpdir+"/cache", 0700)
		if err != nil {
			return err
		}
		defer os.RemoveAll(e.tmpdir + "/cache")
		err = os.Mkdir(e.tmpdir+"/tmp", 0700)
		if err != nil {
			return err
		}
		defer os.RemoveAll(e.tmpdir + "/tmp")

		build := exec.Command("singularity", "build", imageFilename, "docker-archive://"+e.tmpdir+"/image.tar")
		build.Env = os.Environ()
		build.Env = append(build.Env, "SINGULARITY_CACHEDIR="+e.tmpdir+"/cache")
		build.Env = append(build.Env, "SINGULARITY_TMPDIR="+e.tmpdir+"/tmp")
		e.logf("%v", build.Args)
		out, err := build.CombinedOutput()
		// INFO:    Starting build...
		// Getting image source signatures
		// Copying blob ab15617702de done
		// Copying config 651e02b8a2 done
		// Writing manifest to image destination
		// Storing signatures
		// 2021/04/22 14:42:14  info unpack layer: sha256:21cbfd3a344c52b197b9fa36091e66d9cbe52232703ff78d44734f85abb7ccd3
		// INFO:    Creating SIF file...
		// INFO:    Build complete: arvados-jobs.latest.sif
		e.logf("%s", out)
		if err != nil {
			return err
		}
	}

	if containerClient == nil {
		e.imageFilename = imageFilename
		return nil
	}

	// update TTL to now + two weeks
	exp := time.Now().Add(24 * 7 * 2 * time.Hour)

	uuidPath, err := containerClient.PathForUUID("update", sifCollection.UUID)
	if err != nil {
		e.logf("error PathForUUID: %v", err)
		return nil
	}
	var imageCollection arvados.Collection
	err = containerClient.RequestAndDecode(&imageCollection,
		arvados.EndpointCollectionUpdate.Method,
		uuidPath,
		nil, map[string]interface{}{
			"collection": map[string]string{
				"name":     fmt.Sprintf("singularity image for %v", dockerImageID),
				"trash_at": exp.UTC().Format(time.RFC3339),
			},
		})
	if err == nil {
		// If we just wrote the image to the cache, the
		// response also returns the updated PDH
		e.imageFilename = fmt.Sprintf("%s/by_id/%s/image.sif", arvMountPoint, imageCollection.PortableDataHash)
		return nil
	}

	e.logf("error updating/renaming collection for cached sif image: %v", err)
	// Failed to update but maybe it lost a race and there is
	// another cached collection in the same place, so check the cache
	// again
	sifCollection, err = e.checkImageCache(dockerImageID, container, arvMountPoint, containerClient)
	if err != nil {
		return err
	}
	e.imageFilename = fmt.Sprintf("%s/by_id/%s/image.sif", arvMountPoint, sifCollection.PortableDataHash)

	return nil
}

func (e *singularityExecutor) Create(spec containerSpec) error {
	e.spec = spec
	return nil
}

func (e *singularityExecutor) execCmd(path string) *exec.Cmd {
	args := []string{path, "exec", "--containall", "--cleanenv", "--pwd=" + e.spec.WorkingDir}
	if e.fakeroot {
		args = append(args, "--fakeroot")
	}
	if !e.spec.EnableNetwork {
		args = append(args, "--net", "--network=none")
	} else if u, err := user.Current(); err == nil && u.Uid == "0" || e.fakeroot {
		// Specifying --network=bridge fails unless (a) we are
		// root, (b) we are using --fakeroot, or (c)
		// singularity has been configured to allow our
		// uid/gid to use it like so:
		//
		// singularity config global --set 'allow net networks' bridge
		// singularity config global --set 'allow net groups' mygroup
		args = append(args, "--net", "--network=bridge")
	}
	if e.spec.CUDADeviceCount != 0 {
		args = append(args, "--nv")
	}

	readonlyflag := map[bool]string{
		false: "rw",
		true:  "ro",
	}
	var binds []string
	for path, _ := range e.spec.BindMounts {
		binds = append(binds, path)
	}
	sort.Strings(binds)
	for _, path := range binds {
		mount := e.spec.BindMounts[path]
		if path == e.spec.Env["HOME"] {
			// Singularity treats $HOME as special case
			args = append(args, "--home", mount.HostPath+":"+path)
		} else {
			args = append(args, "--bind", mount.HostPath+":"+path+":"+readonlyflag[mount.ReadOnly])
		}
	}

	// This is for singularity 3.5.2. There are some behaviors
	// that will change in singularity 3.6, please see:
	// https://sylabs.io/guides/3.7/user-guide/environment_and_metadata.html
	// https://sylabs.io/guides/3.5/user-guide/environment_and_metadata.html
	env := make([]string, 0, len(e.spec.Env))
	for k, v := range e.spec.Env {
		if k == "HOME" {
			// Singularity treats $HOME as special case,
			// this is handled with --home above
			continue
		}
		env = append(env, "SINGULARITYENV_"+k+"="+v)
	}

	// Singularity always makes all nvidia devices visible to the
	// container.  If a resource manager such as slurm or LSF told
	// us to select specific devices we need to propagate that.
	if cudaVisibleDevices := os.Getenv("CUDA_VISIBLE_DEVICES"); cudaVisibleDevices != "" {
		// If a resource manager such as slurm or LSF told
		// us to select specific devices we need to propagate that.
		env = append(env, "SINGULARITYENV_CUDA_VISIBLE_DEVICES="+cudaVisibleDevices)
	}
	// Singularity's default behavior is to evaluate each
	// SINGULARITYENV_* env var with a shell as a double-quoted
	// string and pass the result to the contained
	// process. Singularity 3.10+ has an option to pass env vars
	// through literally without evaluating, which is what we
	// want. See https://github.com/sylabs/singularity/pull/704
	// and https://dev.arvados.org/issues/19081
	env = append(env, "SINGULARITY_NO_EVAL=1")

	args = append(args, e.imageFilename)
	args = append(args, e.spec.Command...)

	return &exec.Cmd{
		Path:   path,
		Args:   args,
		Env:    env,
		Stdin:  e.spec.Stdin,
		Stdout: e.spec.Stdout,
		Stderr: e.spec.Stderr,
	}
}

func (e *singularityExecutor) Start() error {
	path, err := exec.LookPath("singularity")
	if err != nil {
		return err
	}
	child := e.execCmd(path)
	err = child.Start()
	if err != nil {
		return err
	}
	e.child = child
	return nil
}

func (e *singularityExecutor) CgroupID() string {
	return ""
}

func (e *singularityExecutor) Stop() error {
	if err := e.child.Process.Signal(syscall.Signal(0)); err != nil {
		// process already exited
		return nil
	}
	return e.child.Process.Signal(syscall.SIGKILL)
}

func (e *singularityExecutor) Wait(context.Context) (int, error) {
	err := e.child.Wait()
	if err, ok := err.(*exec.ExitError); ok {
		return err.ProcessState.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return e.child.ProcessState.ExitCode(), nil
}

func (e *singularityExecutor) Close() {
	err := os.RemoveAll(e.tmpdir)
	if err != nil {
		e.logf("error removing temp dir: %s", err)
	}
}

func (e *singularityExecutor) InjectCommand(ctx context.Context, detachKeys, username string, usingTTY bool, injectcmd []string) (*exec.Cmd, error) {
	target, err := e.containedProcess()
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, "nsenter", append([]string{fmt.Sprintf("--target=%d", target), "--all"}, injectcmd...)...), nil
}

var (
	errContainerHasNoIPAddress = errors.New("container has no IP address distinct from host")
)

func (e *singularityExecutor) IPAddress() (string, error) {
	target, err := e.containedProcess()
	if err != nil {
		return "", err
	}
	targetIPs, err := processIPs(target)
	if err != nil {
		return "", err
	}
	selfIPs, err := processIPs(os.Getpid())
	if err != nil {
		return "", err
	}
	for ip := range targetIPs {
		if !selfIPs[ip] {
			return ip, nil
		}
	}
	return "", errContainerHasNoIPAddress
}

func processIPs(pid int) (map[string]bool, error) {
	fibtrie, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/fib_trie", pid))
	if err != nil {
		return nil, err
	}

	addrs := map[string]bool{}
	// When we see a pair of lines like this:
	//
	//              |-- 10.1.2.3
	//                 /32 host LOCAL
	//
	// ...we set addrs["10.1.2.3"] = true
	lines := bytes.Split(fibtrie, []byte{'\n'})
	for linenumber, line := range lines {
		if !bytes.HasSuffix(line, []byte("/32 host LOCAL")) {
			continue
		}
		if linenumber < 1 {
			continue
		}
		i := bytes.LastIndexByte(lines[linenumber-1], ' ')
		if i < 0 || i >= len(line)-7 {
			continue
		}
		addr := string(lines[linenumber-1][i+1:])
		if net.ParseIP(addr).To4() != nil {
			addrs[addr] = true
		}
	}
	return addrs, nil
}

var (
	errContainerNotStarted = errors.New("container has not started yet")
	errCannotFindChild     = errors.New("failed to find any process inside the container")
	reProcStatusPPid       = regexp.MustCompile(`\nPPid:\t(\d+)\n`)
)

// Return the PID of a process that is inside the container (not
// necessarily the topmost/pid=1 process in the container).
func (e *singularityExecutor) containedProcess() (int, error) {
	if e.child == nil || e.child.Process == nil {
		return 0, errContainerNotStarted
	}
	lsns, err := exec.Command("lsns").CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("lsns: %w", err)
	}
	for _, line := range bytes.Split(lsns, []byte{'\n'}) {
		fields := bytes.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if !bytes.Equal(fields[1], []byte("pid")) {
			continue
		}
		pid, err := strconv.ParseInt(string(fields[3]), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("error parsing PID field in lsns output: %q", fields[3])
		}
		for parent := pid; ; {
			procstatus, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", parent))
			if err != nil {
				break
			}
			m := reProcStatusPPid.FindSubmatch(procstatus)
			if m == nil {
				break
			}
			parent, err = strconv.ParseInt(string(m[1]), 10, 64)
			if err != nil {
				break
			}
			if int(parent) == e.child.Process.Pid {
				return int(pid), nil
			}
		}
	}
	return 0, errCannotFindChild
}
