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
}

func NewDiscordClient(cfg DiscordConfig) *DiscordClient {
	var ping string
	if cfg.NotifyUserID != "" {
		ping = fmt.Sprintf("<@%s>", cfg.NotifyUserID)
	} else if cfg.NotifyRoleID != "" {
		ping = fmt.Sprintf("<@&%s>", cfg.NotifyRoleID)
	}

	return &DiscordClient{
		WebhookURL: cfg.WebhookURL,
		NotifyPing: ping,
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
	if d.WebhookURL == "" {
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

	transcriptLink := fmt.Sprintf("https://www.duck-automata.com/live-transcript/%s/", channelKey)

	embed := map[string]any{
		"title":       fmt.Sprintf("%s's Stream Started", fullName),
		"description": fmt.Sprintf("**%s**\n\n[Stream Link](%s) | [Transcript](%s)", streamTitle, streamLink, transcriptLink),
		"url":         streamLink, // Embed Title Link
		"color":       3066993,    // Green
		"image": map[string]string{
			"url": imageUrl,
		},
		"timestamp": timestampStr,
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
			},
		},
	}
	go d.send(payload)
}

func (d *DiscordClient) Notify500Error(err error, contextMsg string) {
	if d.WebhookURL == "" {
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
			},
		},
	}
	go d.send(payload)
}
