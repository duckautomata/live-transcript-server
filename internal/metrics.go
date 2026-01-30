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
		Help: "The duration of connections in seconds.",
		Buckets: []float64{
			1, 5, 10, 30, // seconds
			60, 300, 600, 1800, // minutes
			3600, 5400, 7200, 14400, // hours
		},
	})
	MessagesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "lt_messages_total",
		Help: "The total number of messages.",
	})
	MessageSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "lt_message_size_bytes",
		Help: "The size of messages in bytes.",
		Buckets: []float64{
			4, 16, 32, 64,
			128, 256, 512,
			1024, 8192, 32768, 1048576,
		},
	})
	MessageProcessingDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "lt_message_processing_duration_seconds",
		Help: "The duration of message processing.",
	})
	MediaProcessingDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "lt_media_processing_duration_seconds",
		Help: "The duration of media processing steps in seconds.",
		Buckets: []float64{
			0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10,
		},
	},
		[]string{"step", "key"},
	)
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
	StreamAudioPlayed = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lt_stream_audio_played_per_key",
		Help: "The number of successful calls to the /audio endpoint in a given stream period.",
	},
		[]string{"key"},
	)
	TotalVideoPlayed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "lt_total_video_played_per_key",
		Help: "The total number of successful calls to the /video endpoint.",
	},
		[]string{"key"},
	)
	StreamVideoPlayed = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lt_stream_video_played_per_key",
		Help: "The number of successful calls to the /video endpoint in a given stream period.",
	},
		[]string{"key"},
	)
	TotalFramesDownloads = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "lt_total_frame_downloads_per_key",
		Help: "The total number of successful calls to the /frame endpoint.",
	},
		[]string{"key"},
	)
	StreamFramesDownloads = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lt_stream_frame_downloads_per_key",
		Help: "The number of successful calls to the /frame endpoint in a given stream period.",
	},
		[]string{"key"},
	)
	TotalAudioClipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "lt_total_audio_clipped_per_key",
		Help: "The total number of successful audio clips created.",
	},
		[]string{"key"},
	)
	StreamAudioClipped = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lt_stream_audio_clipped_per_key",
		Help: "The total number of successful audio clips created in a given stream period.",
	},
		[]string{"key"},
	)
	TotalVideoClipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "lt_total_video_clipped_per_key",
		Help: "The total number of successful video clips created.",
	},
		[]string{"key"},
	)
	StreamVideoClipped = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lt_stream_video_clipped_per_key",
		Help: "The total number of successful video clips created in a given stream period.",
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
