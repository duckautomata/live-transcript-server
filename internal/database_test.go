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
	line, err := app.GetLastLine(ctx, channelID)
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
	if err := app.ReplaceTranscript(ctx, channelID, lines); err != nil {
		t.Fatalf("failed to replace transcript: %v", err)
	}

	// Test GetLastLine
	line, err = app.GetLastLine(ctx, channelID)
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
	if err := app.SetStreamLive(ctx, channelID, false); err != nil {
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
	lines, err := app.GetTranscript(ctx, channelID)
	if err != nil {
		t.Fatalf("GetTranscript (empty) failed: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("expected empty transcript, got %d lines", len(lines))
	}

	lastId, err := app.GetLastLineID(ctx, channelID)
	if err != nil {
		t.Fatalf("GetLastLineID (empty) failed: %v", err)
	}
	if lastId != -1 {
		t.Errorf("expected last ID to be -1, got %d", lastId)
	}

	lastLine, err := app.GetLastLine(ctx, channelID)
	if err != nil {
		t.Fatalf("GetLastLine (empty) failed: %v", err)
	}
	if lastLine != nil {
		t.Errorf("expected last line to be nil, got %v", lastLine)
	}

	// 1. InsertTranscriptLine
	line0 := Line{ID: 0, Timestamp: 100, Segments: []Segment{{Text: "Hello"}}}
	if err := app.InsertTranscriptLine(ctx, channelID, line0); err != nil {
		t.Fatalf("InsertTranscriptLine failed: %v", err)
	}

	// 2. GetTranscript
	lines, err = app.GetTranscript(ctx, channelID)
	if err != nil {
		t.Fatalf("GetTranscript failed: %v", err)
	}
	if len(lines) != 1 || lines[0].Segments[0].Text != "Hello" {
		t.Errorf("transcript content mismatch, expected %v, got %v", line0, lines)
	}

	// 3. GetLastLineID
	lastID, err := app.GetLastLineID(ctx, channelID)
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
	if err := app.ReplaceTranscript(ctx, channelID, newLines); err != nil {
		t.Fatalf("ReplaceTranscript failed: %v", err)
	}

	lines, err = app.GetTranscript(ctx, channelID)
	if err != nil {
		t.Fatalf("GetTranscript after replace failed: %v", err)
	}
	if len(lines) != 2 || lines[1].Segments[0].Text != "New World" {
		t.Error("replaced transcript content mismatch")
	}

	// 5. ClearTranscript
	if err := app.ClearTranscript(ctx, channelID); err != nil {
		t.Fatalf("ClearTranscript failed: %v", err)
	}

	lines, err = app.GetTranscript(ctx, channelID)
	if err != nil || len(lines) != 0 {
		t.Error("expected empty transcript after clear")
	}
}

func TestDB_GetLastAvailableMediaIDs(t *testing.T) {
	app := setupTestDB(t)
	defer app.DB.Close()

	ctx := context.Background()
	channelID := "test-media-ids"

	// Test Empty
	lastIDs, err := app.GetLastAvailableMediaIDs(ctx, channelID, 10)
	if err != nil {
		t.Fatalf("failed to get last media IDs (empty): %v", err)
	}
	if len(lastIDs) != 0 {
		t.Error("expected empty last media IDs for empty db")
	}

	// Test with data but no media available
	lines := []Line{
		{ID: 0, Timestamp: 100, Segments: []Segment{{Text: "First"}}},
		{ID: 2, Timestamp: 300, Segments: []Segment{{Text: "Third"}}},
		{ID: 1, Timestamp: 200, Segments: []Segment{{Text: "Second"}}},
	}
	if err := app.ReplaceTranscript(ctx, channelID, lines); err != nil {
		t.Fatalf("failed to replace transcript: %v", err)
	}

	lastIDs, err = app.GetLastAvailableMediaIDs(ctx, channelID, 10)
	if err != nil {
		t.Fatalf("failed to get last media IDs: %v", err)
	}
	if len(lastIDs) != 0 {
		t.Errorf("expected 0 last media IDs, got %d", len(lastIDs))
	}

	// Test with some media available
	lines = []Line{
		{ID: 0, Timestamp: 100, MediaAvailable: true, Segments: []Segment{{Text: "First"}}},
		{ID: 2, Timestamp: 300, MediaAvailable: false, Segments: []Segment{{Text: "Third"}}},
		{ID: 1, Timestamp: 200, MediaAvailable: true, Segments: []Segment{{Text: "Second"}}},
	}
	if err := app.ReplaceTranscript(ctx, channelID, lines); err != nil {
		t.Fatalf("failed to replace transcript: %v", err)
	}

	lastIDs, err = app.GetLastAvailableMediaIDs(ctx, channelID, 10)
	if err != nil {
		t.Fatalf("failed to get last media IDs: %v", err)
	}
	if len(lastIDs) != 2 {
		t.Errorf("expected 2 last media IDs, got %d", len(lastIDs))
	}
	if lastIDs[0] != 0 || lastIDs[1] != 1 {
		t.Errorf("expected last media IDs [0, 1], got %v", lastIDs)
	}

	// Test with more media available than limit
	lines = []Line{
		{ID: 0, Timestamp: 100, MediaAvailable: true, Segments: []Segment{{Text: "First"}}},
		{ID: 2, Timestamp: 300, MediaAvailable: false, Segments: []Segment{{Text: "Third"}}},
		{ID: 1, Timestamp: 200, MediaAvailable: true, Segments: []Segment{{Text: "Second"}}},
	}
	if err := app.ReplaceTranscript(ctx, channelID, lines); err != nil {
		t.Fatalf("failed to replace transcript: %v", err)
	}

	lastIDs, err = app.GetLastAvailableMediaIDs(ctx, channelID, 1)
	if err != nil {
		t.Fatalf("failed to get last media IDs: %v", err)
	}
	if len(lastIDs) != 1 {
		t.Errorf("expected 1 last media ID, got %d", len(lastIDs))
	}
	if lastIDs[0] != 1 {
		t.Errorf("expected last media ID 1, got %d", lastIDs[0])
	}

	// Test with no limit
	lines = []Line{
		{ID: 0, Timestamp: 100, MediaAvailable: true, Segments: []Segment{{Text: "First"}}},
		{ID: 2, Timestamp: 300, MediaAvailable: false, Segments: []Segment{{Text: "Third"}}},
		{ID: 1, Timestamp: 200, MediaAvailable: true, Segments: []Segment{{Text: "Second"}}},
	}
	if err := app.ReplaceTranscript(ctx, channelID, lines); err != nil {
		t.Fatalf("failed to replace transcript: %v", err)
	}

	lastIDs, err = app.GetLastAvailableMediaIDs(ctx, channelID, 0)
	if err != nil {
		t.Fatalf("failed to get last media IDs: %v", err)
	}
	if len(lastIDs) != 0 {
		t.Errorf("expected 0 last media IDs, got %d", len(lastIDs))
	}
}
