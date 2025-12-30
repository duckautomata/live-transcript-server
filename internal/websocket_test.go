package internal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	req, _ := http.NewRequest("POST", server.URL+"/"+key+"/line", strings.NewReader(reqBody))
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
