package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"live-transcript-server/internal/discord"
	"live-transcript-server/internal/media"
	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/model"
	"live-transcript-server/internal/storage"

	"github.com/lithammer/shortuuid/v4"
)

// A full VOD is every raw chunk of a finished stream stitched back into one
// file. It is far too slow to build inside a request (a long stream is
// thousands of chunks and gigabytes of scratch space), so a build runs in the
// background and the admin page polls for its state.
//
// Exactly one copy is ever produced per stream:
//   - the output key is fixed per stream (storage.VodKey), so a rebuild
//     replaces the object rather than adding another,
//   - an in-flight build is tracked in vodRegistry, so a second admin pressing
//     the button joins that build instead of starting a rival one, and
//   - a build is skipped entirely when the artifact is already in storage.

// Build states reported to the admin page.
const (
	vodStateNone    = "none"    // nothing built and nothing running
	vodStateRunning = "running" // a build is in flight
	vodStateDone    = "done"    // the artifact is in storage
	vodStateFailed  = "failed"  // the last build failed; the artifact is absent
)

// Phases of a running build, shown to the admin so a long wait is legible.
const (
	vodPhaseMerging   = "merging chunks"
	vodPhaseEncoding  = "encoding"
	vodPhaseUploading = "uploading"
)

// vodStatus is a point-in-time snapshot of a build. Copied out of the job
// under its lock so readers never touch live fields.
type vodStatus struct {
	State      string
	Phase      string
	Failure    string
	StartedAt  int64
	FinishedAt int64
}

// vodJob is one stream's build. Its fields are written by the goroutine doing
// the work and read by admin requests, so all access goes through the lock.
type vodJob struct {
	mu sync.Mutex
	vodStatus
}

func (j *vodJob) status() vodStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.vodStatus
}

func (j *vodJob) setPhase(phase string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Phase = phase
}

// finish records the build's outcome. A nil err marks the artifact available.
func (j *vodJob) finish(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Phase = ""
	j.FinishedAt = time.Now().Unix()
	if err != nil {
		j.State = vodStateFailed
		j.Failure = err.Error()
		return
	}
	j.State = vodStateDone
}

// vodRegistry tracks builds per channel/stream. It is the piece that makes
// concurrent presses of the button collapse into a single build.
type vodRegistry struct {
	mu   sync.Mutex
	jobs map[string]*vodJob
}

func newVodRegistry() *vodRegistry {
	return &vodRegistry{jobs: make(map[string]*vodJob)}
}

func vodJobKey(channelKey, streamID string) string {
	return channelKey + "/" + streamID
}

// get returns the tracked job for a stream, or nil when none was ever started
// on this server instance.
func (r *vodRegistry) get(channelKey, streamID string) *vodJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[vodJobKey(channelKey, streamID)]
}

// claim hands back the running job for a stream if there is one, and otherwise
// installs a fresh one. started tells the caller which happened: only the
// caller that started a job may run the build for it. A finished or failed job
// is replaced, so a stream can be retried after a failure.
func (r *vodRegistry) claim(channelKey, streamID string) (job *vodJob, started bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := vodJobKey(channelKey, streamID)
	if existing, ok := r.jobs[key]; ok && existing.status().State == vodStateRunning {
		return existing, false
	}

	job = &vodJob{vodStatus: vodStatus{
		State:     vodStateRunning,
		Phase:     vodPhaseMerging,
		StartedAt: time.Now().Unix(),
	}}
	r.jobs[key] = job
	return job, true
}

// forget drops a stream's job record. Called when the stream itself goes away
// so the registry doesn't accumulate entries for streams that no longer exist.
func (r *vodRegistry) forget(channelKey, streamID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.jobs, vodJobKey(channelKey, streamID))
}

// AdminVodResponse is the state of a stream's full-VOD build, returned by both
// GET and POST /{channel}/admin/vod/{streamID}.
type AdminVodResponse struct {
	StreamID string `json:"streamId"`
	State    string `json:"state"`
	// Phase is what a running build is currently doing; empty otherwise.
	Phase string `json:"phase,omitempty"`
	// Error is the failure message of the last build, when state is "failed".
	Error string `json:"error,omitempty"`
	// Format is the container the VOD is rendered into ("mp4" or "m4a").
	Format string `json:"format"`
	// TotalLines is the transcript's length; MediaLines is how many of those
	// lines have media that can go into the VOD, and MissingLines is the gap.
	TotalLines   int `json:"totalLines"`
	MediaLines   int `json:"mediaLines"`
	MissingLines int `json:"missingLines"`
	// URL is the absolute link to a finished VOD on remote storage. Path is
	// the channel-relative download route used with local storage. Exactly one
	// of the two is set, and only once the state is "done".
	URL        string `json:"url,omitempty"`
	Path       string `json:"path,omitempty"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
}

// vodExtension picks the container a stream renders into: video streams are
// remuxed to mp4, everything else is encoded to m4a — the same split the clip
// endpoint makes. An empty return means the stream has no media at all.
func vodExtension(stream *model.Stream) string {
	switch stream.MediaType {
	case "video":
		return ".mp4"
	case "audio":
		return ".m4a"
	default:
		return ""
	}
}

// vodTarget is a validated build target: a stream that is eligible for a VOD,
// plus the counts the admin page needs to warn about gaps.
type vodTarget struct {
	stream     *model.Stream
	ext        string
	totalLines int
	mediaLines int
}

// resolveVodTarget validates the stream in the request path and loads its line
// counts. It writes the error response itself and returns ok=false when the
// stream cannot have a VOD built — unknown, still live, or media-less.
func (app *App) resolveVodTarget(w http.ResponseWriter, r *http.Request, cs *ChannelState) (vodTarget, bool) {
	streamID := r.PathValue("streamID")
	if !isValidID(streamID) {
		http.Error(w, "invalid stream id", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return vodTarget{}, false
	}

	stream, err := app.Store.GetStreamByID(r.Context(), cs.Key, streamID)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to look up stream", "key", cs.Key, "func", "resolveVodTarget", "streamID", streamID, "err", err)
		return vodTarget{}, false
	}
	if stream == nil {
		http.Error(w, "stream not found", http.StatusNotFound)
		return vodTarget{}, false
	}
	if stream.IsLive {
		// The chunk list would keep growing under the build, so the result
		// would be an arbitrary prefix of the stream rather than the VOD.
		http.Error(w, "Cannot build a VOD for a live stream. Wait until the stream has ended (or use \"Stop current stream\"), then try again.", http.StatusConflict)
		return vodTarget{}, false
	}

	ext := vodExtension(stream)
	if ext == "" {
		http.Error(w, "This stream has no media stored, so it has no VOD to build.", http.StatusConflict)
		return vodTarget{}, false
	}

	total, withMedia, err := app.Store.CountTranscriptMedia(r.Context(), cs.Key, streamID)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to count transcript media", "key", cs.Key, "func", "resolveVodTarget", "streamID", streamID, "err", err)
		return vodTarget{}, false
	}

	return vodTarget{stream: stream, ext: ext, totalLines: total, mediaLines: withMedia}, true
}

// vodResponse builds the response body for a target, filling in the state from
// the tracked job and from whether the artifact is present in storage.
func (app *App) vodResponse(ctx context.Context, cs *ChannelState, target vodTarget) AdminVodResponse {
	stream := target.stream
	resp := AdminVodResponse{
		StreamID:     stream.StreamID,
		State:        vodStateNone,
		Format:       target.ext[1:], // drop the leading dot
		TotalLines:   target.totalLines,
		MediaLines:   target.mediaLines,
		MissingLines: max(target.totalLines-target.mediaLines, 0),
	}

	var status vodStatus
	if job := app.Vods.get(cs.Key, stream.StreamID); job != nil {
		status = job.status()
		resp.StartedAt = status.StartedAt
		resp.FinishedAt = status.FinishedAt
	}

	// A running build is the only state the registry decides on its own.
	// Otherwise storage is the source of truth: a VOD built before this server
	// started (or by another instance sharing the bucket) still counts as done,
	// and a job that failed left nothing behind to find.
	if status.State == vodStateRunning {
		resp.State = vodStateRunning
		resp.Phase = status.Phase
		return resp
	}

	key := storage.VodKey(cs.Key, stream.StreamID, target.ext)
	if app.vodArtifactExists(ctx, key) {
		resp.State = vodStateDone
		if app.Storage.IsLocal() {
			// Local media is served by the public download route rather than
			// straight from storage; the page prefixes its own channel base.
			resp.Path = fmt.Sprintf("/download/%s/vod/full%s", stream.StreamID, target.ext)
		} else {
			resp.URL = app.Storage.GetURL(key)
		}
		return resp
	}

	if status.State == vodStateFailed {
		resp.State = vodStateFailed
		resp.Error = status.Failure
	}
	return resp
}

// vodArtifactExists reports whether a finished VOD is in storage. It probes the
// object itself rather than the folder, because a build that dies mid-upload
// can leave the folder behind with nothing usable in it. A probe error is
// treated as absent — the worst case is offering a rebuild that wasn't needed.
func (app *App) vodArtifactExists(ctx context.Context, key string) bool {
	reader, err := app.Storage.Get(ctx, key)
	if err != nil {
		return false
	}
	reader.Close()
	return true
}

// getAdminVodHandler reports the state of a stream's full VOD: whether one
// exists, whether a build is running, and how much of the stream has media to
// include. Side-effect-free — the admin page polls it while a build runs.
func (app *App) getAdminVodHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	target, ok := app.resolveVodTarget(w, r, cs)
	if !ok {
		return
	}
	writeJSON(w, app.vodResponse(r.Context(), cs, target))
}

// postAdminVodHandler starts a full-VOD build, or joins the one already in
// flight. It never produces a second copy: an existing artifact is returned
// as-is, and a running build is reported back rather than duplicated.
func (app *App) postAdminVodHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	target, ok := app.resolveVodTarget(w, r, cs)
	if !ok {
		return
	}
	streamID := target.stream.StreamID

	// Nothing to stitch together.
	if target.mediaLines == 0 {
		http.Error(w, "No media is stored for this stream, so there is nothing to build.", http.StatusConflict)
		return
	}

	// Already built, or already building: hand back the current state. Checking
	// storage first means a rebuild is never started for a VOD that exists.
	switch resp := app.vodResponse(r.Context(), cs, target); resp.State {
	case vodStateDone:
		writeJSON(w, resp)
		return
	case vodStateRunning:
		// Same answer as joining a build below, so a caller cannot tell (and
		// does not need to tell) which of the two ways it joined.
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, resp)
		return
	}

	// Read the chunk list here rather than in the build: the build outlives the
	// request, and the store may be closed on shutdown while it is still going.
	fileIDs, err := app.Store.GetAllMediaFileIDs(r.Context(), cs.Key, streamID)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to get media file ids for vod", "key", cs.Key, "func", "postAdminVodHandler", "streamID", streamID, "err", err)
		return
	}
	if len(fileIDs) == 0 {
		http.Error(w, "No media is stored for this stream, so there is nothing to build.", http.StatusConflict)
		return
	}

	job, started := app.Vods.claim(cs.Key, streamID)
	if started {
		go app.buildVod(job, cs, target, fileIDs)

		app.notifyAdminAction(r, cs, "Started full VOD build",
			discord.AdminField{Name: "Stream ID", Value: streamID, Inline: true},
			discord.AdminField{Name: "Format", Value: target.ext[1:], Inline: true},
			discord.AdminField{Name: "Chunks", Value: strconv.Itoa(len(fileIDs)), Inline: true},
			discord.AdminField{Name: "Lines Without Media", Value: strconv.Itoa(max(target.totalLines-target.mediaLines, 0)), Inline: true},
			discord.AdminField{Name: "Stream Title", Value: target.stream.StreamTitle},
		)
		slog.Info("admin started full vod build", "key", cs.Key, "func", "postAdminVodHandler", "streamID", streamID, "chunks", len(fileIDs), "totalLines", target.totalLines, "mediaLines", target.mediaLines)
	} else {
		slog.Info("admin joined an in-flight vod build", "key", cs.Key, "func", "postAdminVodHandler", "streamID", streamID)
	}

	// Re-derived rather than read off the job: a very fast build may already be
	// done, and vodResponse is the one place that decides what to report.
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, app.vodResponse(r.Context(), cs, target))
}

// buildVod merges every stored chunk of a stream, converts the result into the
// stream's VOD container, and uploads it under the stream's fixed VOD key.
//
// It runs detached from both the request and app.wg: a build takes minutes on
// a long stream, and neither a client disconnect nor a shutdown should be able
// to interrupt shutdown waiting on ffmpeg. Cancellation still reaches the
// downloads through app.ctx, and because the upload is the last step and is
// atomic, an interrupted build simply leaves no artifact to find.
func (app *App) buildVod(job *vodJob, cs *ChannelState, target vodTarget, fileIDs []string) {
	streamID := target.stream.StreamID
	ext := target.ext
	start := time.Now()

	fail := func(err error) {
		job.finish(err)
		slog.Error("full vod build failed", "key", cs.Key, "func", "buildVod", "streamID", streamID, "chunks", len(fileIDs), "durationMs", time.Since(start).Milliseconds(), "err", err)
		app.Discord.NotifyAdminAction(cs.Key, "Full VOD build failed",
			discord.AdminField{Name: "Stream ID", Value: streamID, Inline: true},
			discord.AdminField{Name: "Error", Value: err.Error()},
		)
	}

	// A per-build name so two builds (different streams) never collide in the
	// temp dir, and a crashed build's leftovers are identifiable.
	tempName := "vod_" + streamID + "_" + shortuuid.New()

	job.setPhase(vodPhaseMerging)
	mergedRawPath, err := media.MergeRawAudio(app.ctx, app.Storage, app.TempDir, cs.Key, streamID, fileIDs, tempName)
	if err != nil {
		// MergeRawAudio cleans up its own partial output on error.
		fail(fmt.Errorf("merge raw audio: %w", err))
		return
	}
	defer os.Remove(mergedRawPath)

	job.setPhase(vodPhaseEncoding)
	tempOut := filepath.Join(app.TempDir, tempName+ext)
	// Video only needs a container rewrite; audio has to be re-encoded to m4a
	// or the result is broken. Same rule as clip creation.
	if ext == ".mp4" {
		err = app.Media.Remux(mergedRawPath, tempOut)
	} else {
		err = app.Media.Convert(mergedRawPath, tempOut)
	}
	if err != nil {
		os.Remove(tempOut)
		fail(fmt.Errorf("convert merged audio to %s: %w", ext, err))
		return
	}
	defer os.Remove(tempOut)

	job.setPhase(vodPhaseUploading)
	// Detached from app.ctx: a shutdown mid-upload should finish writing the
	// object rather than leave the build with nothing to show for its work.
	if err := app.uploadFile(context.WithoutCancel(app.ctx), storage.VodKey(cs.Key, streamID, ext), tempOut); err != nil {
		fail(fmt.Errorf("upload vod: %w", err))
		return
	}

	job.finish(nil)
	slog.Info("full vod build finished", "key", cs.Key, "func", "buildVod", "streamID", streamID, "chunks", len(fileIDs), "format", ext, "durationMs", time.Since(start).Milliseconds())
	app.Discord.NotifyAdminAction(cs.Key, "Full VOD build finished",
		discord.AdminField{Name: "Stream ID", Value: streamID, Inline: true},
		discord.AdminField{Name: "Format", Value: ext[1:], Inline: true},
		discord.AdminField{Name: "Took", Value: time.Since(start).Round(time.Second).String(), Inline: true},
		discord.AdminField{Name: "Stream Title", Value: target.stream.StreamTitle},
	)
}
