package internal

import (
	"database/sql"
	"encoding/json"
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
	ID             int             `json:"id"`
	FileID         string          `json:"fileId"`
	Timestamp      int             `json:"timestamp"`
	Segments       json.RawMessage `json:"segments"`
	MediaAvailable bool            `json:"mediaAvailable"`
}

// Stream represents the state of a stream for a channel in the database.
type Stream struct {
	ChannelID     string `json:"channelId"`
	StreamID      string `json:"streamId"`
	StreamTitle   string `json:"streamTitle"`
	StartTime     string `json:"startTime"`
	IsLive        bool   `json:"isLive"`
	MediaType     string `json:"mediaType"`
	ActivatedTime int64  `json:"activatedTime"`
}

// WorkerData represents the full state of the worker. Used to sync the server with the worker.
type WorkerData struct {
	StreamID    string `json:"streamId"`
	StreamTitle string `json:"streamTitle"`
	StartTime   string `json:"startTime"`
	IsLive      bool   `json:"isLive"`
	MediaType   string `json:"mediaType"`
	Transcript  []Line `json:"transcript"`
}

// WorkerStatus represents the status of a worker for a specific key.
type WorkerStatus struct {
	ChannelKey      string `json:"channelKey"`
	WorkerVersion   string `json:"workerVersion"`
	WorkerBuildTime string `json:"workerBuildTime"`
	LastSeen        int64  `json:"lastSeen"`
	IsActive        bool   `json:"isActive"` // Computed field
}

type WorkerStatusRequest struct {
	Version   string   `json:"version"`
	BuildTime string   `json:"build_time"`
	Keys      []string `json:"keys"`
}

// ServerInfo represents the version information of the server.
type ServerInfo struct {
	Version   string `json:"version"`
	BuildTime string `json:"buildTime"`
}

// FullInfoResponse represents the response for the /info endpoint.
type FullInfoResponse struct {
	Server  ServerInfo     `json:"server"`
	Workers []WorkerStatus `json:"workers"`
}

// ===== Client Events =====

type EventType string

const (
	EventNewLine     EventType = "newLine"
	EventNewStream   EventType = "newStream"
	EventStatus      EventType = "status"
	EventSync        EventType = "sync"
	EventPartialSync EventType = "partialSync"
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
	StreamID     string `json:"streamId"`
	StreamTitle  string `json:"streamTitle"`
	StartTime    string `json:"startTime"`
	IsLive       bool   `json:"isLive"`
	MediaType    string `json:"mediaType"`
	MediaBaseURL string `json:"mediaBaseUrl"`
	Transcript   []Line `json:"transcript"`
}

// EventNewLineData represents the data sent to notify the client of a new line in the transcript.
type EventNewLineData struct {
	LineID         int             `json:"lineId"`
	Timestamp      int             `json:"timestamp"`
	UploadTime     int64           `json:"uploadTime"`
	MediaAvailable bool            `json:"mediaAvailable"`
	Segments       json.RawMessage `json:"segments"`
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
	Streams []Stream `json:"streams"`
}

// ===== Server State =====

// ChannelState holds the state and connections for a specific channel.
type ChannelState struct {
	Key               string
	ClientsLock       sync.Mutex
	Clients           []*Client
	ClientConnections int
	ActiveMediaFolder string
	BaseMediaFolder   string
	NumPastStreams    int
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
	Version     string
	BuildTime   string
}
