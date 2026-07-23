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
	TwitchLogin string `yaml:"twitchLogin"`
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
	WebhookURL   string `yaml:"webhookUrl"`
	NotifyUserID string `yaml:"notifyUserId"`
	NotifyRoleID string `yaml:"notifyRoleId"`
	// TranscriptBaseURL is the base URL for transcript links in stream-start
	// notifications, e.g. "https://www.duck-automata.com/live-transcript".
	// If empty, a default is derived from the server version (dev vs prod).
	TranscriptBaseURL string           `yaml:"transcriptBaseUrl"`
	Bot               DiscordBotConfig `yaml:"bot"`
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
	ArchiveURL string          `yaml:"archiveUrl"`
	ArchiveKey string          `yaml:"archiveKey"`
	Database   DatabaseConfig  `yaml:"database"`
	Storage    StorageConfig   `yaml:"storage"`
	Channels   []ChannelConfig `yaml:"channels"`
	Discord    DiscordConfig   `yaml:"discord"`
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
