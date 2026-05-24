# syntax=docker/dockerfile:1.7
# Stage 1 — build the Vite SPA. Node 20 matches the engines field
# in package.json.
FROM node:20-alpine AS builder

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

# Stage 2 — serve the static bundle behind nginx. The bundle is
# tiny (~200KB gzipped) so a separate CDN layer isn't worth it for
# a self-hosted deploy.
FROM nginx:alpine

COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=builder /app/dist /usr/share/nginx/html

# /healthz is served by nginx directly (see nginx.conf) so the
# container is healthy without round-tripping to the API.
HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD wget -qO- http://localhost/healthz || exit 1

EXPOSE 80
