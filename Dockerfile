FROM golang:1.25 AS gobuilder
WORKDIR /app

# Download Go modules and build app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Add ARGs for build time variables
ARG VERSION=dev
ARG BUILD_TIME=unknown
# CGO_ENABLED=1 is default on non-alpine, but good to be explicit
RUN CGO_ENABLED=1 GOOS=linux go build -o ./bin/main -ldflags="-w -s -X main.Version=${VERSION} -X main.BuildTime=${BUILD_TIME}" ./cmd/web

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y ffmpeg ca-certificates tzdata && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=gobuilder /app/bin/main .

RUN mkdir -p /app/tmp && chown -R 1000:1000 /app
VOLUME /app/tmp
USER 1000

EXPOSE 8080
CMD ["./main"]
