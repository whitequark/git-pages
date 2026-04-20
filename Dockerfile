# Install CA certificates.
FROM docker.io/library/alpine:3 AS ca-certificates-builder
RUN apk --no-cache add ca-certificates

# Build supervisor.
FROM docker.io/library/golang:1.26-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS supervisor-builder
RUN apk --no-cache add git
WORKDIR /build
RUN git clone https://github.com/ochinchina/supervisord . && \
    git checkout 16cb640325b3a4962b2ba17d68fb5c2b1e1b6b3c
RUN GOBIN=/usr/bin go install -ldflags "-s -w"

# Build Caddy with S3 storage backend.
FROM docker.io/library/caddy:2.11.2-builder@sha256:10ed0251c5cd1dbb4db0b71ad43121147961a51adfec35febce2c93ea25c24f4 AS caddy-builder
RUN xcaddy build ${CADDY_VERSION} \
    --with=github.com/ss098/certmagic-s3@v0.0.0-20250922022452-8af482af5f39

# Build git-pages.
FROM docker.io/library/golang:1.26-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS git-pages-builder
RUN apk --no-cache add git
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY src/ ./src/
RUN go build -ldflags "-s -w" -o git-pages .

# Compose git-pages and Caddy.
FROM docker.io/library/busybox:1.37.0-musl@sha256:19b646668802469d968a05342a601e78da4322a414a7c09b1c9ee25165042138
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
