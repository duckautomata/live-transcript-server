package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"live-transcript-server/internal/config"
	"live-transcript-server/internal/model"
)

// segment mirrors the transcript segment shape for decoding Segments in tests.
type segment struct {
	Text string `json:"text"`
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:", config.DatabaseConfig{SkipWarmup: true})
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStore_GetLastLine(t *testing.T) {
	s := newTestStore(t)

	ctx := context.Background()
	channelID := "test-last-line"

	// Test Empty
	line, err := s.GetLastLine(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("failed to get last line (empty): %v", err)
	}
	if line != nil {
		t.Error("expected nil line for empty db")
	}

	// Insert Lines
	lines := []model.Line{
		{ID: 0, Timestamp: 100, Segments: json.RawMessage(`[{"text": "First"}]`)},
		{ID: 2, Timestamp: 300, Segments: json.RawMessage(`[{"text": "Third"}]`)},
		{ID: 1, Timestamp: 200, Segments: json.RawMessage(`[{"text": "Second"}]`)},
	}
	if err := s.ReplaceTranscript(ctx, channelID, "test-stream", lines); err != nil {
		t.Fatalf("failed to replace transcript: %v", err)
	}

	// Test GetLastLine
	line, err = s.GetLastLine(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("failed to get last line: %v", err)
	}
	if line == nil {
		t.Fatal("expected line, got nil")
	}
	if line.ID != 2 {
		t.Errorf("expected line ID 2, got %d", line.ID)
	}
	if line.Segments == nil {
		t.Fatal("expected segments, got nil")
	}
	if len(line.Segments) == 0 {
		t.Fatal("expected segments, got empty")
	}
	var segments []segment
	if err := json.Unmarshal(line.Segments, &segments); err != nil {
		t.Fatalf("failed to unmarshal segments: %v", err)
	}
	if len(segments) == 0 {
		t.Fatal("expected segments, got empty")
	}
	if segments[0].Text != "Third" {
		t.Errorf("expected segment 'Third', got %s", segments[0].Text)
	}
}

func TestStore_StreamOperations(t *testing.T) {
	s := newTestStore(t)

	ctx := context.Background()
	channelID := "test-stream-ops"

	// 1. Test GetStream (Empty)
	st, err := s.GetRecentStream(ctx, channelID)
	if err != nil {
		t.Fatalf("GetStream failed: %v", err)
	}
	if st != nil {
		t.Fatal("expected nil stream initially")
	}

	// 2. Test UpsertStream
	newStream := &model.Stream{
		ChannelID:   channelID,
		StreamID:    "vid1",
		StreamTitle: "Video 1",
		StartTime:   "1000",
		IsLive:      true,
		MediaType:   "audio",
	}
	if err := s.UpsertStream(ctx, newStream); err != nil {
		t.Fatalf("UpsertStream failed: %v", err)
	}
	newStream2 := &model.Stream{
		ChannelID:   "different-id",
		StreamID:    "vid2",
		StreamTitle: "Video 2",
		StartTime:   "2000",
		IsLive:      true,
		MediaType:   "video",
	}
	if err := s.UpsertStream(ctx, newStream2); err != nil {
		t.Fatalf("UpsertStream (different ID) failed: %v", err)
	}

	st, err = s.GetRecentStream(ctx, channelID)
	if err != nil {
		t.Fatalf("GetStream failed: %v", err)
	}
	if st == nil {
		t.Fatal("expected stream to be found")
	}
	if st.StreamTitle != "Video 1" {
		t.Errorf("expected title 'Video 1', got '%s'", st.StreamTitle)
	}

	// 3. Test UpsertStream Update
	newStream.StreamTitle = "Video 1 Updated"
	newStream.StartTime = "1001"
	if err := s.UpsertStream(ctx, newStream); err != nil {
		t.Fatalf("UpsertStream (update) failed: %v", err)
	}

	st, err = s.GetRecentStream(ctx, channelID)
	if err != nil {
		t.Fatalf("GetStream (update) failed: %v", err)
	}
	if st.StreamTitle != "Video 1 Updated" {
		t.Errorf("expected updated title, got '%s'", st.StreamTitle)
	}
	if st.StartTime != "1001" {
		t.Errorf("expected updated start time, got '%s'", st.StartTime)
	}

	// 4. Test SetStreamLive
	if err := s.SetStreamLive(ctx, channelID, "vid1", false); err != nil {
		t.Fatalf("SetStreamLive failed: %v", err)
	}

	st, err = s.GetRecentStream(ctx, channelID)
	if err != nil {
		t.Fatalf("GetStream (live update) failed: %v", err)
	}
	if st.IsLive {
		t.Error("expected stream to be not live")
	}
}

func TestStore_TranscriptOperations(t *testing.T) {
	s := newTestStore(t)

	ctx := context.Background()
	channelID := "test-transcript-ops"

	// 0. Get empty transcript
	lines, err := s.GetTranscript(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("GetTranscript (empty) failed: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("expected empty transcript, got %d lines", len(lines))
	}

	lastId, err := s.GetLastLineID(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("GetLastLineID (empty) failed: %v", err)
	}
	if lastId != -1 {
		t.Errorf("expected last ID to be -1, got %d", lastId)
	}

	lastLine, err := s.GetLastLine(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("GetLastLine (empty) failed: %v", err)
	}
	if lastLine != nil {
		t.Errorf("expected last line to be nil, got %v", lastLine)
	}

	// 1. InsertNextLine
	line0 := model.Line{ID: 0, Timestamp: 100, Segments: json.RawMessage(`[{"text": "Hello"}]`)}
	if err := s.InsertNextLine(ctx, channelID, "test-stream", line0); err != nil {
		t.Fatalf("InsertNextLine failed: %v", err)
	}

	// 2. GetTranscript
	lines, err = s.GetTranscript(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("GetTranscript failed: %v", err)
	}
	var segments []segment
	json.Unmarshal(lines[0].Segments, &segments)
	if len(lines) != 1 || segments[0].Text != "Hello" {
		t.Errorf("transcript content mismatch, expected %v, got %v", line0, lines)
	}

	// 3. GetLastLineID
	lastID, err := s.GetLastLineID(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("GetLastLineID failed: %v", err)
	}
	if lastID != 0 {
		t.Errorf("expected last ID 0, got %d", lastID)
	}

	// 4. ReplaceTranscript
	newLines := []model.Line{
		{ID: 0, Timestamp: 100, Segments: json.RawMessage(`[{"text": "New Hello"}]`)},
		{ID: 1, Timestamp: 200, Segments: json.RawMessage(`[{"text": "New World"}]`)},
	}
	if err := s.ReplaceTranscript(ctx, channelID, "test-stream", newLines); err != nil {
		t.Fatalf("ReplaceTranscript failed: %v", err)
	}

	lines, err = s.GetTranscript(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("GetTranscript after replace failed: %v", err)
	}
	json.Unmarshal(lines[1].Segments, &segments)
	if len(lines) != 2 || segments[0].Text != "New World" {
		t.Error("replaced transcript content mismatch")
	}

	// 5. ClearTranscript
	if err := s.DeleteTranscript(ctx, channelID, "test-stream"); err != nil {
		t.Fatalf("DeleteTranscript failed: %v", err)
	}

	lines, err = s.GetTranscript(ctx, channelID, "test-stream")
	if err != nil || len(lines) != 0 {
		t.Error("expected empty transcript after clear")
	}
}

func TestStore_InsertNextLine(t *testing.T) {
	s := newTestStore(t)

	ctx := context.Background()
	channelID := "test-insert-next"
	streamID := "s1"

	// First line into an empty transcript must have ID 0.
	if err := s.InsertNextLine(ctx, channelID, streamID, model.Line{ID: 0, Timestamp: 100, Segments: json.RawMessage(`[]`)}); err != nil {
		t.Fatalf("InsertNextLine (first) failed: %v", err)
	}

	// Sequential append succeeds.
	if err := s.InsertNextLine(ctx, channelID, streamID, model.Line{ID: 1, Timestamp: 200, Segments: json.RawMessage(`[]`)}); err != nil {
		t.Fatalf("InsertNextLine (second) failed: %v", err)
	}

	// A gap is rejected with ErrOutOfSync.
	err := s.InsertNextLine(ctx, channelID, streamID, model.Line{ID: 3, Timestamp: 400, Segments: json.RawMessage(`[]`)})
	if !errors.Is(err, ErrOutOfSync) {
		t.Fatalf("expected ErrOutOfSync for gapped ID, got %v", err)
	}

	// A duplicate is rejected with ErrOutOfSync.
	err = s.InsertNextLine(ctx, channelID, streamID, model.Line{ID: 1, Timestamp: 200, Segments: json.RawMessage(`[]`)})
	if !errors.Is(err, ErrOutOfSync) {
		t.Fatalf("expected ErrOutOfSync for duplicate ID, got %v", err)
	}

	// Wrong first ID on an empty transcript is rejected too.
	err = s.InsertNextLine(ctx, channelID, "empty-stream", model.Line{ID: 1, Timestamp: 100, Segments: json.RawMessage(`[]`)})
	if !errors.Is(err, ErrOutOfSync) {
		t.Fatalf("expected ErrOutOfSync for wrong first ID, got %v", err)
	}

	// Failed inserts must not have modified the transcript.
	lastID, err := s.GetLastLineID(ctx, channelID, streamID)
	if err != nil {
		t.Fatalf("GetLastLineID failed: %v", err)
	}
	if lastID != 1 {
		t.Errorf("expected last ID 1, got %d", lastID)
	}
}

func TestStore_DeleteStreamCascade(t *testing.T) {
	s := newTestStore(t)

	ctx := context.Background()
	channelID := "test-cascade"

	// Two streams, each with transcript lines.
	for _, streamID := range []string{"doomed", "survivor"} {
		if err := s.UpsertStream(ctx, &model.Stream{ChannelID: channelID, StreamID: streamID, StreamTitle: streamID, IsLive: false}); err != nil {
			t.Fatalf("UpsertStream failed: %v", err)
		}
		lines := []model.Line{
			{ID: 0, Timestamp: 100, Segments: json.RawMessage(`[]`)},
			{ID: 1, Timestamp: 200, Segments: json.RawMessage(`[]`)},
		}
		if err := s.ReplaceTranscript(ctx, channelID, streamID, lines); err != nil {
			t.Fatalf("ReplaceTranscript failed: %v", err)
		}
	}

	if err := s.DeleteStreamCascade(ctx, channelID, "doomed"); err != nil {
		t.Fatalf("DeleteStreamCascade failed: %v", err)
	}

	// Stream row and transcript rows are both gone.
	st, err := s.GetStreamByID(ctx, channelID, "doomed")
	if err != nil {
		t.Fatalf("GetStreamByID failed: %v", err)
	}
	if st != nil {
		t.Error("expected doomed stream to be deleted")
	}
	lines, err := s.GetTranscript(ctx, channelID, "doomed")
	if err != nil {
		t.Fatalf("GetTranscript failed: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("expected doomed transcript to be deleted, got %d lines", len(lines))
	}

	// The other stream is untouched.
	st, err = s.GetStreamByID(ctx, channelID, "survivor")
	if err != nil {
		t.Fatalf("GetStreamByID (survivor) failed: %v", err)
	}
	if st == nil {
		t.Error("expected survivor stream to remain")
	}
	lines, err = s.GetTranscript(ctx, channelID, "survivor")
	if err != nil {
		t.Fatalf("GetTranscript (survivor) failed: %v", err)
	}
	if len(lines) != 2 {
		t.Errorf("expected survivor transcript to remain, got %d lines", len(lines))
	}
}

func TestStore_SetMediaAvailable(t *testing.T) {
	s := newTestStore(t)

	ctx := context.Background()
	channelID := "test-set-media"
	streamID := "s1"

	// Missing line reports ErrNotFound.
	err := s.SetMediaAvailable(ctx, channelID, streamID, 0, "f0", true)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing line, got %v", err)
	}

	// Happy path.
	if err := s.InsertNextLine(ctx, channelID, streamID, model.Line{ID: 0, Timestamp: 100, Segments: json.RawMessage(`[]`)}); err != nil {
		t.Fatalf("InsertNextLine failed: %v", err)
	}
	if err := s.SetMediaAvailable(ctx, channelID, streamID, 0, "f0", true); err != nil {
		t.Fatalf("SetMediaAvailable failed: %v", err)
	}
	files, err := s.GetLastAvailableMediaFiles(ctx, channelID, streamID, -1)
	if err != nil {
		t.Fatalf("GetLastAvailableMediaFiles failed: %v", err)
	}
	if files[0] != "f0" {
		t.Errorf("expected media file f0 for line 0, got %v", files)
	}
}

// TestStore_MemoryDBSharedAcrossQueries guards against the pooled-connection
// pitfall where each pool connection gets its own empty :memory: database.
// Concurrent queries would then hit connections without the schema or data.
func TestStore_MemoryDBSharedAcrossQueries(t *testing.T) {
	s := newTestStore(t)

	ctx := context.Background()
	if err := s.UpsertStream(ctx, &model.Stream{ChannelID: "ch1", StreamID: "s1", StreamTitle: "Title", IsLive: true}); err != nil {
		t.Fatalf("UpsertStream failed: %v", err)
	}

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exists, err := s.StreamExists(ctx, "ch1", "s1")
			if err != nil {
				t.Errorf("StreamExists failed: %v", err)
				return
			}
			if !exists {
				t.Error("expected stream to be visible on every connection")
			}
		}()
	}
	wg.Wait()
}

func TestStore_GetLastAvailableMediaFiles(t *testing.T) {
	s := newTestStore(t)

	ctx := context.Background()
	channelID := "test-media-ids"

	// Test Empty
	files, err := s.GetLastAvailableMediaFiles(ctx, channelID, "test-stream", 10)
	if err != nil {
		t.Fatalf("failed to get last media files (empty): %v", err)
	}
	if len(files) != 0 {
		t.Error("expected empty last media files for empty db")
	}

	// Test with data but no media available
	lines := []model.Line{
		{ID: 0, Timestamp: 100, Segments: json.RawMessage(`[{"text": "First"}]`)},
		{ID: 2, Timestamp: 300, Segments: json.RawMessage(`[{"text": "Third"}]`)},
		{ID: 1, Timestamp: 200, Segments: json.RawMessage(`[{"text": "Second"}]`)},
	}
	if err := s.ReplaceTranscript(ctx, channelID, "test-stream", lines); err != nil {
		t.Fatalf("failed to replace transcript: %v", err)
	}

	files, err = s.GetLastAvailableMediaFiles(ctx, channelID, "test-stream", 10)
	if err != nil {
		t.Fatalf("failed to get last media files: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 last media files, got %d", len(files))
	}

	// Test with some media available
	lines = []model.Line{
		{ID: 0, Timestamp: 100, MediaAvailable: true, FileID: "f0", Segments: json.RawMessage(`[{"text": "First"}]`)},
		{ID: 2, Timestamp: 300, MediaAvailable: false, Segments: json.RawMessage(`[{"text": "Third"}]`)},
		{ID: 1, Timestamp: 200, MediaAvailable: true, FileID: "f1", Segments: json.RawMessage(`[{"text": "Second"}]`)},
	}
	if err := s.ReplaceTranscript(ctx, channelID, "test-stream", lines); err != nil {
		t.Fatalf("failed to replace transcript: %v", err)
	}

	files, err = s.GetLastAvailableMediaFiles(ctx, channelID, "test-stream", 10)
	if err != nil {
		t.Fatalf("failed to get last media files: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("expected 2 last media files, got %d", len(files))
	}
	if files[0] != "f0" || files[1] != "f1" {
		t.Errorf("expected last media files [0:f0, 1:f1], got %v", files)
	}

	// Test with limit
	files, err = s.GetLastAvailableMediaFiles(ctx, channelID, "test-stream", 1)
	if err != nil {
		t.Fatalf("failed to get last media files: %v", err)
	}
	if len(files) != 1 {
		t.Errorf("expected 1 last media file, got %d", len(files))
	}
	// The query orders by line_id DESC, so the limit keeps the latest line (ID 1).
	if val, ok := files[1]; !ok || val != "f1" {
		t.Errorf("expected last media file ID 1:f1, got %v", files)
	}
}

func TestStore_GetPastStreams(t *testing.T) {
	s := newTestStore(t)

	ctx := context.Background()
	channelID := "test-past-streams"

	// 1. No streams
	streams, err := s.GetPastStreams(ctx, channelID, "any")
	if err != nil {
		t.Fatalf("GetPastStreams failed: %v", err)
	}
	if len(streams) != 0 {
		t.Errorf("expected 0 streams, got %d", len(streams))
	}

	// 2. Insert Active Stream (Live)
	activeStream := &model.Stream{
		ChannelID:   channelID,
		StreamID:    "active1",
		StreamTitle: "Active 1",
		StartTime:   "1000",
		IsLive:      true,
		MediaType:   "audio",
	}
	if err := s.UpsertStream(ctx, activeStream); err != nil {
		t.Fatalf("UpsertStream active failed: %v", err)
	}

	streams, err = s.GetPastStreams(ctx, channelID, "active1")
	if err != nil {
		t.Fatalf("GetPastStreams failed: %v", err)
	}
	if len(streams) != 0 {
		t.Errorf("expected 0 past streams (active IS live), got %d", len(streams))
	}

	// 3. Insert Past Stream (Not Live)
	pastStream := &model.Stream{
		ChannelID:   channelID,
		StreamID:    "past1",
		StreamTitle: "Past 1",
		StartTime:   "900",
		IsLive:      false,
		MediaType:   "audio",
	}
	if err := s.UpsertStream(ctx, pastStream); err != nil {
		t.Fatalf("UpsertStream past failed: %v", err)
	}
	// Also insert another past stream
	pastStream2 := &model.Stream{
		ChannelID:   channelID,
		StreamID:    "past2",
		StreamTitle: "Past 2",
		StartTime:   "800", // Older
		IsLive:      false,
		MediaType:   "audio",
	}
	if err := s.UpsertStream(ctx, pastStream2); err != nil {
		t.Fatalf("UpsertStream past2 failed: %v", err)
	}

	// 4. GetPastStreams
	streams, err = s.GetPastStreams(ctx, channelID, "active1")
	if err != nil {
		t.Fatalf("GetPastStreams failed: %v", err)
	}
	if len(streams) != 2 {
		t.Errorf("expected 2 past streams, got %d", len(streams))
	} else {
		// Expect order: newest first (past1, then past2)
		if streams[0].StreamID != "past1" {
			t.Errorf("expected first stream to be past1, got %s", streams[0].StreamID)
		}
		if streams[1].StreamID != "past2" {
			t.Errorf("expected second stream to be past2, got %s", streams[1].StreamID)
		}
	}

	// 5. Test Exclude ID: the query filters stream_id != excludeStreamID,
	// so passing "past1" must exclude it even though it is not live.
	streams, err = s.GetPastStreams(ctx, channelID, "past1")
	if err != nil {
		t.Fatalf("GetPastStreams exclude failed: %v", err)
	}
	if len(streams) != 1 {
		t.Errorf("expected 1 past stream (excluding past1), got %d", len(streams))
	}
	if streams[0].StreamID != "past2" {
		t.Errorf("expected stream to be past2, got %s", streams[0].StreamID)
	}
}

func TestStore_DeleteStream(t *testing.T) {
	s := newTestStore(t)

	ctx := context.Background()
	channelID := "test-delete-stream"
	activeID := "stream-to-delete"

	// 1. Insert Stream
	stream := &model.Stream{
		ChannelID:     channelID,
		StreamID:      activeID,
		StreamTitle:   "To Be Deleted",
		StartTime:     "1000",
		IsLive:        true,
		MediaType:     "audio",
		ActivatedTime: 2000,
	}
	if err := s.UpsertStream(ctx, stream); err != nil {
		t.Fatalf("UpsertStream failed: %v", err)
	}

	stream2 := &model.Stream{
		ChannelID:   channelID,
		StreamID:    "another-stream",
		StreamTitle: "Should Not Be Deleted",
		StartTime:   "1000",
		IsLive:      false,
		MediaType:   "audio",
	}
	if err := s.UpsertStream(ctx, stream2); err != nil {
		t.Fatalf("UpsertStream failed: %v", err)
	}

	// 2. Verify existence
	// Note: GetRecentStream retrieves the latest stream.
	st, err := s.GetRecentStream(ctx, channelID)
	if err != nil {
		t.Fatalf("GetStream failed: %v", err)
	}
	if st == nil || st.StreamID != activeID {
		t.Fatal("expected stream to be active")
	}

	// 3. Delete Stream
	if err := s.DeleteStream(ctx, channelID, activeID); err != nil {
		t.Fatalf("DeleteStream failed: %v", err)
	}

	// 4. Verify deletion
	st, err = s.GetRecentStream(ctx, channelID)
	if err != nil {
		t.Fatalf("GetStream after delete failed: %v", err)
	}
	if st == nil {
		t.Errorf("expected to retrieve another stream, got nil")
	}
	if st.StreamID != "another-stream" {
		t.Errorf("expected another stream to be active, got %s", st.StreamID)
	}

	// 5. Delete another stream
	if err := s.DeleteStream(ctx, channelID, "another-stream"); err != nil {
		t.Fatalf("DeleteStream failed: %v", err)
	}

	// 6. Verify deletion
	st, err = s.GetRecentStream(ctx, channelID)
	if err != nil {
		t.Fatalf("GetStream after delete failed: %v", err)
	}
	if st != nil {
		t.Errorf("expected nil stream after deletion, got %v", st)
	}
}

func TestStore_StreamExists(t *testing.T) {
	s := newTestStore(t)

	ctx := context.Background()

	// 1. Check non-existent locally
	exists, err := s.StreamExists(ctx, "ch1", "s1")
	if err != nil {
		t.Fatalf("StreamExists failed: %v", err)
	}
	if exists {
		t.Error("Expected stream to not exist")
	}

	// 2. Insert stream
	s1 := &model.Stream{ChannelID: "ch1", StreamID: "s1", StreamTitle: "Title 1", StartTime: "1000", IsLive: true, MediaType: "audio"}
	if err := s.UpsertStream(ctx, s1); err != nil {
		t.Fatalf("UpsertStream failed: %v", err)
	}

	// 3. Check existence
	exists, err = s.StreamExists(ctx, "ch1", "s1")
	if err != nil {
		t.Fatalf("StreamExists failed: %v", err)
	}
	if !exists {
		t.Error("Expected stream to exist")
	}

	// 4. Check diff channel
	exists, err = s.StreamExists(ctx, "ch2", "s1")
	if err != nil {
		t.Fatalf("StreamExists failed: %v", err)
	}
	if exists {
		t.Error("Expected stream (ch2, s1) to not exist")
	}
}

func TestStore_GetStreamByID(t *testing.T) {
	s := newTestStore(t)

	ctx := context.Background()

	// 1. Check non-existent
	st, err := s.GetStreamByID(ctx, "ch1", "s1")
	if err != nil {
		t.Fatalf("GetStreamByID failed: %v", err)
	}
	if st != nil {
		t.Error("Expected nil stream")
	}

	// 2. Insert stream
	s1 := &model.Stream{ChannelID: "ch1", StreamID: "s1", StreamTitle: "Title 1", StartTime: "1000", IsLive: true, MediaType: "audio"}
	if err := s.UpsertStream(ctx, s1); err != nil {
		t.Fatalf("UpsertStream failed: %v", err)
	}

	// 3. Check existence
	st, err = s.GetStreamByID(ctx, "ch1", "s1")
	if err != nil {
		t.Fatalf("GetStreamByID failed: %v", err)
	}
	if st == nil {
		t.Fatal("Expected stream to exist")
	}
	if st.StreamTitle != "Title 1" {
		t.Errorf("Expected title 'Title 1', got %s", st.StreamTitle)
	}

	// 4. Check diff channel
	st, err = s.GetStreamByID(ctx, "ch2", "s1")
	if err != nil {
		t.Fatalf("GetStreamByID failed: %v", err)
	}
	if st != nil {
		t.Error("Expected stream to not exist for ch2")
	}
}

func TestStore_GetFileIDsInRange_Ordering(t *testing.T) {
	s := newTestStore(t)

	ctx := context.Background()
	channelID := "test-ordering"
	activeID := "s1"

	// 1. Insert lines in random order (ID 3, then 1, then 2)
	lines := []model.Line{
		{ID: 3, Timestamp: 300, FileID: "file1", MediaAvailable: true, Segments: json.RawMessage(`[{"text": "3"}]`)},
		{ID: 1, Timestamp: 100, FileID: "file2", MediaAvailable: true, Segments: json.RawMessage(`[{"text": "1"}]`)},
		{ID: 2, Timestamp: 200, FileID: "file3", MediaAvailable: true, Segments: json.RawMessage(`[{"text": "2"}]`)},
	}
	if err := s.ReplaceTranscript(ctx, channelID, activeID, lines); err != nil {
		t.Fatalf("ReplaceTranscript failed: %v", err)
	}

	// 2. Query range 1-3
	fileIDs, err := s.GetFileIDsInRange(ctx, channelID, activeID, 1, 3)
	if err != nil {
		t.Fatalf("GetFileIDsInRange failed: %v", err)
	}

	// 3. Verify Order
	if len(fileIDs) != 3 {
		t.Fatalf("Expected 3 file IDs, got %d", len(fileIDs))
	}
	if fileIDs[0] != "file2" {
		t.Errorf("Index 0: expected file2, got %s", fileIDs[0])
	}
	if fileIDs[1] != "file3" {
		t.Errorf("Index 1: expected file3, got %s", fileIDs[1])
	}
	if fileIDs[2] != "file1" {
		t.Errorf("Index 2: expected file1, got %s", fileIDs[2])
	}
}

func TestStore_CleanupOrphanedTranscripts(t *testing.T) {
	s := newTestStore(t)

	ctx := context.Background()
	channelID := "test-cleanup"

	// 1. Create a valid stream
	stream := &model.Stream{
		ChannelID:   channelID,
		StreamID:    "valid-stream",
		StreamTitle: "Valid Stream",
		IsLive:      true,
	}
	if err := s.UpsertStream(ctx, stream); err != nil {
		t.Fatalf("UpsertStream failed: %v", err)
	}

	// 2. Add transcript lines for valid stream
	validLines := []model.Line{
		{ID: 1, Timestamp: 100, Segments: json.RawMessage(`[]`)},
		{ID: 2, Timestamp: 200, Segments: json.RawMessage(`[]`)},
	}
	if err := s.ReplaceTranscript(ctx, channelID, "valid-stream", validLines); err != nil {
		t.Fatalf("ReplaceTranscript failed: %v", err)
	}

	// 3. Add transcript lines for an orphaned stream (no stream in DB)
	if err := s.ReplaceTranscript(ctx, channelID, "orphaned-stream", []model.Line{{ID: 1, Timestamp: 100, Segments: json.RawMessage(`[]`)}}); err != nil {
		t.Fatalf("ReplaceTranscript (orphan) failed: %v", err)
	}

	// Verify before cleanup
	lines, err := s.GetTranscript(ctx, channelID, "valid-stream")
	if err != nil || len(lines) != 2 {
		t.Fatalf("Expected 2 valid lines before cleanup, got %d", len(lines))
	}
	lines, err = s.GetTranscript(ctx, channelID, "orphaned-stream")
	if err != nil || len(lines) != 1 {
		t.Fatalf("Expected 1 orphaned line before cleanup, got %d", len(lines))
	}

	// 4. Run Cleanup
	if err := s.CleanupOrphanedTranscripts(ctx); err != nil {
		t.Fatalf("CleanupOrphanedTranscripts failed: %v", err)
	}

	// 5. Verify after cleanup
	lines, err = s.GetTranscript(ctx, channelID, "valid-stream")
	if err != nil || len(lines) != 2 {
		t.Fatalf("Expected 2 valid lines after cleanup, got %d", len(lines))
	}
	lines, err = s.GetTranscript(ctx, channelID, "orphaned-stream")
	if err != nil || len(lines) != 0 {
		t.Fatalf("Expected 0 orphaned lines after cleanup, got %d", len(lines))
	}
}
