#!/bin/bash
# base/debian-trixie/postinst.sh — first-boot seeding inside the composed
# rootfs (docs/design/10#2). Runs once at compose time so the image boots with
# users and runtime dirs already materialized; safe to re-run on boot.
#
# What it sets up:
#   * onyx service users (sysusers.d, installed from debian-trixie/files),
#   * runtime + state dirs (/run/onyx, /mnt/onyx, /var/lib/onyx/*,
#     /etc/onyx/conf.d) from deploy/tmpfiles.d/onyx.conf.
#
# The interactive first-boot wizard (admin user, keyring, network, update
# channel) is served by onyx-api and lands with user management in v0.2.
set -euo pipefail

echo "onyx-postinst: creating onyx users (systemd-sysusers)"
systemd-sysusers --root=/ || {
  echo "onyx-postinst: systemd-sysusers failed, falling back to manual users" >&2
  getent group onyx >/dev/null || groupadd --system onyx
  for u in onyx-core onyx-api onyx-shared onyx-storaged; do
    getent passwd "$u" >/dev/null || useradd --system --gid onyx --no-create-home \
      --home-dir /nonexistent --shell /usr/sbin/nologin "$u"
  done
}

echo "onyx-postinst: materializing runtime/state dirs (systemd-tmpfiles)"
systemd-tmpfiles --create --root=/ --boot || true

echo "onyx-postinst: done"