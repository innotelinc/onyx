#!/bin/sh
set -eu
case "${ONYX_MODE:-nas}" in
    nas|platform) ;;
    *) echo 'ONYX_MODE must be nas or platform' >&2; exit 1 ;;
esac
printf 'window.ONYX_MODE = "%s";\n' "${ONYX_MODE:-nas}" > /usr/share/nginx/html/config.js
