package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"live-transcript-server/internal/model"
	"live-transcript-server/internal/ws"

	"github.com/gorilla/websocket"
)

func TestWebsocketConnection(t *testing.T) {
	key := "test-ws-channel"
	app, mux := setupTestApp(t, []string{key})

	seedExampleData(t, app, key)

	// Create test server
	server := httptest.NewServer(mux)
	defer server.Close()

	// Build WS URL
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/" + key + "/websocket"

	// Connect
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial websocket: %v", err)
	}
	defer conn.Close()

	// Expect HardRefresh (Sync) on connect
	var msg ws.Message
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("failed to read json: %v", err)
	}

	if msg.Event != ws.EventSync {
		t.Errorf("expected event sync, got %s", msg.Event)
	}

	dataMap, ok := msg.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected data to be map, got %T", msg.Data)
	}

	// Check transcript key exists
	if _, ok := dataMap["transcript"]; !ok {
		t.Error("expected transcript field in data")
	}
}

func TestWebsocketBroadcast(t *testing.T) {
	key := "test-ws-broadcast"
	app, mux := setupTestApp(t, []string{key})

	seedExampleData(t, app, key)

	server := httptest.NewServer(mux)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/" + key + "/websocket"

	// Connect Client 1
	ws1, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial ws1: %v", err)
	}
	defer ws1.Close()

	// Read initial message
	var initMsg ws.Message
	ws1.ReadJSON(&initMsg)

	reqBody := `{"id": 2, "timestamp": 1000, "segments": [{"timestamp": 1000, "text": "Broadcast Test"}]}`
	req, _ := http.NewRequest("POST", server.URL+"/"+key+"/line/stream-1", strings.NewReader(reqBody))
	req.Header.Set("X-API-Key", app.ApiKey)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("failed to post line: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("post line failed: %v", resp.Status)
	}

	// Client 1 should receive the update
	var msg ws.Message
	if err := ws1.ReadJSON(&msg); err != nil {
		t.Fatalf("failed to read message: %v", err)
	}

	if msg.Event != ws.EventNewLine {
		t.Errorf("expected event newLine, got: %s", msg.Event)
	}

	dataMap, ok := msg.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected data to be map, got %T", msg.Data)
	}

	// Check segments
	if _, ok := dataMap["segments"]; !ok {
		t.Error("expected segments field in data")
	}
}

func TestWebsocketMaxConnections(t *testing.T) {
	key := "test-ws-max"
	app, mux := setupTestApp(t, []string{key})

	// Lower max conn for test by swapping in a capped hub.
	app.Channels[key].Hub = ws.NewHub(key, 1)

	server := httptest.NewServer(mux)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/" + key + "/websocket"

	// Conn 1
	ws1, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial ws1: %v", err)
	}
	defer ws1.Close()

	// Wait for connection to be registered
	cs := app.Channels[key]
	waitFor(t, time.Second, "client 1 to register", func() bool {
		return cs.Hub.Connections() == 1
	})

	// Conn 2 should fail
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Error("expected error dialing ws2, got nil")
	} else if resp != nil && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %v", resp.StatusCode)
	}
	// Note: Gorilla dialer returns error on non-200 handshake usually.
}

func TestWebsocketDisconnectReleasesSlot(t *testing.T) {
	key := "test-ws-double-close"
	app, mux := setupTestApp(t, []string{key})

	server := httptest.NewServer(mux)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/" + key + "/websocket"

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial ws: %v", err)
	}

	cs := app.Channels[key]
	waitFor(t, time.Second, "client to register", func() bool {
		return cs.Hub.ClientCount() == 1
	})
	if got := cs.Hub.Connections(); got != 1 {
		t.Errorf("expected 1 connection, got %d", got)
	}

	// Client disconnect must release both the client entry and the cap slot.
	// (Idempotency of the underlying Remove is covered in the ws package.)
	conn.Close()
	waitFor(t, time.Second, "slot to be released", func() bool {
		return cs.Hub.Connections() == 0 && cs.Hub.ClientCount() == 0
	})
}

func TestWebsocketPingPong(t *testing.T) {
	key := "test-ws-ping-pong"
	_, mux := setupTestApp(t, []string{key})

	server := httptest.NewServer(mux)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/" + key + "/websocket"

	// Connect
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial ws: %v", err)
	}
	defer conn.Close()

	// Read initial sync message
	var initMsg ws.Message
	conn.ReadJSON(&initMsg)

	// Send Ping
	timestamp := 123456
	pingMsg := ws.Message{
		Event: ws.EventPing,
		Data: map[string]any{
			"timestamp": timestamp,
		},
	}
	if err := conn.WriteJSON(pingMsg); err != nil {
		t.Fatalf("failed to write ping message: %v", err)
	}

	// Read Pong
	var msg ws.Message
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("failed to read message: %v", err)
	}

	if msg.Event != ws.EventPong {
		t.Errorf("expected event pong, got: %s", msg.Event)
	}

	dataMap, ok := msg.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected data to be map, got %T", msg.Data)
	}

	ts, ok := dataMap["timestamp"].(float64)
	if !ok {
		t.Errorf("expected timestamp in data")
	}

	if int(ts) != timestamp {
		t.Errorf("expected timestamp %d, got %d", timestamp, int(ts))
	}
}

func TestWebsocketPartialSync(t *testing.T) {
	key := "test-ws-partial-sync"
	app, mux := setupTestApp(t, []string{key})

	// Seed with 150 lines
	ctx := context.Background()
	// Create stream
	stream := &model.Stream{
		ChannelID:   key,
		StreamID:    "stream-partial",
		StreamTitle: "Partial Sync Test",
		StartTime:   fmt.Sprintf("%d", time.Now().Unix()),
		IsLive:      true,
		MediaType:   "audio",
	}
	app.Store.UpsertStream(ctx, stream)

	lines := make([]model.Line, 150)
	for i := range 150 {
		lines[i] = model.Line{
			ID:        i,
			Timestamp: i * 1000,
			Segments:  json.RawMessage(fmt.Sprintf(`[{"timestamp": %d, "text": "Line %d"}]`, i*1000, i)),
		}
	}
	app.Store.ReplaceTranscript(ctx, key, "stream-partial", lines)

	server := httptest.NewServer(mux)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/" + key + "/websocket"

	// Connect
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial ws: %v", err)
	}
	defer conn.Close()

	// 1. Expect Partial Sync
	var partialMsg ws.Message
	if err := conn.ReadJSON(&partialMsg); err != nil {
		t.Fatalf("failed to read partial msg: %v", err)
	}
	if partialMsg.Event != ws.EventPartialSync {
		t.Fatalf("expected event partialSync, got %s", partialMsg.Event)
	}

	partialData, ok := partialMsg.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map data for partial sync")
	}
	partialTranscript, ok := partialData["transcript"].([]any)
	if !ok {
		t.Fatalf("expected transcript array in partial sync data, got %T", partialData["transcript"])
	}
	if len(partialTranscript) != 100 {
		t.Errorf("expected 100 lines in partial sync, got %d", len(partialTranscript))
	}

	// Check the last line of partial sync is the actual last line (ID 149)
	lastLine := partialTranscript[99].(map[string]any)
	// JSON numbers are float64
	if id, ok := lastLine["id"].(float64); !ok || int(id) != 149 {
		t.Errorf("expected last line id 149, got %v", lastLine["id"])
	}

	// 2. Expect Full Sync
	var syncMsg ws.Message
	if err := conn.ReadJSON(&syncMsg); err != nil {
		t.Fatalf("failed to read sync msg: %v", err)
	}
	if syncMsg.Event != ws.EventSync {
		t.Fatalf("expected event sync, got %s", syncMsg.Event)
	}

	syncData, ok := syncMsg.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map data for sync")
	}
	fullTranscript, ok := syncData["transcript"].([]any)
	if !ok {
		t.Fatalf("expected transcript array")
	}
	if len(fullTranscript) != 150 {
		t.Errorf("expected 150 lines in full sync, got %d", len(fullTranscript))
	}
}
