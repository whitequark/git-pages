# syntax = docker/dockerfile:1

# Build hivemind.
FROM golang:1.25-alpine AS hivemind-builder
RUN apk --no-cache add git
WORKDIR /build
RUN GOBIN=/usr/bin go install github.com/DarthSim/hivemind@v1.1.0

# Build Caddy with S3 storage backend.
FROM caddy:2.10.2-builder AS caddy-builder
RUN xcaddy build ${CADDY_VERSION} \
    --with github.com/ss098/certmagic-s3

# Build git-pages.
FROM golang:1.25-alpine AS git-pages-builder
RUN apk --no-cache add git
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY src/ ./src/
RUN go build -a -o git-pages ./src

# Compose git-pages and Caddy.
FROM alpine:latest
RUN apk --no-cache add ca-certificates
COPY --from=hivemind-builder /usr/bin/hivemind /usr/bin/hivemind
COPY --from=caddy-builder /usr/bin/caddy /usr/bin/caddy
COPY --from=git-pages-builder /build/git-pages /usr/bin/git-pages

WORKDIR /app
RUN mkdir /app/data
COPY Caddyfile /app/Caddyfile
COPY config.toml.example /app/config.toml

RUN addgroup -g 1000 -S appuser && adduser -u 1000 -S appuser -G appuser
RUN chown -R appuser:appuser /app
USER appuser

# Caddy ports:
EXPOSE 80 443 2019
# git-pages ports:
EXPOSE 3000 3001 3002

# While the default command is to run git-pages standalone, the intended configuration
# is to use it with Caddy and store both site data and credentials to an S3-compatible
# object store.
# In a combined configuration, the same container may be used twice, launching either
# `git-caddy` or `caddy run` to start both services.
# In a standalone configuration use port 3000 (http) to connect to git-caddy.
COPY <<EOF Procfile
pages: git-pages
caddy: caddy run
EOF
CMD ["hivemind", "-processes=pages"]
