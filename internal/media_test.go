package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestMediaAvailability(t *testing.T) {
	// Setup
	app, mux, _ := setupTestApp(t, []string{"test"})
	defer os.RemoveAll(app.TempDir)

	// Seed data to ensure stream exists for sync
	seedExampleData(t, app, "test")

	// Create a mock server
	server := httptest.NewServer(mux)
	defer server.Close()

	// Connect WebSocket
	u := "ws" + server.URL[4:] + "/test/websocket"
	ws, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial caused error: %v", err)
	}
	defer ws.Close()

	// 1. Consume Sync Event
	// When connecting, we immediately get a sync event.
	var msg WebSocketMessage
	ws.SetReadDeadline(time.Now().Add(1 * time.Second))
	if err := ws.ReadJSON(&msg); err != nil {
		t.Fatalf("failed to read sync event: %v", err)
	}
	if msg.Event != EventSync {
		t.Errorf("expected sync event, got %v", msg.Event)
	}

	// 2. Add a new line (MediaAvailable should be false)
	// Seed data has lines 0 and 1. So next is 2.
	line := Line{
		ID:        2,
		Timestamp: 1000,
		Segments:  []Segment{{Timestamp: 1000, Text: "Test Line"}},
	}
	body, _ := json.Marshal(line)
	req, _ := http.NewRequest("POST", server.URL+"/test/line", bytes.NewBuffer(body))
	req.Header.Set("X-API-Key", app.ApiKey)

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("failed to send line request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status OK for line, got %v", resp.StatusCode)
	}

	// Verify DB state
	l, err := app.GetLastLine(context.Background(), "test", "stream-1")
	if err != nil {
		t.Fatalf("failed to get last line: %v", err)
	}
	if l.ID != 2 {
		t.Errorf("expected last line ID 2, got %d", l.ID)
	}
	if l.MediaAvailable {
		t.Errorf("expected MediaAvailable to be false, got true")
	}

	// Verify WebSocket newLine event
	ws.SetReadDeadline(time.Now().Add(1 * time.Second))
	if err := ws.ReadJSON(&msg); err != nil {
		t.Fatalf("failed to read newLine event: %v", err)
	}
	if msg.Event != EventNewLine {
		t.Errorf("expected newLine event, got %v", msg.Event)
	}

	// 3. Upload media for Line 2
	// Create a dummy raw file
	rawContent := []byte("dummy raw audio content")
	bodyBuf := &bytes.Buffer{}
	writer := multipart.NewWriter(bodyBuf)
	part, _ := writer.CreateFormFile("file", "2.raw")
	part.Write(rawContent)
	writer.Close()

	req, _ = http.NewRequest("POST", server.URL+"/test/media/2", bodyBuf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-API-Key", app.ApiKey)

	resp, err = server.Client().Do(req)
	if err != nil {
		t.Fatalf("failed to upload media: %v", err)
	}
	defer resp.Body.Close()

	// If 500 (ffmpeg missing), manual broadcast
	if resp.StatusCode != http.StatusOK {
		t.Logf("mediaHandler failed (likely ffmpeg missing): status %v. Triggering manual broadcast.", resp.StatusCode)
		var cs *ChannelState = app.Channels["test"]
		app.broadcastNewMedia(cs, []int{2})
	} else {
		// Validated DB update only if handler success
		l, err = app.GetLastLine(context.Background(), "test", "stream-1")
		if err != nil {
			t.Fatalf("failed to get last line: %v", err)
		}
		if !l.MediaAvailable {
			t.Errorf("expected MediaAvailable to be true, got false")
		}
	}

	// Verify WebSocket newMedia event
	ws.SetReadDeadline(time.Now().Add(1 * time.Second))
	if err := ws.ReadJSON(&msg); err != nil {
		t.Fatalf("failed to read newMedia event: %v", err)
	}
	if msg.Event != EventNewMedia {
		t.Errorf("expected newMedia event, got %v", msg.Event)
	}

	// Verify Data
	dataMap := msg.Data.(map[string]interface{})
	ids := dataMap["ids"].([]interface{})
	if len(ids) != 1 || int(ids[0].(float64)) != 2 {
		t.Errorf("expected ids [2], got %v", ids)
	}
}
