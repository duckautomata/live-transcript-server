package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"live-transcript-server/internal/model"
	"live-transcript-server/internal/storage"
)

// seedVodStream stores an offline stream plus `lines` transcript lines, the
// first `withMedia` of which have a raw chunk in storage. It returns the
// stream ID.
func seedVodStream(tb testing.TB, app *App, channel, streamID, mediaType string, lines, withMedia int) string {
	tb.Helper()
	ctx := context.Background()

	if err := app.Store.UpsertStream(ctx, &model.Stream{
		ChannelID:     channel,
		StreamID:      streamID,
		StreamTitle:   "VOD Test Stream",
		StartTime:     fmt.Sprintf("%d", time.Now().Unix()),
		IsLive:        false,
		MediaType:     mediaType,
		ActivatedTime: time.Now().UnixMicro(),
	}); err != nil {
		tb.Fatalf("upsert stream: %v", err)
	}

	transcript := make([]model.Line, 0, lines)
	for i := range lines {
		l := model.Line{
			ID:        i,
			Timestamp: i * 10,
			Segments:  json.RawMessage(`[{"timestamp":0,"text":"line"}]`),
		}
		if i < withMedia {
			l.FileID = fmt.Sprintf("file%d", i)
			l.MediaAvailable = true
			key := storage.RawKey(channel, streamID, l.FileID)
			body := fmt.Sprintf("chunk%d;", i)
			if _, err := app.Storage.Save(ctx, key, strings.NewReader(body), int64(len(body))); err != nil {
				tb.Fatalf("save raw chunk: %v", err)
			}
		}
		transcript = append(transcript, l)
	}
	if err := app.Store.ReplaceTranscript(ctx, channel, streamID, transcript); err != nil {
		tb.Fatalf("seed transcript: %v", err)
	}
	return streamID
}

func decodeVod(t *testing.T, rec *httptest.ResponseRecorder) AdminVodResponse {
	t.Helper()
	var resp AdminVodResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode vod response: %v (body=%s)", err, rec.Body.String())
	}
	return resp
}

// waitVodState polls the status endpoint until the build leaves the running
// state, then returns the final response.
func waitVodState(t *testing.T, mux *http.ServeMux, channel, streamID string) AdminVodResponse {
	t.Helper()
	var final AdminVodResponse
	waitFor(t, 5*time.Second, "vod build to finish", func() bool {
		rec := adminReq(t, mux, http.MethodGet, "/"+channel+"/admin/vod/"+streamID, "admin-"+channel, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("vod status: %d body=%s", rec.Code, rec.Body.String())
		}
		final = decodeVod(t, rec)
		return final.State != vodStateRunning
	})
	return final
}

func TestVodStatusReportsMissingLines(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	app.Media = fakeProcessor{}
	// 5 lines, only 3 of which ever got media uploaded.
	seedVodStream(t, app, "doki", "stream-vod", "audio", 5, 3)

	rec := adminReq(t, mux, http.MethodGet, "/doki/admin/vod/stream-vod", "admin-doki", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeVod(t, rec)

	if resp.State != vodStateNone {
		t.Errorf("state=%q want %q", resp.State, vodStateNone)
	}
	if resp.TotalLines != 5 || resp.MediaLines != 3 || resp.MissingLines != 2 {
		t.Errorf("counts: total=%d media=%d missing=%d want 5/3/2", resp.TotalLines, resp.MediaLines, resp.MissingLines)
	}
	if resp.Format != "m4a" {
		t.Errorf("format=%q want m4a", resp.Format)
	}
	if resp.URL != "" || resp.Path != "" {
		t.Errorf("no VOD exists yet but got url=%q path=%q", resp.URL, resp.Path)
	}
}

func TestVodStatusRequiresAdminKey(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	seedVodStream(t, app, "doki", "stream-vod", "audio", 2, 2)

	for _, tc := range []struct{ name, method string }{
		{"get", http.MethodGet},
		{"post", http.MethodPost},
	} {
		rec := adminReq(t, mux, tc.method, "/doki/admin/vod/stream-vod", "", nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without key: status=%d want 403", tc.name, rec.Code)
		}
	}
}

func TestVodRefusesLiveStream(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	seedVodStream(t, app, "doki", "stream-vod", "audio", 2, 2)
	if err := app.Store.SetStreamLive(context.Background(), "doki", "stream-vod", true); err != nil {
		t.Fatalf("set live: %v", err)
	}

	rec := adminReq(t, mux, http.MethodPost, "/doki/admin/vod/stream-vod", "admin-doki", nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("post on live stream: status=%d want 409, body=%s", rec.Code, rec.Body.String())
	}
	rec = adminReq(t, mux, http.MethodGet, "/doki/admin/vod/stream-vod", "admin-doki", nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("get on live stream: status=%d want 409, body=%s", rec.Code, rec.Body.String())
	}

	// Nothing may have been written for a live stream.
	if _, err := os.Stat(filepath.Join(app.TempDir, "doki", "stream-vod", "vod")); !os.IsNotExist(err) {
		t.Errorf("live stream produced a vod folder (stat err: %v)", err)
	}
}

func TestVodRejectsUnknownAndMedialessStreams(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})

	rec := adminReq(t, mux, http.MethodPost, "/doki/admin/vod/nope", "admin-doki", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown stream: status=%d want 404", rec.Code)
	}

	rec = adminReq(t, mux, http.MethodPost, "/doki/admin/vod/bad..id", "admin-doki", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid stream id: status=%d want 400", rec.Code)
	}

	// A "none" media-type stream never stored audio, so there is nothing to build.
	seedVodStream(t, app, "doki", "text-only", "none", 3, 0)
	rec = adminReq(t, mux, http.MethodPost, "/doki/admin/vod/text-only", "admin-doki", nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("media-less stream: status=%d want 409, body=%s", rec.Code, rec.Body.String())
	}
}

func TestVodBuildProducesOneFile(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	// The fake processor stands in for ffmpeg: it copies the merged raw file
	// through so the assertions can see the stitched chunks end to end.
	app.Media = fakeProcessor{convert: func(in, out string) error {
		data, err := os.ReadFile(in)
		if err != nil {
			return err
		}
		return os.WriteFile(out, data, 0644)
	}}
	seedVodStream(t, app, "doki", "stream-vod", "audio", 3, 3)

	rec := adminReq(t, mux, http.MethodPost, "/doki/admin/vod/stream-vod", "admin-doki", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start build: status=%d want 202, body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeVod(t, rec).State; got != vodStateRunning {
		t.Errorf("start build: state=%q want %q", got, vodStateRunning)
	}

	final := waitVodState(t, mux, "doki", "stream-vod")
	if final.State != vodStateDone {
		t.Fatalf("state=%q want %q (error=%q)", final.State, vodStateDone, final.Error)
	}
	if final.URL != "" {
		t.Errorf("local storage should not report an absolute url, got %q", final.URL)
	}
	// The link names the file after the stream, and the object itself carries a
	// random ID so it cannot be reached by guessing from the stream ID.
	if !strings.HasPrefix(final.Path, "/download/stream-vod/vod/") {
		t.Fatalf("path=%q want the local download route", final.Path)
	}
	if !strings.Contains(final.Path, "?name=VOD-Test-Stream") {
		t.Errorf("path=%q does not carry the stream's name", final.Path)
	}
	if strings.Contains(final.Path, "/full.m4a") {
		t.Errorf("path=%q uses a guessable file name", final.Path)
	}

	// The artifact holds every chunk, in line order.
	vodDir := filepath.Join(app.TempDir, "doki", "stream-vod", "vod")
	entries, err := os.ReadDir(vodDir)
	if err != nil {
		t.Fatalf("read vod dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("vod dir holds %d files, want exactly 1: %v", len(entries), entries)
	}
	if name := entries[0].Name(); !strings.HasSuffix(name, ".m4a") || len(name) < len("x.m4a")+8 {
		t.Errorf("vod file %q does not look randomly named", name)
	}
	data, err := os.ReadFile(filepath.Join(vodDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read built vod: %v", err)
	}
	if want := "chunk0;chunk1;chunk2;"; string(data) != want {
		t.Errorf("vod content=%q want %q", string(data), want)
	}

	// And the link the admin page is handed actually serves it, as an
	// attachment named after the stream.
	req := httptest.NewRequest(http.MethodGet, "/doki"+final.Path, nil)
	dl := httptest.NewRecorder()
	mux.ServeHTTP(dl, req)
	if dl.Code != http.StatusOK {
		t.Fatalf("download: status=%d want 200", dl.Code)
	}
	if got := dl.Header().Get("Content-Disposition"); got != `attachment; filename="VOD-Test-Stream.m4a"` {
		t.Errorf("Content-Disposition=%q", got)
	}
	if dl.Body.String() != "chunk0;chunk1;chunk2;" {
		t.Errorf("download body=%q", dl.Body.String())
	}
}

// The remote-storage link has to carry Cloudflare's download hint, otherwise a
// multi-gigabyte VOD opens in a player tab instead of saving.
func TestVodRemoteStorageLinkForcesDownload(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	app.Media = fakeProcessor{}
	seedVodStream(t, app, "doki", "stream-vod", "audio", 2, 2)
	app.Storage = &MockRemoteStorage{LocalStorage: app.Storage.(*storage.LocalStorage)}

	adminReq(t, mux, http.MethodPost, "/doki/admin/vod/stream-vod", "admin-doki", nil)
	final := waitVodState(t, mux, "doki", "stream-vod")
	if final.State != vodStateDone {
		t.Fatalf("state=%q want done (error=%q)", final.State, final.Error)
	}
	if final.Path != "" {
		t.Errorf("remote storage should not report a local path, got %q", final.Path)
	}
	if !strings.HasPrefix(final.URL, "https://r2.example.com/doki/stream-vod/vod/") {
		t.Fatalf("url=%q is not the remote object", final.URL)
	}
	if !strings.HasSuffix(final.URL, "?download=true&name=VOD-Test-Stream.m4a") {
		t.Errorf("url=%q is missing the download hint and file name", final.URL)
	}
}

// listErrStorage fails every listing, standing in for a storage backend that
// is briefly unreachable.
type listErrStorage struct {
	*storage.LocalStorage
}

func (listErrStorage) List(ctx context.Context, prefix string) ([]string, error) {
	return nil, fmt.Errorf("storage unreachable")
}

// With random names, "no VOD found" and "could not look" are the same answer
// from a listing. Building on the second would upload a rival copy under a
// different name, so an unreadable storage must stop the build instead.
func TestVodBuildRefusedWhenStorageUnreadable(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	built := make(chan struct{}, 1)
	app.Media = fakeProcessor{convert: func(in, out string) error {
		built <- struct{}{}
		return writePlaceholder(out)
	}}
	seedVodStream(t, app, "doki", "stream-vod", "audio", 2, 2)
	app.Storage = listErrStorage{LocalStorage: app.Storage.(*storage.LocalStorage)}

	rec := adminReq(t, mux, http.MethodPost, "/doki/admin/vod/stream-vod", "admin-doki", nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("post: status=%d want 502, body=%s", rec.Code, rec.Body.String())
	}
	rec = adminReq(t, mux, http.MethodGet, "/doki/admin/vod/stream-vod", "admin-doki", nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("get: status=%d want 502, body=%s", rec.Code, rec.Body.String())
	}

	select {
	case <-built:
		t.Error("a build ran even though storage could not be checked for an existing copy")
	case <-time.After(200 * time.Millisecond):
	}
}

// A title that sanitizes away to nothing must still yield a named file.
func TestVodDownloadNameFallsBackToStreamID(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	app.Media = fakeProcessor{}
	seedVodStream(t, app, "doki", "stream-vod", "audio", 1, 1)
	if err := app.Store.UpsertStream(context.Background(), &model.Stream{
		ChannelID: "doki", StreamID: "stream-vod", StreamTitle: "歌枠", MediaType: "audio",
	}); err != nil {
		t.Fatalf("retitle stream: %v", err)
	}

	adminReq(t, mux, http.MethodPost, "/doki/admin/vod/stream-vod", "admin-doki", nil)
	final := waitVodState(t, mux, "doki", "stream-vod")
	if final.State != vodStateDone {
		t.Fatalf("state=%q want done (error=%q)", final.State, final.Error)
	}
	if !strings.Contains(final.Path, "?name=stream-vod") {
		t.Errorf("path=%q should fall back to the stream id for its file name", final.Path)
	}
}

// TestVodBuildIsSingleFlight is the core of the feature: however many admins
// press the button at once, exactly one build runs and one copy is produced.
func TestVodBuildIsSingleFlight(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})

	var builds int
	var mu sync.Mutex
	release := make(chan struct{})
	app.Media = fakeProcessor{convert: func(in, out string) error {
		mu.Lock()
		builds++
		mu.Unlock()
		<-release // hold the build open so every request races the same job
		return writePlaceholder(out)
	}}
	seedVodStream(t, app, "doki", "stream-vod", "audio", 4, 4)

	const callers = 8
	var wg sync.WaitGroup
	codes := make([]int, callers)
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := adminReq(t, mux, http.MethodPost, "/doki/admin/vod/stream-vod", "admin-doki", nil)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusAccepted {
			t.Errorf("caller %d: status=%d want 202", i, code)
		}
	}

	close(release)
	final := waitVodState(t, mux, "doki", "stream-vod")
	if final.State != vodStateDone {
		t.Fatalf("state=%q want %q (error=%q)", final.State, vodStateDone, final.Error)
	}

	mu.Lock()
	defer mu.Unlock()
	if builds != 1 {
		t.Errorf("%d concurrent requests ran %d builds, want exactly 1", callers, builds)
	}
}

// TestVodBuildSkippedWhenAlreadyBuilt covers the other half of "one copy":
// pressing the button again after a finished build must not redo the work.
func TestVodBuildSkippedWhenAlreadyBuilt(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	var builds int
	var mu sync.Mutex
	app.Media = fakeProcessor{convert: func(in, out string) error {
		mu.Lock()
		builds++
		mu.Unlock()
		return writePlaceholder(out)
	}}
	seedVodStream(t, app, "doki", "stream-vod", "audio", 2, 2)

	adminReq(t, mux, http.MethodPost, "/doki/admin/vod/stream-vod", "admin-doki", nil)
	if final := waitVodState(t, mux, "doki", "stream-vod"); final.State != vodStateDone {
		t.Fatalf("first build state=%q want done (error=%q)", final.State, final.Error)
	}

	// Second press: answered from the existing artifact, no rebuild.
	rec := adminReq(t, mux, http.MethodPost, "/doki/admin/vod/stream-vod", "admin-doki", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("second press: status=%d want 200 (already built)", rec.Code)
	}
	if got := decodeVod(t, rec).State; got != vodStateDone {
		t.Errorf("second press: state=%q want %q", got, vodStateDone)
	}

	mu.Lock()
	defer mu.Unlock()
	if builds != 1 {
		t.Errorf("ran %d builds, want 1", builds)
	}
}

func TestVodBuildFailureIsReportedAndRetryable(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	var attempts int
	var mu sync.Mutex
	app.Media = fakeProcessor{convert: func(in, out string) error {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			return fmt.Errorf("ffmpeg exploded")
		}
		return writePlaceholder(out)
	}}
	seedVodStream(t, app, "doki", "stream-vod", "audio", 2, 2)

	adminReq(t, mux, http.MethodPost, "/doki/admin/vod/stream-vod", "admin-doki", nil)
	failed := waitVodState(t, mux, "doki", "stream-vod")
	if failed.State != vodStateFailed {
		t.Fatalf("state=%q want %q", failed.State, vodStateFailed)
	}
	if !strings.Contains(failed.Error, "ffmpeg exploded") {
		t.Errorf("error=%q does not name the underlying failure", failed.Error)
	}

	// A failed build leaves nothing behind, so the next press starts over.
	rec := adminReq(t, mux, http.MethodPost, "/doki/admin/vod/stream-vod", "admin-doki", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("retry: status=%d want 202, body=%s", rec.Code, rec.Body.String())
	}
	if final := waitVodState(t, mux, "doki", "stream-vod"); final.State != vodStateDone {
		t.Errorf("retry state=%q want done (error=%q)", final.State, final.Error)
	}
}

func TestVodVideoStreamRendersMp4(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	remuxed := make(chan string, 1)
	app.Media = fakeProcessor{
		remux: func(in, out string) error {
			remuxed <- out
			return writePlaceholder(out)
		},
		convert: func(in, out string) error {
			t.Errorf("video stream must be remuxed, not converted")
			return writePlaceholder(out)
		},
	}
	seedVodStream(t, app, "doki", "vid", "video", 2, 2)

	rec := adminReq(t, mux, http.MethodPost, "/doki/admin/vod/vid", "admin-doki", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: status=%d want 202, body=%s", rec.Code, rec.Body.String())
	}
	final := waitVodState(t, mux, "doki", "vid")
	if final.State != vodStateDone {
		t.Fatalf("state=%q want done (error=%q)", final.State, final.Error)
	}
	if final.Format != "mp4" || !strings.Contains(final.Path, ".mp4?") {
		t.Errorf("format=%q path=%q want an mp4", final.Format, final.Path)
	}
	select {
	case out := <-remuxed:
		if filepath.Ext(out) != ".mp4" {
			t.Errorf("remux output %q is not an mp4", out)
		}
	default:
		t.Error("remux was never called")
	}
}

// Deleting a stream must not leave its build record behind in the registry.
func TestVodJobForgottenOnStreamDelete(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})
	app.Media = fakeProcessor{}
	seedVodStream(t, app, "doki", "stream-vod", "audio", 2, 2)

	adminReq(t, mux, http.MethodPost, "/doki/admin/vod/stream-vod", "admin-doki", nil)
	waitVodState(t, mux, "doki", "stream-vod")
	if app.Vods.get("doki", "stream-vod") == nil {
		t.Fatal("expected a tracked job after a build")
	}

	rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/stream/stream-vod?media=true", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete stream: status=%d want 204, body=%s", rec.Code, rec.Body.String())
	}
	if job := app.Vods.get("doki", "stream-vod"); job != nil {
		t.Errorf("job record survived the stream delete: %+v", job.status())
	}
}
