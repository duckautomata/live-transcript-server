package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebsocketConnection(t *testing.T) {
	key := "test-ws-channel"
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()

	seedExampleData(t, app, key)

	// Create test server
	server := httptest.NewServer(mux)
	defer server.Close()

	// Build WS URL
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/" + key + "/websocket"

	// Connect
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial websocket: %v", err)
	}
	defer ws.Close()

	// Expect HardRefresh (Sync) on connect
	var msg WebSocketMessage
	if err := ws.ReadJSON(&msg); err != nil {
		t.Fatalf("failed to read json: %v", err)
	}

	if msg.Event != EventSync {
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
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()

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
	var initMsg WebSocketMessage
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
	var msg WebSocketMessage
	if err := ws1.ReadJSON(&msg); err != nil {
		t.Fatalf("failed to read message: %v", err)
	}

	if msg.Event != EventNewLine {
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
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()

	// Lower max conn for test
	app.MaxConn = 1

	server := httptest.NewServer(mux)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/" + key + "/websocket"

	// Conn 1
	ws1, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial ws1: %v", err)
	}
	defer ws1.Close()

	// Conn 2 should fail
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Error("expected error dialing ws2, got nil")
	} else if resp != nil && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %v", resp.StatusCode)
	}
	// Note: Gorilla dialer returns error on non-200 handshake usually.
}

func TestWebsocketDoubleClose(t *testing.T) {
	key := "test-ws-double-close"
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()

	server := httptest.NewServer(mux)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/" + key + "/websocket"

	// Connect
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial ws: %v", err)
	}
	// We do NOT defer ws.Close() here because we want to manually close it via server side to test

	// Get ChannelState
	cs, ok := app.Channels[key]
	if !ok {
		t.Fatalf("channel state not found for key %s", key)
	}

	// Wait for connection to be registered (it happens in wsHandler goroutine)
	// We can loop check cs.ClientConnections
	// Simple retry loop
	registered := false
	for i := 0; i < 10; i++ {
		cs.ClientsLock.Lock()
		if cs.ClientConnections == 1 {
			registered = true
			cs.ClientsLock.Unlock()
			break
		}
		cs.ClientsLock.Unlock()
	}
	if !registered {
		t.Fatal("client did not register in time")
	}

	cs.ClientsLock.Lock()
	conn := cs.Clients[0]
	cs.ClientsLock.Unlock()

	// Initial check
	if cs.ClientConnections != 1 {
		t.Errorf("expected 1 connection, got %d", cs.ClientConnections)
	}

	// First Close
	err = cs.closeSocket(conn)
	if err != nil {
		t.Logf("first closeSocket returned error (might be expected if conn closed): %v", err)
	}

	// Check
	if cs.ClientConnections != 0 {
		t.Errorf("expected 0 connections after first close, got %d", cs.ClientConnections)
	}

	// Second Close (Duplicate)
	err = cs.closeSocket(conn)
	if err != nil {
		t.Logf("second closeSocket returned error: %v", err)
	}

	// Check again - should still be 0, NOT -1
	if cs.ClientConnections != 0 {
		t.Errorf("expected 0 connections after second close, got %d", cs.ClientConnections)
	}

	ws.Close() // Clean up client side
}

func TestWebsocketPingPong(t *testing.T) {
	key := "test-ws-ping-pong"
	_, mux, db := setupTestApp(t, []string{key})
	defer db.Close()

	server := httptest.NewServer(mux)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/" + key + "/websocket"

	// Connect
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial ws: %v", err)
	}
	defer ws.Close()

	// Read initial sync message
	var initMsg WebSocketMessage
	ws.ReadJSON(&initMsg)

	// Send Ping
	timestamp := 123456
	pingMsg := WebSocketMessage{
		Event: EventPing,
		Data: map[string]interface{}{
			"timestamp": timestamp,
		},
	}
	if err := ws.WriteJSON(pingMsg); err != nil {
		t.Fatalf("failed to write ping message: %v", err)
	}

	// Read Pong
	var msg WebSocketMessage
	if err := ws.ReadJSON(&msg); err != nil {
		t.Fatalf("failed to read message: %v", err)
	}

	if msg.Event != EventPong {
		t.Errorf("expected event pong, got: %s", msg.Event)
	}

	dataMap, ok := msg.Data.(map[string]interface{})
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

func TestIsClientDisconnectError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "Normal Closure",
			err:      &websocket.CloseError{Code: websocket.CloseNormalClosure},
			expected: true,
		},
		{
			name:     "Net Closed Connection",
			err:      net.ErrClosed,
			expected: true,
		},
		{
			name:     "Going Away",
			err:      &websocket.CloseError{Code: websocket.CloseGoingAway},
			expected: true,
		},
		{
			name:     "No Status Received",
			err:      &websocket.CloseError{Code: websocket.CloseNoStatusReceived},
			expected: true,
		},
		{
			name:     "Abnormal Closure (1006)",
			err:      &websocket.CloseError{Code: websocket.CloseAbnormalClosure},
			expected: true,
		},
		{
			name:     "Other Close Error",
			err:      &websocket.CloseError{Code: websocket.CloseProtocolError},
			expected: false,
		},
		{
			name:     "Generic Error",
			err:      errors.New("some random error"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isClientDisconnectError(tt.err); got != tt.expected {
				t.Errorf("isClientDisconnectError() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestWebsocketPartialSync(t *testing.T) {
	key := "test-ws-partial-sync"
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()

	// Seed with 150 lines
	ctx := context.Background()
	// Create stream
	stream := &Stream{
		ChannelID:   key,
		StreamID:    "stream-partial",
		StreamTitle: "Partial Sync Test",
		StartTime:   fmt.Sprintf("%d", time.Now().Unix()),
		IsLive:      true,
		MediaType:   "audio",
	}
	app.UpsertStream(ctx, stream)

	lines := make([]Line, 150)
	for i := 0; i < 150; i++ {
		lines[i] = Line{
			ID:        i,
			Timestamp: i * 1000,
			Segments:  json.RawMessage(fmt.Sprintf(`[{"timestamp": %d, "text": "Line %d"}]`, i*1000, i)),
		}
	}
	app.ReplaceTranscript(ctx, key, "stream-partial", lines)

	server := httptest.NewServer(mux)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/" + key + "/websocket"

	// Connect
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial ws: %v", err)
	}
	defer ws.Close()

	// 1. Expect Partial Sync
	var partialMsg WebSocketMessage
	if err := ws.ReadJSON(&partialMsg); err != nil {
		t.Fatalf("failed to read partial msg: %v", err)
	}
	if partialMsg.Event != EventPartialSync {
		t.Fatalf("expected event partialSync, got %s", partialMsg.Event)
	}

	partialData, ok := partialMsg.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map data for partial sync")
	}
	partialTranscript, ok := partialData["transcript"].([]interface{})
	if !ok {
		t.Fatalf("expected transcript array in partial sync data, got %T", partialData["transcript"])
	}
	if len(partialTranscript) != 100 {
		t.Errorf("expected 100 lines in partial sync, got %d", len(partialTranscript))
	}

	// Check the last line of partial sync is the actual last line (ID 149)
	lastLine := partialTranscript[99].(map[string]interface{})
	// JSON numbers are float64
	if id, ok := lastLine["id"].(float64); !ok || int(id) != 149 {
		t.Errorf("expected last line id 149, got %v", lastLine["id"])
	}

	// 2. Expect Full Sync
	var syncMsg WebSocketMessage
	if err := ws.ReadJSON(&syncMsg); err != nil {
		t.Fatalf("failed to read sync msg: %v", err)
	}
	if syncMsg.Event != EventSync {
		t.Fatalf("expected event sync, got %s", syncMsg.Event)
	}

	syncData, ok := syncMsg.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map data for sync")
	}
	fullTranscript, ok := syncData["transcript"].([]interface{})
	if !ok {
		t.Fatalf("expected transcript array")
	}
	if len(fullTranscript) != 150 {
		t.Errorf("expected 150 lines in full sync, got %d", len(fullTranscript))
	}
}
