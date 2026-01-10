package internal

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// converts a b64 endocing of binary media data and saves it to a file. Returns file path
func (cs *ChannelState) RawB64ToFile(rawB64 string, id int, ext string) (string, error) {
	filePath := filepath.Join(cs.MediaFolder, fmt.Sprintf("%d.raw", id))

	decodedData, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil {
		return "", fmt.Errorf("error, unable to decode b64 media: %v", err)
	}

	os.MkdirAll(cs.MediaFolder, 0755)
	err = os.WriteFile(filePath, decodedData, 0755)
	if err != nil {
		return "", fmt.Errorf("error, unable to write media to file '%s': %v", filePath, err)
	}

	return filePath, nil
}

// Binary copy all raw chunks into a single raw file. start and end are inclusive. Returns the merged media path.
func (cs *ChannelState) MergeRawAudio(start, end int, uniqueID string) (string, error) {
	rawFilePath := filepath.Join(cs.MediaFolder, fmt.Sprintf("%s.raw", uniqueID))

	// Merge raw media into a single raw file
	outputFile, err := os.Create(rawFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to create output file: %w", err)
	}
	defer outputFile.Close()

	for i := start; i <= end; i++ {
		inputFilename := filepath.Join(cs.MediaFolder, fmt.Sprintf("%d.raw", i))
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

func (cs *ChannelState) ResetAudioFile() {
	os.RemoveAll(cs.MediaFolder)
	os.MkdirAll(cs.MediaFolder, 0755)
}
