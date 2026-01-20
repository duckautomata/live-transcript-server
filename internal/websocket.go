package internal

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"slices"

	"github.com/gorilla/websocket"
)

// isClientDisconnectError checks if the error is due to a client disconnecting.
func isClientDisconnectError(err error) bool {
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

// writePump pumps messages from the hub to the websocket connection.
//
// A goroutine running writePump must be started for each connection. The
// application ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (c *Client) writePump(cs *ChannelState) {
	defer func() {
		cs.closeSocket(c)
	}()

	for {
		msg, ok := <-c.send
		if !ok {
			// The hub closed the channel.
			c.conn.WriteMessage(websocket.CloseMessage, []byte{})
			return
		}

		if err := c.conn.WriteJSON(msg); err != nil {
			if isClientDisconnectError(err) {
				slog.Debug("client disconnected before message was sent", "key", cs.Key, "err", err)
			} else {
				slog.Error("failed to write message to client", "key", cs.Key, "err", err)
				WebsocketError.Inc()
			}
			return
		}
	}
}

func (cs *ChannelState) readLoop(client *Client) error {
	for {
		var msg WebSocketMessage
		if err := client.conn.ReadJSON(&msg); err != nil {
			if isClientDisconnectError(err) {
				return nil
			}
			return err
		}

		if msg.Event == EventPing {
			dataMap, ok := msg.Data.(map[string]any)
			if !ok {
				slog.Error("invalid ping data format", "key", cs.Key, "func", "readLoop")
				continue
			}

			// extract timestamp safely
			var timestamp int
			if ts, ok := dataMap["timestamp"].(float64); ok {
				timestamp = int(ts)
			}

			pongMsg := WebSocketMessage{
				Event: EventPong,
				Data: EventPingPongData{
					Timestamp: timestamp,
				},
			}

			select {
			case client.send <- pongMsg:
			default:
				slog.Error("failed to send pong: buffer full", "key", cs.Key, "func", "readLoop")
			}
		}
	}
}

// Send new line to all clients. If newLine is nil, then the last line from the database is used.
func (app *App) broadcastNewLine(ctx context.Context, cs *ChannelState, activeID string, uploadTime int64, newLine *Line) {
	if newLine == nil {
		lastLine, err := app.GetLastLine(ctx, cs.Key, activeID)
		if err != nil {
			slog.Error("failed to get last line for refresh", "key", cs.Key, "err", err)
			return
		}
		newLine = lastLine
	}
	if newLine == nil {
		return
	}

	data := EventNewLineData{
		LineID:     newLine.ID,
		Timestamp:  newLine.Timestamp,
		UploadTime: uploadTime,
		Segments:   newLine.Segments,
	}

	msg := WebSocketMessage{
		Event: EventNewLine,
		Data:  data,
	}

	cs.broadcast(msg)
}

// broadcastNewMedia sends a newMedia event to all clients with the map of latest available media Files.
func (app *App) broadcastNewMedia(cs *ChannelState, streamID string, files map[int]string) {
	data := EventNewMediaData{
		StreamID: streamID,
		Files:    files,
	}

	msg := WebSocketMessage{
		Event: EventNewMedia,
		Data:  data,
	}

	cs.broadcast(msg)
}

// Send full transcript to client
func (app *App) syncClient(ctx context.Context, cs *ChannelState, client *Client) {
	stream, err := app.GetStream(ctx, cs.Key)
	if err != nil {
		slog.Error("failed to get stream for sync", "key", cs.Key, "err", err)
		return
	}
	if stream == nil {
		stream = &Stream{
			ActiveID:    "",
			ActiveTitle: "",
			StartTime:   "0",
			MediaType:   "none",
			IsLive:      false,
		}
	}

	syncData := &EventSyncData{
		ActiveID:     stream.ActiveID,
		ActiveTitle:  stream.ActiveTitle,
		StartTime:    stream.StartTime,
		MediaType:    stream.MediaType,
		MediaBaseURL: app.Storage.GetURL(""),
		IsLive:       stream.IsLive,
		Transcript:   make([]Line, 0),
	}

	transcript, err := app.GetTranscript(ctx, cs.Key, stream.ActiveID)
	if err != nil {
		slog.Error("failed to get transcript for sync", "key", cs.Key, "err", err)
		return
	}
	syncData.Transcript = transcript

	outData := WebSocketMessage{
		Event: EventSync,
		Data:  syncData,
	}
	startTime := time.Now()
	MessagesTotal.Inc()

	select {
	case client.send <- outData:
	default:
		slog.Error("failed to send sync message: buffer full", "key", cs.Key)
		cs.closeSocket(client)
		return
	}

	// Send past streams
	pastStreams, err := app.GetPastStreams(ctx, cs.Key, stream.ActiveID)
	if err == nil && len(pastStreams) > 0 {
		pastStreamsMsg := WebSocketMessage{
			Event: EventPastStreams,
			Data:  EventPastStreamsData{Streams: pastStreams},
		}
		select {
		case client.send <- pastStreamsMsg:
		default:
			slog.Error("failed to send past streams message: buffer full", "key", cs.Key)
			cs.closeSocket(client)
			return
		}
	}

	MessageProcessingDuration.Observe(time.Since(startTime).Seconds())
}

func (cs *ChannelState) broadcast(msg WebSocketMessage) {
	startTime := time.Now()
	// MessageSize.Observe(float64(len(msg))) // Size metric is hard with any
	cs.ClientsLock.Lock()
	for _, c := range cs.Clients {
		MessagesTotal.Inc()
		select {
		case c.send <- msg:
		default:
			go cs.closeSocket(c)
		}
	}
	cs.ClientsLock.Unlock()
	MessageProcessingDuration.Observe(time.Since(startTime).Seconds())
}

func (cs *ChannelState) closeSocket(client *Client) error {
	cs.ClientsLock.Lock()
	defer cs.ClientsLock.Unlock()

	for i, c := range cs.Clients {
		if c == client {
			cs.Clients = slices.Delete(cs.Clients, i, i+1)
			cs.ClientConnections--

			ActiveConnections.Dec()
			ClientsPerKey.WithLabelValues(cs.Key).Dec()

			close(client.send)
			return client.conn.Close()
		}
	}
	// Connection already closed or not found
	return nil
}

func (app *App) wsHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("channel")
	cs, ok := app.Channels[key]
	if !ok {
		http.Error(w, "Channel not found", http.StatusNotFound)
		return
	}

	if cs.ClientConnections >= app.MaxConn {
		http.Error(w, "Max number of connection already reached", http.StatusBadRequest)
		slog.Error("max number of connections already reached", "key", cs.Key, "func", "wsHandler", "maxConn", app.MaxConn)
		WebsocketError.Inc()
		return
	}

	conn, err := app.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		if _, ok := err.(websocket.HandshakeError); !ok {
			slog.Error("unable to establish handshake with client", "key", cs.Key, "func", "wsHandler", "err", err)
		} else {
			slog.Error("unable to initiate ws connection", "key", cs.Key, "func", "wsHandler", "err", err)
		}
		WebsocketError.Inc()
		return
	}

	ActiveConnections.Inc()
	TotalConnections.Inc()
	ClientsPerKey.WithLabelValues(cs.Key).Inc()
	startTime := time.Now()

	client := &Client{conn: conn, send: make(chan WebSocketMessage, 256)}

	cs.ClientsLock.Lock()
	cs.ClientConnections++
	cs.Clients = append(cs.Clients, client)
	cs.ClientsLock.Unlock()

	go client.writePump(cs)

	defer func() {
		ConnectionDuration.Observe(time.Since(startTime).Seconds())
		cs.closeSocket(client)
	}()

	app.syncClient(r.Context(), cs, client)

	err = cs.readLoop(client)
	if err != nil {
		slog.Error("error in clients readloop", "key", cs.Key, "func", "wsHandler", "err", err)
		WebsocketError.Inc()
	}
}
