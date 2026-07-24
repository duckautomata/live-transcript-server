package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/model"
	"live-transcript-server/internal/storage"
	"live-transcript-server/internal/ws"
)

// activateStream activates a stream and notifies all clients.
// Returns true if the stream was activated and a message was sent, false otherwise.
func (app *App) activateStream(ctx context.Context, cs *ChannelState, streamID string, streamTitle string, startTime string, mediaType string) bool {
	currentStream, err := app.Store.GetRecentStream(ctx, cs.Key)
	if err != nil {
		slog.Error("failed to get stream from db", "key", cs.Key, "err", err)
		return false
	}

	var msg ws.Message

	// If no stream exists, or the ID is different, it's a new stream
	if currentStream == nil || currentStream.StreamID != streamID {
		metrics.StreamAudioPlayed.WithLabelValues(cs.Key).Set(0)
		metrics.StreamFramesDownloads.WithLabelValues(cs.Key).Set(0)
		metrics.StreamAudioClipped.WithLabelValues(cs.Key).Set(0)
		metrics.StreamVideoClipped.WithLabelValues(cs.Key).Set(0)
		metrics.StreamAudioTrimmed.WithLabelValues(cs.Key).Set(0)
		metrics.StreamVideoTrimmed.WithLabelValues(cs.Key).Set(0)

		// Remove the previous stream's activation metric, if there was one.
		if currentStream != nil && currentStream.StreamID != "" {
			deleted := metrics.ActivatedStreams.DeleteLabelValues(cs.Key, currentStream.StreamID, currentStream.StreamTitle)
			if deleted {
				slog.Info("removed old stream activation metric", "key", cs.Key, "func", "activateStream", "oldStreamID", currentStream.StreamID)
			}
		}

		newStream := &model.Stream{
			ChannelID:     cs.Key,
			StreamID:      streamID,
			StreamTitle:   streamTitle,
			StartTime:     startTime,
			IsLive:        true,
			MediaType:     mediaType,
			ActivatedTime: time.Now().UnixMicro(),
		}

		// Deactivate previous stream if it was live
		if currentStream != nil && currentStream.IsLive {
			if err := app.Store.SetStreamLive(ctx, cs.Key, currentStream.StreamID, false); err != nil {
				slog.Error("failed to deactivate previous stream", "key", cs.Key, "streamID", currentStream.StreamID, "err", err)
			}
		}

		if err := app.Store.UpsertStream(ctx, newStream); err != nil {
			slog.Error("failed to upsert new stream", "key", cs.Key, "err", err)
			return false
		}

		app.Discord.NotifyStreamStart(cs.Key, streamID, streamTitle, startTime)

		app.applyRetention(ctx, cs, streamID)

		// Broadcast pastStreams event if there is any
		pastStreams, err := app.Store.GetPastStreams(ctx, cs.Key, streamID)
		if err != nil {
			slog.Error("failed to get past streams for broadcast", "key", cs.Key, "err", err)
		} else {
			// an empty slice will be a nil pointer (null in the json message)
			// client should be able to interpret a null array as empty.
			cs.Hub.Broadcast(ws.Message{
				Event: ws.EventPastStreams,
				Data:  ws.EventPastStreamsData{Streams: pastStreams},
			})
		}
		msg = ws.Message{
			Event: ws.EventNewStream,
			Data: ws.EventNewStreamData{
				StreamID:     newStream.StreamID,
				StreamTitle:  newStream.StreamTitle,
				StartTime:    newStream.StartTime,
				MediaType:    newStream.MediaType,
				MediaBaseURL: app.Storage.GetURL(""),
				IsLive:       newStream.IsLive,
			},
		}
		slog.Debug("received new stream id, sending newstream event", "key", cs.Key, "func", "activateStream", "streamID", streamID)

	} else {
		// Same stream ID
		if !currentStream.IsLive {
			// Reactivate: Update the specific stream to be live
			if err := app.Store.SetStreamLive(ctx, cs.Key, currentStream.StreamID, true); err != nil {
				slog.Error("failed to set stream live", "key", cs.Key, "err", err)
				return false
			}
			currentStream.IsLive = true
			msg = ws.Message{
				Event: ws.EventStatus,
				Data: ws.EventStatusData{
					StreamID:    currentStream.StreamID,
					StreamTitle: currentStream.StreamTitle,
					IsLive:      currentStream.IsLive,
				},
			}
			slog.Debug("reactivating existing stream, sending status event", "key", cs.Key, "func", "activateStream", "streamID", streamID)
		} else {
			// Already active
			slog.Debug("stream is already active, skipping event", "key", cs.Key, "func", "activateStream", "streamID", streamID)
		}
	}

	if msg.Event != "" {
		cs.Hub.Broadcast(msg)
		app.bumpAdminChange(cs.Key)
		return true
	}

	return false
}

// applyRetention enforces the per-channel stream retention policy after a new
// stream is activated.
//
// Local storage: keep the active stream plus NumPastStreams others (by
// activated_time), delete the rest from the DB and disk.
//
// R2 storage: the bucket's lifecycle rules delete old media; here we only
// reconcile the DB by dropping streams whose media has already disappeared.
func (app *App) applyRetention(ctx context.Context, cs *ChannelState, activeStreamID string) {
	allStreams, err := app.Store.GetAllStreams(ctx, cs.Key)
	if err != nil {
		slog.Error("failed to get all streams for rotation", "key", cs.Key, "err", err)
		return
	}

	if app.Storage.IsLocal() {
		pastStreamsKept := 0
		for _, stream := range allStreams {
			// Always keep the specific active stream we just activated/reactivated
			if stream.StreamID == activeStreamID {
				continue
			}
			if pastStreamsKept < cs.NumPastStreams {
				pastStreamsKept++
				continue
			}

			if err := app.Store.DeleteStreamCascade(ctx, cs.Key, stream.StreamID); err != nil {
				slog.Error("failed to delete stream from db", "key", cs.Key, "streamID", stream.StreamID, "err", err)
				continue
			}
			app.deleteStreamStorageAsync(cs.Key, stream.StreamID)
		}
		return
	}

	// R2 Storage: if a stream is missing from R2 (deleted by lifecycle
	// policy), remove it from the DB.
	if len(allStreams) <= 1 {
		return
	}
	for _, stream := range allStreams {
		if stream.StreamID == activeStreamID {
			continue
		}
		exists, err := app.Storage.StreamExists(ctx, storage.StreamPrefix(cs.Key, stream.StreamID))
		if err != nil {
			slog.Error("failed to check if stream exists in storage", "key", cs.Key, "streamID", stream.StreamID, "err", err)
			continue
		}
		if !exists {
			slog.Info("stream not found in storage (likely deleted by lifecycle), removing from db", "key", cs.Key, "streamID", stream.StreamID)
			if err := app.Store.DeleteStreamCascade(ctx, cs.Key, stream.StreamID); err != nil {
				slog.Error("failed to delete stream from db", "key", cs.Key, "streamID", stream.StreamID, "err", err)
			}
		}
	}
}

// deleteStreamStorageAsync removes a stream's media folder from storage in the
// background so request handlers don't block on bulk deletes.
func (app *App) deleteStreamStorageAsync(channelKey, streamID string) {
	storageKey := storage.StreamPrefix(channelKey, streamID)
	app.wg.Add(1)
	go func() {
		defer app.wg.Done()
		slog.Info("deleting old stream storage (async)", "key", channelKey, "storageKey", storageKey)
		if err := app.Storage.DeleteFolder(context.Background(), storageKey); err != nil {
			slog.Error("failed to delete stream folder from storage", "key", channelKey, "storageKey", storageKey, "err", err)
		} else {
			slog.Info("successfully deleted old stream storage", "key", channelKey, "storageKey", storageKey)
		}
	}()
}

// deactivateStream deactivates a stream and notifies all clients.
// Returns true if the stream was deactivated and a message was sent, false otherwise.
func (app *App) deactivateStream(ctx context.Context, cs *ChannelState, streamID string) bool {
	currentStream, err := app.Store.GetRecentStream(ctx, cs.Key)
	if err != nil {
		slog.Error("failed to get stream from db", "key", cs.Key, "err", err)
		return false
	}

	if currentStream == nil || currentStream.StreamID != streamID || !currentStream.IsLive {
		return false
	}

	deleted := metrics.ActivatedStreams.DeleteLabelValues(cs.Key, currentStream.StreamID, currentStream.StreamTitle)
	if deleted {
		slog.Info("successfully removed stream metric on deactivation", "key", cs.Key, "func", "deactivateStream", "streamID", streamID)
	} else {
		slog.Info("failed to remove stream metric on deactivation", "key", cs.Key, "func", "deactivateStream", "streamID", streamID)
	}

	if err := app.Store.SetStreamLive(ctx, cs.Key, streamID, false); err != nil {
		slog.Error("failed to set stream not live", "key", cs.Key, "err", err)
		return false
	}

	slog.Debug("deactivating stream", "key", cs.Key, "func", "deactivateStream", "activeID", streamID)
	cs.Hub.Broadcast(ws.Message{
		Event: ws.EventStatus,
		Data: ws.EventStatusData{
			StreamID:    currentStream.StreamID,
			StreamTitle: currentStream.StreamTitle,
			IsLive:      false,
		},
	})
	app.bumpAdminChange(cs.Key)
	return true
}

// removeStream deletes a stream's metadata and transcript from the DB and
// clears the Prometheus activation metric. If deleteMedia is true, it also
// asynchronously removes the stream's media folder from storage; otherwise
// the media files are left intact (useful for local testing against shared
// storage). On success, broadcasts a deletedStream event to all WebSocket
// clients for the channel so they can drop the stream from local state.
func (app *App) removeStream(ctx context.Context, cs *ChannelState, stream *model.Stream, deleteMedia bool) error {
	if err := app.Store.DeleteStreamCascade(ctx, cs.Key, stream.StreamID); err != nil {
		return fmt.Errorf("delete stream: %w", err)
	}
	if stream.StreamTitle != "" {
		metrics.ActivatedStreams.DeleteLabelValues(cs.Key, stream.StreamID, stream.StreamTitle)
	}

	// Notify connected clients. Done after the DB delete succeeds so we never
	// announce a deletion that didn't actually happen.
	cs.Hub.Broadcast(ws.Message{
		Event: ws.EventDeletedStream,
		Data: ws.EventDeletedStreamData{
			StreamID:    stream.StreamID,
			StreamTitle: stream.StreamTitle,
			WasLive:     stream.IsLive,
		},
	})

	if deleteMedia {
		app.deleteStreamStorageAsync(cs.Key, stream.StreamID)
	}
	return nil
}

// broadcastNewLine sends a new line to all clients. If newLine is nil, the
// last line from the database is used.
func (app *App) broadcastNewLine(ctx context.Context, cs *ChannelState, activeID string, uploadTime int64, newLine *model.Line) {
	if newLine == nil {
		lastLine, err := app.Store.GetLastLine(ctx, cs.Key, activeID)
		if err != nil {
			slog.Error("failed to get last line for refresh", "key", cs.Key, "err", err)
			return
		}
		newLine = lastLine
	}
	if newLine == nil {
		return
	}

	cs.Hub.Broadcast(ws.Message{
		Event: ws.EventNewLine,
		Data: ws.EventNewLineData{
			LineID:      newLine.ID,
			Timestamp:   newLine.Timestamp,
			UploadTime:  uploadTime,
			Segments:    newLine.Segments,
			VodAccurate: newLine.VodAccurate,
		},
	})
}

// broadcastNewMedia sends a newMedia event to all clients with the map of
// latest available media files.
func (app *App) broadcastNewMedia(cs *ChannelState, streamID string, files map[int]string) {
	cs.Hub.Broadcast(ws.Message{
		Event: ws.EventNewMedia,
		Data: ws.EventNewMediaData{
			StreamID: streamID,
			Files:    files,
		},
	})
}
