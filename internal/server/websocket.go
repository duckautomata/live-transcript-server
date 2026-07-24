package server

import (
	"compress/flate"
	"context"
	"log/slog"
	"net/http"
	"time"

	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/model"
	"live-transcript-server/internal/ws"

	"github.com/gorilla/websocket"
)

// wsHandler upgrades the connection, registers the client with the channel's
// hub, sends the initial sync, and then runs the read loop until the client
// disconnects.
func (app *App) wsHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	// Reserve a slot before upgrading so the connection cap is enforced
	// atomically (no TOCTOU between the check and the register).
	if !cs.Hub.Reserve() {
		http.Error(w, "Max number of connection already reached", http.StatusBadRequest)
		slog.Error("max number of connections already reached", "key", cs.Key, "func", "wsHandler", "maxConn", app.MaxConn)
		metrics.WebsocketError.Inc()
		return
	}

	conn, err := app.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Release the slot we reserved above.
		cs.Hub.Unreserve()
		if _, ok := err.(websocket.HandshakeError); !ok {
			slog.Error("unable to establish handshake with client", "key", cs.Key, "func", "wsHandler", "err", err)
		} else {
			slog.Error("unable to initiate ws connection", "key", cs.Key, "func", "wsHandler", "err", err)
		}
		metrics.WebsocketError.Inc()
		return
	}

	startTime := time.Now()

	// Enable compression for writes
	conn.EnableWriteCompression(true)
	conn.SetCompressionLevel(flate.BestCompression)

	client := cs.Hub.Add(app.ctx, conn)
	defer func() {
		metrics.ConnectionDuration.Observe(time.Since(startTime).Seconds())
		cs.Hub.Remove(client)
	}()

	app.syncClient(r.Context(), cs, client)

	if err := cs.Hub.ReadLoop(client); err != nil {
		slog.Error("error in clients readloop", "key", cs.Key, "func", "wsHandler", "err", err)
		metrics.WebsocketError.Inc()
	}
}

// syncClient sends the current stream state and transcript to a newly
// connected client.
func (app *App) syncClient(ctx context.Context, cs *ChannelState, client *ws.Client) {
	stream, err := app.Store.GetRecentStream(ctx, cs.Key)
	if err != nil {
		slog.Error("failed to get stream for sync", "key", cs.Key, "err", err)
		return
	}
	if stream == nil {
		stream = &model.Stream{
			StreamID:    "",
			StreamTitle: "",
			StartTime:   "0",
			MediaType:   "none",
			IsLive:      false,
		}
	}

	syncData := &ws.EventSyncData{
		StreamID:     stream.StreamID,
		StreamTitle:  stream.StreamTitle,
		StartTime:    stream.StartTime,
		MediaType:    stream.MediaType,
		MediaBaseURL: app.Storage.GetURL(""),
		IsLive:       stream.IsLive,
		Transcript:   make([]model.Line, 0),
	}

	transcript, err := app.Store.GetTranscript(ctx, cs.Key, stream.StreamID)
	if err != nil {
		slog.Error("failed to get transcript for sync", "key", cs.Key, "err", err)
		return
	}
	syncData.Transcript = transcript

	// Send a partial sync first if the transcript is large, so the client can
	// render the tail immediately while the full payload transfers.
	if len(transcript) > 100 {
		partialSyncData := *syncData
		partialSyncData.Transcript = transcript[len(transcript)-100:]
		if !client.TrySend(ws.Message{Event: ws.EventPartialSync, Data: partialSyncData}) {
			slog.Error("failed to send partial sync message: buffer full or closed", "key", cs.Key)
			cs.Hub.Remove(client)
			return
		}
	}

	startTime := time.Now()
	metrics.MessagesTotal.Inc()

	if !client.TrySend(ws.Message{Event: ws.EventSync, Data: syncData}) {
		slog.Error("failed to send sync message: buffer full or closed", "key", cs.Key)
		cs.Hub.Remove(client)
		return
	}

	// Send past streams
	pastStreams, err := app.Store.GetPastStreams(ctx, cs.Key, stream.StreamID)
	if err == nil && len(pastStreams) > 0 {
		if !client.TrySend(ws.Message{Event: ws.EventPastStreams, Data: ws.EventPastStreamsData{Streams: pastStreams}}) {
			slog.Error("failed to send past streams message: buffer full or closed", "key", cs.Key)
			cs.Hub.Remove(client)
			return
		}
	}

	metrics.MessageProcessingDuration.Observe(time.Since(startTime).Seconds())
}
