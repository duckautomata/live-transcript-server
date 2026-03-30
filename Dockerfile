# 1. Build Stage
FROM golang:1.26-alpine AS gobuilder
WORKDIR /app

# Install build tools (required for CGO on Alpine)
RUN apk add --no-cache build-base

COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=1 GOOS=linux go build -o ./bin/main -ldflags="-w -s" ./cmd/web

# 2. Run Stage
FROM alpine:latest

# Install dependencies + curl for healthcheck
RUN apk add --no-cache ffmpeg ca-certificates tzdata curl

WORKDIR /app

# Create a non-root user (UID 10001)
RUN adduser \
    --disabled-password \
    --gecos "" \
    --home "/nonexistent" \
    --shell "/sbin/nologin" \
    --no-create-home \
    --uid 10001 \
    appuser && \
    mkdir -p /app/tmp && \
    chown -R 10001:0 /app/tmp && \
    chmod -R g=u /app/tmp

# Should stay at the very bottom to prevent any cache breaks.
COPY --from=gobuilder --chown=10001:0 /app/bin/main .

VOLUME ["/app/tmp"]
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD curl -f http://localhost:8080/healthcheck || exit 1

USER 10001
EXPOSE 8080

ARG VERSION="unknown"
ARG BUILD_TIME="unknown"
ENV VERSION=${VERSION}
ENV BUILD_TIME=${BUILD_TIME}

CMD ["./main"]
