// Package logging configures the process-wide slog logger: Info+ text output
// to stdout plus Debug+ JSON output to a size-rotated log file.
package logging

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"

	"gopkg.in/natefinch/lumberjack.v2"
)

// multiHandler fans a record out to every handler that accepts its level.
type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newHandlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		newHandlers[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: newHandlers}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	newHandlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		newHandlers[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: newHandlers}
}

// Setup configures slog to emit Info+ records to stdout (text) and Debug+
// records to a size-rotating JSON log file at logPath. The file rotates once
// it reaches 1MB; up to 10 rotated backups are retained for 90 days, left
// uncompressed so the archived logs stay greppable on disk. It returns an
// io.Closer that flushes and closes the file writer, which the caller should
// close on shutdown.
func Setup(logPath string) io.Closer {
	fileWriter := &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    1,    // megabytes; rotate when the log file reaches 1MB
		MaxBackups: 10,   // keep up to 10 rotated files
		MaxAge:     90,   // days to retain rotated files
		LocalTime:  true, // timestamp backups in local time
	}

	consoleHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})

	fileHandler := slog.NewJSONHandler(fileWriter, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})

	slog.SetDefault(slog.New(&multiHandler{handlers: []slog.Handler{consoleHandler, fileHandler}}))

	return fileWriter
}
