# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

ARG HOSTTYPE
ARG BRANCH
ARG GOVERSION

FROM ubuntu:bionic as build_x86_64
# Install go
ONBUILD ARG GOVERSION
ONBUILD ADD generated/go${GOVERSION}.linux-amd64.tar.gz /usr/local/
ONBUILD RUN ln -s /usr/local/go/bin/go /usr/local/bin/
# Install nodejs and npm
ONBUILD ADD generated/node-v10.23.1-linux-x64.tar.xz /usr/local/
ONBUILD RUN ln -s /usr/local/node-v10.23.1-linux-x64/bin/* /usr/local/bin/
# No cross compilation support for ubuntu1804 because of https://bugs.debian.org/cgi-bin/bugreport.cgi?bug=983477

FROM ubuntu:bionic as build_aarch64
# Install go
ONBUILD ARG GOVERSION
ONBUILD ADD generated/go${GOVERSION}.linux-arm64.tar.gz /usr/local/
ONBUILD RUN ln -s /usr/local/go/bin/go /usr/local/bin/
# Install nodejs and npm
ONBUILD ADD generated/node-v10.23.1-linux-arm64.tar.xz /usr/local/
ONBUILD RUN ln -s /usr/local/node-v10.23.1-linux-arm64/bin/* /usr/local/bin/

FROM build_${HOSTTYPE}

MAINTAINER Arvados Package Maintainers <packaging@arvados.org>

ENV DEBIAN_FRONTEND noninteractive

SHELL ["/bin/bash", "-c"]
# Install dependencies.
RUN /usr/bin/apt-get update && /usr/bin/apt-get install -q -y python3.8 python3-pip libcurl4-gnutls-dev libgnutls28-dev curl git libattr1-dev libfuse-dev libpq-dev unzip tzdata python3.8-venv python3.8-dev libpam-dev equivs

# Install virtualenv
RUN /usr/bin/pip3 install 'virtualenv<20'

# Install RVM
ADD generated/mpapis.asc /tmp/
ADD generated/pkuczynski.asc /tmp/
RUN gpg --import --no-tty /tmp/mpapis.asc && \
    gpg --import --no-tty /tmp/pkuczynski.asc && \
    curl -L https://get.rvm.io | bash -s stable && \
    /usr/local/rvm/bin/rvm install 2.7 -j $(grep -c processor /proc/cpuinfo) && \
    /usr/local/rvm/bin/rvm alias create default ruby-2.7 && \
    echo "gem: --no-document" >> ~/.gemrc && \
    /usr/local/rvm/bin/rvm-exec default gem install bundler --version 2.2.19 && \
    /usr/local/rvm/bin/rvm-exec default gem install fpm --version 1.10.2

RUN /usr/local/rvm/bin/rvm-exec default bundle config --global jobs $(let a=$(grep -c processor /proc/cpuinfo )-1; echo $a)
# Cf. https://build.betterup.com/one-weird-trick-that-will-speed-up-your-bundle-install/
ENV MAKE "make --jobs $(grep -c processor /proc/cpuinfo)"

# Preseed the go module cache and the ruby gems, using the currently checked
# out branch of the source tree. This avoids potential compatibility issues
# between the version of Ruby and certain gems.
RUN git clone --depth 1 git://git.arvados.org/arvados.git /tmp/arvados && \
    cd /tmp/arvados && \
    if [[ -n "${BRANCH}" ]]; then git checkout ${BRANCH}; fi && \
    cd /tmp/arvados/services/api && \
    /usr/local/rvm/bin/rvm-exec default bundle install && \
    cd /tmp/arvados/apps/workbench && \
    /usr/local/rvm/bin/rvm-exec default bundle install && \
    cd /tmp/arvados && \
    go mod download

ENV WORKSPACE /arvados
CMD ["/usr/local/rvm/bin/rvm-exec", "default", "bash", "/jenkins/run-build-packages.sh", "--target", "ubuntu1804"]
