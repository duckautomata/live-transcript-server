package storage

import (
	"context"
	"io"
)

// Storage defines the interface for media file storage operations.
type Storage interface {
	// Save saves data to the underlying storage.
	// key is the relative path/key for the file.
	// Returns the public URL or file path.
	Save(ctx context.Context, key string, data io.Reader) (string, error)

	// Get retrieves data from the underlying storage.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// GetURL returns the public URL for the given key.
	// For local storage, this might return a relative path or file:// URL,
	// but the server typically serves these via http.
	GetURL(key string) string

	// Delete deletes the file at key.
	Delete(ctx context.Context, key string) error

	// DeleteFolder deletes the folder and all its contents at key.
	DeleteFolder(ctx context.Context, key string) error

	// StreamExists checks if the stream data exists in storage
	StreamExists(ctx context.Context, key string) (bool, error)

	IsLocal() bool
}
