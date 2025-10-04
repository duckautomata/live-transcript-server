package internal

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"slices"

	"github.com/gorilla/websocket"
)

func (w *WebSocketServer) readLoop(conn *websocket.Conn) error {
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return nil
		}
		slog.Debug("received message from client", "key", w.key, "func", "readLoop", "readMessage", string(message))
	}
}

func (w *WebSocketServer) refreshAll() {
	if len(w.clientData.Transcript) == 0 {
		return
	}

	var sb strings.Builder
	sb.WriteString("![]refresh\n")

	// w.transcriptLock.Lock()
	lastLine := w.clientData.Transcript[len(w.clientData.Transcript)-1]
	// w.transcriptLock.Unlock()

	sb.WriteString(fmt.Sprintf("%d\n%d", lastLine.ID, lastLine.Timestamp))
	for _, seg := range lastLine.Segments {
		sb.WriteString(fmt.Sprintf("\n%d\n%s", seg.Timestamp, seg.Text))
	}

	w.broadcast([]byte(sb.String()))
}

func (w *WebSocketServer) hardRefresh(conn *websocket.Conn) {
	// Very susecptiale to deadlock.
	// w.clientsLock.Lock()
	// w.streamLock.Lock()
	// w.transcriptLock.Lock()
	outData := HardRefreshData{
		Event: "hardrefresh",
		Data:  w.clientData,
	}
	startTime := time.Now()
	MessagesTotal.Inc()
	if err := conn.WriteJSON(outData); err != nil {
		WebsocketError.Inc()
		defer w.closeSocket(conn)
	}

	// w.transcriptLock.Unlock()
	// w.streamLock.Unlock()
	// w.clientsLock.Unlock()
	MessageProcessingDuration.Observe(time.Since(startTime).Seconds())
}

func (w *WebSocketServer) broadcast(msg []byte) {
	startTime := time.Now()
	MessageSize.Observe(float64(len(msg)))
	MessagesTotal.Inc()
	w.clientsLock.Lock()
	for _, c := range w.clients {
		go func(msg []byte) {
			if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
				WebsocketError.Inc()
				defer w.closeSocket(c)
			}
		}(msg)
	}
	w.clientsLock.Unlock()
	MessageProcessingDuration.Observe(time.Since(startTime).Seconds())
}

func (w *WebSocketServer) closeSocket(conn *websocket.Conn) error {
	ActiveConnections.Dec()
	ClientsPerKey.WithLabelValues(w.key).Dec()

	w.clientsLock.Lock()
	for i, c := range w.clients {
		if c == conn {
			w.clients = slices.Delete(w.clients, i, i+1)
			break
		}
	}
	w.clientConnections--
	w.clientsLock.Unlock()
	return conn.Close()
}

func (ws *WebSocketServer) wsHandler(w http.ResponseWriter, r *http.Request) {
	if ws.clientConnections >= ws.maxConn {
		http.Error(w, "Max number of connection already reached", http.StatusBadRequest)
		slog.Error("max number of connections already reached", "key", ws.key, "func", "wsHandler", "maxConn", ws.maxConn)
		WebsocketError.Inc()
		return
	}

	conn, err := ws.upgrader.Upgrade(w, r, nil)
	if err != nil {
		if _, ok := err.(websocket.HandshakeError); !ok {
			slog.Error("unable to establish handshake with client", "key", ws.key, "func", "wsHandler", "err", err)
		} else {
			slog.Error("unable to initiate ws connection", "key", ws.key, "func", "wsHandler", "err", err)
		}
		WebsocketError.Inc()
		return
	}

	ActiveConnections.Inc()
	TotalConnections.Inc()
	ClientsPerKey.WithLabelValues(ws.key).Inc()
	startTime := time.Now()

	ws.clientsLock.Lock()
	ws.clientConnections++
	ws.clients = append(ws.clients, conn)
	ws.clientsLock.Unlock()
	defer func() {
		ConnectionDuration.Observe(time.Since(startTime).Seconds())
		ws.closeSocket(conn)
	}()

	ws.hardRefresh(conn)

	err = ws.readLoop(conn)
	if err != nil {
		slog.Error("error in clients readloop", "key", ws.key, "func", "wsHandler", "err", err)
		WebsocketError.Inc()
	}
}
