package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type LocalStorage struct {
	BaseDir   string
	PublicURL string // Optional base URL for serving files, e.g. "http://localhost:8080/files"
}

func NewLocalStorage(baseDir string, publicURL string) (*LocalStorage, error) {
	// Ensure baseDir exists
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}
	return &LocalStorage{
		BaseDir:   baseDir,
		PublicURL: publicURL,
	}, nil
}

// resolve maps key to a path under BaseDir, rejecting keys that would escape
// it. Defense in depth: primary key validation happens at the HTTP layer.
func (s *LocalStorage) resolve(key string) (string, error) {
	if strings.Contains(key, "..") {
		return "", fmt.Errorf("invalid storage key %q: contains \"..\"", key)
	}
	fullPath := filepath.Join(s.BaseDir, key) // Join cleans the result
	base := filepath.Clean(s.BaseDir)
	if fullPath != base && !strings.HasPrefix(fullPath, base+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid storage key %q: escapes base directory", key)
	}
	return fullPath, nil
}

func (s *LocalStorage) Save(ctx context.Context, key string, data io.Reader, contentLength int64) (string, error) {
	fullPath, err := s.resolve(key)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(fullPath)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	// Write to a temp file in the destination directory, then rename into
	// place: readers never observe a partially written file, and the rename
	// stays on one filesystem so it is atomic.
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(fullPath)+".tmp*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()

	if _, err := io.Copy(tmp, data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to write data to %s: %w", fullPath, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to close temp file for %s: %w", fullPath, err)
	}
	if err := os.Rename(tmpPath, fullPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to move temp file to %s: %w", fullPath, err)
	}

	return s.GetURL(key), nil
}

func (s *LocalStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	fullPath, err := s.resolve(key)
	if err != nil {
		return nil, err
	}
	return os.Open(fullPath)
}

func (s *LocalStorage) GetURL(key string) string {
	if s.PublicURL != "" {
		return fmt.Sprintf("%s/%s", s.PublicURL, key)
	}
	return key
}

func (s *LocalStorage) DeleteFolder(ctx context.Context, key string) error {
	fullPath, err := s.resolve(key)
	if err != nil {
		return err
	}
	return os.RemoveAll(fullPath)
}

func (s *LocalStorage) IsLocal() bool {
	return true
}

func (s *LocalStorage) StreamExists(ctx context.Context, key string) (bool, error) {
	// Callers pass prefixes with a trailing slash (see StreamPrefix);
	// filepath.Join in resolve normalizes it away, so a directory stat works
	// for both forms.
	fullPath, err := s.resolve(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(fullPath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
