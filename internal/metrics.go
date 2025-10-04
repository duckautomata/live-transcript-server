package internal

import (
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Server Based
	ActiveConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "lt_active_connections",
		Help: "The current number of active connections.",
	})
	TotalConnections = promauto.NewCounter(prometheus.CounterOpts{
		Name: "lt_total_connections",
		Help: "The total number of connections.",
	})
	ConnectionDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "lt_connection_duration_seconds",
		Help: "The duration of connections.",
	})
	MessagesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "lt_messages_total",
		Help: "The total number of messages.",
	})
	MessageSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "lt_message_size_bytes",
		Help: "The size of messages in bytes.",
	})
	MessageProcessingDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "lt_message_processing_duration_seconds",
		Help: "The duration of message processing.",
	})
	ServerOOS = promauto.NewCounter(prometheus.CounterOpts{
		Name: "lt_server_oos",
		Help: "The total number of times the server was out-of-sync with the client.",
	})
	WebsocketError = promauto.NewCounter(prometheus.CounterOpts{
		Name: "lt_websocket_errors",
		Help: "The total number of errors for the Websocket.",
	})
	Http400Errors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "lt_400_errors",
		Help: "The total number of HTTP 4xx client errors.",
	})
	Http500Errors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "lt_500_errors",
		Help: "The total number of HTTP 5xx server errors.",
	})
	MemoryUsage = promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "lt_memory_usage_bytes",
		Help: "The current memory usage.",
	},
		func() float64 {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			return float64(m.Alloc)
		},
	)

	// Key Based
	ClientsPerKey = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lt_clients_per_key",
		Help: "The number of clients per key.",
	},
		[]string{"key"},
	)
	TotalAudioPlayed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "lt_total_audio_played_per_key",
		Help: "The total number of successful calls to the /audio endpoint.",
	},
		[]string{"key"},
	)
	TotalAudioClipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "lt_total_audio_clipped_per_key",
		Help: "The total number of successful audio clips created.",
	},
		[]string{"key"},
	)
	TotalVideoClipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "lt_total_video_clipped_per_key",
		Help: "The total number of successful video clips created.",
	},
		[]string{"key"},
	)
	ActivatedStreams = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lt_activated_streams_per_key",
		Help: "Details of the currently active stream per key, with the value as the start timestamp.",
	},
		[]string{"key", "stream_id", "stream_title"},
	)
)
