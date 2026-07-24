// Package media wraps the server's ffmpeg invocations and raw-audio merging.
package media

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Processor is the ffmpeg seam. It replaces the old mutable package-level
// Ffmpeg* function vars: tests substitute a fake Processor instead of
// swapping globals.
type Processor interface {
	// Convert transcodes inputPath into outputPath, dropping any video
	// stream.
	Convert(inputPath, outputPath string) error

	// Remux rewrites inputPath's container into outputPath without
	// re-encoding.
	Remux(inputPath, outputPath string) error

	// Trim copies the [start, end) span of inputPath (seconds) into
	// outputPath without re-encoding.
	Trim(inputPath, outputPath string, start, end float64) error

	// ExtractFrame writes a single frame of inputPath into outputPath,
	// scaled to the given height with aspect ratio preserved.
	ExtractFrame(inputPath, outputPath string, height int) error
}

// FFmpeg implements Processor by shelling out to the ffmpeg binary on PATH.
// exec.Command fails naturally where ffmpeg is absent.
type FFmpeg struct{}

var _ Processor = FFmpeg{}

func (FFmpeg) Remux(inputPath, outputPath string) error {
	cmd := exec.Command("ffmpeg",
		"-i", inputPath,
		"-c", "copy",
		"-movflags", "+faststart",
		"-y",
		outputPath)

	output, err := cmd.CombinedOutput() // Capture both stdout and stderr
	if err != nil {
		return fmt.Errorf("ffmpeg conversion failed: %w, output: %s", err, string(output))
	}

	return nil
}

func (FFmpeg) Convert(inputPath, outputPath string) error {
	// -vn disables video recording, keeping only the audio channel
	cmd := exec.Command("ffmpeg", "-i", inputPath, "-vn", "-movflags", "+faststart", "-y", outputPath)

	output, err := cmd.CombinedOutput() // Capture both stdout and stderr
	if err != nil {
		return fmt.Errorf("ffmpeg conversion failed: %w, output: %s", err, string(output))
	}

	return nil
}

func (FFmpeg) ExtractFrame(inputPath, outputPath string, height int) error {
	// -vf "scale=-1:height" maintains aspect ratio while setting height
	scaleFilter := fmt.Sprintf("scale=-1:%d", height)

	cmd := exec.Command("ffmpeg",
		"-i", inputPath,
		"-vframes", "1",
		"-vf", scaleFilter,
		"-q:v", "5", // Standard quality for jpeg
		"-y",
		outputPath)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg frame extraction failed: %w, output: %s", err, string(output))
	}

	return nil
}

func (FFmpeg) Trim(inputPath, outputPath string, start, end float64) error {
	duration := end - start
	if duration <= 0 {
		return fmt.Errorf("invalid duration: %f", duration)
	}

	cmd := exec.Command("ffmpeg",
		"-ss", fmt.Sprintf("%f", start),
		"-i", inputPath,
		"-t", fmt.Sprintf("%f", duration),
		"-c", "copy",
		"-avoid_negative_ts", "make_zero",
		"-movflags", "+faststart",
		"-y",
		outputPath)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg trim failed: %w, output: %s", err, string(output))
	}

	return nil
}

// ChangeExtension replaces path's extension with newExt.
// example/name.abc -> example/name.def
func ChangeExtension(path, newExt string) string {
	if !strings.HasPrefix(newExt, ".") {
		newExt = "." + newExt
	}
	oldExt := filepath.Ext(path)
	return strings.TrimSuffix(path, oldExt) + newExt
}
