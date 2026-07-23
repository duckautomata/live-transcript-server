package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"live-transcript-server/internal/model"
	"live-transcript-server/internal/ws"

	"github.com/gorilla/websocket"
)

func BenchmarkSync(b *testing.B) {
	// Setup with in-memory DB
	key := "bench-channel"
	app, mux := setupTestApp(b, []string{key})
	ctx := context.Background()

	// 1. Generate heavy data
	streamID := "bench-stream"
	app.Store.UpsertStream(ctx, &model.Stream{ChannelID: key, StreamID: streamID, IsLive: true})

	lines := make([]model.Line, 10000)
	for i := range 10000 {
		lines[i] = model.Line{
			ID:        i,
			Timestamp: i * 100,
			Segments:  json.RawMessage(fmt.Sprintf(`[{"text": "Line %d content is here and it might be long"}]`, i)),
		}
	}
	app.Store.ReplaceTranscript(ctx, key, streamID, lines)

	server := httptest.NewServer(mux)
	defer server.Close()

	u := "ws" + strings.TrimPrefix(server.URL, "http") + "/" + key + "/websocket"

	for b.Loop() {
		// Connect and measure time to sync
		conn, _, err := websocket.DefaultDialer.Dial(u, nil)
		if err != nil {
			b.Fatalf("dial failed: %v", err)
		}

		// Consume messages until sync
		for {
			var msg ws.Message
			if err := conn.ReadJSON(&msg); err != nil {
				b.Fatalf("read failed: %v", err)
			}
			if msg.Event == ws.EventSync {
				break
			}
		}
		conn.Close()
	}
}
