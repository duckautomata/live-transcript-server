package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"live-transcript-server/internal/config"
	"live-transcript-server/internal/model"
	"live-transcript-server/internal/store"
)

// setupTestApp creates a new App instance with an in-memory database for
// testing. It returns the App and the ServeMux (router). Accepting testing.TB
// lets benchmarks share the helper.
func setupTestApp(tb testing.TB, channels []string) (*App, *http.ServeMux) {
	tb.Helper()

	st, err := store.Open(":memory:", config.DatabaseConfig{SkipWarmup: true})
	if err != nil {
		tb.Fatalf("failed to init db: %v", err)
	}

	var channelConfigs []config.ChannelConfig
	for _, c := range channels {
		channelConfigs = append(channelConfigs, config.ChannelConfig{
			Name:           c,
			NumPastStreams: 1,
			AdminKey:       "admin-" + c, // deterministic per-channel admin key for tests
		})
	}
	cfg := config.Config{
		Credentials: config.Credentials{ApiKey: "test-api-key"},
		Channels:    channelConfigs,
		Storage:     config.StorageConfig{Type: "local"},
	}

	app, err := NewApp(cfg, st, tb.TempDir(), "test-version", "test-build-time")
	if err != nil {
		tb.Fatalf("failed to construct app: %v", err)
	}
	if err := app.Init(context.Background()); err != nil {
		tb.Fatalf("failed to init app: %v", err)
	}

	// Closing the app also closes the store.
	tb.Cleanup(func() {
		app.Close()
	})

	mux := http.NewServeMux()
	app.RegisterRoutes(mux)

	return app, mux
}

// seedExampleData populates the database with some initial data for testing.
func seedExampleData(tb testing.TB, app *App, channelID string) {
	tb.Helper()

	ctx := context.Background()

	stream := &model.Stream{
		ChannelID:   channelID,
		StreamID:    "stream-1",
		StreamTitle: "Test Stream Title",
		StartTime:   fmt.Sprintf("%d", time.Now().Unix()),
		IsLive:      true,
		MediaType:   "audio",
	}
	if err := app.Store.UpsertStream(ctx, stream); err != nil {
		tb.Fatalf("failed to insert stream: %v", err)
	}

	lines := []model.Line{
		{
			ID:        0,
			Timestamp: 100,
			Segments:  json.RawMessage(`[{"timestamp": 100, "text": "Hello world"}]`),
		},
		{
			ID:        1,
			Timestamp: 200,
			Segments:  json.RawMessage(`[{"timestamp": 200, "text": "This is a test"}]`),
		},
	}
	if err := app.Store.ReplaceTranscript(ctx, channelID, "stream-1", lines); err != nil {
		tb.Fatalf("failed to seed transcript: %v", err)
	}
}

// segment mirrors the worker's segment JSON for decoding line segments in
// assertions.
type segment struct {
	Timestamp int    `json:"timestamp"`
	Text      string `json:"text"`
}

// fakeProcessor is a media.Processor for tests: every operation just writes a
// small placeholder output file, and any hook that is set overrides that.
// Tests inject it via app.Media (replacing the old package-level Ffmpeg* var
// swapping, which couldn't survive the package split).
type fakeProcessor struct {
	convert func(in, out string) error
	remux   func(in, out string) error
	trim    func(in, out string, start, end float64) error
	frame   func(in, out string, height int) error
}

func writePlaceholder(out string) error {
	return os.WriteFile(out, []byte("converted"), 0644)
}

func (f fakeProcessor) Convert(in, out string) error {
	if f.convert != nil {
		return f.convert(in, out)
	}
	return writePlaceholder(out)
}

func (f fakeProcessor) Remux(in, out string) error {
	if f.remux != nil {
		return f.remux(in, out)
	}
	return writePlaceholder(out)
}

func (f fakeProcessor) Trim(in, out string, start, end float64) error {
	if f.trim != nil {
		return f.trim(in, out, start, end)
	}
	return writePlaceholder(out)
}

func (f fakeProcessor) ExtractFrame(in, out string, height int) error {
	if f.frame != nil {
		return f.frame(in, out, height)
	}
	return writePlaceholder(out)
}

// waitFor polls cond every 10ms until it returns true or the timeout elapses,
// failing the test on timeout. Replaces the hand-rolled poll loops the suite
// accumulated.
func waitFor(tb testing.TB, timeout time.Duration, what string, cond func() bool) {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	tb.Fatalf("timed out after %s waiting for %s", timeout, what)
}
