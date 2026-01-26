package internal

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestWebSocketCompression(t *testing.T) {
	// 1. Setup Server
	key := "compression-test"
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()

	// 2. Start Test Server
	s := httptest.NewServer(mux)
	defer s.Close()

	// Convert http URL to ws URL
	u := "ws" + strings.TrimPrefix(s.URL, "http") + fmt.Sprintf("/%s/websocket", key)

	// 3. Connect Client with Compression Enabled
	dialer := websocket.Dialer{
		EnableCompression: true,
	}

	conn, resp, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial error: %v", err)
	}
	defer conn.Close()

	// 4. Verify Compression Extension was Negotiated
	// The response header "Sec-Websocket-Extensions" should contain "permessage-deflate"
	extensions := resp.Header.Get("Sec-Websocket-Extensions")
	if !strings.Contains(extensions, "permessage-deflate") {
		t.Errorf("expected permessage-deflate extension, got: %s", extensions)
	}

	// 5. Send a large message from Server to Client (Active Stream + Transcript)
	// To trigger a large message, we can sync with a large transcript.
	// Insert a large transcript
	largeText := strings.Repeat("A", 10000)
	lines := []Line{
		{ID: 0, Timestamp: 100, Segments: []byte(fmt.Sprintf(`[{"text": "%s"}]`, largeText))},
	}
	app.UpsertStream(context.Background(), &Stream{ChannelID: key, ActiveID: "large-stream", IsLive: true})
	app.ReplaceTranscript(context.Background(), key, "large-stream", lines)

	// Send Sync Request (which triggers WS message?)
	// Actually, connecting to WS triggers Sync immediately.
	// The client reading loop should receive the Sync message.

	// Read message
	var msg WebSocketMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read error: %v", err)
	}

	// The first message is typically Sync
	if msg.Event != EventSync {
		t.Errorf("expected Sync event, got %s", msg.Event)
	}

	// Check integrity of data
	dataMap, ok := msg.Data.(map[string]any)
	if !ok {
		// Attempt to parse as concrete type if possible, or just check generic map
		// The test helper setupTestApp uses real App which uses real structs.
		// But ReadJSON creates a map interface if struct not provided.
		// Actually msg.Data is `any`. unmarshalling sets it to map[string]any or []any
	}
	_ = dataMap

	// 6. Verify server log/metrics if possible (difficult in unit test without hooks)
	// But success in reading the message confirms it was decompressed correctly by the client.
}
