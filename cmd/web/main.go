package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"live-transcript-server/internal"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Set via environment variables VERSION and BUILD_TIME
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
	if v := os.Getenv("VERSION"); v != "" {
		Version = v
	}
	if bt := os.Getenv("BUILD_TIME"); bt != "" {
		BuildTime = bt
	}

	// --- Logging Setup ---
	if err := os.MkdirAll(filepath.Join("tmp", "_logs"), 0755); err != nil {
		fmt.Printf("failed to create log directory: %v\n", err)
	}

	timestamp := time.Now().Format("2006-01-02_15-04-05")
	fileName := fmt.Sprintf("%s-server.log", timestamp)
	filePath := filepath.Join("tmp", "_logs", fileName)

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
	app := internal.NewApp(config, db, "tmp", Version, BuildTime)
	mux := http.NewServeMux()
	app.RegisterRoutes(mux)

	// --- Global Endpoints ---
	mux.HandleFunc("GET /healthcheck", healthcheckHandler)
	mux.HandleFunc("GET /version", versionHandler)
	mux.Handle("GET /metrics", promhttp.Handler())

	// Start background tasks
	app.StartMaintenanceLoop()

	// Start Discord bot (no-op if not configured)
	if app.DiscordBot != nil {
		if err := app.DiscordBot.Start(); err != nil {
			slog.Error("failed to start discord bot", "func", "main", "err", err)
		}
	}

	corsHandler := internal.CorsMiddleware(mux)
	server := &http.Server{
		Addr:              ":8080",
		Handler:           corsHandler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	slog.Info("WebSocket server listening on port 8080", "func", "main")

	// --- Signal Handling ---
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Start server in its own goroutine
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("unable to start WebSocket server", "func", "main", "err", err)
			os.Exit(1)
		}
	}()

	// Wait for signal
	<-stop

	slog.Info("shutting down server...", "func", "main")

	// --- Shutdown Sequence ---
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	// Shutdown HTTP Server first
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("failed to shutdown HTTP server", "func", "main", "err", err)
	}

	// Close Discord bot connection
	if app.DiscordBot != nil {
		if err := app.DiscordBot.Close(); err != nil {
			slog.Error("failed to close discord bot", "func", "main", "err", err)
		}
	}

	// Close App and DB
	if err := app.Close(); err != nil {
		slog.Error("failed to close app", "func", "main", "err", err)
	}

	slog.Info("server exited cleanly", "func", "main")
}
