package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"live-transcript-server/internal/storage"
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

func FuzzChangeExtension(f *testing.F) {
	f.Add("example/name.abc", ".def")
	f.Add("test", "def")
	f.Fuzz(func(t *testing.T, path, newExt string) {
		got := ChangeExtension(path, newExt)

		want := newExt
		if !strings.HasPrefix(want, ".") {
			want = "." + want
		}
		if !strings.HasSuffix(got, want) {
			t.Errorf("ChangeExtension(%q, %q) = %q; missing extension suffix %q", path, newExt, got, want)
		}
		if stem := strings.TrimSuffix(path, filepath.Ext(path)); !strings.HasPrefix(got, stem) {
			t.Errorf("ChangeExtension(%q, %q) = %q; lost the stem %q", path, newExt, got, stem)
		}
	})
}

// newTestStorage returns a LocalStorage rooted in a fresh temp dir, which
// doubles as the merge temp dir in these tests.
func newTestStorage(t *testing.T) (*storage.LocalStorage, string) {
	t.Helper()
	tmpDir := t.TempDir()
	st, err := storage.NewLocalStorage(tmpDir, "")
	if err != nil {
		t.Fatalf("NewLocalStorage failed: %v", err)
	}
	return st, tmpDir
}

func saveRaw(t *testing.T, st *storage.LocalStorage, channelKey, streamID, fileID, content string) {
	t.Helper()
	key := storage.RawKey(channelKey, streamID, fileID)
	if _, err := st.Save(context.TODO(), key, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("failed to save %s: %v", key, err)
	}
}

func TestMergeRawAudio(t *testing.T) {
	st, tmpDir := newTestStorage(t)

	channelKey := "testchannel"
	streamID := "stream1"

	saveRaw(t, st, channelKey, streamID, "file1", "Part1")
	saveRaw(t, st, channelKey, streamID, "file2", "Part2")

	fileIDs := []string{"file1", "file2"}

	mergedPath, err := MergeRawAudio(context.TODO(), st, tmpDir, channelKey, streamID, fileIDs, "merged")
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

// TestMergeRawAudioCleansUpOnError ensures a failed merge does not leave an
// orphaned {outputName}.raw file (or temp merge dir) behind in tempDir.
func TestMergeRawAudioCleansUpOnError(t *testing.T) {
	st, tmpDir := newTestStorage(t)

	channelKey := "testchannel"
	streamID := "stream1"

	// Only file1 exists; the missing second file forces a download error mid-merge.
	saveRaw(t, st, channelKey, streamID, "file1", "Part1")

	outputName := "orphan_check"
	fileIDs := []string{"file1", "missing_file"}

	if _, err := MergeRawAudio(context.TODO(), st, tmpDir, channelKey, streamID, fileIDs, outputName); err == nil {
		t.Fatal("expected MergeRawAudio to fail when a source file is missing, got nil")
	}

	// The merged output file must not be left behind in tempDir.
	mergedPath := filepath.Join(tmpDir, outputName+".raw")
	if _, statErr := os.Stat(mergedPath); !os.IsNotExist(statErr) {
		t.Errorf("orphaned merged file left behind at %s (stat err: %v)", mergedPath, statErr)
	}

	// No temp merge directories should remain either.
	if leftovers, _ := filepath.Glob(filepath.Join(tmpDir, "merge_"+outputName+"_*")); len(leftovers) > 0 {
		t.Errorf("orphaned temp merge dirs left behind: %v", leftovers)
	}
}

// TestMergeRawAudioSurfacesDownloadError is the regression test for the old
// errChan plumbing, whose abort check consumed the only buffered error and
// reported a useless "unexpected missing file path" instead: a failure in the
// middle of a large set must surface the real underlying error, naming the
// key that failed, and leave no merged file behind.
func TestMergeRawAudioSurfacesDownloadError(t *testing.T) {
	st, tmpDir := newTestStorage(t)

	channelKey := "testchannel"
	streamID := "stream1"

	// Enough files to saturate the download workers.
	fileIDs := make([]string, 40)
	for i := range fileIDs {
		fileID := fmt.Sprintf("file%d", i)
		fileIDs[i] = fileID
		saveRaw(t, st, channelKey, streamID, fileID, "part")
	}
	// A mid-set chunk that does not exist in storage.
	fileIDs[20] = "missing_file"

	outputName := "real_error_check"
	_, err := MergeRawAudio(context.TODO(), st, tmpDir, channelKey, streamID, fileIDs, outputName)
	if err == nil {
		t.Fatal("expected MergeRawAudio to fail when a source file is missing, got nil")
	}

	failedKey := storage.RawKey(channelKey, streamID, "missing_file")
	if !strings.Contains(err.Error(), failedKey) {
		t.Errorf("error %q does not name the failed key %q", err, failedKey)
	}

	mergedPath := filepath.Join(tmpDir, outputName+".raw")
	if _, statErr := os.Stat(mergedPath); !os.IsNotExist(statErr) {
		t.Errorf("orphaned merged file left behind at %s (stat err: %v)", mergedPath, statErr)
	}
}

func TestMergeRawAudioCanceledContext(t *testing.T) {
	st, tmpDir := newTestStorage(t)

	channelKey := "testchannel"
	streamID := "stream1"

	fileIDs := make([]string, 5)
	for i := range fileIDs {
		fileID := fmt.Sprintf("file%d", i)
		fileIDs[i] = fileID
		saveRaw(t, st, channelKey, streamID, fileID, "part")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	outputName := "canceled_check"
	start := time.Now()
	_, err := MergeRawAudio(ctx, st, tmpDir, channelKey, streamID, fileIDs, outputName)
	if err == nil {
		t.Fatal("expected MergeRawAudio to fail with a canceled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	// Generous bound: the point is that it does not hang, not exact timing.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("MergeRawAudio did not return promptly after cancellation: %v", elapsed)
	}

	mergedPath := filepath.Join(tmpDir, outputName+".raw")
	if _, statErr := os.Stat(mergedPath); !os.IsNotExist(statErr) {
		t.Errorf("orphaned merged file left behind at %s (stat err: %v)", mergedPath, statErr)
	}
}
