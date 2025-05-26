package internal

import (
	"bytes"
	"encoding/base64"
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func (g *GobArchive) GobToClientData(dataBuffer *bytes.Reader) (*ClientData, error) {
	if dataBuffer == nil {
		return nil, fmt.Errorf("dataBuffer must not be nil")
	}
	decoder := gob.NewDecoder(dataBuffer)

	var data ClientData
	if err := decoder.Decode(&data); err != nil {
		return nil, err
	}

	return &data, nil
}

func (g *GobArchive) FileToClientData() (*ClientData, error) {
	file, err := os.Open(g.fileName)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	decoder := gob.NewDecoder(file)
	var data ClientData
	if err := decoder.Decode(&data); err != nil {
		return nil, err
	}

	return &data, nil
}

func (g *GobArchive) ClientDataToFile(data *ClientData) error {
	file, err := os.Create(g.fileName)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := gob.NewEncoder(file)
	if err := encoder.Encode(data); err != nil {
		return err
	}

	return nil
}

// example/name.abc -> example/name.def
func ChangeExtension(filePath string, newExtension string) string {
	ext := filepath.Ext(filePath)
	if ext != "" {
		return filePath[:len(filePath)-len(ext)] + newExtension
	}
	return filePath + newExtension
}

func FfmpegRemux(inputFilePath, outputFilePath string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("ffmpeg", "-i", inputFilePath, "-c", "copy", outputFilePath)
	case "darwin", "linux":
		cmd = exec.Command("ffmpeg", "-i", inputFilePath, "-c", "copy", outputFilePath)
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}

	output, err := cmd.CombinedOutput() // Capture both stdout and stderr
	if err != nil {
		return fmt.Errorf("ffmpeg conversion failed: %w, output: %s", err, string(output))
	}

	return nil
}

func FfmpegConvert(inputFilePath, outputFilePath string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("ffmpeg", "-i", inputFilePath, outputFilePath)
	case "darwin", "linux":
		cmd = exec.Command("ffmpeg", "-i", inputFilePath, outputFilePath)
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}

	output, err := cmd.CombinedOutput() // Capture both stdout and stderr
	if err != nil {
		return fmt.Errorf("ffmpeg conversion failed: %w, output: %s", err, string(output))
	}

	return nil
}

// converts a b64 endocing of binary media data and saves it to a file. Returns file path
func (w *WebSocketServer) RawB64ToFile(rawB64 string, id int, ext string) (string, error) {
	filePath := filepath.Join(w.mediaFolder, fmt.Sprintf("%d.raw", id))

	decodedData, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil {
		return "", fmt.Errorf("error, unable to decode b64 media: %v", err)
	}

	os.MkdirAll(w.mediaFolder, 0755)
	err = os.WriteFile(filePath, decodedData, 0755)
	if err != nil {
		return "", fmt.Errorf("error, unable to write media to file '%s': %v", filePath, err)
	}

	return filePath, nil
}

// Binary copy all raw chunks into a single faw file. start and end are inclusive. Returns the merged media path and if this has already been converted to mp3.
func (w *WebSocketServer) MergeRawAudio(start, end int, clipExt string) (string, bool, error) {
	mediaFilePath := filepath.Join(w.mediaFolder, fmt.Sprintf("%d-%d%s", start, end, clipExt))
	rawFilePath := filepath.Join(w.mediaFolder, fmt.Sprintf("%d-%d.raw", start, end))

	// Check if the file already exists. No need to duplicate work
	_, err := os.Stat(mediaFilePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", false, fmt.Errorf("failed to check file '%s': %w", mediaFilePath, err)
		}
	} else {
		// file already exists, no need to recreate it
		return mediaFilePath, true, nil
	}

	// Merge raw media into a single raw file
	outputFile, err := os.Create(rawFilePath)
	if err != nil {
		return "", false, fmt.Errorf("failed to create output file: %w", err)
	}
	defer outputFile.Close()

	for i := start; i <= end; i++ {
		inputFilename := filepath.Join(w.mediaFolder, fmt.Sprintf("%d.raw", i))
		inputFile, err := os.Open(inputFilename)
		if os.IsNotExist(err) {
			return "", false, fmt.Errorf("file '%s' not found", inputFilename)
		}

		if err != nil {
			return "", false, fmt.Errorf("failed to open input file %s: %w", inputFilename, err)
		}
		defer inputFile.Close()

		_, err = io.Copy(outputFile, inputFile)
		if err != nil {
			return "", false, fmt.Errorf("failed to copy from %s to output: %w", inputFilename, err)
		}
	}

	return rawFilePath, false, nil
}

func (w *WebSocketServer) ResetAudioFile() {
	os.RemoveAll(w.mediaFolder)
	os.MkdirAll(w.mediaFolder, 0755)
}
