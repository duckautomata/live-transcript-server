// Package ws implements the per-channel realtime fan-out hub for WebSocket
// clients: connection accounting, broadcast, and the read/write pumps.
package ws

import (
	"encoding/json"

	"live-transcript-server/internal/model"
)

// EventType identifies the kind of message sent over the WebSocket connection.
type EventType string

const (
	EventNewLine       EventType = "newLine"
	EventNewStream     EventType = "newStream"
	EventStatus        EventType = "status"
	EventSync          EventType = "sync"
	EventPartialSync   EventType = "partialSync"
	EventNewMedia      EventType = "newMedia"
	EventPing          EventType = "ping"
	EventPong          EventType = "pong"
	EventPastStreams   EventType = "pastStreams"
	EventDeletedStream EventType = "deletedStream"
)

// Message represents a message sent over the WebSocket connection.
type Message struct {
	Event EventType `json:"event"`
	Data  any       `json:"data"`
}

// EventSyncData represents the data sent to sync the client with the server.
type EventSyncData struct {
	StreamID     string       `json:"streamId"`
	StreamTitle  string       `json:"streamTitle"`
	StartTime    string       `json:"startTime"`
	IsLive       bool         `json:"isLive"`
	MediaType    string       `json:"mediaType"`
	MediaBaseURL string       `json:"mediaBaseUrl"`
	Transcript   []model.Line `json:"transcript"`
}

// EventNewLineData represents the data sent to notify the client of a new line in the transcript.
type EventNewLineData struct {
	LineID         int             `json:"lineId"`
	Timestamp      int             `json:"timestamp"`
	UploadTime     int64           `json:"uploadTime"`
	MediaAvailable bool            `json:"mediaAvailable"`
	Segments       json.RawMessage `json:"segments"`
	VodAccurate    bool            `json:"vodAccurate"`
}

// EventNewStreamData represents the data sent to notify the client of a new stream.
type EventNewStreamData struct {
	StreamID     string `json:"streamId"`
	StreamTitle  string `json:"streamTitle"`
	StartTime    string `json:"startTime"`
	MediaType    string `json:"mediaType"`
	MediaBaseURL string `json:"mediaBaseUrl"`
	IsLive       bool   `json:"isLive"`
}

// EventStatusData represents the data sent to notify the client of a change in the stream status.
type EventStatusData struct {
	StreamID    string `json:"streamId"`
	StreamTitle string `json:"streamTitle"`
	IsLive      bool   `json:"isLive"`
}

// EventNewMediaData represents the data sent to notify the client of available media.
type EventNewMediaData struct {
	StreamID string         `json:"streamId"`
	Files    map[int]string `json:"files"` // LineID -> FileID
}

// EventPingPongData represents the data sent when the client pings the server.
type EventPingPongData struct {
	Timestamp int `json:"timestamp"`
}

// EventPastStreamsData represents the data sent to notify the client of past streams.
type EventPastStreamsData struct {
	Streams []model.Stream `json:"streams"`
}

// EventDeletedStreamData notifies clients that a stream has been removed
// (typically by an admin via the admin UI). Clients should drop the stream
// from their local state and refresh past-stream lists.
type EventDeletedStreamData struct {
	StreamID    string `json:"streamId"`
	StreamTitle string `json:"streamTitle"`
	WasLive     bool   `json:"wasLive"`
}
