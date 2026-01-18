package internal

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"sync"

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
	ActiveID    string `json:"activeId"`
	ActiveTitle string `json:"activeTitle"`
	StartTime   string `json:"startTime"`
	IsLive      bool   `json:"isLive"`
	MediaType   string `json:"mediaType"`
	Transcript  []Line `json:"transcript"`
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
	ActiveID    string `json:"activeId"`
	ActiveTitle string `json:"activeTitle"`
	StartTime   string `json:"startTime"`
	MediaType   string `json:"mediaType"`
	IsLive      bool   `json:"isLive"`
}

// EventStatusData represents the data sent to notify the client of a change in the stream status.
type EventStatusData struct {
	ActiveID    string `json:"activeId"`
	ActiveTitle string `json:"activeTitle"`
	IsLive      bool   `json:"isLive"`
}

// EventNewMediaData represents the data sent to notify the client of available media.
type EventNewMediaData struct {
	AvailableIDs []int `json:"ids"`
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
}

func NewApp(apiKey string, db *sql.DB, channelsConfig []ChannelConfig, tempDir string) *App {
	app := &App{
		ApiKey: apiKey,
		DB:     db,
		Upgrader: websocket.Upgrader{
			ReadBufferSize:    1024,
			WriteBufferSize:   1024,
			EnableCompression: true,
			CheckOrigin:       func(r *http.Request) bool { return true },
		},
		Channels:    make(map[string]*ChannelState),
		MaxConn:     10_000, // through testing, assuming a steady flow of connections, 10k connections will use 200 millicores
		MaxClipSize: 30,
		TempDir:     tempDir,
	}

	for _, config := range channelsConfig {
		baseMediaFolder := filepath.Join(app.TempDir, config.Name)
		app.Channels[config.Name] = &ChannelState{
			Key:             config.Name,
			Clients:         make([]*Client, 0, 1000),
			BaseMediaFolder: baseMediaFolder,
			// ActiveMediaFolder will be set on stream activation
			ClientConnections: 0,
			NumPastStreams:    config.NumPastStreams,
		}
	}

	return app
}
