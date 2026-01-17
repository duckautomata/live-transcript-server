package internal

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

// example/name.abc -> example/name.def
func ChangeExtension(filePath string, newExtension string) string {
	ext := filepath.Ext(filePath)
	if ext != "" {
		return filePath[:len(filePath)-len(ext)] + newExtension
	}
	return filePath + newExtension
}

var FfmpegRemux = func(inputFilePath, outputFilePath string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("ffmpeg",
			"-i", inputFilePath,
			"-c", "copy",
			"-movflags", "+faststart",
			"-y", // Overwrite output file
			outputFilePath)
	case "darwin", "linux":
		cmd = exec.Command("ffmpeg",
			"-i", inputFilePath,
			"-c", "copy",
			"-movflags", "+faststart",
			"-y", // Overwrite output file
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

// Binary copy all raw chunks into a single raw file. start and end are inclusive. Returns the merged media path.
func (cs *ChannelState) MergeRawAudio(mediaFolder string, start, end int, uniqueID string) (string, error) {
	rawFilePath := filepath.Join(mediaFolder, fmt.Sprintf("%s.raw", uniqueID))

	// Merge raw media into a single raw file
	outputFile, err := os.Create(rawFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to create output file: %w", err)
	}
	defer outputFile.Close()

	for i := start; i <= end; i++ {
		inputFilename := filepath.Join(mediaFolder, fmt.Sprintf("%d.raw", i))
		inputFile, err := os.Open(inputFilename)
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file '%s' not found", inputFilename)
		}

		if err != nil {
			return "", fmt.Errorf("failed to open input file %s: %w", inputFilename, err)
		}

		_, err = io.Copy(outputFile, inputFile)
		inputFile.Close() // Close explicitly inside loop
		if err != nil {
			return "", fmt.Errorf("failed to copy from %s to output: %w", inputFilename, err)
		}
	}

	return rawFilePath, nil
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
