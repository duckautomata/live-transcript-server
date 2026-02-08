package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func BenchmarkSync(b *testing.B) {
	// Setup with in-memory DB
	key := "bench-channel"
	app, mux, db := setupTestApp(&testing.T{}, []string{key})
	defer db.Close()
	ctx := context.Background()

	// 1. Generate heavy data
	streamID := "bench-stream"
	app.UpsertStream(ctx, &Stream{ChannelID: key, StreamID: streamID, IsLive: true})

	lines := make([]Line, 10000)
	for i := range 10000 {
		lines[i] = Line{
			ID:        i,
			Timestamp: i * 100,
			Segments:  json.RawMessage(fmt.Sprintf(`[{"text": "Line %d content is here and it might be long"}]`, i)),
		}
	}
	app.ReplaceTranscript(ctx, key, streamID, lines)

	server := httptest.NewServer(mux)
	defer server.Close()

	u := "ws" + strings.TrimPrefix(server.URL, "http") + "/" + key + "/websocket"

	for b.Loop() {
		// Connect and measure time to sync
		ws, _, err := websocket.DefaultDialer.Dial(u, nil)
		if err != nil {
			b.Fatalf("dial failed: %v", err)
		}

		// Consume messages until sync
		for {
			var msg WebSocketMessage
			if err := ws.ReadJSON(&msg); err != nil {
				b.Fatalf("read failed: %v", err)
			}
			if msg.Event == EventSync {
				break
			}
		}
		ws.Close()
	}
}
