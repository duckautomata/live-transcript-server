# Global default args, cachebust is used to force-refresh specific steps.
ARG CACHEBUST=1

# 1. Build Stage
FROM golang:1.25-alpine AS gobuilder
ARG CACHEBUST
ARG VERSION=dev
ARG BUILD_TIME=unknown
WORKDIR /app

# Install build tools (required for CGO on Alpine)
RUN echo "Cache bust: ${CACHEBUST}" && apk add --no-cache build-base

COPY go.mod go.sum ./
RUN echo "Cache bust: ${CACHEBUST}" && go mod download
COPY . .

RUN echo "Cache bust: ${CACHEBUST}" && \
    CGO_ENABLED=1 GOOS=linux go build -o ./bin/main \
    -ldflags="-w -s -X main.Version=${VERSION} -X main.BuildTime=${BUILD_TIME}" \
    ./cmd/web

# 2. Run Stage
FROM alpine:latest
ARG CACHEBUST

# Install dependencies + curl for healthcheck
RUN echo "Cache bust: ${CACHEBUST}" && apk add --no-cache ffmpeg ca-certificates tzdata curl

WORKDIR /app

# Create a non-root user (UID 10001)
RUN adduser \
    --disabled-password \
    --gecos "" \
    --home "/nonexistent" \
    --shell "/sbin/nologin" \
    --no-create-home \
    --uid 10001 \
    appuser

COPY --from=gobuilder /app/bin/main .

# PERMISSIONS SETUP:
# 1. Create the directory
# 2. Set ownership to User 10001 and Group 0 (Root Group)
# 3. Set permissions so the Group (0) has the same access as the User
RUN mkdir -p /app/tmp && \
    chown -R 10001:0 /app/tmp && \
    chmod -R g=u /app/tmp

# Define volume
VOLUME ["/app/tmp"]

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD curl -f http://localhost:8080/healthcheck || exit 1

# Switch to the non-root user
USER 10001

EXPOSE 8080
CMD ["./main"]
