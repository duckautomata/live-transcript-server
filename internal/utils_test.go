package internal

import (
	"encoding/base64"
	"os"
	"path/filepath"
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

func TestRawB64ToFile(t *testing.T) {
	tmpDir := t.TempDir()
	cs := &ChannelState{
		MediaFolder: tmpDir,
	}

	data := "Hello World"
	b64Data := base64.StdEncoding.EncodeToString([]byte(data))

	path, err := cs.RawB64ToFile(b64Data, 1, ".raw")
	if err != nil {
		t.Fatalf("RawB64ToFile failed: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	if string(content) != data {
		t.Errorf("expected %s, got %s", data, string(content))
	}
}

func TestMergeRawAudio(t *testing.T) {
	tmpDir := t.TempDir()
	cs := &ChannelState{
		MediaFolder: tmpDir,
	}

	// Create dummy raw files
	data1 := []byte("Part1")
	data2 := []byte("Part2")

	os.WriteFile(filepath.Join(tmpDir, "1.raw"), data1, 0644)
	os.WriteFile(filepath.Join(tmpDir, "2.raw"), data2, 0644)

	mergedPath, err := cs.MergeRawAudio(1, 2, "merged")
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

func TestResetAudioFile(t *testing.T) {
	tmpDir := t.TempDir()
	folder := filepath.Join(tmpDir, "media")
	os.MkdirAll(folder, 0755)

	cs := &ChannelState{
		MediaFolder: folder,
	}

	// Create test file
	os.WriteFile(filepath.Join(folder, "test.raw"), []byte("test"), 0644)

	cs.ResetAudioFile()

	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Error("expected folder to be empty")
	}
}
