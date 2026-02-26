package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"live-transcript-server/internal/storage"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func TestServer_ActivateDeactivate(t *testing.T) {
	key := "test-channel"
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()
	apiKey := app.ApiKey
	ctx := context.Background()

	// Test Activate
	// URL must match the registered pattern /{channel}/activate
	req, _ := http.NewRequest("POST", fmt.Sprintf("/%s/activate?id=stream1&title=TestStream&startTime=%d&mediaType=video", key, time.Now().Unix()), nil)
	req.Header.Set("X-API-Key", apiKey)
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK, got %v body: %s", rr.Code, rr.Body.String())
	}

	// Verify DB state
	stream, err := app.GetRecentStream(ctx, key)
	if err != nil {
		t.Fatalf("failed to get stream: %v", err)
	}
	if stream == nil {
		t.Fatal("stream not found in db")
	}
	if stream.StreamID != "stream1" {
		t.Errorf("expected streamID stream1, got %s", stream.StreamID)
	}
	if stream.StreamTitle != "TestStream" {
		t.Errorf("expected streamTitle TestStream, got %s", stream.StreamTitle)
	}
	if !stream.IsLive {
		t.Error("expected stream to be live")
	}
	if stream.MediaType != "video" {
		t.Errorf("expected mediaType video, got %s", stream.MediaType)
	}

	// Insert transcripts for tests
	lines := []Line{
		{ID: 0, Timestamp: 100, Segments: json.RawMessage(`[{"text": "First"}]`)},
		{ID: 2, Timestamp: 300, Segments: json.RawMessage(`[{"text": "Third"}]`)},
		{ID: 1, Timestamp: 200, Segments: json.RawMessage(`[{"text": "Second"}]`)},
	}
	if err := app.ReplaceTranscript(ctx, key, "stream1", lines); err != nil {
		t.Fatalf("failed to replace transcript: %v", err)
	}
	// Verify transcripts are there
	transcripts, err := app.GetTranscript(ctx, key, "stream1")
	if err != nil {
		t.Fatalf("failed to get transcripts: %v", err)
	}
	if len(transcripts) != 3 {
		t.Errorf("expected 3 transcripts, got %d", len(transcripts))
	}

	// Test activate again with same ID but different title
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/activate?id=stream1&title=TestStream2&startTime=%d&mediaType=none", key, time.Now().Unix()), nil)
	req.Header.Set("X-API-Key", apiKey)
	rr = httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusAlreadyReported {
		t.Errorf("expected status 208, got %v body: %s", rr.Code, rr.Body.String())
	}
	// Verify DB state
	stream, err = app.GetRecentStream(ctx, key)
	if err != nil {
		t.Fatalf("failed to get stream: %v", err)
	}
	if stream == nil {
		t.Fatal("stream not found in db")
	}
	if stream.StreamID != "stream1" {
		t.Errorf("expected streamID stream1, got %s", stream.StreamID)
	}
	if stream.StreamTitle != "TestStream" {
		t.Errorf("expected streamTitle TestStream, got %s", stream.StreamTitle)
	}
	if !stream.IsLive {
		t.Error("expected stream to be live")
	}
	if stream.MediaType != "video" {
		t.Errorf("expected mediaType video, got %s", stream.MediaType)
	}
	// Verify transcripts are still there
	transcripts, err = app.GetTranscript(ctx, key, "stream1")
	if err != nil {
		t.Fatalf("failed to get transcripts: %v", err)
	}
	if len(transcripts) != 3 {
		t.Errorf("expected 3 transcripts, got %d", len(transcripts))
	}

	// Test Deactivate
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/deactivate?id=stream1", key), nil)
	req.Header.Set("X-API-Key", apiKey)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK, got %v", rr.Code)
	}
	stream, err = app.GetRecentStream(ctx, key)
	if err != nil {
		t.Fatalf("failed to get stream: %v", err)
	}
	if stream.IsLive {
		t.Error("expected stream to not be live")
	}
	// Verify transcripts are still there
	transcripts, err = app.GetTranscript(ctx, key, "stream1")
	if err != nil {
		t.Fatalf("failed to get transcripts: %v", err)
	}
	if len(transcripts) != 3 {
		t.Errorf("expected 3 transcripts, got %d", len(transcripts))
	}

	// Test Deactivate again
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/deactivate?id=stream1", key), nil)
	req.Header.Set("X-API-Key", apiKey)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusAlreadyReported {
		t.Errorf("expected status 208, got %v", rr.Code)
	}
	// Verify transcripts are still there
	transcripts, err = app.GetTranscript(ctx, key, "stream1")
	if err != nil {
		t.Fatalf("failed to get transcripts: %v", err)
	}
	if len(transcripts) != 3 {
		t.Errorf("expected 3 transcripts, got %d", len(transcripts))
	}

	// Test Reactivate only isLive should change
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/activate?id=stream1&title=TestStream2&startTime=%d&mediaType=none", key, time.Now().Unix()), nil)
	req.Header.Set("X-API-Key", apiKey)
	rr = httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK, got %v", rr.Code)
	}
	// Verify DB state
	stream, err = app.GetRecentStream(ctx, key)
	if err != nil {
		t.Fatalf("failed to get stream: %v", err)
	}
	if stream == nil {
		t.Fatal("stream not found in db")
	}
	if stream.StreamID != "stream1" {
		t.Errorf("expected streamID stream1, got %s", stream.StreamID)
	}
	if stream.StreamTitle != "TestStream" {
		t.Errorf("expected streamTitle TestStream, got %s", stream.StreamTitle)
	}
	if !stream.IsLive {
		t.Error("expected stream to be live")
	}
	if stream.MediaType != "video" {
		t.Errorf("expected mediaType video, got %s", stream.MediaType)
	}
	// Verify transcripts are still there
	transcripts, err = app.GetTranscript(ctx, key, "stream1")
	if err != nil {
		t.Fatalf("failed to get transcripts: %v", err)
	}
	if len(transcripts) != 3 {
		t.Errorf("expected 3 transcripts, got %d", len(transcripts))
	}

	// Test deactivate with wrong ID
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/deactivate?id=stream2", key), nil)
	req.Header.Set("X-API-Key", apiKey)
	rr = httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusAlreadyReported {
		t.Errorf("expected status 208, got %v", rr.Code)
	}
	stream, err = app.GetRecentStream(ctx, key)
	if err != nil {
		t.Fatalf("failed to get stream: %v", err)
	}
	if !stream.IsLive {
		t.Error("expected stream to be live")
	}
	// Verify transcripts are still there
	transcripts, err = app.GetTranscript(ctx, key, "stream1")
	if err != nil {
		t.Fatalf("failed to get transcripts: %v", err)
	}
	if len(transcripts) != 3 {
		t.Errorf("expected 3 transcripts, got %d", len(transcripts))
	}

	// Test activate with different ID should update all values in db and transcripts should be reset
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/activate?id=stream2&title=TestStream2&startTime=%d&mediaType=none", key, time.Now().Unix()), nil)
	req.Header.Set("X-API-Key", apiKey)
	rr = httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK, got %v", rr.Code)
	}
	// Verify DB state
	stream, err = app.GetRecentStream(ctx, key)
	if err != nil {
		t.Fatalf("failed to get stream: %v", err)
	}
	if stream == nil {
		t.Fatal("stream not found in db")
	}
	if stream.StreamID != "stream2" {
		t.Errorf("expected streamID stream2, got %s", stream.StreamID)
	}
	if stream.StreamTitle != "TestStream2" {
		t.Errorf("expected streamTitle TestStream2, got %s", stream.StreamTitle)
	}
	if !stream.IsLive {
		t.Error("expected stream to be live")
	}
	if stream.MediaType != "none" {
		t.Errorf("expected mediaType none, got %s", stream.MediaType)
	}
	// Verify transcripts are reset (because looking for stream2)
	transcripts, err = app.GetTranscript(ctx, key, "stream2")
	if err != nil {
		t.Fatalf("failed to get transcripts: %v", err)
	}
	if len(transcripts) != 0 {
		t.Errorf("expected 0 transcripts, got %d", len(transcripts))
	}
}

func TestServer_LineUpdate(t *testing.T) {
	key := "test-channel-line"
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()
	apiKey := app.ApiKey

	// Activate first (helper to avoid boilerplate)
	// We need to access the channel state to call activateStream directly OR just hit the API.
	// Hitting API is safer for integration test style.
	reqActivate, _ := http.NewRequest("POST", fmt.Sprintf("/%s/activate?id=stream2&title=TestLine&startTime=%d", key, time.Now().Unix()), nil)
	reqActivate.Header.Set("X-API-Key", apiKey)
	mux.ServeHTTP(httptest.NewRecorder(), reqActivate)

	// Test Line 0
	line0 := Line{
		ID:        0,
		Timestamp: 100,
		Segments:  json.RawMessage(`[{"timestamp": 100, "text": "Hello"}]`),
	}
	body, _ := json.Marshal(line0)
	req, _ := http.NewRequest("POST", fmt.Sprintf("/%s/line/stream2", key), bytes.NewBuffer(body))
	req.Header.Set("X-API-Key", apiKey)
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK, got %v body: %s", rr.Code, rr.Body.String())
	}

	// Verify DB
	lines, err := app.GetTranscript(context.TODO(), key, "stream2")
	if err != nil {
		t.Fatalf("failed to get transcript: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if lines[0].ID != 0 {
		t.Errorf("expected line ID 0, got %d", lines[0].ID)
	}

	// Test Invalid Line ID (Skip to 2)
	line2 := Line{
		ID:        2, // Should be 1
		Timestamp: 200,
		Segments:  json.RawMessage(`[{"timestamp": 200, "text": "World"}]`),
	}
	body, _ = json.Marshal(line2)
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/line/stream2", key), bytes.NewBuffer(body))
	req.Header.Set("X-API-Key", apiKey)
	rr = httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Errorf("expected status Conflict (409), got %v", rr.Code)
	}
}

func TestServer_MediaUpload(t *testing.T) {
	key := "test-channel-media"
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()
	apiKey := app.ApiKey
	ctx := context.Background()

	// Insert Stream to satisfy mediaHandler check
	stream := &Stream{ChannelID: key, StreamID: "stream_media", StreamTitle: "Test Stream", StartTime: "12345", IsLive: true, MediaType: "audio"}
	if err := app.UpsertStream(ctx, stream); err != nil {
		t.Fatalf("failed to insert stream: %v", err)
	}

	// Create a dummy multipart body
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "test.raw")
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte("dummy audio data"))
	writer.Close()

	// Mock FfmpegConvert
	originalFfmpegConvert := FfmpegConvert
	FfmpegConvert = func(inputFilePath, outputFilePath string) error {
		// Just copy input to output or create invalid file to simulate success
		return os.WriteFile(outputFilePath, []byte("converted"), 0644)
	}
	defer func() { FfmpegConvert = originalFfmpegConvert }()

	// Check if file exists using FileID from DB
	transcript, err := app.GetTranscript(ctx, key, "stream_media")
	if err != nil {
		t.Fatalf("failed to get transcript: %v", err)
	}
	if len(transcript) != 0 {
		t.Fatal("transcript is not empty")
	}

	// Let's modify test flow: Insert Line -> Upload Media -> Check.
	app.InsertTranscriptLine(ctx, key, "stream_media", Line{ID: 0, Timestamp: 100})

	req, _ := http.NewRequest("POST", fmt.Sprintf("/%s/media/stream_media/0", key), body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-API-Key", apiKey)
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK, got %v body: %s", rr.Code, rr.Body.String())
	}

	// Verify MediaAvailable
	lines, err := app.GetTranscript(ctx, key, "stream_media")
	if err != nil {
		t.Fatalf("failed to get transcript: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !lines[0].MediaAvailable {
		t.Error("expected media available")
	}
	if lines[0].FileID == "" {
		t.Error("expected fileID to be set")
	}

	// Check file existence
	fileID := lines[0].FileID
	rawPath := filepath.Join(app.Channels[key].BaseMediaFolder, "stream_media", "raw", fileID+".raw")
	if _, err := os.Stat(rawPath); os.IsNotExist(err) {
		t.Errorf("expected raw file to exist at %s", rawPath)
	}
}

func TestServer_Sync(t *testing.T) {
	key := "test-channel-sync"
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()
	apiKey := app.ApiKey

	// Simulate existing data via DB direct insert to simulate state
	app.UpsertStream(context.TODO(), &Stream{ChannelID: key, StreamID: "stream3", StreamTitle: "Test Sync", StartTime: fmt.Sprintf("%d", time.Now().Unix()), IsLive: true, MediaType: "none"})
	app.InsertTranscriptLine(context.TODO(), key, "stream3", Line{ID: 0, Timestamp: 100})

	// Sync Data
	syncData := EventSyncData{
		StreamID:    "stream3",
		StreamTitle: "Test Sync Updated",
		StartTime:   fmt.Sprintf("%d", time.Now().Unix()),
		IsLive:      true,
		MediaType:   "none",
		Transcript: []Line{
			{ID: 0, Timestamp: 100, Segments: json.RawMessage(`[{"timestamp": 100, "text": "Resynced 1"}]`)},
			{ID: 1, Timestamp: 200, Segments: json.RawMessage(`[{"timestamp": 200, "text": "Resynced 2"}]`)},
		},
	}
	body, _ := json.Marshal(syncData)
	req, _ := http.NewRequest("POST", fmt.Sprintf("/%s/sync", key), bytes.NewBuffer(body))
	req.Header.Set("X-API-Key", apiKey)
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK, got %v", rr.Code)
	}

	// Verify DB state
	stream, err := app.GetRecentStream(context.TODO(), key)
	if err != nil {
		t.Fatalf("failed to get stream: %v", err)
	}
	if stream.StreamTitle != "Test Sync Updated" {
		t.Errorf("expected title updated, got %s", stream.StreamTitle)
	}

	lines, err := app.GetTranscript(context.TODO(), key, "stream3")
	if err != nil {
		t.Fatalf("failed to get transcript: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	var segments []Segment
	json.Unmarshal(lines[1].Segments, &segments)
	if segments[0].Text != "Resynced 2" {
		t.Errorf("expected segment text 'Resynced 2', got %s", segments[0].Text)
	}

	// Verify Media Resync
	// 1. Create a dummy media file for line 1 (ID 1)
	// We are in "stream3" (activeID from UpsertStream above)
	// So path should be Base/stream3/1.m4a
	mediaPath := filepath.Join(app.Channels[key].BaseMediaFolder, "stream3", "1.m4a")
	// Ensure directory exists
	os.MkdirAll(filepath.Dir(mediaPath), 0755)
	if err := os.WriteFile(mediaPath, []byte("dummy"), 0644); err != nil {
		t.Fatalf("failed to create dummy media file: %v", err)
	}
	// Simulate DB state for media (server now checks DB, not disk)
	if err := app.SetMediaAvailable(context.TODO(), key, "stream3", 1, "1", true); err != nil {
		t.Fatalf("failed to set media available: %v", err)
	}

	// 2. Sync again with MediaAvailable set to false for line 1
	syncData2 := EventSyncData{
		StreamID:    "stream3",
		StreamTitle: "Test Sync Media",
		StartTime:   fmt.Sprintf("%d", time.Now().Unix()),
		IsLive:      true,
		MediaType:   "none",
		Transcript: []Line{
			{ID: 1, Timestamp: 200, Segments: json.RawMessage(`[{"timestamp": 200, "text": "Resynced 2 Media Check"}]`), MediaAvailable: false},
		},
	}
	body, _ = json.Marshal(syncData2)
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/sync", key), bytes.NewBuffer(body))
	req.Header.Set("X-API-Key", apiKey)
	rr = httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK, got %v", rr.Code)
	}

	// 3. Verify that MediaAvailable is true in DB
	lines, err = app.GetTranscript(context.TODO(), key, "stream3")
	if err != nil {
		t.Fatalf("failed to get transcript: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !lines[0].MediaAvailable {
		t.Errorf("expected MediaAvailable to be true for line 1 (m4a), got false")
	}

	// 4. Verify Raw File Resync
	// Create a dummy raw file for line 2 (ID 2)
	rawPath := filepath.Join(app.Channels[key].BaseMediaFolder, "stream3", "2.raw")
	if err := os.WriteFile(rawPath, []byte("dummy raw"), 0644); err != nil {
		t.Fatalf("failed to create dummy raw file: %v", err)
	}
	// Simulate DB state for media (server now checks DB, not disk)
	// Must insert line first so we can update it
	if err := app.InsertTranscriptLine(context.TODO(), key, "stream3", Line{ID: 2, Timestamp: 300}); err != nil {
		t.Fatalf("failed to insert line 2: %v", err)
	}
	if err := app.SetMediaAvailable(context.TODO(), key, "stream3", 2, "2", true); err != nil {
		t.Fatalf("failed to set media available: %v", err)
	}

	// Sync with line 2 having MediaAvailable false
	syncData3 := EventSyncData{
		StreamID:    "stream3",
		StreamTitle: "Test Sync Media Raw",
		StartTime:   fmt.Sprintf("%d", time.Now().Unix()),
		IsLive:      true,
		MediaType:   "none",
		Transcript: []Line{
			{ID: 2, Timestamp: 300, Segments: json.RawMessage(`[{"timestamp": 300, "text": "Resynced 2 Raw Check"}]`), MediaAvailable: false},
		},
	}
	body, _ = json.Marshal(syncData3)
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/sync", key), bytes.NewBuffer(body))
	req.Header.Set("X-API-Key", apiKey)
	rr = httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK, got %v", rr.Code)
	}

	// Verify that MediaAvailable is true for line 2
	lines, err = app.GetTranscript(context.TODO(), key, "stream3")
	if err != nil {
		t.Fatalf("failed to get transcript: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if lines[0].ID != 2 {
		t.Errorf("expected line ID 2, got %d", lines[0].ID)
	}
	if !lines[0].MediaAvailable {
		t.Errorf("expected MediaAvailable to be true for line 2 (raw), got false")
	}
}

func TestServer_Persistence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")
	db, _ := InitDB(dbPath, DatabaseConfig{})

	key := "persist-channel"
	apiKey := "key"
	// Use manual setup to mimic restart behavior easily (just accessing DB)

	app := NewApp(apiKey, db, []ChannelConfig{{Name: key, NumPastStreams: 1}}, StorageConfig{Type: "local"}, dir, "test-version", "test-build-time")

	mux := http.NewServeMux()
	app.RegisterRoutes(mux)

	req, _ := http.NewRequest("POST", fmt.Sprintf("/%s/activate?id=persist&title=Persist&startTime=12345", key), nil)
	req.Header.Set("X-API-Key", apiKey)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	db.Close()

	// Re-open DB
	db2, _ := InitDB(dbPath, DatabaseConfig{})
	defer db2.Close()
	app2 := &App{DB: db2}

	stream, err := app2.GetRecentStream(context.TODO(), key)
	if err != nil {
		t.Fatalf("failed to get stream: %v", err)
	}
	if stream == nil {
		t.Fatal("stream not found after restart")
	}
	if stream.StreamID != "persist" {
		t.Errorf("expected persist, got %s", stream.StreamID)
	}
}

func TestServer_GetTranscriptEndpoint(t *testing.T) {
	key := "test-transcript-endpoint"
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()
	ctx := context.Background()

	// Seed data
	stream1ID := "stream1"
	app.UpsertStream(ctx, &Stream{ChannelID: key, StreamID: stream1ID, IsLive: false})
	app.ReplaceTranscript(ctx, key, stream1ID, []Line{
		{ID: 0, Segments: json.RawMessage(`[{"text": "Stream 1 Line 0"}]`)},
	})

	stream2ID := "stream2"
	app.UpsertStream(ctx, &Stream{ChannelID: key, StreamID: stream2ID, IsLive: true})
	app.ReplaceTranscript(ctx, key, stream2ID, []Line{
		{ID: 0, Segments: json.RawMessage(`[{"text": "Stream 2 Line 0"}]`)},
	})

	// Test Get Stream 1
	req, _ := http.NewRequest("GET", fmt.Sprintf("/%s/transcript/%s", key, stream1ID), nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK for stream1, got %v", rr.Code)
	}
	var lines []Line
	json.Unmarshal(rr.Body.Bytes(), &lines)
	if len(lines) != 1 {
		t.Fatalf("unexpected content for stream1: %v", lines)
	}
	var segments []Segment
	json.Unmarshal(lines[0].Segments, &segments)
	if segments[0].Text != "Stream 1 Line 0" {
		t.Errorf("unexpected content for stream1: %v", lines)
	}

	// Test Get Stream 2
	req, _ = http.NewRequest("GET", fmt.Sprintf("/%s/transcript/%s", key, stream2ID), nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK for stream2, got %v", rr.Code)
	}
	json.Unmarshal(rr.Body.Bytes(), &lines)
	if len(lines) != 1 {
		t.Fatalf("unexpected content for stream2: %v", lines)
	}
	json.Unmarshal(lines[0].Segments, &segments)
	if segments[0].Text != "Stream 2 Line 0" {
		t.Errorf("unexpected content for stream2: %v", lines)
	}

	// Test Get Non-existent Stream
	req, _ = http.NewRequest("GET", fmt.Sprintf("/%s/transcript/invalid", key), nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	// Should return empty list (or 200 with empty list), logic returns []Line{} which is valid JSON "[]"
	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK for invalid stream, got %v", rr.Code)
	}
	json.Unmarshal(rr.Body.Bytes(), &lines)
	if len(lines) != 0 {
		t.Errorf("expected empty list for invalid stream, got %v", lines)
	}
}

func TestServer_MediaEndpoints(t *testing.T) {
	key := "test-media-endpoints"
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()
	apiKey := app.ApiKey
	ctx := context.Background()

	// 1. Setup Data
	// Activate stream
	app.UpsertStream(ctx, &Stream{ChannelID: key, StreamID: "s1", StreamTitle: "Stream 1", StartTime: "12345", IsLive: true, MediaType: "video"})

	// Prepare pointers
	// app.Storage is LocalStorage rooted at app.TempDir.
	// Keys are relative to app.TempDir.
	// Structure: channel/s1/raw/fileID.raw
	// Ensure folders exist via storage save or manual mkdir?
	// Manual mkdir is easier for test.
	rawFolder := filepath.Join(app.Channels[key].BaseMediaFolder, "s1", "raw")
	os.MkdirAll(rawFolder, 0755)
	audioFolder := filepath.Join(app.Channels[key].BaseMediaFolder, "s1", "audio")
	os.MkdirAll(audioFolder, 0755)

	// Create dummy media files
	// s1 folder logic expects subfolders now (audio, frame)
	// Write dummy audio file
	os.WriteFile(filepath.Join(audioFolder, "1.m4a"), []byte("audio"), 0644)

	// Create frame folder and write dummy frame
	frameFolder := filepath.Join(app.Channels[key].BaseMediaFolder, "s1", "frame")
	os.MkdirAll(frameFolder, 0755)
	os.WriteFile(filepath.Join(frameFolder, "1.jpg"), []byte("image"), 0644)

	// Create dummy raw files for clip testing (ids 0-10)
	// And insert Lines into DB with FileID
	for i := 0; i <= 10; i++ {
		fileID := fmt.Sprintf("file_%d", i)
		line := Line{
			ID:             i,
			Timestamp:      i * 1000,
			MediaAvailable: true,
			FileID:         fileID,
			Segments:       json.RawMessage(`[{"text": "test"}]`),
		}
		app.InsertTranscriptLine(ctx, key, "s1", line)

		// Write raw file to storage (raw folder)
		os.WriteFile(filepath.Join(rawFolder, fileID+".raw"), []byte("raw_audio"), 0644)

		// Also ensure file for Trim test (Line 1) exists in clips
		if i == 1 {
			// Trim source for file ID 1 (must be in clips folder)
			clipsFolder := filepath.Join(app.Channels[key].BaseMediaFolder, "s1", "clips")
			os.MkdirAll(clipsFolder, 0755)
			os.WriteFile(filepath.Join(clipsFolder, fileID+".m4a"), []byte("audio_source"), 0644)
		}
	}

	// Mock FFmpeg for clip/trim
	originalFfmpegConvert := FfmpegConvert
	originalFfmpegTrim := FfmpegTrim
	originalFfmpegRemux := FfmpegRemux
	FfmpegConvert = func(inputFilePath, outputFilePath string) error {
		return os.WriteFile(outputFilePath, []byte("converted"), 0644)
	}
	FfmpegTrim = func(inputPath, outputPath string, start, end float64) error {
		return os.WriteFile(outputPath, []byte("trimmed"), 0644)
	}
	FfmpegRemux = func(inputPath, outputPath string) error {
		return os.WriteFile(outputPath, []byte("remuxed"), 0644)
	}
	defer func() {
		FfmpegConvert = originalFfmpegConvert
		FfmpegTrim = originalFfmpegTrim
		FfmpegRemux = originalFfmpegRemux
	}()

	// 2. Test streamHandler
	// Valid request
	req, _ := http.NewRequest("GET", fmt.Sprintf("/%s/stream/s1/audio/1.m4a", key), nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("streamHandler: expected OK, got %v", rr.Code)
	}
	if rr.Header().Get("Content-Type") != "audio/mp4" {
		t.Errorf("streamHandler: expected audio/mp4, got %s", rr.Header().Get("Content-Type"))
	}
	if rr.Header().Get("Content-Disposition") != "" {
		t.Errorf("streamHandler: expected no content-disposition, got %s", rr.Header().Get("Content-Disposition"))
	}

	// Invalid Stream ID
	req, _ = http.NewRequest("GET", fmt.Sprintf("/%s/stream/s2/audio/1.m4a", key), nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("streamHandler: expected NotFound for wrong streamID, got %v", rr.Code)
	}

	// 3. Test downloadHandler
	req, _ = http.NewRequest("GET", fmt.Sprintf("/%s/download/s1/audio/1.m4a", key), nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("downloadHandler: expected OK, got %v", rr.Code)
	}
	if rr.Header().Get("Content-Disposition") != "attachment; filename=\"s1_1.m4a\"" {
		t.Errorf("downloadHandler: expected attachment; filename=\"s1_1.m4a\", got %s", rr.Header().Get("Content-Disposition"))
	}

	// 3b. Test downloadHandler with custom name
	req, _ = http.NewRequest("GET", fmt.Sprintf("/%s/download/s1/audio/1.m4a?name=MyCustomFile", key), nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("downloadHandler with name: expected OK, got %v", rr.Code)
	}
	if rr.Header().Get("Content-Disposition") != "attachment; filename=\"MyCustomFile.m4a\"" {
		t.Errorf("downloadHandler with name: expected attachment; filename=\"MyCustomFile.m4a\", got %s", rr.Header().Get("Content-Disposition"))
	}

	// 4. Test getFrameHandler
	req, _ = http.NewRequest("GET", fmt.Sprintf("/%s/frame/s1/1.jpg", key), nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("getFrameHandler: expected OK, got %v", rr.Code)
	}
	if rr.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("getFrameHandler: expected image/jpeg, got %s", rr.Header().Get("Content-Type"))
	}

	// 5. Test clipHandler (POST)
	clipReq := map[string]any{
		"stream_id": "s1",
		"start":     0,
		"end":       10,
		"type":      "m4a",
	}
	body, _ := json.Marshal(clipReq)
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/clip", key), bytes.NewBuffer(body))
	req.Header.Set("X-API-Key", apiKey)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("clipHandler: expected OK, got %v body: %s", rr.Code, rr.Body.String())
	}
	var clipResp map[string]string
	json.NewDecoder(rr.Body).Decode(&clipResp)
	if clipResp["clip_id"] == "" {
		t.Error("clipHandler: expected clip_id in response")
	}

	// 6. Test trimHandler (POST)
	trimReq := map[string]any{
		"stream_id":   "s1",
		"clip_id":     "file_1", // Trimming file ID file_1
		"file_format": "m4a",
		"start":       0.0,
		"end":         5.0,
		"filename":    "trimmed_output",
	}
	body, _ = json.Marshal(trimReq)
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/trim", key), bytes.NewBuffer(body))
	req.Header.Set("X-API-Key", apiKey)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("trimHandler: expected OK, got %v body: %s", rr.Code, rr.Body.String())
	}
	var trimResp map[string]string
	json.NewDecoder(rr.Body).Decode(&trimResp)
	if trimResp["status"] != "success" {
		t.Errorf("trimHandler: expected success, got %s", trimResp["status"])
	}
	if trimResp["clip_id"] == "" {
		t.Errorf("trimHandler: expected valid clip_id, got empty")
	}
	if trimResp["clip_id"] == "1" {
		t.Errorf("trimHandler: expected new clip_id, got same as input '1'")
	}

	// 7. Test clipHandler Missing Media
	clipReqMissing := map[string]any{
		"stream_id": "s1",
		"start":     0,
		"end":       20, // Range 0-20, but we only have 0-10
		"type":      "m4a",
	}
	body, _ = json.Marshal(clipReqMissing)
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/clip", key), bytes.NewBuffer(body))
	req.Header.Set("X-API-Key", apiKey)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("clipHandler missing media: expected 500, got %v body: %s", rr.Code, rr.Body.String())
	}

	// 8. Test clipHandler MP4 Sidecar
	// Ensure we have media for range 0-10
	clipReqMp4 := map[string]any{
		"stream_id": "s1",
		"start":     0,
		"end":       10,
		"type":      "mp4",
	}
	body, _ = json.Marshal(clipReqMp4)
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/clip", key), bytes.NewBuffer(body))
	req.Header.Set("X-API-Key", apiKey)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("clipHandler mp4: expected OK, got %v body: %s", rr.Code, rr.Body.String())
	}
	var mp4Resp map[string]string
	json.NewDecoder(rr.Body).Decode(&mp4Resp)
	clipID := mp4Resp["clip_id"]
	if clipID == "" {
		t.Fatal("clipHandler mp4: expected clip_id")
	}

	// Check Sidecar existence
	// Storage is local, check standard path clips/id.m4a
	clipsFolder := filepath.Join(app.Channels[key].BaseMediaFolder, "s1", "clips")
	sidecarPath := filepath.Join(clipsFolder, clipID+".m4a")
	if _, err := os.Stat(sidecarPath); os.IsNotExist(err) {
		t.Errorf("expected sidecar m4a to exist at %s", sidecarPath)
	}
	// Check Main MP4
	mainPath := filepath.Join(clipsFolder, clipID+".mp4")
	if _, err := os.Stat(mainPath); os.IsNotExist(err) {
		t.Errorf("expected main mp4 to exist at %s", mainPath)
	}
}

func TestServer_ActivateStream_Retention_UnderThreshold(t *testing.T) {
	key := "test-retention-under"
	app, _, db := setupTestApp(t, []string{key})
	defer db.Close()
	ctx := context.Background()

	// Update NumPastStreams to 2
	cs := app.Channels[key]
	cs.NumPastStreams = 2

	// Initialize with 1 Past Stream (p1)
	p1 := &Stream{ChannelID: key, StreamID: "p1", StartTime: "2000", IsLive: false}
	app.UpsertStream(ctx, p1)
	app.InsertTranscriptLine(ctx, key, "p1", Line{ID: 0, Segments: json.RawMessage(`[{"text": "P1 Content"}]`)})
	folder := filepath.Join(cs.BaseMediaFolder, "p1")
	os.MkdirAll(folder, 0755)
	app.Channels[key].ActiveMediaFolder = folder

	// Activate S1 (New Active)
	app.activateStream(ctx, cs, "s1", "Stream 1", "3000", "audio")

	// Verify S1 is active
	s, err := app.GetRecentStream(ctx, key)
	if err != nil {
		t.Fatalf("GetStream failed: %v", err)
	}
	if s.StreamID != "s1" {
		t.Errorf("expected active stream s1, got %s", s.StreamID)
	}

	// Verify P1 still exists (1 past stream <= 2)
	past, err := app.GetPastStreams(ctx, key, "s1")
	if err != nil {
		t.Fatalf("GetPastStreams failed: %v", err)
	}
	if len(past) != 1 || past[0].StreamID != "p1" {
		t.Errorf("expected 1 past stream (p1), got %v", past)
	}
	// Verify transcript for P1 exists
	lines, _ := app.GetTranscript(ctx, key, "p1")
	if len(lines) == 0 {
		t.Error("expected p1 transcript to exist")
	}
}

func TestServer_ActivateStream_Retention_EqualThreshold(t *testing.T) {
	key := "test-retention-equal"
	app, _, db := setupTestApp(t, []string{key})
	defer db.Close()
	ctx := context.Background()

	// Update NumPastStreams to 2
	cs := app.Channels[key]
	cs.NumPastStreams = 2

	// Initialize S1 (Live) and P1 (Past)
	// Initialize S1 (Live) and P1 (Past)
	p1 := &Stream{ChannelID: key, StreamID: "p1", StartTime: "2000", IsLive: false, ActivatedTime: 2000}
	s1 := &Stream{ChannelID: key, StreamID: "s1", StartTime: "3000", IsLive: true, ActivatedTime: 3000}
	app.UpsertStream(ctx, p1)
	app.UpsertStream(ctx, s1)
	app.InsertTranscriptLine(ctx, key, "p1", Line{ID: 0, Segments: json.RawMessage(`[{"text": "P1 Content"}]`)})

	// Create folders
	os.MkdirAll(filepath.Join(cs.BaseMediaFolder, "p1"), 0755)
	os.MkdirAll(filepath.Join(cs.BaseMediaFolder, "s1"), 0755)
	cs.ActiveMediaFolder = filepath.Join(cs.BaseMediaFolder, "s1")

	// We have S1 (Active), P1 (Past). Total 2.
	// Activate S2. S1 becomes Past. P1 remains.
	// New State: S2 (Active), S1 (Past), P1 (Past).
	// Total Past = 2. Equal to NumPastStreams (2).
	// Expect all to be kept.
	app.activateStream(ctx, cs, "s2", "Stream 2", "4000", "audio")

	past, err := app.GetPastStreams(ctx, key, "s2")
	if err != nil {
		t.Fatalf("GetPastStreams failed: %v", err)
	}
	if len(past) != 2 {
		t.Errorf("expected 2 past streams, got %d", len(past))
	}
	// Verify order (newest first): S1, P1
	if len(past) == 2 {
		if past[0].StreamID != "s1" {
			t.Errorf("expected past[0] to be s1, got %s", past[0].StreamID)
		}
		if past[1].StreamID != "p1" {
			t.Errorf("expected past[1] to be p1, got %s", past[1].StreamID)
		}
	}
	// Verify transcripts
	lines, _ := app.GetTranscript(ctx, key, "p1")
	if len(lines) == 0 {
		t.Error("expected p1 transcript to exist")
	}
}

func TestServer_ActivateStream_Retention_OverThreshold(t *testing.T) {
	key := "test-retention-over"
	app, _, db := setupTestApp(t, []string{key})
	defer db.Close()
	ctx := context.Background()

	// Update NumPastStreams to 2
	cs := app.Channels[key]
	cs.NumPastStreams = 2

	// Setup: S2 (Active), S1, P1
	// Setup: S2 (Active), S1, P1
	p1 := &Stream{ChannelID: key, StreamID: "p1", StartTime: "2000", IsLive: false, ActivatedTime: 2000}
	s1 := &Stream{ChannelID: key, StreamID: "s1", StartTime: "3000", IsLive: false, ActivatedTime: 3000}
	s2 := &Stream{ChannelID: key, StreamID: "s2", StartTime: "4000", IsLive: true, ActivatedTime: 4000}
	app.UpsertStream(ctx, p1)
	app.UpsertStream(ctx, s1)
	app.UpsertStream(ctx, s2)

	app.InsertTranscriptLine(ctx, key, "p1", Line{ID: 0, Segments: json.RawMessage(`[{"text": "P1 Content"}]`)})

	// Create folders
	os.MkdirAll(filepath.Join(cs.BaseMediaFolder, "p1"), 0755)
	os.MkdirAll(filepath.Join(cs.BaseMediaFolder, "s1"), 0755)
	os.MkdirAll(filepath.Join(cs.BaseMediaFolder, "s2"), 0755)
	cs.ActiveMediaFolder = filepath.Join(cs.BaseMediaFolder, "s2")

	// Activate S3. State becomes: S3 (Active). Past: S2, S1, P1.
	// 3 Past streams. Limit is 2.
	// Should delete P1. Keep S2, S1.
	app.activateStream(ctx, cs, "s3", "Stream 3", "5000", "audio")

	past, err := app.GetPastStreams(ctx, key, "s3")
	if err != nil {
		t.Fatalf("GetPastStreams failed: %v", err)
	}
	if len(past) != 2 {
		t.Errorf("expected 2 past streams after pruning, got %d", len(past))
	}
	if len(past) == 2 {
		if past[0].StreamID != "s2" {
			t.Errorf("expected past[0] to be s2, got %s", past[0].StreamID)
		}
		if past[1].StreamID != "s1" {
			t.Errorf("expected past[1] to be s1, got %s", past[1].StreamID)
		}
	}

	// Verify P1 transcript deleted
	lines, _ := app.GetTranscript(ctx, key, "p1")
	if len(lines) != 0 {
		t.Error("expected p1 transcript to be deleted")
	}

	// Verify P1 folder deleted from filesystem (async wait)
	p1Path := filepath.Join(cs.BaseMediaFolder, "p1")
	deleted := false
	for range 20 { // Wait up to 2 seconds (20 * 100ms)
		if _, err := os.Stat(p1Path); os.IsNotExist(err) {
			deleted = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !deleted {
		t.Error("expected p1 folder to be deleted (timed out waiting)")
	}
}

func TestServer_ActivateStream_Retention_MassiveOverflow(t *testing.T) {
	key := "test-retention-overflow"
	app, _, db := setupTestApp(t, []string{key})
	defer db.Close()
	ctx := context.Background()

	// Update NumPastStreams to 2
	cs := app.Channels[key]
	cs.NumPastStreams = 2

	// Setup: S3 (Active), S2, S1, P4, P5
	// Setup: S3 (Active), S2, S1, P4, P5
	p5 := &Stream{ChannelID: key, StreamID: "p5", StartTime: "0100", IsLive: false, ActivatedTime: 100} // Very old
	p4 := &Stream{ChannelID: key, StreamID: "p4", StartTime: "0500", IsLive: false, ActivatedTime: 500} // Old
	s1 := &Stream{ChannelID: key, StreamID: "s1", StartTime: "3000", IsLive: false, ActivatedTime: 3000}
	s2 := &Stream{ChannelID: key, StreamID: "s2", StartTime: "4000", IsLive: false, ActivatedTime: 4000}
	s3 := &Stream{ChannelID: key, StreamID: "s3", StartTime: "5000", IsLive: true, ActivatedTime: 5000}

	app.UpsertStream(ctx, p5)
	app.UpsertStream(ctx, p4)
	app.UpsertStream(ctx, s1)
	app.UpsertStream(ctx, s2)
	app.UpsertStream(ctx, s3)

	app.InsertTranscriptLine(ctx, key, "s1", Line{ID: 0, Segments: json.RawMessage(`[{"text": "S1 Content"}]`)})

	// Create folders
	os.MkdirAll(filepath.Join(cs.BaseMediaFolder, "p5"), 0755)
	os.MkdirAll(filepath.Join(cs.BaseMediaFolder, "p4"), 0755)
	os.MkdirAll(filepath.Join(cs.BaseMediaFolder, "s1"), 0755)
	os.MkdirAll(filepath.Join(cs.BaseMediaFolder, "s2"), 0755)
	os.MkdirAll(filepath.Join(cs.BaseMediaFolder, "s3"), 0755)
	cs.ActiveMediaFolder = filepath.Join(cs.BaseMediaFolder, "s3")

	// Calling activateStream for S4.
	// New State: S4 (Active). Past candidates: S3, S2, S1, P4, P5.
	// Ordered by time DESC: S3, S2, S1, P4, P5.
	// Keep 2 past: S3, S2.
	// Delete S1, P4, P5.
	app.activateStream(ctx, cs, "s4", "Stream 4", "6000", "audio")

	past, err := app.GetPastStreams(ctx, key, "s4")
	if err != nil {
		t.Fatalf("GetPastStreams failed: %v", err)
	}
	if len(past) != 2 {
		t.Errorf("expected 2 past streams, got %d", len(past))
	}
	if len(past) == 2 {
		if past[0].StreamID != "s3" {
			t.Errorf("expected past[0] to be s3, got %s", past[0].StreamID)
		}
		if past[1].StreamID != "s2" {
			t.Errorf("expected past[1] to be s2, got %s", past[1].StreamID)
		}
	}
	// Verify S1 deleted
	lines, _ := app.GetTranscript(ctx, key, "s1")
	if len(lines) != 0 {
		t.Error("expected s1 transcript to be deleted")
	}
}

func TestServer_MediaEndpoints_RemoteDisabled(t *testing.T) {
	key := "test-remote-disabled"
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()

	// Replace storage with a mock remote storage
	mockStore := &MockRemoteStorage{
		LocalStorage: app.Storage.(*storage.LocalStorage),
	}
	app.Storage = mockStore

	// 1. Test streamHandler Disabled
	req, _ := http.NewRequest("GET", fmt.Sprintf("/%s/stream/s1/audio/1.m4a", key), nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("streamHandler: expected BadRequest (400), got %v", rr.Code)
	}
	if rr.Body.String() != "Endpoint disabled for remote storage\n" {
		t.Errorf("unexpected body: %s", rr.Body.String())
	}

	// 2. Test downloadHandler Disabled
	req, _ = http.NewRequest("GET", fmt.Sprintf("/%s/download/s1/audio/1.m4a", key), nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("downloadHandler: expected BadRequest (400), got %v", rr.Code)
	}

	// 3. Test getFrameHandler Disabled
	req, _ = http.NewRequest("GET", fmt.Sprintf("/%s/frame/s1/1.jpg", key), nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("getFrameHandler: expected BadRequest (400), got %v", rr.Code)
	}
}

type MockRemoteStorage struct {
	*storage.LocalStorage
}

func (m *MockRemoteStorage) IsLocal() bool {
	return false
}

func (m *MockRemoteStorage) GetURL(key string) string {
	return "https://r2.example.com/" + key
}

func TestNewApp_WorkerStatusInitialization(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "startup.db")
	db, err := InitDB(dbPath, DatabaseConfig{})
	if err != nil {
		t.Fatalf("Failed to init db: %v", err)
	}
	defer db.Close()

	key := "test-startup-channel"
	buildTime := "2024-01-01T12:00:00Z"
	version := "1.0.0"

	// Insert a stale record to verify reset
	staleKey := "stale-key"
	_, err = db.Exec("INSERT INTO worker_status (channel_key, worker_version, worker_build_time, last_seen) VALUES (?, ?, ?, ?)", staleKey, "old", "old", 0)
	if err != nil {
		t.Fatalf("Failed to insert stale record: %v", err)
	}

	// Initialize App
	app := NewApp(
		"test-api-key",
		db,
		[]ChannelConfig{{Name: key, NumPastStreams: 1}},
		StorageConfig{Type: "local"},
		dir,
		version,
		buildTime,
	)

	// Check worker_status
	ctx := context.Background()
	statuses, err := app.GetAllWorkerStatus(ctx)
	if err != nil {
		t.Fatalf("Failed to get worker statuses: %v", err)
	}

	if len(statuses) != 1 {
		t.Errorf("Expected 1 worker status (stale removed), got %d", len(statuses))
		return
	}

	status := statuses[0]
	if status.ChannelKey != key {
		t.Errorf("Expected channel key %s, got %s", key, status.ChannelKey)
	}
	if status.ChannelKey == staleKey {
		t.Error("Stale key should have been removed")
	}

	if status.WorkerVersion != "N/A" {
		t.Errorf("Expected worker version 'N/A', got %s", status.WorkerVersion)
	}
	if status.WorkerBuildTime != buildTime {
		t.Errorf("Expected worker build time %s, got %s", buildTime, status.WorkerBuildTime)
	}

	// Check last seen is recent (within 5 seconds)
	now := time.Now().Unix()
	if now-status.LastSeen > 5 {
		t.Errorf("Expected last seen to be recent, got %d (now=%d)", status.LastSeen, now)
	}
}

func TestPostClipHandler_StreamID(t *testing.T) {
	key := "test-clip-handler"
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()
	ctx := context.Background()

	// 1. Insert Stream 1 (Video)
	stream1 := &Stream{
		ChannelID: key,
		StreamID:  "s1",
		IsLive:    false,
		MediaType: "video",
	}
	app.UpsertStream(ctx, stream1)

	// 2. Insert Stream 2 (Audio) - make it recent
	stream2 := &Stream{
		ChannelID: key,
		StreamID:  "s2",
		IsLive:    true,
		MediaType: "audio",
	}
	app.UpsertStream(ctx, stream2)

	// 3. Request for Clip on Stream 1 (Video)
	body := map[string]any{
		"stream_id": "s1",
		"start":     10,
		"end":       20,
		"type":      "mp4",
	}
	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "/"+key+"/clip", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code == http.StatusMethodNotAllowed || strings.Contains(rr.Body.String(), "Video clipping is disabled") {
		t.Errorf("Handler used wrong stream media type. Expected video (s1), got audio (s2 behavior).")
	}
}
