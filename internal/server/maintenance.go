package server

import (
	"context"
	"log/slog"
	"time"

	"live-transcript-server/internal/storage"
	"live-transcript-server/internal/ws"
)

// workerActiveWindow is how recently a worker must have been seen to be
// considered active — shared by the public /status endpoint, the admin info
// endpoint, and the offline-alert sweep.
const workerActiveWindow = 5 * time.Minute

// StartMaintenanceLoop starts the periodic background sweeps: orphaned
// transcript cleanup, R2 DB/storage reconciliation, worker liveness alerts,
// and incoming-queue TTL cleanup. All loops stop when the app context is
// canceled.
func (app *App) StartMaintenanceLoop() {
	slog.Info("starting maintenance loop", "func", "StartMaintenanceLoop", "storage_is_local", app.Storage.IsLocal())

	app.runPeriodic(12*time.Hour, true, app.databaseCleanup)
	if !app.Storage.IsLocal() {
		app.runPeriodic(4*time.Hour, true, app.pruneExpiredStreams)
	}
	app.runPeriodic(2*time.Hour, false, app.checkWorkerStatus)
	app.runPeriodic(15*time.Minute, true, app.cleanupIncomingStreams)
}

// runPeriodic runs fn every interval until the app context is canceled. When
// immediately is true, fn also runs once right away.
func (app *App) runPeriodic(interval time.Duration, immediately bool, fn func()) {
	app.wg.Add(1)
	go func() {
		defer app.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		if immediately {
			fn()
		}
		for {
			select {
			case <-ticker.C:
				fn()
			case <-app.ctx.Done():
				return
			}
		}
	}()
}

// cleanupIncomingStreams removes Pingcord-queued URLs that have aged past the
// configured TTL. This is the safety net for the case where a stream URL was
// announced but no worker ever consumed it.
func (app *App) cleanupIncomingStreams() {
	cutoff := time.Now().Add(-app.IncomingStreamTTL).Unix()
	removed, err := app.Store.CleanupExpiredIncomingStreams(context.Background(), cutoff)
	if err != nil {
		slog.Error("failed to cleanup expired incoming streams", "func", "cleanupIncomingStreams", "err", err)
		return
	}
	if removed > 0 {
		slog.Info("removed expired incoming streams", "func", "cleanupIncomingStreams", "count", removed, "ttl", app.IncomingStreamTTL.String())
	}
}

// checkWorkerStatus logs an error and pings Discord if any workers have been
// inactive for more than workerActiveWindow.
func (app *App) checkWorkerStatus() {
	ctx := context.Background()

	workers, err := app.Store.GetAllWorkerStatus(ctx)
	if err != nil {
		slog.Error("failed to get worker status for monitoring", "err", err)
		return
	}

	now := time.Now().Unix()
	for _, w := range workers {
		if w.ChannelKey != "test" && w.ChannelKey != "dev" && now-w.LastSeen >= int64(workerActiveWindow.Seconds()) {
			lastSeenTime := time.Unix(w.LastSeen, 0).Format(time.RFC3339)
			timeAgo := (time.Duration(now-w.LastSeen) * time.Second).String()
			slog.Error("worker is not active", "key", w.ChannelKey, "last_seen", lastSeenTime, "time_ago", timeAgo)
			app.Discord.NotifyWorkerOffline(w.ChannelKey, w.LastSeen)
		}
	}
}

// databaseCleanup removes transcript lines for streams that no longer exist in
// the database.
func (app *App) databaseCleanup() {
	slog.Debug("starting database cleanup", "func", "databaseCleanup")
	if err := app.Store.CleanupOrphanedTranscripts(context.Background()); err != nil {
		slog.Error("failed to cleanup orphaned transcripts", "func", "databaseCleanup", "err", err)
	}
}

// pruneExpiredStreams checks R2 storage for missing streams and removes them
// from the database.
func (app *App) pruneExpiredStreams() {
	slog.Debug("starting prune expired streams sweep", "func", "pruneExpiredStreams")
	ctx := context.Background()

	for _, cs := range app.Channels {
		streams, err := app.Store.GetAllStreams(ctx, cs.Key)
		if err != nil {
			slog.Error("failed to get streams for pruning", "key", cs.Key, "err", err)
			continue
		}
		if len(streams) < 1 {
			continue
		}

		updatesMade := false
		// The first (most recent) stream is included because it's possible the
		// active stream itself was deleted in R2: this happens when no new
		// streams have started and all media has aged out.
		for i := range streams {
			stream := streams[i]

			// We don't store media for "none" media types, so StreamExists
			// would always report them missing. Give them a flat 7-day TTL
			// instead, matching the R2 lifecycle window.
			if stream.MediaType == "none" {
				if time.Since(time.UnixMicro(stream.ActivatedTime)) < 7*24*time.Hour {
					continue
				}
			}

			// Skip streams younger than 24 hours.
			if time.Since(time.UnixMicro(stream.ActivatedTime)) < 24*time.Hour {
				continue
			}

			// Probe /raw because it is created at the time of the stream. A
			// clip created days later would keep the clips/ folder alive and
			// delay pruning if we probed the stream root instead.
			exists, err := app.Storage.StreamExists(ctx, storage.RawPrefix(cs.Key, stream.StreamID))
			if err != nil {
				slog.Error("Pruning: check failed", "key", cs.Key, "streamID", stream.StreamID, "err", err)
				continue
			}
			if !exists {
				slog.Info("Pruning: stream missing in R2, deleting from db", "key", cs.Key, "streamID", stream.StreamID)
				if err := app.Store.DeleteStreamCascade(ctx, cs.Key, stream.StreamID); err != nil {
					slog.Error("Pruning: failed to delete stream", "key", cs.Key, "streamID", stream.StreamID, "err", err)
					continue
				}
				updatesMade = true
			}
		}

		if updatesMade {
			// Broadcast the refreshed list of past streams.
			currentStream, _ := app.Store.GetRecentStream(ctx, cs.Key)
			streamID := ""
			if currentStream != nil {
				streamID = currentStream.StreamID
			}

			finalPastStreams, err := app.Store.GetPastStreams(ctx, cs.Key, streamID)
			if err == nil {
				cs.Hub.Broadcast(ws.Message{
					Event: ws.EventPastStreams,
					Data:  ws.EventPastStreamsData{Streams: finalPastStreams},
				})
			}
		}
	}
}
