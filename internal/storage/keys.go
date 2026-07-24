package storage

import "fmt"

// This file is the single home for storage key construction. Every key in the
// bucket (or under LocalStorage.BaseDir) follows the layout
// {channel}/{streamID}/{kind}/{file}; building keys here keeps the layout in
// one place instead of scattered fmt.Sprintf calls.

// RawKey returns the key for a raw audio chunk.
func RawKey(channel, stream, fileID string) string {
	return fmt.Sprintf("%s/%s/raw/%s.raw", channel, stream, fileID)
}

// AudioKey returns the key for a converted m4a audio chunk.
func AudioKey(channel, stream, fileID string) string {
	return fmt.Sprintf("%s/%s/audio/%s.m4a", channel, stream, fileID)
}

// FrameKey returns the key for an extracted video frame.
func FrameKey(channel, stream, fileID string) string {
	return fmt.Sprintf("%s/%s/frame/%s.jpg", channel, stream, fileID)
}

// ClipKey returns the key for a clip. ext includes the leading dot
// (e.g. ".mp4").
func ClipKey(channel, stream, id, ext string) string {
	return fmt.Sprintf("%s/%s/clips/%s%s", channel, stream, id, ext)
}

// StreamPrefix returns the object-listing prefix covering everything under a
// stream. The trailing slash matters: without it, prefix "chan/123" also
// matches sibling streams like "chan/1234" (prefix aliasing), so existence
// probes and folder deletes would hit the wrong stream.
func StreamPrefix(channel, stream string) string {
	return fmt.Sprintf("%s/%s/", channel, stream)
}

// RawPrefix returns the object-listing prefix covering a stream's raw audio
// chunks. See StreamPrefix for why the trailing slash is required.
func RawPrefix(channel, stream string) string {
	return fmt.Sprintf("%s/%s/raw/", channel, stream)
}
