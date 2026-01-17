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
	if err := app.ReplaceTranscript(ctx, key, lines); err != nil {
		t.Fatalf("failed to replace transcript: %v", err)
	}
	// Verify transcripts are there
	transcripts, err := app.GetTranscript(ctx, key)
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
	transcripts, err = app.GetTranscript(ctx, key)
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
	transcripts, err = app.GetTranscript(ctx, key)
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
	transcripts, err = app.GetTranscript(ctx, key)
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
	transcripts, err = app.GetTranscript(ctx, key)
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
	transcripts, err = app.GetTranscript(ctx, key)
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
	// Verify transcripts are reset
	transcripts, err = app.GetTranscript(ctx, key)
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
	lines, err := app.GetTranscript(context.TODO(), key)
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
	lines, err := app.GetTranscript(ctx, key)
	if err != nil {
		t.Fatalf("failed to get transcript: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("expected 0 lines, got %d", len(lines))
	}

	// Insert line with media available flag set to false
	app.InsertTranscriptLine(ctx, key, Line{ID: 0, Timestamp: 200, MediaAvailable: false, Segments: []Segment{{Timestamp: 200, Text: "World"}}})

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
	lines, err = app.GetTranscript(ctx, key)
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
	app.InsertTranscriptLine(context.TODO(), key, Line{ID: 0, Timestamp: 100})

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

	lines, err := app.GetTranscript(context.TODO(), key)
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
	lines, err = app.GetTranscript(context.TODO(), key)
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
	lines, err = app.GetTranscript(context.TODO(), key)
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
