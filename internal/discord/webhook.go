// Package discord holds the server's two Discord integrations: an outbound
// webhook notifier (Client) for operator alerts and stream announcements, and
// an inbound gateway listener (Bot) that queues Pingcord-announced streams.
package discord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"live-transcript-server/internal/config"
)

// notify500Throttle is the minimum gap between 500-error webhook alerts. An
// error storm would otherwise spawn one goroutine and one webhook POST per
// failed request; anything inside the window is dropped, not queued.
const notify500Throttle = 10 * time.Second

// presentation is how a channel is rendered in stream-start notifications.
type presentation struct {
	displayName string
	twitchLogin string
}

// Client sends operator notifications to a Discord webhook. All Notify*
// methods are fire-and-forget: they post from a goroutine and only log
// delivery failures. A zero WebhookURL disables every notification.
type Client struct {
	WebhookURL string
	NotifyPing string
	Version    string

	transcriptBaseURL string
	channels          map[string]presentation
	httpClient        *http.Client

	// mu guards last500Alert, the Notify500Error throttle timestamp.
	mu           sync.Mutex
	last500Alert time.Time
}

// NewClient constructs a webhook client from config. The ping mention prefers
// a user over a role when both are configured. Per-channel presentation comes
// from the channel configs: DisplayName defaults to the channel key and
// TwitchLogin defaults to the lowercased display name.
func NewClient(cfg config.DiscordConfig, version string, channels []config.ChannelConfig) *Client {
	var ping string
	if cfg.NotifyUserID != "" {
		ping = fmt.Sprintf("<@%s>", cfg.NotifyUserID)
	} else if cfg.NotifyRoleID != "" {
		ping = fmt.Sprintf("<@&%s>", cfg.NotifyRoleID)
	}

	chans := make(map[string]presentation, len(channels))
	for _, cc := range channels {
		p := presentation{displayName: cc.DisplayName, twitchLogin: cc.TwitchLogin}
		if p.displayName == "" {
			p.displayName = cc.Name
		}
		if p.twitchLogin == "" {
			p.twitchLogin = strings.ToLower(p.displayName)
		}
		chans[cc.Name] = p
	}

	return &Client{
		WebhookURL:        cfg.WebhookURL,
		NotifyPing:        ping,
		Version:           version,
		transcriptBaseURL: cfg.TranscriptBaseURL,
		channels:          chans,
		httpClient:        &http.Client{Timeout: 10 * time.Second},
	}
}

// send sends the actual payload to the discord webhook.
func (d *Client) send(payload map[string]any) {
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

	resp, err := d.httpClient.Do(req)
	if err != nil {
		slog.Error("failed to send discord notification", "err", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Error("discord webhook returned error", "status", resp.Status)
	}
}

// NotifyStreamStart announces a stream going live. An all-digit streamID is
// treated as a Twitch stream (mediaType does not encode the platform), any
// other value as a YouTube video ID.
func (d *Client) NotifyStreamStart(channelKey, streamID, streamTitle, startTime string) {
	if d.WebhookURL == "" {
		return
	}

	// Convert Unix timestamp to RFC3339 for Discord embed timestamp
	timestampStr := time.Now().Format(time.RFC3339)
	if parsedTime, err := strconv.ParseInt(startTime, 10, 64); err == nil {
		timestampStr = time.Unix(parsedTime, 0).Format(time.RFC3339)
	}

	fullName := channelKey
	twitchLogin := strings.ToLower(channelKey)
	if p, ok := d.channels[channelKey]; ok {
		fullName = p.displayName
		twitchLogin = p.twitchLogin
	}

	isTwitchStream := len(streamID) > 0
	for _, char := range streamID {
		if !unicode.IsDigit(char) {
			isTwitchStream = false
			break
		}
	}

	var streamLink string
	var imageUrl string
	if isTwitchStream {
		streamLink = fmt.Sprintf("https://twitch.tv/%s", twitchLogin)
		imageUrl = fmt.Sprintf("https://static-cdn.jtvnw.net/previews-ttv/live_user_%s-1280x720.jpg", twitchLogin)
	} else {
		streamLink = fmt.Sprintf("https://www.youtube.com/watch?v=%s", streamID)
		imageUrl = fmt.Sprintf("https://i.ytimg.com/vi/%s/maxresdefault.jpg", streamID)
	}

	var transcriptLink string
	if d.transcriptBaseURL != "" {
		transcriptLink = d.transcriptBaseURL + "/" + channelKey + "/"
	} else {
		domain := "www.duck-automata.com"
		if d.Version == "dev" {
			domain = "dev.duck-automata.com"
		}
		transcriptLink = fmt.Sprintf("https://%s/live-transcript/%s/", domain, channelKey)
	}

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

// NotifyWorkerOffline alerts that a channel's worker has stopped reporting.
func (d *Client) NotifyWorkerOffline(channelKey string, lastSeen int64) {
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
func (d *Client) NotifyDiscordBotOffline(lastSeen int64) {
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
func (d *Client) NotifyDiscordBotRecovered() {
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
func (d *Client) NotifyDiscordBotStartupError(err error) {
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

// Notify500Error alerts on a 500 response. Alerts within notify500Throttle of
// the previous one are dropped so an error storm cannot spam the webhook or
// pile up sender goroutines.
func (d *Client) Notify500Error(err error, contextMsg string) {
	if d.WebhookURL == "" {
		return
	}

	d.mu.Lock()
	if time.Since(d.last500Alert) < notify500Throttle {
		d.mu.Unlock()
		slog.Warn("dropping throttled 500-error discord notification", "func", "Client.Notify500Error", "context", contextMsg, "err", err)
		return
	}
	d.last500Alert = time.Now()
	d.mu.Unlock()

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
