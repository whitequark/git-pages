# Install CA certificates.
FROM docker.io/library/alpine:latest@sha256:865b95f46d98cf867a156fe4a135ad3fe50d2056aa3f25ed31662dff6da4eb62 AS ca-certificates-builder
RUN apk --no-cache add ca-certificates

# Build supervisor.
FROM docker.io/library/golang:1.25-alpine@sha256:ac09a5f469f307e5da71e766b0bd59c9c49ea460a528cc3e6686513d64a6f1fb AS supervisor-builder
RUN apk --no-cache add git
WORKDIR /build
RUN git clone https://github.com/ochinchina/supervisord . && \
    git checkout 16cb640325b3a4962b2ba17d68fb5c2b1e1b6b3c
RUN GOBIN=/usr/bin go install -ldflags "-s -w"

# Build Caddy with S3 storage backend.
FROM docker.io/library/caddy:2.10.2-builder@sha256:6644af24bde2b4dbb07eb57637051abd2aa713e9787fa1eb544c3f31a0620898 AS caddy-builder
RUN xcaddy build ${CADDY_VERSION} \
    --with=github.com/ss098/certmagic-s3@v0.0.0-20250922022452-8af482af5f39

# Build git-pages.
FROM docker.io/library/golang:1.25-alpine@sha256:ac09a5f469f307e5da71e766b0bd59c9c49ea460a528cc3e6686513d64a6f1fb AS git-pages-builder
RUN apk --no-cache add git
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY src/ ./src/
RUN go build -ldflags "-s -w" -o git-pages .

# Compose git-pages and Caddy.
FROM docker.io/library/busybox:1.37.0-musl@sha256:b259afe60d4b88dbdb31908ca9524ef5308afd01aea17f4ce44ddb3c6a882929
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
