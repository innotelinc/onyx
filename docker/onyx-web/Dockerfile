# syntax=docker/dockerfile:1
# onyx-web — the Prism SPA placeholder (v0.2 milestone). For now it serves a
# landing page that links every platform surface, so app.onyx.innotel.us is a
# real endpoint from day one. The React SPA replaces index.html in v0.2.
FROM nginx:1.27-alpine
COPY docker/onyx-web/index.html /usr/share/nginx/html/index.html
