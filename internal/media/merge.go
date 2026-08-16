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

// mergeConcurrency bounds both the number of parallel chunk downloads and how
// many downloaded chunks may sit on disk ahead of the writer.
const mergeConcurrency = 32

// MergeRawAudio merges raw audio files from storage into a single raw file.
// Chunks are downloaded in parallel into a temp directory but appended in
// fileIDs order and deleted as soon as they are appended, so the scratch space
// a merge needs stays bounded by mergeConcurrency chunks no matter how many
// files it is given — a full-stream merge would otherwise materialize every
// chunk of the stream at once. Returns the path to the merged file (which
// lives in tempDir; the caller owns its cleanup).
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

	// Cancelling dlCtx stops pending and in-flight downloads. Every return path
	// below goes through abort() or the deferred cancel, so no download outlives
	// the call — which is what makes the deferred RemoveAll safe.
	dlCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type chunk struct {
		path string
		err  error
	}
	// One buffered slot per chunk: a downloader always hands its result off
	// without blocking, so it can never leak waiting on a reader that gave up.
	results := make([]chan chunk, len(fileIDs))
	for i := range results {
		results[i] = make(chan chunk, 1)
	}

	// A slot is taken before a chunk is downloaded and released only once the
	// writer has appended and deleted it. That, not the download count, is what
	// caps how much disk a merge occupies.
	slots := make(chan struct{}, mergeConcurrency)

	var wg sync.WaitGroup
	// The producer holds a count for itself the whole time it runs, so the
	// counter can never hit zero between two of its own Adds.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i, fileID := range fileIDs {
			select {
			case slots <- struct{}{}:
			case <-dlCtx.Done():
				return
			}
			wg.Add(1)
			go func(i int, fileID string) {
				defer wg.Done()
				path, err := downloadChunk(dlCtx, st, downloadDir, channelKey, streamID, fileID, i)
				results[i] <- chunk{path: path, err: err}
			}(i, fileID)
		}
	}()

	// abort stops the outstanding downloads and waits for them before handing
	// err back, so the deferred cleanups never race a live goroutine.
	abort := func(err error) (string, error) {
		cancel()
		wg.Wait()
		return "", err
	}

	for i := range fileIDs {
		// Checked before the receive: a caller that cancelled mid-merge should
		// get the cancellation, not whichever chunk happened to already be in
		// its slot (local storage reads ignore context entirely).
		if err := ctx.Err(); err != nil {
			return abort(err)
		}

		var res chunk
		select {
		case res = <-results[i]:
		case <-ctx.Done():
			return abort(ctx.Err())
		}
		if res.err != nil {
			return abort(res.err)
		}

		f, err := os.Open(res.path)
		if err != nil {
			return abort(fmt.Errorf("failed to open chunk %s for merging: %v", res.path, err))
		}
		_, copyErr := io.Copy(mergedFile, f)
		f.Close()
		if copyErr != nil {
			return abort(fmt.Errorf("failed to append chunk %s to merged file: %v", res.path, copyErr))
		}

		os.Remove(res.path)
		<-slots
	}

	wg.Wait()
	success = true
	return mergedFilePath, nil
}

// downloadChunk copies one raw chunk out of storage into downloadDir and
// returns its path. The index prefix keeps names unique even when the same
// file ID appears twice in a merge.
func downloadChunk(ctx context.Context, st storage.Storage, downloadDir, channelKey, streamID, fileID string, i int) (string, error) {
	key := storage.RawKey(channelKey, streamID, fileID)
	reader, err := st.Get(ctx, key)
	if err != nil {
		return "", fmt.Errorf("failed to get raw file %s: %w", key, err)
	}
	defer reader.Close()

	tempPath := filepath.Join(downloadDir, fmt.Sprintf("%d_%s.raw", i, fileID))
	f, err := os.Create(tempPath)
	if err != nil {
		return "", fmt.Errorf("failed to create temp file %s: %w", tempPath, err)
	}
	if _, err := io.Copy(f, reader); err != nil {
		f.Close()
		return "", fmt.Errorf("failed to write temp file %s: %w", tempPath, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("failed to close temp file %s: %w", tempPath, err)
	}
	return tempPath, nil
}
