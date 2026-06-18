# ── build ──────────────────────────────────────────────────────────────
FROM node:20-alpine AS build
WORKDIR /src
COPY package.json package-lock.json* ./
RUN if [ -f package-lock.json ]; then npm ci; else npm install; fi
COPY . .
RUN npm run build

# ── serve ──────────────────────────────────────────────────────────────
# Static SPA over the unprivileged nginx image. Vite's base is /manage/, so
# the built assets are copied under that path and the SPA fallback in
# nginx.conf keeps react-router (basename "/manage") working on deep links.
FROM nginxinc/nginx-unprivileged:1.27-alpine
COPY --from=build /src/dist /usr/share/nginx/html/manage
COPY nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 8080

LABEL org.opencontainers.image.title="stube-admin"
LABEL org.opencontainers.image.description="Stube admin UI — first-run setup + day-2 management."
LABEL org.opencontainers.image.source="https://github.com/zaentrum/stube"
