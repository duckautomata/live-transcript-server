package internal

import (
	"context"
	"fmt"
	"live-transcript-server/internal/storage"
	"os"
	"strings"
	"testing"
)

func TestChangeExtension(t *testing.T) {
	tests := []struct {
		input    string
		newExt   string
		expected string
	}{
		{"test.abc", ".def", "test.def"},
		{"test", ".def", "test.def"},
		{"/path/to/test.abc", ".xyz", "/path/to/test.xyz"},
	}

	for _, test := range tests {
		result := ChangeExtension(test.input, test.newExt)
		if result != test.expected {
			t.Errorf("expected %s, got %s", test.expected, result)
		}
	}
}

func TestMergeRawAudio(t *testing.T) {
	tmpDir := t.TempDir()

	// Setup App with LocalStorage
	storage, _ := storage.NewLocalStorage(tmpDir, "")
	app := &App{
		TempDir: tmpDir,
		Storage: storage,
	}

	channelKey := "testchannel"
	streamID := "stream1"

	// Create dummy raw files in storage structure
	// Key: channelKey/streamID/raw/fileID.raw
	file1ID := "file1"
	file2ID := "file2"

	file1Key := fmt.Sprintf("%s/%s/raw/%s.raw", channelKey, streamID, file1ID)
	file2Key := fmt.Sprintf("%s/%s/raw/%s.raw", channelKey, streamID, file2ID)

	app.Storage.Save(context.TODO(), file1Key, strings.NewReader("Part1"), int64(len("Part1")))
	app.Storage.Save(context.TODO(), file2Key, strings.NewReader("Part2"), int64(len("Part2")))

	fileIDs := []string{file1ID, file2ID}

	mergedPath, err := app.MergeRawAudio(context.TODO(), channelKey, streamID, fileIDs, "merged")
	if err != nil {
		t.Fatalf("MergeRawAudio failed: %v", err)
	}

	content, err := os.ReadFile(mergedPath)
	if err != nil {
		t.Fatalf("failed to read merged file: %v", err)
	}

	expected := "Part1Part2"
	if string(content) != expected {
		t.Errorf("expected %s, got %s", expected, string(content))
	}
}
