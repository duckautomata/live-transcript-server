FROM golang:latest AS gobuilder
WORKDIR /app

# Download Go modules and build app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o ./bin/main -ldflags="-w -s" ./cmd/web

FROM alpine:latest
RUN apk add --no-cache ffmpeg
WORKDIR /app
COPY --from=gobuilder /app/bin/main .

RUN mkdir -p /app/tmp && chown -R 1000:1000 /app
VOLUME /app/tmp
USER 1000

EXPOSE 8080
CMD ["./main"]
