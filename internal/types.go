package internal

import (
	"fmt"
	"net/http"
	"path/filepath"
	"sync"

	"github.com/gorilla/websocket"
)

type Segments struct {
	Timestamp int    `json:"timestamp"`
	Text      string `json:"text"`
}
type Line struct {
	ID        int        `json:"id"`
	Timestamp int        `json:"timestamp"`
	Segments  []Segments `json:"segments"`
}
type ClientData struct {
	ActiveID    string `json:"activeId"`
	ActiveTitle string `json:"activeTitle"`
	StartTime   string `json:"startTime"`
	IsLive      bool   `json:"isLive"`
	MediaType   string `json:"mediaType"`
	Transcript  []Line `json:"transcript"`
}

type UpdateData struct {
	NewLine    Line   `json:"line"`
	RawB64Data string `json:"rawB64Data"`
}

type HardRefreshData struct {
	Event string      `json:"event"`
	Data  *ClientData `json:"clientData"`
}

type GobArchive struct {
	fileName string
}

type WebSocketServer struct {
	key               string
	username          string
	password          string
	streamLock        sync.Mutex
	transcriptLock    sync.Mutex
	clientsLock       sync.Mutex
	upgrader          websocket.Upgrader
	archive           *GobArchive
	clientData        *ClientData
	clients           []*websocket.Conn
	maxConn           int
	clientConnections int
	maxClipSize       int
	mediaFolder       string
}

func NewGobArchive(filename string) *GobArchive {
	return &GobArchive{
		fileName: filename,
	}
}

func NewClientData() *ClientData {
	return &ClientData{
		ActiveID:    "",
		ActiveTitle: "",
		StartTime:   "",
		MediaType:   "none",
		IsLive:      false,
		Transcript:  make([]Line, 0),
	}
}

func NewWebSocketServer(key string, username string, password string) *WebSocketServer {
	return &WebSocketServer{
		key:      key,
		username: username,
		password: password,
		upgrader: websocket.Upgrader{
			ReadBufferSize:    1024,
			WriteBufferSize:   1024,
			EnableCompression: true,
			CheckOrigin:       func(r *http.Request) bool { return true },
		},
		archive:           NewGobArchive(filepath.Join("tmp", key, fmt.Sprintf("%s.gob", key))),
		clients:           make([]*websocket.Conn, 0, 1000),
		clientData:        NewClientData(),
		maxConn:           1000,
		clientConnections: 0,
		maxClipSize:       30,
		mediaFolder:       filepath.Join("tmp", key, "media"),
	}
}
