package ws

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	"live-transcript-server/internal/metrics"

	"github.com/gorilla/websocket"
)

// IsClientDisconnectError checks if the error is due to a client disconnecting.
func IsClientDisconnectError(err error) bool {
	// 1. Check for polite WebSocket close codes (User closed tab, etc.)
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure) {
		return true
	}

	// 1.5 Check for net.ErrClosed (use of closed network connection)
	if errors.Is(err, net.ErrClosed) {
		return true
	}

	// 2. Check for underlying network/OS errors (Broken Pipe or Connection Reset)
	// This handles "writev: broken pipe" and "read: connection reset by peer"
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		var syscallErr *os.SyscallError
		if errors.As(opErr.Err, &syscallErr) {
			if syscallErr.Err == syscall.EPIPE || syscallErr.Err == syscall.ECONNRESET {
				return true
			}
		}
	}

	return false
}

// Client is a middleman between the websocket connection and the hub.
type Client struct {
	conn *websocket.Conn
	send chan Message
	done chan struct{}
	// closeOnce guards close(done). The send channel is deliberately never
	// closed — done is the removal signal and the GC reclaims send — so
	// TrySend can never panic on a closed channel.
	closeOnce sync.Once
}

// TrySend queues msg for delivery without blocking. It returns false if the
// client has been removed from the hub or its send buffer is full.
func (c *Client) TrySend(msg Message) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.send <- msg:
		return true
	case <-c.done:
		return false
	default:
		return false
	}
}

// Hub tracks the WebSocket clients for a single channel key and fans
// broadcast messages out to them.
type Hub struct {
	key string

	mu      sync.Mutex
	clients []*Client
	// connections counts reserved connection slots, not registered clients:
	// Reserve takes a slot before the HTTP upgrade so the cap is enforced
	// atomically (a plain read-then-upgrade would let concurrent upgrades
	// slip past maxConn — TOCTOU).
	connections int
	maxConn     int

	wg sync.WaitGroup
}

// NewHub returns a hub for the given channel key, capped at maxConn
// simultaneous connections.
func NewHub(key string, maxConn int) *Hub {
	return &Hub{key: key, maxConn: maxConn}
}

// Reserve claims a connection slot ahead of the WebSocket upgrade. It returns
// false when the hub is at capacity. Every successful Reserve must be paired
// with either Add (and a later Remove) or Unreserve.
func (h *Hub) Reserve() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.connections >= h.maxConn {
		return false
	}
	h.connections++
	return true
}

// Unreserve releases a slot claimed by Reserve when the upgrade failed and no
// client will be added.
func (h *Hub) Unreserve() {
	h.mu.Lock()
	h.connections--
	h.mu.Unlock()
}

// Add registers a new client for conn and starts its write pump. The caller
// must have claimed a slot with Reserve; Add does not touch the connection
// count. The pump exits when the client is removed or ctx is done.
func (h *Hub) Add(ctx context.Context, conn *websocket.Conn) *Client {
	c := &Client{
		conn: conn,
		send: make(chan Message, 256),
		done: make(chan struct{}),
	}

	h.mu.Lock()
	h.clients = append(h.clients, c)
	h.mu.Unlock()

	metrics.ActiveConnections.Inc()
	metrics.TotalConnections.Inc()
	metrics.ClientsPerKey.WithLabelValues(h.key).Inc()

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.writePump(ctx, c)
	}()

	return c
}

// Remove unregisters the client, releases its connection slot, and closes the
// underlying connection. It is idempotent: a second Remove of the same client
// finds nothing and returns false without touching the slot count.
func (h *Hub) Remove(c *Client) bool {
	h.mu.Lock()
	found := false
	for i, cl := range h.clients {
		if cl == c {
			h.clients = slices.Delete(h.clients, i, i+1)
			h.connections--
			found = true
			break
		}
	}
	h.mu.Unlock()

	if !found {
		return false
	}

	metrics.ActiveConnections.Dec()
	metrics.ClientsPerKey.WithLabelValues(h.key).Dec()

	c.closeOnce.Do(func() { close(c.done) })
	c.conn.Close()
	return true
}

// Broadcast sends msg to every registered client. Clients that cannot accept
// the message (buffer full or already closed) are removed synchronously after
// the client list is released.
func (h *Hub) Broadcast(msg Message) {
	startTime := time.Now()

	var stale []*Client
	h.mu.Lock()
	for _, c := range h.clients {
		metrics.MessagesTotal.Inc()
		if !c.TrySend(msg) {
			stale = append(stale, c)
		}
	}
	h.mu.Unlock()

	for _, c := range stale {
		h.Remove(c)
	}

	metrics.MessageProcessingDuration.Observe(time.Since(startTime).Seconds())
}

// Connections returns the number of reserved connection slots. This includes
// slots claimed by Reserve whose clients have not yet been added.
func (h *Hub) Connections() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connections
}

// ClientCount returns the number of registered clients.
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Wait blocks until every write pump started by Add has exited. Used during
// server shutdown.
func (h *Hub) Wait() {
	h.wg.Wait()
}

// ReadLoop reads client messages until the connection drops, answering pings
// with pongs. A clean client disconnect returns nil; any other read error is
// returned.
func (h *Hub) ReadLoop(c *Client) error {
	// Bound hostile frames. No read deadline: the client ping cadence is not
	// known to the server.
	c.conn.SetReadLimit(64 * 1024)

	for {
		var msg Message
		if err := c.conn.ReadJSON(&msg); err != nil {
			if IsClientDisconnectError(err) {
				return nil
			}
			return err
		}

		if msg.Event == EventPing {
			dataMap, ok := msg.Data.(map[string]any)
			if !ok {
				slog.Error("invalid ping data format", "key", h.key, "func", "readLoop")
				continue
			}

			// extract timestamp safely
			var timestamp int
			if ts, ok := dataMap["timestamp"].(float64); ok {
				timestamp = int(ts)
			}

			pongMsg := Message{
				Event: EventPong,
				Data: EventPingPongData{
					Timestamp: timestamp,
				},
			}

			if !c.TrySend(pongMsg) {
				slog.Error("failed to send pong: buffer full or closed", "key", h.key, "func", "readLoop")
			}
		}
	}
}

// writePump pumps messages from the hub to the websocket connection.
//
// A goroutine running writePump is started by Add for each connection. The
// hub ensures that there is at most one writer to a connection by executing
// all writes from this goroutine.
func (h *Hub) writePump(ctx context.Context, c *Client) {
	// Remove is idempotent, so every exit path can safely funnel through it.
	defer h.Remove(c)

	for {
		select {
		case msg := <-c.send:
			if err := c.conn.WriteJSON(msg); err != nil {
				if IsClientDisconnectError(err) {
					slog.Debug("client disconnected before message was sent", "key", h.key, "err", err)
				} else {
					slog.Error("failed to write message to client", "key", h.key, "err", err)
					metrics.WebsocketError.Inc()
				}
				return
			}
		case <-c.done:
			// The client was removed; best-effort close frame.
			c.conn.WriteMessage(websocket.CloseMessage, []byte{})
			return
		case <-ctx.Done():
			return
		}
	}
}
