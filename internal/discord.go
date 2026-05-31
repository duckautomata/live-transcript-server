package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type DiscordClient struct {
	WebhookURL string
	NotifyPing string
	Disabled   bool
	Version    string
}

func NewDiscordClient(cfg DiscordConfig, version string) *DiscordClient {
	var ping string
	if cfg.NotifyUserID != "" {
		ping = fmt.Sprintf("<@%s>", cfg.NotifyUserID)
	} else if cfg.NotifyRoleID != "" {
		ping = fmt.Sprintf("<@&%s>", cfg.NotifyRoleID)
	}

	return &DiscordClient{
		WebhookURL: cfg.WebhookURL,
		NotifyPing: ping,
		Version:    version,
	}
}

// send sends the actual payload to the discord webhook.
func (d *DiscordClient) send(payload map[string]any) {
	if d.WebhookURL == "" {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("failed to marshal discord payload", "err", err)
		return
	}

	req, err := http.NewRequest("POST", d.WebhookURL, bytes.NewBuffer(body))
	if err != nil {
		slog.Error("failed to create discord request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Error("failed to send discord notification", "err", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Error("discord webhook returned error", "status", resp.Status)
	}
}

func (d *DiscordClient) NotifyStreamStart(channelKey, streamID, streamTitle, startTime string) {
	if d.Disabled || d.WebhookURL == "" {
		return
	}

	// Convert Unix timestamp to RFC3339 for Discord embed timestamp
	timestampStr := time.Now().Format(time.RFC3339)
	if parsedTime, err := strconv.ParseInt(startTime, 10, 64); err == nil {
		timestampStr = time.Unix(parsedTime, 0).Format(time.RFC3339)
	}

	var streamLink string
	var imageUrl string
	var fullName string

	switch channelKey {
	case "doki":
		fullName = "Dokibird"
	case "mint":
		fullName = "Mint Fantôme"
	default:
		fullName = channelKey
	}

	isTwitchStream := len(streamID) > 0
	for _, char := range streamID {
		if !unicode.IsDigit(char) {
			isTwitchStream = false
			break
		}
	}

	if isTwitchStream {
		streamLink = fmt.Sprintf("https://twitch.tv/%s", strings.ToLower(fullName))
		imageUrl = fmt.Sprintf("https://static-cdn.jtvnw.net/previews-ttv/live_user_%s-1280x720.jpg", strings.ToLower(fullName))
	} else {
		streamLink = fmt.Sprintf("https://www.youtube.com/watch?v=%s", streamID)
		imageUrl = fmt.Sprintf("https://i.ytimg.com/vi/%s/maxresdefault.jpg", streamID)
	}

	domain := "www.duck-automata.com"
	if d.Version == "dev" {
		domain = "dev.duck-automata.com"
	}
	transcriptLink := fmt.Sprintf("https://%s/live-transcript/%s/", domain, channelKey)

	embed := map[string]any{
		"title":       fmt.Sprintf("%s's Stream Started", fullName),
		"description": fmt.Sprintf("**%s**\n\n[Stream Link](%s) | [Transcript](%s)", streamTitle, streamLink, transcriptLink),
		"url":         streamLink, // Embed Title Link
		"color":       3066993,    // Green
		"image": map[string]string{
			"url": imageUrl,
		},
		"timestamp": timestampStr,
		"footer": map[string]string{
			"text": fmt.Sprintf("Version: %s", d.Version),
		},
	}

	payload := map[string]any{
		"embeds": []map[string]any{embed},
	}
	go d.send(payload)
}

func (d *DiscordClient) NotifyWorkerOffline(channelKey string, lastSeen int64) {
	if d.WebhookURL == "" {
		return
	}
	timeAgo := time.Since(time.Unix(lastSeen, 0)).Round(time.Second).String()
	payload := map[string]any{
		"content": d.NotifyPing,
		"embeds": []map[string]any{
			{
				"title":       "Worker Offline Alert",
				"description": fmt.Sprintf("Worker for channel **%s** has been inactive.\nLast seen: %s ago", channelKey, timeAgo),
				"color":       15158332, // Red
				"timestamp":   time.Now().Format(time.RFC3339),
				"footer": map[string]string{
					"text": fmt.Sprintf("Version: %s", d.Version),
				},
			},
		},
	}
	go d.send(payload)
}

// NotifyDiscordBotOffline alerts that the Discord listener bot's gateway has
// gone stale (stopped receiving updates). lastSeen is the Unix time of the last
// heartbeat ACK. Mirrors NotifyWorkerOffline so it pings the same operator.
func (d *DiscordClient) NotifyDiscordBotOffline(lastSeen int64) {
	if d.WebhookURL == "" {
		return
	}
	timeAgo := time.Since(time.Unix(lastSeen, 0)).Round(time.Second).String()
	payload := map[string]any{
		"content": d.NotifyPing,
		"embeds": []map[string]any{
			{
				"title":       "Discord Bot Offline Alert",
				"description": fmt.Sprintf("The Discord listener bot has stopped receiving gateway updates and may miss stream starts.\nLast heartbeat: %s ago.\nForcing a reconnect.", timeAgo),
				"color":       15158332, // Red
				"timestamp":   time.Now().Format(time.RFC3339),
				"footer": map[string]string{
					"text": fmt.Sprintf("Version: %s", d.Version),
				},
			},
		},
	}
	go d.send(payload)
}

// NotifyDiscordBotRecovered announces that the listener bot's gateway is
// receiving updates again, sent once after a prior offline alert.
func (d *DiscordClient) NotifyDiscordBotRecovered() {
	if d.WebhookURL == "" {
		return
	}
	payload := map[string]any{
		"content": d.NotifyPing,
		"embeds": []map[string]any{
			{
				"title":       "Discord Bot Recovered",
				"description": "The Discord listener bot is receiving gateway updates again.",
				"color":       3066993, // Green
				"timestamp":   time.Now().Format(time.RFC3339),
				"footer": map[string]string{
					"text": fmt.Sprintf("Version: %s", d.Version),
				},
			},
		},
	}
	go d.send(payload)
}

// NotifyDiscordBotStartupError alerts that the Discord listener bot failed its
// initial gateway connection at startup (e.g. a transient Discord API timeout).
// The watchdog keeps retrying, so a recovery ping follows once it connects.
func (d *DiscordClient) NotifyDiscordBotStartupError(err error) {
	if d.WebhookURL == "" {
		return
	}
	payload := map[string]any{
		"content": d.NotifyPing,
		"embeds": []map[string]any{
			{
				"title":       "Discord Bot Failed to Start",
				"description": fmt.Sprintf("The Discord listener bot could not connect to the gateway at startup and is not receiving stream announcements.\n**Error:** %v\nRetrying automatically.", err),
				"color":       15158332, // Red
				"timestamp":   time.Now().Format(time.RFC3339),
				"footer": map[string]string{
					"text": fmt.Sprintf("Version: %s", d.Version),
				},
			},
		},
	}
	go d.send(payload)
}

func (d *DiscordClient) Notify500Error(err error, contextMsg string) {
	if d.Disabled || d.WebhookURL == "" {
		return
	}
	payload := map[string]any{
		"content": d.NotifyPing,
		"embeds": []map[string]any{
			{
				"title":       "500 Internal Server Error",
				"description": fmt.Sprintf("**Context:** %s\n**Error:** %v", contextMsg, err),
				"color":       16744448, // Orange
				"timestamp":   time.Now().Format(time.RFC3339),
				"footer": map[string]string{
					"text": fmt.Sprintf("Version: %s", d.Version),
				},
			},
		},
	}
	go d.send(payload)
}
