package internal

import (
	"context"
	"io"
	"log/slog"
	"os"

	"gopkg.in/natefinch/lumberjack.v2"
)

type MultiHandler struct {
	handlers []slog.Handler
}

func NewMultiHandler(handlers ...slog.Handler) *MultiHandler {
	return &MultiHandler{handlers: handlers}
}

func (m *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newHandlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		newHandlers[i] = h.WithAttrs(attrs)
	}
	return NewMultiHandler(newHandlers...)
}

func (m *MultiHandler) WithGroup(name string) slog.Handler {
	newHandlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		newHandlers[i] = h.WithGroup(name)
	}
	return NewMultiHandler(newHandlers...)
}

// SetupLogging configures slog to emit Info+ records to stdout (text) and
// Debug+ records to a size-rotating JSON log file at logPath. The file rotates
// once it reaches 1MB; up to 10 rotated backups are retained for 90 days, left
// uncompressed so the archived logs stay greppable on disk. It returns an
// io.Closer that flushes and closes the file writer, which the caller should
// close on shutdown.
func SetupLogging(logPath string) io.Closer {
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

	multiHandler := NewMultiHandler(consoleHandler, fileHandler)

	logger := slog.New(multiHandler)
	slog.SetDefault(logger)

	return fileWriter
}
