package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func (s *LocalStorage) Save(ctx context.Context, key string, data io.Reader, contentLength int64) (string, error) {
	fullPath := filepath.Join(s.BaseDir, key)
	dir := filepath.Dir(fullPath)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	file, err := os.Create(fullPath)
	if err != nil {
		return "", fmt.Errorf("failed to create file %s: %w", fullPath, err)
	}
	defer file.Close()

	if _, err := io.Copy(file, data); err != nil {
		return "", fmt.Errorf("failed to write data to %s: %w", fullPath, err)
	}

	return s.GetURL(key), nil
}

func (s *LocalStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	fullPath := filepath.Join(s.BaseDir, key)
	return os.Open(fullPath)
}

func (s *LocalStorage) GetURL(key string) string {
	if s.PublicURL != "" {
		return fmt.Sprintf("%s/%s", s.PublicURL, key)
	}
	return key
}

func (s *LocalStorage) Delete(ctx context.Context, key string) error {
	fullPath := filepath.Join(s.BaseDir, key)
	return os.Remove(fullPath)
}

func (s *LocalStorage) DeleteFolder(ctx context.Context, key string) error {
	fullPath := filepath.Join(s.BaseDir, key)
	return os.RemoveAll(fullPath)
}

func (s *LocalStorage) IsLocal() bool {
	return true
}

func (s *LocalStorage) StreamExists(ctx context.Context, key string) (bool, error) {
	fullPath := filepath.Join(s.BaseDir, key)
	_, err := os.Stat(fullPath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
