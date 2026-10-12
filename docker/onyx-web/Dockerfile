# syntax=docker/dockerfile:1
# onyx-web — the Prism SPA shell. It is intentionally static and dependency-free
# so the admin surface remains lightweight; API-backed Flint and Obsidian views
# are served from the same image.
FROM nginx:1.27-alpine
ENV ONYX_MODE=nas
COPY --chmod=0644 docker/onyx-web/index.html /usr/share/nginx/html/index.html
COPY --chmod=0644 docker/onyx-web/config.js /usr/share/nginx/html/config.js
COPY --chmod=0644 docker/onyx-web/default.conf /etc/nginx/conf.d/default.conf
COPY --chmod=0755 docker/onyx-web/40-onyx-mode.sh /docker-entrypoint.d/40-onyx-mode.sh
