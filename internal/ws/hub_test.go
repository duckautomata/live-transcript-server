package ws

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newTestConn upgrades a real WebSocket connection over an httptest server and
// returns both ends. Cleanup closes the client side; the server side is closed
// by Hub.Remove in the tests.
func newTestConn(t *testing.T) (serverConn, clientConn *websocket.Conn) {
	t.Helper()

	upgrader := websocket.Upgrader{}
	connCh := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connCh <- conn
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial websocket: %v", err)
	}
	t.Cleanup(func() { clientConn.Close() })

	select {
	case serverConn = <-connCh:
	case <-time.After(2 * time.Second):
		t.Fatal("server side of test connection never arrived")
	}
	return serverConn, clientConn
}

// addTestClient reserves a slot and adds a client over a real connection. A
// goroutine drains the client side so the write pump never blocks.
func addTestClient(t *testing.T, h *Hub) *Client {
	t.Helper()

	serverConn, clientConn := newTestConn(t)
	go func() {
		for {
			if _, _, err := clientConn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	if !h.Reserve() {
		t.Fatal("Reserve failed while under capacity")
	}
	return h.Add(context.Background(), serverConn)
}

func TestReserveHonorsCap(t *testing.T) {
	h := NewHub("test-cap", 2)

	if !h.Reserve() {
		t.Fatal("first Reserve failed")
	}
	if !h.Reserve() {
		t.Fatal("second Reserve failed")
	}
	if h.Reserve() {
		t.Error("third Reserve succeeded past maxConn")
	}
	if got := h.Connections(); got != 2 {
		t.Errorf("expected 2 reserved connections, got %d", got)
	}

	h.Unreserve()
	if got := h.Connections(); got != 1 {
		t.Errorf("expected 1 reserved connection after Unreserve, got %d", got)
	}
	if !h.Reserve() {
		t.Error("Reserve failed after Unreserve released a slot")
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	h := NewHub("test-double-remove", 4)
	c := addTestClient(t, h)

	if got := h.Connections(); got != 1 {
		t.Fatalf("expected 1 connection, got %d", got)
	}

	if !h.Remove(c) {
		t.Error("first Remove returned false")
	}
	if got := h.Connections(); got != 0 {
		t.Errorf("expected 0 connections after first Remove, got %d", got)
	}

	// Second Remove must not find the client or decrement the slot again.
	if h.Remove(c) {
		t.Error("second Remove returned true")
	}
	if got := h.Connections(); got != 0 {
		t.Errorf("expected 0 connections after second Remove, got %d", got)
	}

	h.Wait()
}

func TestTrySendAfterRemove(t *testing.T) {
	h := NewHub("test-send-after-remove", 4)
	c := addTestClient(t, h)

	if !c.TrySend(Message{Event: EventPing, Data: EventPingPongData{Timestamp: 1}}) {
		t.Error("TrySend failed on a live client")
	}

	h.Remove(c)

	if c.TrySend(Message{Event: EventPing, Data: EventPingPongData{Timestamp: 2}}) {
		t.Error("TrySend succeeded after Remove")
	}

	h.Wait()
}

func TestBroadcastDropsFullClient(t *testing.T) {
	h := NewHub("test-full-buffer", 4)

	// Build a client without a write pump so its buffer never drains, and
	// register it by hand (Reserve + direct insert mirrors Add without the
	// pump).
	serverConn, _ := newTestConn(t)
	c := &Client{conn: serverConn, send: make(chan Message, 1), done: make(chan struct{})}
	if !h.Reserve() {
		t.Fatal("Reserve failed while under capacity")
	}
	h.mu.Lock()
	h.clients = append(h.clients, c)
	h.mu.Unlock()

	msg := Message{Event: EventStatus, Data: EventStatusData{StreamID: "s1"}}
	if !c.TrySend(msg) {
		t.Fatal("TrySend failed to fill the buffer")
	}

	// The buffer is now full: Broadcast must drop the client, not panic.
	h.Broadcast(msg)

	if got := h.ClientCount(); got != 0 {
		t.Errorf("expected full client to be removed, ClientCount = %d", got)
	}
	if got := h.Connections(); got != 0 {
		t.Errorf("expected 0 connections after drop, got %d", got)
	}
	if c.TrySend(msg) {
		t.Error("TrySend succeeded on a dropped client")
	}
}

func TestConcurrentBroadcastRemoveTrySend(t *testing.T) {
	h := NewHub("test-hammer", 100)

	const numClients = 8
	clients := make([]*Client, 0, numClients)
	for range numClients {
		clients = append(clients, addTestClient(t, h))
	}

	msg := Message{Event: EventNewLine, Data: EventNewLineData{LineID: 1}}
	var wg sync.WaitGroup

	// Broadcasters.
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				h.Broadcast(msg)
			}
		}()
	}

	// Direct senders on every client, racing the removers below.
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				c.TrySend(msg)
			}
		}()
	}

	// Removers for half the clients, plus double-removers racing them.
	for _, c := range clients[:numClients/2] {
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h.Remove(c)
			}()
		}
	}

	wg.Wait()

	for _, c := range clients {
		h.Remove(c)
	}
	if got := h.ClientCount(); got != 0 {
		t.Errorf("expected 0 clients after cleanup, got %d", got)
	}
	h.Wait()
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
			if got := IsClientDisconnectError(tt.err); got != tt.expected {
				t.Errorf("IsClientDisconnectError() = %v, want %v", got, tt.expected)
			}
		})
	}
}
