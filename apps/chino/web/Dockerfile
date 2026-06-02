FROM node:20-alpine AS build
WORKDIR /src
COPY package.json package-lock.json* ./
RUN if [ -f package-lock.json ]; then npm ci; else npm install; fi
COPY . .
RUN npm run build

FROM nginxinc/nginx-unprivileged:1.27-alpine
COPY --from=build /src/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 8080

LABEL org.opencontainers.image.source="https://github.com/nalet/stube"
LABEL org.opencontainers.image.title="chino-web"
