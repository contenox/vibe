# Static contenox.com image: builds the Astro site from this repo and serves
# it with nginx.
#
# Build context is the runtime repo root (the site pulls content from docs/;
# design tokens are vendored locally in website/src/styles/tokens.css):
#
#   docker build -f website/Dockerfile .          # from the runtime repo
#   docker build -f runtime/website/Dockerfile runtime   # from a superproject
#
# The server listens on 3000 to satisfy the existing site deployment contract
# (containerPort + GET / probes).

FROM node:24-alpine AS builder

WORKDIR /src

COPY docs ./docs
COPY website ./website

WORKDIR /src/website
RUN npm ci
RUN npm run build

FROM nginx:1.27-alpine

COPY website/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=builder /src/website/dist /usr/share/nginx/html

EXPOSE 3000
