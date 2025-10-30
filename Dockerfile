# Install CA certificates.
FROM docker.io/library/alpine:latest@sha256:4b7ce07002c69e8f3d704a9c5d6fd3053be500b7f1c69fc0d80990c2ad8dd412 AS ca-certificates-builder
RUN apk --no-cache add ca-certificates

# Build supervisor.
FROM docker.io/library/golang:1.25-alpine@sha256:aee43c3ccbf24fdffb7295693b6e33b21e01baec1b2a55acc351fde345e9ec34 AS supervisor-builder
RUN apk --no-cache add git
WORKDIR /build
RUN git clone https://github.com/ochinchina/supervisord . && \
    git checkout 16cb640325b3a4962b2ba17d68fb5c2b1e1b6b3c
RUN GOBIN=/usr/bin go install -ldflags "-s -w"

# Build Caddy with S3 storage backend.
FROM docker.io/library/caddy:2.10.2-builder@sha256:53f91ad7c5f1ab9a607953199b7c1e10920c570ae002aef913d68ed7464fb19f AS caddy-builder
RUN xcaddy build ${CADDY_VERSION} \
    --with=github.com/ss098/certmagic-s3@v0.0.0-20250922022452-8af482af5f39

# Build git-pages.
FROM docker.io/library/golang:1.25-alpine@sha256:aee43c3ccbf24fdffb7295693b6e33b21e01baec1b2a55acc351fde345e9ec34 AS git-pages-builder
RUN apk --no-cache add git
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY src/ ./src/
RUN go build -ldflags "-s -w" -o git-pages .

# Compose git-pages and Caddy.
FROM docker.io/library/busybox:1.37.0-musl@sha256:ef13e7482851632be3faf5bd1d28d4727c0810901d564b35416f309975a12a30
COPY --from=ca-certificates-builder /etc/ssl/cert.pem /etc/ssl/cert.pem
COPY --from=supervisor-builder /usr/bin/supervisord /bin/supervisord
COPY --from=caddy-builder /usr/bin/caddy /bin/caddy
COPY --from=git-pages-builder /build/git-pages /bin/git-pages

WORKDIR /app
RUN mkdir /app/data
COPY conf/supervisord.conf /app/supervisord.conf
COPY conf/Caddyfile /app/Caddyfile
COPY conf/config.example.toml /app/config.toml

# Caddy ports:
EXPOSE 80/tcp 443/tcp 443/udp
# git-pages ports:
EXPOSE 3000/tcp 3001/tcp 3002/tcp

# While the default command is to run git-pages standalone, the intended configuration
# is to use it with Caddy and store both site data and credentials to an S3-compatible
# object store.
#  * In a standalone configuration, the default, git-caddy listens on port 3000 (http).
#  * In a combined configuration, supervisord launches both git-caddy and Caddy, and
#    Caddy listens on ports 80 (http) and 443 (https).
CMD ["git-pages"]
# CMD ["supervisord"]
