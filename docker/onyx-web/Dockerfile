# syntax=docker/dockerfile:1
# onyx-web — the ONYX console. It is intentionally static and dependency-free
# so the admin surface remains lightweight; every API-backed view is served
# from the same image. The Unity palette and its switch ship beside the page as
# byte-identical copies of the shared theme, so the console and the landing page
# cannot drift into two opinions about colour.
FROM nginx:1.27-alpine
COPY docker/onyx-web/index.html /usr/share/nginx/html/index.html
COPY docker/onyx-web/unity-theme.css /usr/share/nginx/html/unity-theme.css
COPY docker/onyx-web/unity-theme.js /usr/share/nginx/html/unity-theme.js
