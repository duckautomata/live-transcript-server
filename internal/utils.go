package internal

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// example/name.abc -> example/name.def
func ChangeExtension(path, newExt string) string {
	if !strings.HasPrefix(newExt, ".") {
		newExt = "." + newExt
	}
	oldExt := filepath.Ext(path)
	return strings.TrimSuffix(path, oldExt) + newExt
}

var FfmpegRemux = func(inputFilePath, outputFilePath string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("ffmpeg",
			"-i", inputFilePath,
			"-c", "copy",
			"-movflags", "+faststart",
			"-y",
			outputFilePath)
	case "darwin", "linux":
		cmd = exec.Command("ffmpeg",
			"-i", inputFilePath,
			"-c", "copy",
			"-movflags", "+faststart",
			"-y",
			outputFilePath)
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}

	output, err := cmd.CombinedOutput() // Capture both stdout and stderr
	if err != nil {
		return fmt.Errorf("ffmpeg conversion failed: %w, output: %s", err, string(output))
	}

	return nil
}

var FfmpegConvert = func(inputFilePath, outputFilePath string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "windows":
		// Added -vn to disable video recording, keeping only the audio channel
		cmd = exec.Command("ffmpeg", "-i", inputFilePath, "-vn", "-movflags", "+faststart", "-y", outputFilePath)
	case "darwin", "linux":
		// Added -vn to disable video recording, keeping only the audio channel
		cmd = exec.Command("ffmpeg", "-i", inputFilePath, "-vn", "-movflags", "+faststart", "-y", outputFilePath)
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}

	output, err := cmd.CombinedOutput() // Capture both stdout and stderr
	if err != nil {
		return fmt.Errorf("ffmpeg conversion failed: %w, output: %s", err, string(output))
	}

	return nil
}

var FfmpegExtractFrame = func(inputFilePath, outputFilePath string, height int) error {
	var cmd *exec.Cmd

	// -vf "scale=-1:height" maintains aspect ratio while setting height
	scaleFilter := fmt.Sprintf("scale=-1:%d", height)

	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("ffmpeg",
			"-i", inputFilePath,
			"-vframes", "1",
			"-vf", scaleFilter,
			"-q:v", "5", // Standard quality for jpeg
			"-y",
			outputFilePath)
	case "darwin", "linux":
		cmd = exec.Command("ffmpeg",
			"-i", inputFilePath,
			"-vframes", "1",
			"-vf", scaleFilter,
			"-q:v", "5",
			"-y",
			outputFilePath)
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg frame extraction failed: %w, output: %s", err, string(output))
	}

	return nil
}

var FfmpegTrim = func(inputFilePath, outputFilePath string, start, end float64) error {
	var cmd *exec.Cmd

	duration := end - start
	if duration <= 0 {
		return fmt.Errorf("invalid duration: %f", duration)
	}

	switch runtime.GOOS {
	case "windows", "darwin", "linux":
		cmd = exec.Command("ffmpeg",
			"-ss", fmt.Sprintf("%f", start),
			"-i", inputFilePath,
			"-t", fmt.Sprintf("%f", duration),
			"-c", "copy",
			"-avoid_negative_ts", "make_zero",
			"-movflags", "+faststart",
			"-y",
			outputFilePath)
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg trim failed: %w, output: %s", err, string(output))
	}

	return nil
}

// MergeRawAudio merges raw audio files from storage into a single raw file.
// It downloads the files to a temp directory in parallel, concatenates them, and returns the path to the merged file.
func (app *App) MergeRawAudio(ctx context.Context, channelKey, streamID string, fileIDs []string, outputName string) (string, error) {
	if len(fileIDs) == 0 {
		return "", fmt.Errorf("no files to merge")
	}

	mergedFilePath := filepath.Join(app.TempDir, fmt.Sprintf("%s.raw", outputName))
	mergedFile, err := os.Create(mergedFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to create merged file: %v", err)
	}
	defer mergedFile.Close()

	// Temporary directory for chunks to ensure thread safety and easy cleanup
	tempDir, err := os.MkdirTemp(app.TempDir, fmt.Sprintf("merge_%s_*", outputName))
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir for merge: %v", err)
	}
	defer os.RemoveAll(tempDir)

	type downloadResult struct {
		index int
		path  string
		err   error
	}

	results := make([]string, len(fileIDs))

	// Concurrency control
	concurrency := 32
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	errChan := make(chan error, 1)

	for i, fileID := range fileIDs {
		wg.Add(1)
		go func(i int, fileID string) {
			defer wg.Done()

			// Acquire semaphore
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			// Check if we should abort due to previous error
			select {
			case <-errChan:
				return
			default:
			}

			// Path: activeChannel/streamID/raw/fileID.raw
			key := fmt.Sprintf("%s/%s/raw/%s.raw", channelKey, streamID, fileID)
			tempPath := filepath.Join(tempDir, fmt.Sprintf("%d_%s.raw", i, fileID))

			// Download file from storage
			reader, err := app.Storage.Get(ctx, key)
			if err != nil {
				select {
				case errChan <- fmt.Errorf("failed to get raw file %s: %w", key, err):
				default:
				}
				return
			}
			defer reader.Close()

			f, err := os.Create(tempPath)
			if err != nil {
				select {
				case errChan <- fmt.Errorf("failed to create temp file %s: %w", tempPath, err):
				default:
				}
				return
			}
			defer f.Close()

			if _, err := io.Copy(f, reader); err != nil {
				select {
				case errChan <- fmt.Errorf("failed to write temp file %s: %w", tempPath, err):
				default:
				}
				return
			}

			// Store success path (thread-safe logic handled by index assignment essentially,
			// but we are just writing to slice index which is safe if indices are distinct)
			results[i] = tempPath
		}(i, fileID)
	}

	wg.Wait()

	// Check for errors
	select {
	case err := <-errChan:
		return "", err
	default:
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

	return mergedFilePath, nil
}

// rotateFolders handles the rotation of media folders, keeping a specified number of past streams.
func rotateFolders(baseMediaFolder string, numPastStreams int, activeId string, key string) {
	// 1. List all folders in BaseMediaFolder
	files, err := os.ReadDir(baseMediaFolder)
	if err == nil {
		var streamFolders []os.DirEntry
		for _, file := range files {
			if file.IsDir() {
				streamFolders = append(streamFolders, file)
			}
		}

		// 2. Sort folders by modification time (oldest first).
		type folderInfo struct {
			Name    string
			ModTime time.Time
		}
		var folders []folderInfo
		for _, f := range streamFolders {
			info, err := f.Info()
			if err == nil {
				folders = append(folders, folderInfo{Name: f.Name(), ModTime: info.ModTime()})
			}
		}

		// Sort by ModTime descending (newest first)
		sort.Slice(folders, func(i, j int) bool {
			return folders[i].ModTime.After(folders[j].ModTime)
		})

		// 3. Keep NumPastStreams + 1 (current)
		// If NumPastStreams == 0, keep 1 (current).
		keepCount := numPastStreams + 1
		if len(folders) > keepCount {
			for i := keepCount; i < len(folders); i++ {
				// Don't delete if it's somehow the current active one (safety check)
				if folders[i].Name == activeId {
					continue
				}
				pathToDelete := filepath.Join(baseMediaFolder, folders[i].Name)
				slog.Info("deleting old stream folder", "key", key, "path", pathToDelete)
				os.RemoveAll(pathToDelete)
			}
		}
	} else {
		slog.Warn("failed to list base media folder for rotation", "key", key, "err", err)
	}
}
