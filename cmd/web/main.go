package main

import (
	"fmt"
	"live-transcript-server/internal"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// To be set via ldflags
var (
	Version   = "local"
	BuildTime = "unknown"
)

func healthcheckHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"alive": true}`))
}

func main() {
	if err := os.MkdirAll("tmp", 0755); err != nil {
		slog.Error("failed to create log directory", "func", "main", "path", "tmp", "err", err)
	}

	timestamp := time.Now().Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("%s-server.log", timestamp)
	filePath := filepath.Join("tmp", fileName)

	// Open the log file.
	logFile, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		slog.Error("unable to open log file", "func", "main", "path", filePath, "err", err)
	}
	defer logFile.Close()

	internal.SetupLogging(logFile)

	slog.Info("server starting up", "version", Version, "build_time", BuildTime)

	config, err := internal.GetConfig()
	if err != nil {
		slog.Error("unable to read in config", "func", "main", "err", err)
	}

	servers := make([]*internal.WebSocketServer, len(config.Channels))

	for _, channel := range config.Channels {
		server := internal.NewWebSocketServer(channel, config.Credentials.ApiKey)
		server.Initialize(http.HandleFunc)
		servers = append(servers, server)
	}

	http.HandleFunc("/healthcheck", healthcheckHandler)
	http.Handle("/metrics", promhttp.Handler())

	slog.Info("WebSocket server listening on port 8080", "func", "main")
	err = http.ListenAndServe(":8080", nil)
	if err != nil {
		slog.Error("unable to start WebSocket server", "func", "main", "err", err)
	}
}
