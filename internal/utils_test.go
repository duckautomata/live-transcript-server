package internal

import (
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

func TestMergeRawAudio(t *testing.T) {
	tmpDir := t.TempDir()
	cs := &ChannelState{}

	// Create dummy raw files
	data1 := []byte("Part1")
	data2 := []byte("Part2")

	os.WriteFile(filepath.Join(tmpDir, "1.raw"), data1, 0644)
	os.WriteFile(filepath.Join(tmpDir, "2.raw"), data2, 0644)

	mergedPath, err := cs.MergeRawAudio(tmpDir, 1, 2, "merged")
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
