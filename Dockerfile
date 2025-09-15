FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY src/ ./src/
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o git-pages ./src
FROM alpine:latest
RUN apk --no-cache add ca-certificates git
RUN addgroup -g 1000 -S appuser && \
    adduser -u 1000 -S appuser -G appuser

WORKDIR /app
COPY --from=builder /app/git-pages .

USER appuser
EXPOSE 3333
CMD ["./git-pages"]
