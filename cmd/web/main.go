package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"live-transcript-server/internal/config"
	"live-transcript-server/internal/logging"
	"live-transcript-server/internal/server"
	"live-transcript-server/internal/store"

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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"version":"` + Version + `","buildTime":"` + BuildTime + `"}`))
}

func main() {
	if v := os.Getenv("VERSION"); v != "" {
		Version = v
	}
	if bt := os.Getenv("BUILD_TIME"); bt != "" {
		BuildTime = bt
	}

	// --- Logging Setup ---
	// lumberjack creates the log directory and file lazily, then rotates the
	// file once it reaches 1MB.
	logPath := filepath.Join("tmp", "_logs", "server.log")
	logCloser := logging.Setup(logPath)
	defer logCloser.Close()

	slog.Info("========== SERVER START ==========")
	slog.Info("server starting up", "version", Version, "build_time", BuildTime)

	// --- App Setup ---
	cfg, err := config.Load("config.yaml")
	if err != nil {
		slog.Error("unable to read in config", "func", "main", "err", err)
		os.Exit(1)
	}

	// Fail closed: an empty API key would disable authentication on every
	// worker endpoint (activate/sync/line/media/...). Refuse to start rather
	// than silently expose them.
	if cfg.Credentials.ApiKey == "" {
		slog.Error("refusing to start: credentials.apiKey is empty; worker endpoints would be unauthenticated", "func", "main")
		os.Exit(1)
	}

	// --- Database Setup ---
	dbPath := filepath.Join("tmp", "server.db")
	st, err := store.Open(dbPath, cfg.Database)
	if err != nil {
		slog.Error("unable to initialize database", "func", "main", "path", dbPath, "err", err)
		os.Exit(1)
	}

	app, err := server.NewApp(cfg, st, "tmp", Version, BuildTime)
	if err != nil {
		slog.Error("unable to construct app", "func", "main", "err", err)
		os.Exit(1)
	}
	if err := app.Init(context.Background()); err != nil {
		slog.Error("unable to initialize app environment", "func", "main", "err", err)
		os.Exit(1)
	}

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

	httpServer := &http.Server{
		Addr:              ":8080",
		Handler:           server.CorsMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	slog.Info("WebSocket server listening on port 8080", "func", "main")

	// --- Signal Handling ---
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Start server in its own goroutine
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("unable to start WebSocket server", "func", "main", "err", err)
			os.Exit(1)
		}
	}()

	// Wait for signal
	<-stop

	slog.Info("========== SERVER STOP ==========")
	slog.Info("shutting down server...", "func", "main")

	// --- Shutdown Sequence ---
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	// Release parked long-polls so Shutdown doesn't wait for them
	app.Notifier.Release()

	// Shutdown HTTP Server first
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
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
