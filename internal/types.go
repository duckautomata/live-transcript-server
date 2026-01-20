package internal

import (
	"database/sql"
	"sync"

	"live-transcript-server/internal/storage"

	"github.com/gorilla/websocket"
)

// ===== Data Models =====

// Client is a middleman between the websocket connection and the hub.
type Client struct {
	conn *websocket.Conn
	send chan WebSocketMessage
}

type Segment struct {
	Timestamp int    `json:"timestamp"`
	Text      string `json:"text"`
}

type Line struct {
	ID             int       `json:"id"`
	FileID         string    `json:"fileId"`
	Timestamp      int       `json:"timestamp"`
	Segments       []Segment `json:"segments"`
	MediaAvailable bool      `json:"mediaAvailable"`
}

// Stream represents the state of a stream for a channel in the database.
type Stream struct {
	ChannelID   string `json:"channelId"`
	ActiveID    string `json:"activeId"`
	ActiveTitle string `json:"activeTitle"`
	StartTime   string `json:"startTime"`
	IsLive      bool   `json:"isLive"`
	MediaType   string `json:"mediaType"`
}

// WorkerData represents the full state of the worker. Used to sync the server with the worker.
type WorkerData struct {
	ActiveID    string `json:"activeId"`
	ActiveTitle string `json:"activeTitle"`
	StartTime   string `json:"startTime"`
	IsLive      bool   `json:"isLive"`
	MediaType   string `json:"mediaType"`
	Transcript  []Line `json:"transcript"`
}

// ===== Client Events =====

type EventType string

const (
	EventNewLine     EventType = "newLine"
	EventNewStream   EventType = "newStream"
	EventStatus      EventType = "status"
	EventSync        EventType = "sync"
	EventNewMedia    EventType = "newMedia"
	EventPing        EventType = "ping"
	EventPong        EventType = "pong"
	EventPastStreams EventType = "pastStreams"
)

// WebSocketMessage represents a message sent over the WebSocket connection.
type WebSocketMessage struct {
	Event EventType `json:"event"`
	Data  any       `json:"data"`
}

// EventSyncData represents the data sent to sync the client with the server.
type EventSyncData struct {
	ActiveID     string `json:"activeId"`
	ActiveTitle  string `json:"activeTitle"`
	StartTime    string `json:"startTime"`
	IsLive       bool   `json:"isLive"`
	MediaType    string `json:"mediaType"`
	MediaBaseURL string `json:"mediaBaseUrl"`
	Transcript   []Line `json:"transcript"`
}

// EventNewLineData represents the data sent to notify the client of a new line in the transcript.
type EventNewLineData struct {
	LineID         int       `json:"lineId"`
	Timestamp      int       `json:"timestamp"`
	UploadTime     int64     `json:"uploadTime"`
	MediaAvailable bool      `json:"mediaAvailable"`
	Segments       []Segment `json:"segments"`
}

// EventNewStreamData represents the data sent to notify the client of a new stream.
type EventNewStreamData struct {
	ActiveID     string `json:"activeId"`
	ActiveTitle  string `json:"activeTitle"`
	StartTime    string `json:"startTime"`
	MediaType    string `json:"mediaType"`
	MediaBaseURL string `json:"mediaBaseUrl"`
	IsLive       bool   `json:"isLive"`
}

// EventStatusData represents the data sent to notify the client of a change in the stream status.
type EventStatusData struct {
	ActiveID    string `json:"activeId"`
	ActiveTitle string `json:"activeTitle"`
	IsLive      bool   `json:"isLive"`
}

// EventNewMediaData represents the data sent to notify the client of available media.
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
	Streams []Stream `json:"streams"`
}

// ===== Server State =====

// ChannelState holds the state and connections for a specific channel.
type ChannelState struct {
	Key               string
	ClientsLock       sync.Mutex
	Clients           []*Client
	ClientConnections int
	// ActiveMediaFolder is the folder where the current active stream media is stored.
	// This changes when a new stream is activated.
	ActiveMediaFolder string
	// BaseMediaFolder is the root folder for this channel where all stream folders are created.
	BaseMediaFolder string
	NumPastStreams  int
}

// App holds the application-wide dependencies and configuration.
type App struct {
	ApiKey      string
	DB          *sql.DB
	Upgrader    websocket.Upgrader
	Channels    map[string]*ChannelState
	MaxConn     int
	MaxClipSize int
	TempDir     string
	Storage     storage.Storage
}
