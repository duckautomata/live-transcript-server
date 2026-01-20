package internal

import (
	"context"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestDB_GetLastLine(t *testing.T) {
	app := setupTestDB(t)
	defer app.DB.Close()

	ctx := context.Background()
	channelID := "test-last-line"

	// Test Empty
	line, err := app.GetLastLine(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("failed to get last line (empty): %v", err)
	}
	if line != nil {
		t.Error("expected nil line for empty db")
	}

	// Insert Lines
	lines := []Line{
		{ID: 0, Timestamp: 100, Segments: []Segment{{Text: "First"}}},
		{ID: 2, Timestamp: 300, Segments: []Segment{{Text: "Third"}}},
		{ID: 1, Timestamp: 200, Segments: []Segment{{Text: "Second"}}},
	}
	if err := app.ReplaceTranscript(ctx, channelID, "test-stream", lines); err != nil {
		t.Fatalf("failed to replace transcript: %v", err)
	}

	// Test GetLastLine
	line, err = app.GetLastLine(ctx, channelID, "test-stream")
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
	if line.Segments[0].Text != "Third" {
		t.Errorf("expected segment 'Third', got %s", line.Segments[0].Text)
	}
}

func TestDB_StreamOperations(t *testing.T) {
	app := setupTestDB(t)
	defer app.DB.Close()

	ctx := context.Background()
	channelID := "test-stream-ops"

	// 1. Test GetStream (Empty)
	s, err := app.GetStream(ctx, channelID)
	if err != nil {
		t.Fatalf("GetStream failed: %v", err)
	}
	if s != nil {
		t.Fatal("expected nil stream initially")
	}

	// 2. Test UpsertStream
	newStream := &Stream{
		ChannelID:   channelID,
		ActiveID:    "vid1",
		ActiveTitle: "Video 1",
		StartTime:   "1000",
		IsLive:      true,
		MediaType:   "audio",
	}
	if err := app.UpsertStream(ctx, newStream); err != nil {
		t.Fatalf("UpsertStream failed: %v", err)
	}
	newStream2 := &Stream{
		ChannelID:   "different-id",
		ActiveID:    "vid2",
		ActiveTitle: "Video 2",
		StartTime:   "2000",
		IsLive:      true,
		MediaType:   "video",
	}
	if err := app.UpsertStream(ctx, newStream2); err != nil {
		t.Fatalf("UpsertStream (different ID) failed: %v", err)
	}

	s, err = app.GetStream(ctx, channelID)
	if err != nil {
		t.Fatalf("GetStream failed: %v", err)
	}
	if s == nil {
		t.Fatal("expected stream to be found")
	}
	if s.ActiveTitle != "Video 1" {
		t.Errorf("expected title 'Video 1', got '%s'", s.ActiveTitle)
	}

	// 3. Test UpsertStream Update
	newStream.ActiveTitle = "Video 1 Updated"
	newStream.StartTime = "1001"
	if err := app.UpsertStream(ctx, newStream); err != nil {
		t.Fatalf("UpsertStream (update) failed: %v", err)
	}

	s, err = app.GetStream(ctx, channelID)
	if err != nil {
		t.Fatalf("GetStream (update) failed: %v", err)
	}
	if s.ActiveTitle != "Video 1 Updated" {
		t.Errorf("expected updated title, got '%s'", s.ActiveTitle)
	}
	if s.StartTime != "1001" {
		t.Errorf("expected updated start time, got '%s'", s.StartTime)
	}

	// 4. Test SetStreamLive
	// Set specific stream explicitly active (although logic usually sets it false)
	// But SetStreamLive takes (ctx, channel, activeID, bool)
	if err := app.SetStreamLive(ctx, channelID, "vid1", false); err != nil {
		t.Fatalf("SetStreamLive failed: %v", err)
	}

	s, err = app.GetStream(ctx, channelID)
	if err != nil {
		t.Fatalf("GetStream (live update) failed: %v", err)
	}
	if s.IsLive {
		t.Error("expected stream to be not live")
	}
}

func TestDB_TranscriptOperations(t *testing.T) {
	app := setupTestDB(t)
	defer app.DB.Close()

	ctx := context.Background()
	channelID := "test-transcript-ops"

	// 0. Get empty transcript
	lines, err := app.GetTranscript(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("GetTranscript (empty) failed: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("expected empty transcript, got %d lines", len(lines))
	}

	lastId, err := app.GetLastLineID(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("GetLastLineID (empty) failed: %v", err)
	}
	if lastId != -1 {
		t.Errorf("expected last ID to be -1, got %d", lastId)
	}

	lastLine, err := app.GetLastLine(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("GetLastLine (empty) failed: %v", err)
	}
	if lastLine != nil {
		t.Errorf("expected last line to be nil, got %v", lastLine)
	}

	// 1. InsertTranscriptLine
	line0 := Line{ID: 0, Timestamp: 100, Segments: []Segment{{Text: "Hello"}}}
	if err := app.InsertTranscriptLine(ctx, channelID, "test-stream", line0); err != nil {
		t.Fatalf("InsertTranscriptLine failed: %v", err)
	}

	// 2. GetTranscript
	lines, err = app.GetTranscript(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("GetTranscript failed: %v", err)
	}
	if len(lines) != 1 || lines[0].Segments[0].Text != "Hello" {
		t.Errorf("transcript content mismatch, expected %v, got %v", line0, lines)
	}

	// 3. GetLastLineID
	lastID, err := app.GetLastLineID(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("GetLastLineID failed: %v", err)
	}
	if lastID != 0 {
		t.Errorf("expected last ID 0, got %d", lastID)
	}

	// 4. ReplaceTranscript
	newLines := []Line{
		{ID: 0, Timestamp: 100, Segments: []Segment{{Text: "New Hello"}}},
		{ID: 1, Timestamp: 200, Segments: []Segment{{Text: "New World"}}},
	}
	if err := app.ReplaceTranscript(ctx, channelID, "test-stream", newLines); err != nil {
		t.Fatalf("ReplaceTranscript failed: %v", err)
	}

	lines, err = app.GetTranscript(ctx, channelID, "test-stream")
	if err != nil {
		t.Fatalf("GetTranscript after replace failed: %v", err)
	}
	if len(lines) != 2 || lines[1].Segments[0].Text != "New World" {
		t.Error("replaced transcript content mismatch")
	}

	// 5. ClearTranscript
	if err := app.DeleteTranscript(ctx, channelID, "test-stream"); err != nil {
		t.Fatalf("DeleteTranscript failed: %v", err)
	}

	lines, err = app.GetTranscript(ctx, channelID, "test-stream")
	if err != nil || len(lines) != 0 {
		t.Error("expected empty transcript after clear")
	}
}

func TestDB_GetLastAvailableMediaFiles(t *testing.T) {
	app := setupTestDB(t)
	defer app.DB.Close()

	ctx := context.Background()
	channelID := "test-media-ids"

	// Test Empty
	files, err := app.GetLastAvailableMediaFiles(ctx, channelID, "test-stream", 10)
	if err != nil {
		t.Fatalf("failed to get last media files (empty): %v", err)
	}
	if len(files) != 0 {
		t.Error("expected empty last media files for empty db")
	}

	// Test with data but no media available
	lines := []Line{
		{ID: 0, Timestamp: 100, Segments: []Segment{{Text: "First"}}},
		{ID: 2, Timestamp: 300, Segments: []Segment{{Text: "Third"}}},
		{ID: 1, Timestamp: 200, Segments: []Segment{{Text: "Second"}}},
	}
	if err := app.ReplaceTranscript(ctx, channelID, "test-stream", lines); err != nil {
		t.Fatalf("failed to replace transcript: %v", err)
	}

	files, err = app.GetLastAvailableMediaFiles(ctx, channelID, "test-stream", 10)
	if err != nil {
		t.Fatalf("failed to get last media files: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 last media files, got %d", len(files))
	}

	// Test with some media available
	lines = []Line{
		{ID: 0, Timestamp: 100, MediaAvailable: true, FileID: "f0", Segments: []Segment{{Text: "First"}}},
		{ID: 2, Timestamp: 300, MediaAvailable: false, Segments: []Segment{{Text: "Third"}}},
		{ID: 1, Timestamp: 200, MediaAvailable: true, FileID: "f1", Segments: []Segment{{Text: "Second"}}},
	}
	if err := app.ReplaceTranscript(ctx, channelID, "test-stream", lines); err != nil {
		t.Fatalf("failed to replace transcript: %v", err)
	}

	files, err = app.GetLastAvailableMediaFiles(ctx, channelID, "test-stream", 10)
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
	files, err = app.GetLastAvailableMediaFiles(ctx, channelID, "test-stream", 1)
	if err != nil {
		t.Fatalf("failed to get last media files: %v", err)
	}
	if len(files) != 1 {
		t.Errorf("expected 1 last media file, got %d", len(files))
	}
	// It should return the latest one (ID 1, FileID f1) or ID 0?
	// GetLastAvailableMediaFiles query: "... ORDER BY line_id DESC LIMIT ?"
	// So it should return ID 1.
	if val, ok := files[1]; !ok || val != "f1" {
		t.Errorf("expected last media file ID 1:f1, got %v", files)
	}
}

func TestDB_GetPastStreams(t *testing.T) {
	app := setupTestDB(t)
	defer app.DB.Close()

	ctx := context.Background()
	channelID := "test-past-streams"

	// 1. No streams
	streams, err := app.GetPastStreams(ctx, channelID, "any")
	if err != nil {
		t.Fatalf("GetPastStreams failed: %v", err)
	}
	if len(streams) != 0 {
		t.Errorf("expected 0 streams, got %d", len(streams))
	}

	// 2. Insert Active Stream (Live)
	activeStream := &Stream{
		ChannelID:   channelID,
		ActiveID:    "active1",
		ActiveTitle: "Active 1",
		StartTime:   "1000",
		IsLive:      true,
		MediaType:   "audio",
	}
	if err := app.UpsertStream(ctx, activeStream); err != nil {
		t.Fatalf("UpsertStream active failed: %v", err)
	}

	streams, err = app.GetPastStreams(ctx, channelID, "active1")
	if err != nil {
		t.Fatalf("GetPastStreams failed: %v", err)
	}
	if len(streams) != 0 {
		t.Errorf("expected 0 past streams (active IS live), got %d", len(streams))
	}

	// 3. Insert Past Stream (Not Live)
	pastStream := &Stream{
		ChannelID:   channelID,
		ActiveID:    "past1",
		ActiveTitle: "Past 1",
		StartTime:   "900",
		IsLive:      false,
		MediaType:   "audio",
	}
	if err := app.UpsertStream(ctx, pastStream); err != nil {
		t.Fatalf("UpsertStream past failed: %v", err)
	}
	// Also insert another past stream
	pastStream2 := &Stream{
		ChannelID:   channelID,
		ActiveID:    "past2",
		ActiveTitle: "Past 2",
		StartTime:   "800", // Older
		IsLive:      false,
		MediaType:   "audio",
	}
	if err := app.UpsertStream(ctx, pastStream2); err != nil {
		t.Fatalf("UpsertStream past2 failed: %v", err)
	}

	// 4. GetPastStreams
	streams, err = app.GetPastStreams(ctx, channelID, "active1")
	if err != nil {
		t.Fatalf("GetPastStreams failed: %v", err)
	}
	if len(streams) != 2 {
		t.Errorf("expected 2 past streams, got %d", len(streams))
	} else {
		// Expect order: newest first (past1, then past2)
		if streams[0].ActiveID != "past1" {
			t.Errorf("expected first stream to be past1, got %s", streams[0].ActiveID)
		}
		if streams[1].ActiveID != "past2" {
			t.Errorf("expected second stream to be past2, got %s", streams[1].ActiveID)
		}
	}

	// 5. Test Exclude ID (simulate "past1" is arguably the current stream but marked not live?
	// Or simply verify the exclude logic works even if stream is found)
	// The SQL is: active_id != ? AND is_live = 0.
	// So if we pass "past1" as activeID, it should be excluded.
	streams, err = app.GetPastStreams(ctx, channelID, "past1")
	if err != nil {
		t.Fatalf("GetPastStreams exclude failed: %v", err)
	}
	if len(streams) != 1 {
		t.Errorf("expected 1 past stream (excluding past1), got %d", len(streams))
	}
	if streams[0].ActiveID != "past2" {
		t.Errorf("expected stream to be past2, got %s", streams[0].ActiveID)
	}
}

func TestDB_DeleteStream(t *testing.T) {
	app := setupTestDB(t)
	defer app.DB.Close()

	ctx := context.Background()
	channelID := "test-delete-stream"
	activeID := "stream-to-delete"

	// 1. Insert Stream
	stream := &Stream{
		ChannelID:   channelID,
		ActiveID:    activeID,
		ActiveTitle: "To Be Deleted",
		StartTime:   "1000",
		IsLive:      true,
		MediaType:   "audio",
	}
	if err := app.UpsertStream(ctx, stream); err != nil {
		t.Fatalf("UpsertStream failed: %v", err)
	}

	stream2 := &Stream{
		ChannelID:   channelID,
		ActiveID:    "another-stream",
		ActiveTitle: "Should Not Be Deleted",
		StartTime:   "1000",
		IsLive:      false,
		MediaType:   "audio",
	}
	if err := app.UpsertStream(ctx, stream2); err != nil {
		t.Fatalf("UpsertStream failed: %v", err)
	}

	// 2. Verify existence
	// Note: GetStream retrieves the latest stream.
	s, err := app.GetStream(ctx, channelID)
	if err != nil {
		t.Fatalf("GetStream failed: %v", err)
	}
	if s == nil || s.ActiveID != activeID {
		t.Fatal("expected stream to be active")
	}

	// 3. Delete Stream
	if err := app.DeleteStream(ctx, channelID, activeID); err != nil {
		t.Fatalf("DeleteStream failed: %v", err)
	}

	// 4. Verify deletion
	s, err = app.GetStream(ctx, channelID)
	if err != nil {
		t.Fatalf("GetStream after delete failed: %v", err)
	}
	if s == nil {
		t.Errorf("expected to retrieve another stream, got nil")
	}
	if s.ActiveID != "another-stream" {
		t.Errorf("expected another stream to be active, got %s", s.ActiveID)
	}

	// 5. Delete another stream
	if err := app.DeleteStream(ctx, channelID, "another-stream"); err != nil {
		t.Fatalf("DeleteStream failed: %v", err)
	}

	// 6. Verify deletion
	s, err = app.GetStream(ctx, channelID)
	if err != nil {
		t.Fatalf("GetStream after delete failed: %v", err)
	}
	if s != nil {
		t.Errorf("expected nil stream after deletion, got %v", s)
	}
}

func TestDB_StreamExists(t *testing.T) {
	app := setupTestDB(t)
	defer app.DB.Close()

	ctx := context.Background()

	// 1. Check non-existent locally
	exists, err := app.StreamExists(ctx, "ch1", "s1")
	if err != nil {
		t.Fatalf("StreamExists failed: %v", err)
	}
	if exists {
		t.Error("Expected stream to not exist")
	}

	// 2. Insert stream
	s1 := &Stream{ChannelID: "ch1", ActiveID: "s1", ActiveTitle: "Title 1", StartTime: "1000", IsLive: true, MediaType: "audio"}
	if err := app.UpsertStream(ctx, s1); err != nil {
		t.Fatalf("UpsertStream failed: %v", err)
	}

	// 3. Check existence
	exists, err = app.StreamExists(ctx, "ch1", "s1")
	if err != nil {
		t.Fatalf("StreamExists failed: %v", err)
	}
	if !exists {
		t.Error("Expected stream to exist")
	}

	// 4. Check diff channel
	exists, err = app.StreamExists(ctx, "ch2", "s1")
	if err != nil {
		t.Fatalf("StreamExists failed: %v", err)
	}
	if exists {
		t.Error("Expected stream (ch2, s1) to not exist")
	}
}

func TestDB_GetStreamByID(t *testing.T) {
	app := setupTestDB(t)
	defer app.DB.Close()

	ctx := context.Background()

	// 1. Check non-existent
	s, err := app.GetStreamByID(ctx, "ch1", "s1")
	if err != nil {
		t.Fatalf("GetStreamByID failed: %v", err)
	}
	if s != nil {
		t.Error("Expected nil stream")
	}

	// 2. Insert stream
	s1 := &Stream{ChannelID: "ch1", ActiveID: "s1", ActiveTitle: "Title 1", StartTime: "1000", IsLive: true, MediaType: "audio"}
	if err := app.UpsertStream(ctx, s1); err != nil {
		t.Fatalf("UpsertStream failed: %v", err)
	}

	// 3. Check existence
	s, err = app.GetStreamByID(ctx, "ch1", "s1")
	if err != nil {
		t.Fatalf("GetStreamByID failed: %v", err)
	}
	if s == nil {
		t.Fatal("Expected stream to exist")
	}
	if s.ActiveTitle != "Title 1" {
		t.Errorf("Expected title 'Title 1', got %s", s.ActiveTitle)
	}

	// 4. Check diff channel
	s, err = app.GetStreamByID(ctx, "ch2", "s1")
	if err != nil {
		t.Fatalf("GetStreamByID failed: %v", err)
	}
	if s != nil {
		t.Error("Expected stream to not exist for ch2")
	}
}

func TestDB_GetFileIDsInRange_Ordering(t *testing.T) {
	app := setupTestDB(t)
	defer app.DB.Close()

	ctx := context.Background()
	channelID := "test-ordering"
	activeID := "s1"

	// 1. Insert lines in random order
	// Insert ID 3, then 1, then 2
	lines := []Line{
		{ID: 3, Timestamp: 300, FileID: "file1", MediaAvailable: true, Segments: []Segment{{Text: "3"}}},
		{ID: 1, Timestamp: 100, FileID: "file2", MediaAvailable: true, Segments: []Segment{{Text: "1"}}},
		{ID: 2, Timestamp: 200, FileID: "file3", MediaAvailable: true, Segments: []Segment{{Text: "2"}}},
	}

	for _, l := range lines {
		if err := app.InsertTranscriptLine(ctx, channelID, activeID, l); err != nil {
			t.Fatalf("InsertTranscriptLine failed: %v", err)
		}
	}

	// 2. Query range 1-3
	fileIDs, err := app.GetFileIDsInRange(ctx, channelID, activeID, 1, 3)
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
