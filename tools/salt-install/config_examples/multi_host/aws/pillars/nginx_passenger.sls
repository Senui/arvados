---
# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

{%- set passenger_pkg = 'nginx-mod-http-passenger'
                          if grains.osfinger in ('CentOS Linux-7') else
                        'libnginx-mod-http-passenger' %}
{%- set passenger_mod = '/usr/lib64/nginx/modules/ngx_http_passenger_module.so'
                          if grains.osfinger in ('CentOS Linux-7',) else
                        '/usr/lib/nginx/modules/ngx_http_passenger_module.so' %}
{%- set passenger_ruby = '/usr/local/rvm/wrappers/default/ruby'
                           if grains.osfinger in ('CentOS Linux-7', 'Ubuntu-18.04', 'Debian-10') else
                         '/usr/bin/ruby' %}
{%- set max_reqs = "__CONTROLLER_MAX_CONCURRENT_REQUESTS__" %}

### NGINX
nginx:
  __NGINX_INSTALL_SOURCE__: true
  lookup:
    passenger_package: {{ passenger_pkg }}
  ### PASSENGER
  passenger:
    passenger_ruby: {{ passenger_ruby }}
    passenger_max_pool_size: {{ "__CONTROLLER_NGINX_WORKERS__" or grains['num_cpus'] }}
    {%- if max_reqs != "" %}
    # Default is 100 -- Configuring this a bit higher than API.MaxConcurrentRequests
    # to be able to handle /metrics requests even on heavy load situations.
    passenger_max_request_queue_size: {{ (max_reqs|int * 1.1)|round|int }}
    {%- endif %}

  ### SERVER
  server:
    config:
      # Needed for RVM, harmless otherwise. Cf. https://dev.arvados.org/issues/19015
      env: GEM_HOME
      # As we now differentiate where passenger is required or not, we need to
      # load this module conditionally, so we add this conditional just to use
      # the same pillar file
      {% if "install_from_phusionpassenger" == "__NGINX_INSTALL_SOURCE__" %}
      # This is required to get the passenger module loaded
      # In Debian it can be done with this
      # include: 'modules-enabled/*.conf'
      load_module: {{ passenger_mod }}
      {% endif %}
      worker_processes: {{ "__CONTROLLER_NGINX_WORKERS__" or grains['num_cpus'] }}
      {%- if max_reqs != "" %}
      worker_rlimit_nofile: {{ (max_reqs|int * 3)|round|int }}
      events:
        worker_connections: {{ (max_reqs|int * 3)|round|int }}
      {%- else %}
      worker_rlimit_nofile: 4096
      events:
        worker_connections: 1024
      {%- endif %}

  ### SNIPPETS
  snippets:
    # Based on https://ssl-config.mozilla.org/#server=nginx&version=1.14.2&config=intermediate&openssl=1.1.1d&guideline=5.4
    ssl_hardening_default.conf:
      - ssl_session_timeout: 1d
      - ssl_session_cache: 'shared:arvadosSSL:10m'
      - ssl_session_tickets: 'off'

      # intermediate configuration
      - ssl_protocols: TLSv1.2 TLSv1.3
      - ssl_ciphers: ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-ECDSA-CHACHA20-POLY1305:ECDHE-RSA-CHACHA20-POLY1305:DHE-RSA-AES128-GCM-SHA256:DHE-RSA-AES256-GCM-SHA384
      - ssl_prefer_server_ciphers: 'off'

      # HSTS (ngx_http_headers_module is required) (63072000 seconds)
      - add_header: 'Strict-Transport-Security "max-age=63072000" always'

      # OCSP stapling
      - ssl_stapling: 'on'
      - ssl_stapling_verify: 'on'

      # verify chain of trust of OCSP response using Root CA and Intermediate certs
      # - ssl_trusted_certificate /path/to/root_CA_cert_plus_intermediates

      # curl https://ssl-config.mozilla.org/ffdhe2048.txt > /path/to/dhparam
      # - ssl_dhparam: /path/to/dhparam

      # replace with the IP address of your resolver
      # - resolver: 127.0.0.1

  ### SITES
  servers:
    managed:
      # Remove default webserver
      default:
        enabled: false
