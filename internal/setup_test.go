package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// setupTestApp creates a new App instance with an in-memory database for testing.
// It returns the App, the ServeMux (router), and the *sql.DB connection.
// The caller is responsible for closing the db.
func setupTestApp(t *testing.T, channels []string) (*App, *http.ServeMux, *sql.DB) {
	t.Helper()

	// Use in-memory database
	db, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}

	apiKey := "test-api-key"
	var channelConfigs []ChannelConfig
	for _, c := range channels {
		channelConfigs = append(channelConfigs, ChannelConfig{Name: c, NumPastStreams: 1})
	}
	app := NewApp(apiKey, db, channelConfigs, StorageConfig{Type: "local"}, t.TempDir())

	mux := http.NewServeMux()
	app.RegisterRoutes(mux)

	return app, mux, db
}

// seedExampleData populates the database with some initial data for testing.
func seedExampleData(t *testing.T, app *App, channelID string) {
	t.Helper()

	ctx := context.Background()

	// Insert a stream
	stream := &Stream{
		ChannelID:   channelID,
		ActiveID:    "stream-1",
		ActiveTitle: "Test Stream Title",
		StartTime:   fmt.Sprintf("%d", time.Now().Unix()),
		IsLive:      true,
		MediaType:   "audio",
	}
	if err := app.UpsertStream(ctx, stream); err != nil {
		t.Fatalf("failed to insert stream: %v", err)
	}

	// Insert some transcript lines
	lines := []Line{
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
	if err := app.ReplaceTranscript(ctx, channelID, "stream-1", lines); err != nil {
		t.Fatalf("failed to seed transcript: %v", err)
	}
}

// setupTestDB creates a standalone in-memory DB for tests that don't need the full App.
func setupTestDB(t *testing.T) *App {
	t.Helper()
	db, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	return &App{DB: db}
}
