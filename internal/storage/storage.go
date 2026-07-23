package storage

import (
	"context"
	"fmt"
	"io"

	"live-transcript-server/internal/config"
)

// Storage defines the interface for media file storage operations.
type Storage interface {
	// Save saves data to the underlying storage.
	// key is the relative path/key for the file.
	// contentLength must be provided for correct handling by object storage.
	// Returns the public URL or file path.
	Save(ctx context.Context, key string, data io.Reader, contentLength int64) (string, error)

	// Get retrieves data from the underlying storage.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// GetURL returns the public URL for the given key.
	// For local storage, this might return a relative path or file:// URL,
	// but the server typically serves these via http.
	GetURL(key string) string

	// DeleteFolder deletes the folder and all its contents at key.
	DeleteFolder(ctx context.Context, key string) error

	// StreamExists checks if the stream data exists in storage
	StreamExists(ctx context.Context, key string) (bool, error)

	IsLocal() bool
}

// New constructs the storage backend selected by cfg.Type: "" or "local"
// builds a LocalStorage rooted at localBaseDir, "r2" builds an R2Storage from
// cfg.R2. Any other type is an error rather than a silent fallback to local.
func New(ctx context.Context, cfg config.StorageConfig, localBaseDir string) (Storage, error) {
	switch cfg.Type {
	case "", "local":
		return NewLocalStorage(localBaseDir, "")
	case "r2":
		return NewR2Storage(ctx, cfg.R2.AccountId, cfg.R2.AccessKeyId, cfg.R2.SecretAccessKey, cfg.R2.Bucket, cfg.R2.PublicUrl)
	default:
		return nil, fmt.Errorf("unknown storage type %q", cfg.Type)
	}
}
