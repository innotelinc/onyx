# syntax=docker/dockerfile:1
# onyx-web — the Prism SPA shell. It is intentionally static and dependency-free
# so the admin surface remains lightweight; API-backed Flint and Obsidian views
# are served from the same image.
FROM nginx:1.27-alpine
COPY docker/onyx-web/index.html /usr/share/nginx/html/index.html
