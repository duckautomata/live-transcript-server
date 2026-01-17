package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	stream, err := app.GetStream(ctx, key)
	if err != nil {
		t.Fatalf("failed to get stream: %v", err)
	}
	if stream == nil {
		t.Fatal("stream not found in db")
	}
	if stream.ActiveID != "stream1" {
		t.Errorf("expected activeId stream1, got %s", stream.ActiveID)
	}
	if stream.ActiveTitle != "TestStream" {
		t.Errorf("expected activeTitle TestStream, got %s", stream.ActiveTitle)
	}
	if !stream.IsLive {
		t.Error("expected stream to be live")
	}
	if stream.MediaType != "video" {
		t.Errorf("expected mediaType video, got %s", stream.MediaType)
	}

	// Insert transcripts for tests
	lines := []Line{
		{ID: 0, Timestamp: 100, Segments: []Segment{{Text: "First"}}},
		{ID: 2, Timestamp: 300, Segments: []Segment{{Text: "Third"}}},
		{ID: 1, Timestamp: 200, Segments: []Segment{{Text: "Second"}}},
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
	stream, err = app.GetStream(ctx, key)
	if err != nil {
		t.Fatalf("failed to get stream: %v", err)
	}
	if stream == nil {
		t.Fatal("stream not found in db")
	}
	if stream.ActiveID != "stream1" {
		t.Errorf("expected activeId stream1, got %s", stream.ActiveID)
	}
	if stream.ActiveTitle != "TestStream" {
		t.Errorf("expected activeTitle TestStream, got %s", stream.ActiveTitle)
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
	stream, err = app.GetStream(ctx, key)
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
	stream, err = app.GetStream(ctx, key)
	if err != nil {
		t.Fatalf("failed to get stream: %v", err)
	}
	if stream == nil {
		t.Fatal("stream not found in db")
	}
	if stream.ActiveID != "stream1" {
		t.Errorf("expected activeId stream1, got %s", stream.ActiveID)
	}
	if stream.ActiveTitle != "TestStream" {
		t.Errorf("expected activeTitle TestStream, got %s", stream.ActiveTitle)
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
	stream, err = app.GetStream(ctx, key)
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
	stream, err = app.GetStream(ctx, key)
	if err != nil {
		t.Fatalf("failed to get stream: %v", err)
	}
	if stream == nil {
		t.Fatal("stream not found in db")
	}
	if stream.ActiveID != "stream2" {
		t.Errorf("expected activeId stream2, got %s", stream.ActiveID)
	}
	if stream.ActiveTitle != "TestStream2" {
		t.Errorf("expected activeTitle TestStream2, got %s", stream.ActiveTitle)
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
		Segments: []Segment{
			{Timestamp: 100, Text: "Hello"},
		},
	}
	body, _ := json.Marshal(line0)
	req, _ := http.NewRequest("POST", fmt.Sprintf("/%s/line", key), bytes.NewBuffer(body))
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
		Segments: []Segment{
			{Timestamp: 200, Text: "World"},
		},
	}
	body, _ = json.Marshal(line2)
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/line", key), bytes.NewBuffer(body))
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

	// We MUST insert a stream first for this test to pass with new logic.
	app.UpsertStream(ctx, &Stream{ChannelID: key, ActiveID: "stream_media", IsLive: true, MediaType: "none"})
	// Now the handler will find this stream and set ActiveMediaFolder to BaseMediaFolder/stream_media

	req, _ := http.NewRequest("POST", fmt.Sprintf("/%s/media/0", key), body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-API-Key", apiKey)
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK, got %v body: %s", rr.Code, rr.Body.String())
	}

	// Check if file exists
	rawPath := filepath.Join(app.Channels[key].BaseMediaFolder, "stream_media", "0.raw")
	if _, err := os.Stat(rawPath); os.IsNotExist(err) {
		t.Errorf("expected raw file to exist at %s", rawPath)
	}

	// Check if media available flag is not set
	lines, err := app.GetTranscript(ctx, key, "stream_media")
	if err != nil {
		t.Fatalf("failed to get transcript: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("expected 0 lines, got %d", len(lines))
	}

	// Insert line with media available flag set to false
	app.InsertTranscriptLine(ctx, key, "stream_media", Line{ID: 0, Timestamp: 200, MediaAvailable: false, Segments: []Segment{{Timestamp: 200, Text: "World"}}})

	// Upload media again
	body = new(bytes.Buffer)
	writer = multipart.NewWriter(body)
	part, err = writer.CreateFormFile("file", "test.raw")
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte("dummy audio data"))
	writer.Close()
	req, _ = http.NewRequest("POST", fmt.Sprintf("/%s/media/0", key), body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-API-Key", apiKey)
	rr = httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK, got %v body: %s", rr.Code, rr.Body.String())
	}

	// Check if media available flag is set
	lines, err = app.GetTranscript(ctx, key, "stream_media")
	if err != nil {
		t.Fatalf("failed to get transcript: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !lines[0].MediaAvailable {
		t.Errorf("expected media available flag to be true, got %v", lines[0].MediaAvailable)
	}
}

func TestServer_Sync(t *testing.T) {
	key := "test-channel-sync"
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()
	apiKey := app.ApiKey

	// Simulate existing data via DB direct insert to simulate state
	app.UpsertStream(context.TODO(), &Stream{ChannelID: key, ActiveID: "stream3", ActiveTitle: "Test Sync", StartTime: fmt.Sprintf("%d", time.Now().Unix()), IsLive: true, MediaType: "none"})
	app.InsertTranscriptLine(context.TODO(), key, "stream3", Line{ID: 0, Timestamp: 100})

	// Sync Data
	syncData := EventSyncData{
		ActiveID:    "stream3",
		ActiveTitle: "Test Sync Updated",
		StartTime:   fmt.Sprintf("%d", time.Now().Unix()),
		IsLive:      true,
		MediaType:   "none",
		Transcript: []Line{
			{ID: 0, Timestamp: 100, Segments: []Segment{{Timestamp: 100, Text: "Resynced 1"}}},
			{ID: 1, Timestamp: 200, Segments: []Segment{{Timestamp: 200, Text: "Resynced 2"}}},
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
	stream, err := app.GetStream(context.TODO(), key)
	if err != nil {
		t.Fatalf("failed to get stream: %v", err)
	}
	if stream.ActiveTitle != "Test Sync Updated" {
		t.Errorf("expected title updated, got %s", stream.ActiveTitle)
	}

	lines, err := app.GetTranscript(context.TODO(), key, "stream3")
	if err != nil {
		t.Fatalf("failed to get transcript: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if lines[1].Segments[0].Text != "Resynced 2" {
		t.Errorf("expected segment text 'Resynced 2', got %s", lines[1].Segments[0].Text)
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

	// 2. Sync again with MediaAvailable set to false for line 1
	syncData2 := EventSyncData{
		ActiveID:    "stream3",
		ActiveTitle: "Test Sync Media",
		StartTime:   fmt.Sprintf("%d", time.Now().Unix()),
		IsLive:      true,
		MediaType:   "none",
		Transcript: []Line{
			{ID: 1, Timestamp: 200, Segments: []Segment{{Timestamp: 200, Text: "Resynced 2 Media Check"}}, MediaAvailable: false},
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

	// Sync with line 2 having MediaAvailable false
	syncData3 := EventSyncData{
		ActiveID:    "stream3",
		ActiveTitle: "Test Sync Media Raw",
		StartTime:   fmt.Sprintf("%d", time.Now().Unix()),
		IsLive:      true,
		MediaType:   "none",
		Transcript: []Line{
			{ID: 2, Timestamp: 300, Segments: []Segment{{Timestamp: 300, Text: "Resynced 2 Raw Check"}}, MediaAvailable: false},
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
	db, _ := InitDB(dbPath)

	key := "persist-channel"
	apiKey := "key"
	// Use manual setup to mimic restart behavior easily (just accessing DB)

	app := NewApp(apiKey, db, []ChannelConfig{{Name: key, NumPastStreams: 1}}, dir)
	// Directly call activate via App method if we export it or via channel state lookup
	// For now, let's use the DB operations directly to verify persistence of the DB logic itself,
	// but the test is "Server_Persistence", suggesting valid server flow.
	// Let's use Request.

	mux := http.NewServeMux()
	app.RegisterRoutes(mux)

	req, _ := http.NewRequest("POST", fmt.Sprintf("/%s/activate?id=persist&title=Persist&startTime=12345", key), nil)
	req.Header.Set("X-API-Key", apiKey)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	db.Close()

	// Re-open DB
	db2, _ := InitDB(dbPath)
	defer db2.Close()
	app2 := &App{DB: db2}

	stream, err := app2.GetStream(context.TODO(), key)
	if err != nil {
		t.Fatalf("failed to get stream: %v", err)
	}
	if stream == nil {
		t.Fatal("stream not found after restart")
	}
	if stream.ActiveID != "persist" {
		t.Errorf("expected persist, got %s", stream.ActiveID)
	}
}

func TestServer_GetTranscriptEndpoint(t *testing.T) {
	key := "test-transcript-endpoint"
	app, mux, db := setupTestApp(t, []string{key})
	defer db.Close()
	ctx := context.Background()

	// Seed data
	stream1ID := "stream1"
	app.UpsertStream(ctx, &Stream{ChannelID: key, ActiveID: stream1ID, IsLive: false})
	app.ReplaceTranscript(ctx, key, stream1ID, []Line{
		{ID: 0, Segments: []Segment{{Text: "Stream 1 Line 0"}}},
	})

	stream2ID := "stream2"
	app.UpsertStream(ctx, &Stream{ChannelID: key, ActiveID: stream2ID, IsLive: true})
	app.ReplaceTranscript(ctx, key, stream2ID, []Line{
		{ID: 0, Segments: []Segment{{Text: "Stream 2 Line 0"}}},
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
	if len(lines) != 1 || lines[0].Segments[0].Text != "Stream 1 Line 0" {
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
	if len(lines) != 1 || lines[0].Segments[0].Text != "Stream 2 Line 0" {
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
	app.UpsertStream(ctx, &Stream{ChannelID: key, ActiveID: "s1", ActiveTitle: "Stream 1", StartTime: "12345", IsLive: true, MediaType: "video"})
	// Create dummy media files
	// s1 folder
	s1Folder := filepath.Join(app.Channels[key].BaseMediaFolder, "s1")
	os.MkdirAll(s1Folder, 0755)
	app.Channels[key].ActiveMediaFolder = s1Folder

	os.WriteFile(filepath.Join(s1Folder, "1.m4a"), []byte("audio"), 0644)
	os.WriteFile(filepath.Join(s1Folder, "1.jpg"), []byte("image"), 0644)

	// Create dummy raw files for clip testing (ids 0-10)
	for i := 0; i <= 10; i++ {
		os.WriteFile(filepath.Join(s1Folder, fmt.Sprintf("%d.raw", i)), []byte("raw_audio"), 0644)
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
	req, _ := http.NewRequest("GET", fmt.Sprintf("/%s/stream/s1/1.m4a", key), nil)
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
	req, _ = http.NewRequest("GET", fmt.Sprintf("/%s/stream/s2/1.m4a", key), nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("streamHandler: expected NotFound for wrong streamID, got %v", rr.Code)
	}

	// 3. Test downloadHandler
	req, _ = http.NewRequest("GET", fmt.Sprintf("/%s/download/s1/1.m4a", key), nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("downloadHandler: expected OK, got %v", rr.Code)
	}
	if rr.Header().Get("Content-Disposition") != "attachment; filename=\"1.m4a\"" {
		t.Errorf("downloadHandler: expected attachment; filename=\"1.m4a\", got %s", rr.Header().Get("Content-Disposition"))
	}

	// 3b. Test downloadHandler with custom name
	req, _ = http.NewRequest("GET", fmt.Sprintf("/%s/download/s1/1.m4a?name=MyCustomFile", key), nil)
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
	clipReq := map[string]interface{}{
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
	if clipResp["id"] == "" {
		t.Error("clipHandler: expected id in response")
	}

	// 6. Test trimHandler (POST)
	trimReq := map[string]interface{}{
		"stream_id": "s1",
		"id":        "1", // Trimming filed ID 1
		"start":     0.0,
		"end":       5.0,
		"filename":  "trimmed_output",
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
	if trimResp["id"] != "1" {
		t.Errorf("trimHandler: expected id '1' (in-place replacement), got '%s'", trimResp["id"])
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
	p1 := &Stream{ChannelID: key, ActiveID: "p1", StartTime: "2000", IsLive: false}
	app.UpsertStream(ctx, p1)
	app.InsertTranscriptLine(ctx, key, "p1", Line{ID: 0, Segments: []Segment{{Text: "P1 Content"}}})
	folder := filepath.Join(cs.BaseMediaFolder, "p1")
	os.MkdirAll(folder, 0755)
	app.Channels[key].ActiveMediaFolder = folder

	// Activate S1 (New Active)
	app.activateStream(ctx, cs, "s1", "Stream 1", "3000", "audio")

	// Verify S1 is active
	s, err := app.GetStream(ctx, key)
	if err != nil {
		t.Fatalf("GetStream failed: %v", err)
	}
	if s.ActiveID != "s1" {
		t.Errorf("expected active stream s1, got %s", s.ActiveID)
	}

	// Verify P1 still exists (1 past stream <= 2)
	past, err := app.GetPastStreams(ctx, key, "s1")
	if err != nil {
		t.Fatalf("GetPastStreams failed: %v", err)
	}
	if len(past) != 1 || past[0].ActiveID != "p1" {
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
	p1 := &Stream{ChannelID: key, ActiveID: "p1", StartTime: "2000", IsLive: false}
	s1 := &Stream{ChannelID: key, ActiveID: "s1", StartTime: "3000", IsLive: true}
	app.UpsertStream(ctx, p1)
	app.UpsertStream(ctx, s1)
	app.InsertTranscriptLine(ctx, key, "p1", Line{ID: 0, Segments: []Segment{{Text: "P1 Content"}}})

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
		if past[0].ActiveID != "s1" {
			t.Errorf("expected past[0] to be s1, got %s", past[0].ActiveID)
		}
		if past[1].ActiveID != "p1" {
			t.Errorf("expected past[1] to be p1, got %s", past[1].ActiveID)
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
	p1 := &Stream{ChannelID: key, ActiveID: "p1", StartTime: "2000", IsLive: false}
	s1 := &Stream{ChannelID: key, ActiveID: "s1", StartTime: "3000", IsLive: false}
	s2 := &Stream{ChannelID: key, ActiveID: "s2", StartTime: "4000", IsLive: true}
	app.UpsertStream(ctx, p1)
	app.UpsertStream(ctx, s1)
	app.UpsertStream(ctx, s2)

	app.InsertTranscriptLine(ctx, key, "p1", Line{ID: 0, Segments: []Segment{{Text: "P1 Content"}}})

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
		if past[0].ActiveID != "s2" {
			t.Errorf("expected past[0] to be s2, got %s", past[0].ActiveID)
		}
		if past[1].ActiveID != "s1" {
			t.Errorf("expected past[1] to be s1, got %s", past[1].ActiveID)
		}
	}

	// Verify P1 transcript deleted
	lines, _ := app.GetTranscript(ctx, key, "p1")
	if len(lines) != 0 {
		t.Error("expected p1 transcript to be deleted")
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
	p5 := &Stream{ChannelID: key, ActiveID: "p5", StartTime: "0100", IsLive: false} // Very old
	p4 := &Stream{ChannelID: key, ActiveID: "p4", StartTime: "0500", IsLive: false} // Old
	s1 := &Stream{ChannelID: key, ActiveID: "s1", StartTime: "3000", IsLive: false}
	s2 := &Stream{ChannelID: key, ActiveID: "s2", StartTime: "4000", IsLive: false}
	s3 := &Stream{ChannelID: key, ActiveID: "s3", StartTime: "5000", IsLive: true}

	app.UpsertStream(ctx, p5)
	app.UpsertStream(ctx, p4)
	app.UpsertStream(ctx, s1)
	app.UpsertStream(ctx, s2)
	app.UpsertStream(ctx, s3)

	app.InsertTranscriptLine(ctx, key, "s1", Line{ID: 0, Segments: []Segment{{Text: "S1 Content"}}})

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
		if past[0].ActiveID != "s3" {
			t.Errorf("expected past[0] to be s3, got %s", past[0].ActiveID)
		}
		if past[1].ActiveID != "s2" {
			t.Errorf("expected past[1] to be s2, got %s", past[1].ActiveID)
		}
	}
	// Verify S1 deleted
	lines, _ := app.GetTranscript(ctx, key, "s1")
	if len(lines) != 0 {
		t.Error("expected s1 transcript to be deleted")
	}
}
