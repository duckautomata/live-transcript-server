// Package model holds the shared data types exchanged between the worker,
// the database, and web clients. It is a leaf package: it may not import any
// other package from this module.
package model

import "encoding/json"

// Line is a single transcript line.
type Line struct {
	ID             int             `json:"id"`
	FileID         string          `json:"fileId"`
	Timestamp      int             `json:"timestamp"`
	Segments       json.RawMessage `json:"segments"`
	MediaAvailable bool            `json:"mediaAvailable"`
	VodAccurate    bool            `json:"vodAccurate"`
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

// WorkerData represents the full state of the worker. Used to sync the server
// with the worker.
type WorkerData struct {
	StreamID    string `json:"streamId"`
	StreamTitle string `json:"streamTitle"`
	StartTime   string `json:"startTime"`
	IsLive      bool   `json:"isLive"`
	MediaType   string `json:"mediaType"`
	Transcript  []Line `json:"transcript"`
}

// WorkerStatus represents the status of a worker for a specific channel key.
type WorkerStatus struct {
	ChannelKey      string `json:"channelKey"`
	WorkerVersion   string `json:"workerVersion"`
	WorkerBuildTime string `json:"workerBuildTime"`
	LastSeen        int64  `json:"lastSeen"`
	IsActive        bool   `json:"isActive"` // Computed field
}

// WorkerStatusRequest is the body of the worker's POST /status heartbeat.
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

// FullInfoResponse is the response for the public GET /status endpoint.
type FullInfoResponse struct {
	Server  ServerInfo     `json:"server"`
	Workers []WorkerStatus `json:"workers"`
}
