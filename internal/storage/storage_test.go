package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"live-transcript-server/internal/config"
)

// errReader fails partway through a read, simulating a producer dying
// mid-upload.
type errReader struct{}

func (errReader) Read(p []byte) (int, error) {
	return 0, errors.New("simulated read failure")
}

func newLocal(t *testing.T) *LocalStorage {
	t.Helper()
	s, err := NewLocalStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewLocalStorage failed: %v", err)
	}
	return s
}

func TestLocalStorageRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newLocal(t)

	key := RawKey("chan", "stream1", "file1")
	content := "hello raw audio"

	url, err := s.Save(ctx, key, strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if url != key {
		t.Errorf("Save returned %q, want %q (no PublicURL configured)", url, key)
	}

	reader, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatalf("reading saved file failed: %v", err)
	}
	if string(got) != content {
		t.Errorf("Get returned %q, want %q", string(got), content)
	}

	// StreamExists must accept prefixes both with and without the trailing
	// slash (filepath.Join normalizes it away).
	for _, prefix := range []string{StreamPrefix("chan", "stream1"), "chan/stream1"} {
		exists, err := s.StreamExists(ctx, prefix)
		if err != nil {
			t.Fatalf("StreamExists(%q) failed: %v", prefix, err)
		}
		if !exists {
			t.Errorf("StreamExists(%q) = false, want true", prefix)
		}
	}
	exists, err := s.StreamExists(ctx, StreamPrefix("chan", "no-such-stream"))
	if err != nil {
		t.Fatalf("StreamExists failed: %v", err)
	}
	if exists {
		t.Error("StreamExists = true for a missing stream, want false")
	}

	if err := s.DeleteFolder(ctx, StreamPrefix("chan", "stream1")); err != nil {
		t.Fatalf("DeleteFolder failed: %v", err)
	}
	exists, err = s.StreamExists(ctx, StreamPrefix("chan", "stream1"))
	if err != nil {
		t.Fatalf("StreamExists after delete failed: %v", err)
	}
	if exists {
		t.Error("StreamExists = true after DeleteFolder, want false")
	}
	if _, err := s.Get(ctx, key); err == nil {
		t.Error("Get succeeded after DeleteFolder, want error")
	}
}

// TestLocalStoragePartialWriteInvisible ensures a failed Save leaves nothing
// behind: no destination file and no temp file for a reader to observe.
func TestLocalStoragePartialWriteInvisible(t *testing.T) {
	ctx := context.Background()
	s := newLocal(t)

	key := "chan/stream1/raw/broken.raw"
	if _, err := s.Save(ctx, key, errReader{}, 100); err == nil {
		t.Fatal("Save with erroring reader succeeded, want error")
	}

	if _, err := os.Stat(filepath.Join(s.BaseDir, key)); !os.IsNotExist(err) {
		t.Errorf("destination file exists after failed Save (stat err: %v)", err)
	}

	// The destination directory must be empty — no orphaned temp files.
	entries, err := os.ReadDir(filepath.Join(s.BaseDir, "chan", "stream1", "raw"))
	if err != nil {
		t.Fatalf("failed to read destination dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("leftover files after failed Save: %v", names)
	}
}

// TestLocalStorageSaveOverwriteAtomic ensures overwriting an existing key
// never exposes a partial file: a failed overwrite leaves the old content.
func TestLocalStorageSaveOverwriteAtomic(t *testing.T) {
	ctx := context.Background()
	s := newLocal(t)

	key := "chan/stream1/raw/file.raw"
	if _, err := s.Save(ctx, key, strings.NewReader("original"), 8); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if _, err := s.Save(ctx, key, errReader{}, 100); err == nil {
		t.Fatal("Save with erroring reader succeeded, want error")
	}

	got, err := os.ReadFile(filepath.Join(s.BaseDir, key))
	if err != nil {
		t.Fatalf("reading file after failed overwrite: %v", err)
	}
	if string(got) != "original" {
		t.Errorf("file content after failed overwrite = %q, want %q", string(got), "original")
	}
}

func TestLocalStorageRejectsTraversal(t *testing.T) {
	ctx := context.Background()
	s := newLocal(t)

	// Plant a file outside BaseDir that a traversal key would reach.
	outside := filepath.Join(filepath.Dir(s.BaseDir), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0644); err != nil {
		t.Fatalf("failed to plant outside file: %v", err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	for _, key := range []string{
		"../outside.txt",
		"chan/../../outside.txt",
		"..",
		"chan/..",
	} {
		if _, err := s.Save(ctx, key, strings.NewReader("x"), 1); err == nil {
			t.Errorf("Save(%q) succeeded, want traversal rejection", key)
		}
		if _, err := s.Get(ctx, key); err == nil {
			t.Errorf("Get(%q) succeeded, want traversal rejection", key)
		}
		if err := s.DeleteFolder(ctx, key); err == nil {
			t.Errorf("DeleteFolder(%q) succeeded, want traversal rejection", key)
		}
		if _, err := s.StreamExists(ctx, key); err == nil {
			t.Errorf("StreamExists(%q) succeeded, want traversal rejection", key)
		}
	}

	if got, err := os.ReadFile(outside); err != nil || string(got) != "secret" {
		t.Errorf("outside file was touched (content %q, err %v)", string(got), err)
	}
}

func TestKeyBuilders(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"RawKey", RawKey("chan", "s1", "f1"), "chan/s1/raw/f1.raw"},
		{"AudioKey", AudioKey("chan", "s1", "f1"), "chan/s1/audio/f1.m4a"},
		{"FrameKey", FrameKey("chan", "s1", "f1"), "chan/s1/frame/f1.jpg"},
		{"ClipKey", ClipKey("chan", "s1", "clip1", ".mp4"), "chan/s1/clips/clip1.mp4"},
		{"StreamPrefix", StreamPrefix("chan", "s1"), "chan/s1/"},
		{"RawPrefix", RawPrefix("chan", "s1"), "chan/s1/raw/"},
	}
	for _, test := range tests {
		if test.got != test.want {
			t.Errorf("%s = %q, want %q", test.name, test.got, test.want)
		}
	}
}

func TestNewFactory(t *testing.T) {
	ctx := context.Background()

	// Empty type defaults to local.
	s, err := New(ctx, config.StorageConfig{}, t.TempDir())
	if err != nil {
		t.Fatalf("New with empty type failed: %v", err)
	}
	if !s.IsLocal() {
		t.Error("New with empty type did not build local storage")
	}

	// Explicit "local".
	s, err = New(ctx, config.StorageConfig{Type: "local"}, t.TempDir())
	if err != nil {
		t.Fatalf("New with type local failed: %v", err)
	}
	if !s.IsLocal() {
		t.Error("New with type local did not build local storage")
	}

	// Unknown types must error instead of silently falling back to local.
	if _, err := New(ctx, config.StorageConfig{Type: "s3"}, t.TempDir()); err == nil {
		t.Error("New with unknown type succeeded, want error")
	}
}
