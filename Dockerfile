FROM golang:1.25-alpine AS gobuilder
WORKDIR /app

# We need gcc and libc-dev for CGo (required by mattn/go-sqlite3)
RUN apk add --no-cache gcc libc-dev

# Download Go modules and build app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Add ARGs for build time variables
ARG VERSION=dev
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=1 GOOS=linux go build -o ./bin/main -ldflags="-w -s -X main.Version=${VERSION} -X main.BuildTime=${BUILD_TIME}" ./cmd/web

FROM alpine:latest
RUN apk add --no-cache ffmpeg ca-certificates tzdata
WORKDIR /app
COPY --from=gobuilder /app/bin/main .

RUN mkdir -p /app/tmp && chown -R 1000:1000 /app
VOLUME /app/tmp
USER 1000

EXPOSE 8080
CMD ["./main"]
