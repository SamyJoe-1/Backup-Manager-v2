#!/usr/bin/env bash
# Builds small "fresh VPS" images that run real systemd, one per supported distribution.
set -euo pipefail
build() { # build TAG BASE INSTALL-COMMAND
  docker build -q -t "bm-vps:$1" - >/dev/null <<D
FROM $2
ENV container=docker
RUN $3
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
D
  echo "bm-vps:$1"
}
APT='apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq systemd systemd-sysv curl ca-certificates iproute2 procps >/dev/null && apt-get clean && rm -rf /var/lib/apt/lists/* && rm -f /lib/systemd/system/multi-user.target.wants/getty* /etc/systemd/system/getty.target.wants/*'
build debian12 debian:bookworm-slim "$APT"
build ubuntu24 ubuntu:24.04 "$APT"
build ubuntu22 ubuntu:22.04 "$APT"
build rocky9 rockylinux:9 'dnf install -y -q systemd procps-ng iproute >/dev/null && dnf clean all'
