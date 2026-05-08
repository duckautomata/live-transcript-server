package internal

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type ChannelConfig struct {
	Name           string `yaml:"name"`
	NumPastStreams int    `yaml:"numPastStreams"`
	AdminKey       string `yaml:"adminKey"`
}

type R2Config struct {
	AccountId       string `yaml:"accountId"`
	AccessKeyId     string `yaml:"accessKeyId"`
	SecretAccessKey string `yaml:"secretAccessKey"`
	Bucket          string `yaml:"bucket"`
	PublicUrl       string `yaml:"publicUrl"`
}

type StorageConfig struct {
	Type string   `yaml:"type"` // "local" or "r2"
	R2   R2Config `yaml:"r2"`
}

type DiscordBotConfig struct {
	Token            string            `yaml:"token"`
	ChannelIDs       []string          `yaml:"channelIds"`
	ChannelMap       map[string]string `yaml:"channelMap"`
	StreamTTLMinutes int               `yaml:"streamTtlMinutes"`
}

type DiscordConfig struct {
	WebhookURL   string           `yaml:"webhookUrl"`
	NotifyUserID string           `yaml:"notifyUserId"`
	NotifyRoleID string           `yaml:"notifyRoleId"`
	Bot          DiscordBotConfig `yaml:"bot"`
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

type Config struct {
	Credentials struct {
		ApiKey string `yaml:"apiKey"`
	} `yaml:"credentials"`
	Database DatabaseConfig  `yaml:"database"`
	Storage  StorageConfig   `yaml:"storage"`
	Channels []ChannelConfig `yaml:"channels"`
	Discord  DiscordConfig   `yaml:"discord"`
}

func GetConfig() (Config, error) {
	configFile := "config.yaml"
	data, err := os.ReadFile(configFile)
	if err != nil {
		return Config{}, fmt.Errorf("unable to read yaml file: %v", err)
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return Config{}, fmt.Errorf("unable to unmarshal yaml: %v", err)
	}

	return config, nil
}
