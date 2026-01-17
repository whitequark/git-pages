# Install CA certificates.
FROM docker.io/library/alpine:3 AS ca-certificates-builder
RUN apk --no-cache add ca-certificates

# Build supervisor.
FROM docker.io/library/golang:1.25-alpine@sha256:e6898559d553d81b245eb8eadafcb3ca38ef320a9e26674df59d4f07a4fd0b07 AS supervisor-builder
RUN apk --no-cache add git
WORKDIR /build
RUN git clone https://github.com/ochinchina/supervisord . && \
    git checkout 16cb640325b3a4962b2ba17d68fb5c2b1e1b6b3c
RUN GOBIN=/usr/bin go install -ldflags "-s -w"

# Build Caddy with S3 storage backend.
FROM docker.io/library/caddy:2.10.2-builder@sha256:b6424b4a90e25fde5cb9fd8e1da716159a313869ac3ba1c34b11c50781acab81 AS caddy-builder
RUN xcaddy build ${CADDY_VERSION} \
    --with=github.com/ss098/certmagic-s3@v0.0.0-20250922022452-8af482af5f39

# Build git-pages.
FROM docker.io/library/golang:1.25-alpine@sha256:e6898559d553d81b245eb8eadafcb3ca38ef320a9e26674df59d4f07a4fd0b07 AS git-pages-builder
RUN apk --no-cache add git
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY src/ ./src/
RUN go build -ldflags "-s -w" -o git-pages .

# Compose git-pages and Caddy.
FROM docker.io/library/busybox:1.37.0-musl@sha256:03db190ed4c1ceb1c55d179a0940e2d71d42130636a780272629735893292223
COPY --from=ca-certificates-builder /etc/ssl/cert.pem /etc/ssl/cert.pem
COPY --from=supervisor-builder /usr/bin/supervisord /bin/supervisord
COPY --from=caddy-builder /usr/bin/caddy /bin/caddy
COPY --from=git-pages-builder /build/git-pages /bin/git-pages

WORKDIR /app
RUN mkdir /app/data
COPY conf/supervisord.conf /app/supervisord.conf
COPY conf/Caddyfile /app/Caddyfile
COPY conf/config.docker.toml /app/config.toml

# Caddy ports:
EXPOSE 80/tcp 443/tcp 443/udp
# git-pages ports:
EXPOSE 3000/tcp 3001/tcp 3002/tcp

# While the default command is to run git-pages standalone, the intended configuration
# is to use it with Caddy and store both site data and credentials to an S3-compatible
# object store.
#  * In a standalone configuration, the default, git-pages listens on port 3000 (http).
#  * In a combined configuration, supervisord launches both git-pages and Caddy, and
#    Caddy listens on ports 80 (http) and 443 (https).
CMD ["git-pages"]
# CMD ["supervisord"]
