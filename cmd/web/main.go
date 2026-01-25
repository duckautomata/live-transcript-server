package main

import (
	"encoding/json"
	"errors"
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

func versionHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]string{
		"version":   Version,
		"buildTime": BuildTime,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("failed to write /version JSON response", "err", err)
	}
}

func main() {
	// --- Logging Setup ---
	if err := os.MkdirAll("tmp", 0755); err != nil {
		fmt.Printf("failed to create log directory: %v\n", err)
	}

	timestamp := time.Now().Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("%s-server.log", timestamp)
	filePath := filepath.Join("tmp", fileName)

	// Open the log file.
	logFile, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		fmt.Printf("unable to open log file: %v\n", err)
	}
	defer logFile.Close()

	internal.SetupLogging(logFile)

	slog.Info("server starting up", "version", Version, "build_time", BuildTime)

	// --- App Setup ---
	config, err := internal.GetConfig()
	if err != nil {
		slog.Error("unable to read in config", "func", "main", "err", err)
		os.Exit(1)
	}

	// --- Database Setup ---
	dbPath := filepath.Join("tmp", "server.db")
	db, err := internal.InitDB(dbPath, config.Database)
	if err != nil {
		slog.Error("unable to initialize database", "func", "main", "path", dbPath, "err", err)
		os.Exit(1)
	}
	defer db.Close()
	app := internal.NewApp(config.Credentials.ApiKey, db, config.Channels, config.Storage, "tmp")
	mux := http.NewServeMux()
	app.RegisterRoutes(mux)

	// --- Global Endpoints ---
	mux.HandleFunc("GET /healthcheck", healthcheckHandler)
	mux.HandleFunc("GET /version", versionHandler)
	mux.Handle("GET /metrics", promhttp.Handler())

	// Start background tasks
	app.StartReconciliationLoop()

	corsHandler := internal.CorsMiddleware(mux)
	server := &http.Server{
		Addr:              ":8080",
		Handler:           corsHandler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	slog.Info("WebSocket server listening on port 8080", "func", "main")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("unable to start WebSocket server", "func", "main", "err", err)
		os.Exit(1)
	}
}
