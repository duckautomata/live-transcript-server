package media

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"live-transcript-server/internal/storage"
)

// mergeConcurrency bounds the number of parallel chunk downloads.
const mergeConcurrency = 32

// MergeRawAudio merges raw audio files from storage into a single raw file.
// It downloads the files to a temp directory in parallel, concatenates them
// in fileIDs order, and returns the path to the merged file (which lives in
// tempDir; the caller owns its cleanup).
func MergeRawAudio(ctx context.Context, st storage.Storage, tempDir, channelKey, streamID string, fileIDs []string, outputName string) (string, error) {
	if len(fileIDs) == 0 {
		return "", fmt.Errorf("no files to merge")
	}

	mergedFilePath := filepath.Join(tempDir, fmt.Sprintf("%s.raw", outputName))
	mergedFile, err := os.Create(mergedFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to create merged file: %v", err)
	}
	// Remove the merged file unless we successfully hand it back to the caller.
	// Registered before the Close defer so that (LIFO) the file is closed first,
	// then removed. Without this, any error below orphans the .raw file in tempDir.
	success := false
	defer func() {
		if !success {
			os.Remove(mergedFilePath)
		}
	}()
	defer mergedFile.Close()

	// Temporary directory for chunks to ensure thread safety and easy cleanup
	downloadDir, err := os.MkdirTemp(tempDir, fmt.Sprintf("merge_%s_*", outputName))
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir for merge: %v", err)
	}
	defer os.RemoveAll(downloadDir)

	results := make([]string, len(fileIDs))

	// The first failure is recorded exactly once and cancels dlCtx so
	// in-flight downloads stop; later failures are dropped.
	dlCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var firstErr error
	var failOnce sync.Once
	fail := func(err error) {
		failOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	sem := make(chan struct{}, mergeConcurrency)
	var wg sync.WaitGroup

	for i, fileID := range fileIDs {
		wg.Add(1)
		go func(i int, fileID string) {
			defer wg.Done()

			// Acquire semaphore
			select {
			case sem <- struct{}{}:
			case <-dlCtx.Done():
				return
			}
			defer func() { <-sem }()

			// Abort if a previous download already failed
			if dlCtx.Err() != nil {
				return
			}

			key := storage.RawKey(channelKey, streamID, fileID)
			tempPath := filepath.Join(downloadDir, fmt.Sprintf("%d_%s.raw", i, fileID))

			// Download file from storage
			reader, err := st.Get(dlCtx, key)
			if err != nil {
				fail(fmt.Errorf("failed to get raw file %s: %w", key, err))
				return
			}
			defer reader.Close()

			f, err := os.Create(tempPath)
			if err != nil {
				fail(fmt.Errorf("failed to create temp file %s: %w", tempPath, err))
				return
			}
			defer f.Close()

			if _, err := io.Copy(f, reader); err != nil {
				fail(fmt.Errorf("failed to write temp file %s: %w", tempPath, err))
				return
			}

			// Distinct indices make the slice writes race-free.
			results[i] = tempPath
		}(i, fileID)
	}

	wg.Wait()

	if firstErr != nil {
		return "", firstErr
	}
	// A cancelled caller context makes goroutines return without recording
	// an error; surface the cancellation rather than a missing-path error.
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Concatenate files in order
	for _, path := range results {
		if path == "" {
			return "", fmt.Errorf("unexpected missing file path in merge results")
		}

		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("failed to open chunk %s for merging: %v", path, err)
		}
		if _, err := io.Copy(mergedFile, f); err != nil {
			f.Close()
			return "", fmt.Errorf("failed to append chunk %s to merged file: %v", path, err)
		}
		f.Close()
	}

	success = true
	return mergedFilePath, nil
}
