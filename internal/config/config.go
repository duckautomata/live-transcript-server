// Package config defines the server's YAML configuration schema and loader.
// It is a leaf package shared by every binary in cmd/ so the schema is defined
// exactly once.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type ChannelConfig struct {
	Name           string `yaml:"name"`
	NumPastStreams int    `yaml:"numPastStreams"`
	AdminKey       string `yaml:"adminKey"`
	// MembersName is the archive server's channel name for this channel's
	// membership keys. Empty means the membership-key admin section is
	// disabled for this channel.
	MembersName string `yaml:"membersName"`
	// DisplayName is the human-readable channel name used in Discord
	// notifications (e.g. "Dokibird" for channel "doki"). Defaults to Name.
	DisplayName string `yaml:"displayName"`
	// TwitchLogin is the channel's Twitch login used to build stream links
	// for Twitch streams. Defaults to lowercase DisplayName.
	//
	// It doubles as the Twitch live-detection target, and detection requires
	// it to be set EXPLICITLY: Helix answers an unknown login with an empty
	// data array rather than an error, so a wrong lowercase-displayName guess
	// reads as "permanently offline" and never surfaces as a failure.
	TwitchLogin string `yaml:"twitchLogin"`
	// YouTubeChannelId is the channel's "UC..." YouTube channel ID, used only
	// by live detection. Empty disables YouTube detection for this channel.
	// It cannot be derived from any other field, so it is resolved by hand at
	// config time and validated at boot rather than at runtime, where a
	// failure would be silent and permanent.
	YouTubeChannelId string `yaml:"youtubeChannelId"`
}

type R2Config struct {
	AccountId       string `yaml:"accountId"`
	AccessKeyId     string `yaml:"accessKeyId"`
	SecretAccessKey string `yaml:"secretAccessKey"`
	Bucket          string `yaml:"bucket"`
	PublicUrl       string `yaml:"publicUrl"`
}

type StorageConfig struct {
	Type string   `yaml:"type"` // "local" or "r2"; empty defaults to "local"
	R2   R2Config `yaml:"r2"`
}

type DiscordBotConfig struct {
	Token            string            `yaml:"token"`
	ChannelIDs       []string          `yaml:"channelIds"`
	ChannelMap       map[string]string `yaml:"channelMap"`
	StreamTTLMinutes int               `yaml:"streamTtlMinutes"`
}

type DiscordConfig struct {
	WebhookURL string `yaml:"webhookUrl"`
	// AdminWebhookURL receives the audit record of every admin operation. Set
	// it to route the (chatty, low-urgency) admin log to its own channel;
	// empty falls back to WebhookURL, and admin audit posts are disabled only
	// when both are empty.
	AdminWebhookURL string `yaml:"adminWebhookUrl"`
	// DetectWebhookURL receives live-detection observations. A shadow-mode
	// soak posts one of these per broadcast per channel, so routing them to
	// their own channel keeps the operator alert feed readable. Empty falls
	// back to AdminWebhookURL (which itself falls back to WebhookURL).
	DetectWebhookURL string `yaml:"detectWebhookUrl"`
	NotifyUserID     string `yaml:"notifyUserId"`
	NotifyRoleID     string `yaml:"notifyRoleId"`
	// TranscriptBaseURL is the base URL for transcript links in stream-start
	// notifications, e.g. "https://www.duck-automata.com/live-transcript".
	// If empty, a default is derived from the server version (dev vs prod).
	TranscriptBaseURL string           `yaml:"transcriptBaseUrl"`
	Bot               DiscordBotConfig `yaml:"bot"`
}

// LiveDetectTwitchConfig configures Twitch live detection.
//
// EventSub is the seconds-latency path and polling is the safety net beneath
// it, not an alternative to it: Helix responses are edge-cached, so polling
// detects on the order of a minute no matter how fast it runs. Both legs feed
// the same ledger, so running them together costs one extra notification's
// worth of nothing and tells you which one won.
type LiveDetectTwitchConfig struct {
	Enabled      bool   `yaml:"enabled"`
	ClientId     string `yaml:"clientId"`
	ClientSecret string `yaml:"clientSecret"`
	// EventSub enables the inbound webhook. Requires publicBaseUrl and
	// eventSubSecret. Disabling it leaves polling as the only Twitch leg,
	// which downgrades latency from seconds to about a minute.
	EventSub bool `yaml:"eventSub"`
	// EventSubSecret signs EventSub callbacks. Twitch requires 10-100 ASCII
	// characters and never returns it on a read, so changing it invalidates
	// every existing subscription and forces them to be recreated.
	// Generate with: openssl rand -hex 32
	EventSubSecret string `yaml:"eventSubSecret"`
	// PollSeconds is the Helix polling cadence. Values below 60 are clamped:
	// Twitch caches API responses per edge server, so polling faster returns
	// inconsistent results (including a phantom stream-id change that reads as
	// a restart) without detecting anything sooner.
	PollSeconds int `yaml:"pollSeconds"`
}

// LiveDetectYouTubeConfig configures YouTube live detection.
//
// Only API-key-authenticated endpoints are used. Unauthenticated youtube.com
// surfaces (InnerTube, the RSS feed, HTML scraping) are deliberately absent:
// from a datacenter IP their failure mode is reputational drift over days,
// which no health check catches in useful time, and at this channel count they
// would save a few hundred quota units out of ten thousand.
type LiveDetectYouTubeConfig struct {
	Enabled bool   `yaml:"enabled"`
	ApiKey  string `yaml:"apiKey"`
	// WebSub enables push discovery through Google's PubSubHubbub hub.
	// Requires publicBaseUrl. The push carries no live state - it only hands
	// us a video id early, which is what lets the state poller catch the
	// go-live within seconds instead of waiting for a discovery pass.
	WebSub bool `yaml:"webSub"`
	// WebSubSecret is the base secret for per-topic HMAC keys.
	// Generate with: openssl rand -hex 32
	WebSubSecret string `yaml:"webSubSecret"`
	// DiscoverySeconds is how often the uploads playlist is scanned for video
	// ids we have not seen. This is the latency floor for an UNSCHEDULED
	// surprise go-live; scheduled streams and premieres are already on the
	// watchlist and are caught by the state poller in seconds.
	DiscoverySeconds int `yaml:"discoverySeconds"`
	// DailyUnitBudget caps quota spend, leaving headroom under the API's
	// 10,000/day project allocation for retries and restarts.
	DailyUnitBudget int `yaml:"dailyUnitBudget"`
	// SearchAudit runs a low-rate search.list cross-check that alarms when
	// YouTube says a channel is live and the ledger disagrees. It draws on
	// search.list's own 100-calls-per-day bucket, separate from the units.
	SearchAudit bool `yaml:"searchAudit"`
}

// LiveDetectConfig configures server-side live detection.
//
// Detection observes YouTube and Twitch for configured channels going live,
// announces what it sees through the admin-configured notification events,
// and - only when QueueIncoming is set - queues the stream for the worker.
// Without QueueIncoming it is an observer: it never queues work and never
// touches the streams table, which is how a new deployment proves detection
// trustworthy before the worker is allowed to depend on it.
type LiveDetectConfig struct {
	Enabled bool `yaml:"enabled"`
	// QueueIncoming makes a detected live broadcast queue its URL for the
	// worker, exactly as a Pingcord announcement would. Off by default so an
	// existing shadow-mode deployment keeps observing until the operator has
	// read the soak results and opts in.
	QueueIncoming bool `yaml:"queueIncoming"`
	// PublicBaseURL is this server's externally reachable base URL, e.g.
	// "https://api.example.com". Required for EventSub and WebSub, which
	// register an absolute callback with a third party and therefore cannot
	// derive it from an inbound request. No trailing slash.
	PublicBaseURL string `yaml:"publicBaseUrl"`
	// StaleAlertMinutes is how long a detection leg may go without a
	// successful poll before an alert fires.
	StaleAlertMinutes int                     `yaml:"staleAlertMinutes"`
	Twitch            LiveDetectTwitchConfig  `yaml:"twitch"`
	YouTube           LiveDetectYouTubeConfig `yaml:"youtube"`
}

type DatabaseConfig struct {
	JournalMode   string `yaml:"journal_mode"`
	BusyTimeoutMS int    `yaml:"busy_timeout_ms"`
	Synchronous   string `yaml:"synchronous"`
	CacheSizeKB   int    `yaml:"cache_size_kb"`
	TempStore     string `yaml:"temp_store"`
	MmapSizeBytes int64  `yaml:"mmap_size_bytes"`
	SkipWarmup    bool   `yaml:"skip_warmup"`
}

type Credentials struct {
	ApiKey string `yaml:"apiKey"`
}

type Config struct {
	Credentials Credentials `yaml:"credentials"`
	// ArchiveURL and ArchiveKey connect the admin page to the archive server
	// for membership-key management. Leave blank to disable the feature.
	ArchiveURL string           `yaml:"archiveUrl"`
	ArchiveKey string           `yaml:"archiveKey"`
	Database   DatabaseConfig   `yaml:"database"`
	Storage    StorageConfig    `yaml:"storage"`
	Channels   []ChannelConfig  `yaml:"channels"`
	LiveDetect LiveDetectConfig `yaml:"liveDetect"`
	Discord    DiscordConfig    `yaml:"discord"`
}

// Load reads and validates the configuration at path.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("unable to read config file %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("unable to unmarshal %s: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate rejects configurations that would previously fail silently at
// runtime (e.g. a typoed storage type falling back to local storage).
func (c Config) Validate() error {
	switch c.Storage.Type {
	case "", "local", "r2":
	default:
		return fmt.Errorf("storage.type must be \"local\" or \"r2\", got %q", c.Storage.Type)
	}
	seen := make(map[string]bool, len(c.Channels))
	for _, ch := range c.Channels {
		if ch.Name == "" {
			return fmt.Errorf("channels[].name must not be empty")
		}
		if seen[ch.Name] {
			return fmt.Errorf("duplicate channel name %q", ch.Name)
		}
		seen[ch.Name] = true
	}
	return nil
}
