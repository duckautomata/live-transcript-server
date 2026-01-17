package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

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
	os.WriteFile(filepath.Join(app.Channels[key].MediaFolder, "1.m4a"), []byte("audio"), 0644)
	os.WriteFile(filepath.Join(app.Channels[key].MediaFolder, "1.jpg"), []byte("image"), 0644)

	// Create dummy raw files for clip testing (ids 0-10)
	for i := 0; i <= 10; i++ {
		os.WriteFile(filepath.Join(app.Channels[key].MediaFolder, fmt.Sprintf("%d.raw", i)), []byte("raw_audio"), 0644)
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
