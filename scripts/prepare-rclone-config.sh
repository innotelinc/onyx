#!/usr/bin/env bash
# Shared catalog contains cloud tokens and service-account private keys.
set -euo pipefail
catalog_dir="${1:?usage: prepare-rclone-config.sh <directory>}"
mkdir -p "$catalog_dir"
catalog_dir="$(realpath "$catalog_dir")"
if [ "$(id -u)" = "0" ]; then
  (umask 077; [ -f "$catalog_dir/rclone.conf" ] || : > "$catalog_dir/rclone.conf")
  chown 100:101 "$catalog_dir" "$catalog_dir/rclone.conf"
  chmod 0700 "$catalog_dir"
  chmod 0600 "$catalog_dir/rclone.conf"
else
  # setup already requires Docker. Use its root user to assign the bind
  # mount to the same uid/gid that API, backupd and objectstore run with.
  docker run --rm --user 0 --entrypoint /bin/sh \
    --mount "type=bind,source=$catalog_dir,target=/catalog" alpine:3.20 -ec '
      umask 077
      [ -f /catalog/rclone.conf ] || : > /catalog/rclone.conf
      chown 100:101 /catalog /catalog/rclone.conf
      chmod 0700 /catalog
      chmod 0600 /catalog/rclone.conf
    '
fi
