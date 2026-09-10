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
	"live-transcript-server/internal/model"
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
// delivery failures. A zero WebhookURL disables every notification except the
// admin audit, which follows its own (defaulted) AdminWebhookURL.
type Client struct {
	WebhookURL string
	// AdminWebhookURL is where admin-operation audit records go. It falls back
	// to WebhookURL when not configured separately.
	AdminWebhookURL string
	// DetectWebhookURL is where live-detection observations go. Empty falls
	// back to AdminWebhookURL; see detectWebhookURL.
	DetectWebhookURL string
	NotifyPing       string
	Version          string

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

	adminURL := cfg.AdminWebhookURL
	if adminURL == "" {
		adminURL = cfg.WebhookURL
	}

	return &Client{
		WebhookURL:        cfg.WebhookURL,
		AdminWebhookURL:   adminURL,
		DetectWebhookURL:  cfg.DetectWebhookURL,
		NotifyPing:        ping,
		Version:           version,
		transcriptBaseURL: cfg.TranscriptBaseURL,
		channels:          chans,
		httpClient:        &http.Client{Timeout: 10 * time.Second},
	}
}

// send sends the actual payload to the default discord webhook.
func (d *Client) send(payload map[string]any) {
	d.sendTo(d.WebhookURL, payload)
}

// sendTo posts payload to a specific webhook URL. An empty URL is a no-op, so
// an unconfigured webhook silently disables the notifications that use it.
func (d *Client) sendTo(webhookURL string, payload map[string]any) {
	if webhookURL == "" {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("failed to marshal discord payload", "err", err)
		return
	}

	req, err := http.NewRequest("POST", webhookURL, bytes.NewBuffer(body))
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

// Discord embed limits. Names and values past these lengths make the API
// reject the whole payload, so NotifyAdminAction truncates instead.
const (
	maxEmbedFields     = 25
	maxFieldNameLength = 256
	maxFieldValueLen   = 1024
)

// AdminField is one labelled detail of an admin operation (e.g. "Stream ID" /
// "abc123"), rendered as an embed field. Order is preserved.
type AdminField struct {
	Name  string
	Value string
	// Inline packs the field beside its neighbours instead of on its own row.
	Inline bool
}

// NotifyAdminAction records a completed admin operation on the admin webhook:
// what was done (action), which channel it was done to (channelKey), and the
// operation's specifics (fields). It is an audit trail, not an alert - it
// never pings the operator.
//
// Callers must invoke it only after the operation has actually succeeded, and
// must never pass a secret (admin key, membership key) as a field value.
func (d *Client) NotifyAdminAction(channelKey, action string, fields ...AdminField) {
	if d.AdminWebhookURL == "" {
		return
	}

	embedFields := make([]map[string]any, 0, len(fields)+1)
	add := func(f AdminField) {
		if len(embedFields) >= maxEmbedFields {
			return
		}
		value := f.Value
		if value == "" {
			value = "," // Discord rejects an empty field value.
		}
		embedFields = append(embedFields, map[string]any{
			"name":   truncate(f.Name, maxFieldNameLength),
			"value":  truncate(value, maxFieldValueLen),
			"inline": f.Inline,
		})
	}

	add(AdminField{Name: "Channel Key", Value: channelKey, Inline: true})
	for _, f := range fields {
		add(f)
	}

	payload := map[string]any{
		"embeds": []map[string]any{
			{
				"title":     fmt.Sprintf("Admin: %s", action),
				"color":     5793266, // Blurple
				"fields":    embedFields,
				"timestamp": time.Now().Format(time.RFC3339),
				"footer": map[string]string{
					"text": fmt.Sprintf("Version: %s", d.Version),
				},
			},
		},
	}
	go d.sendTo(d.AdminWebhookURL, payload)
}

// truncate shortens s to at most max characters, marking any cut with an
// ellipsis so a clipped value never reads as the complete one.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
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

// NotifyStreamDetected reports that live detection observed a broadcast go
// live. This is the entire output of shadow mode: detection runs, measures
// itself, and tells the operator - it never queues anything for the worker.
//
// The numbers are the point. StartedAt is what the platform says; DetectedAt
// is when we saw it; the delay between them is what decides whether this
// approach is trustworthy enough to drive the worker. Mechanism names which
// detection path won the race, so a soak shows not just whether detection
// works but which half of it is carrying the result.
//
// Read the delay with the mechanism in mind: a push path (EventSub, WebSub)
// measures close to true end-to-end latency, while a polling path also carries
// the platform API's own cache lag, which can be tens of seconds and is not
// something this server can shorten.
//
// Posts to the detection webhook, which falls back to the admin webhook and
// then the main one - detection is high-volume during a soak and does not ping.
// Nil-receiver-safe so a detector built without a Discord client still runs.
func (d *Client) NotifyStreamDetected(b model.DetectedBroadcast) {
	if d == nil || d.detectWebhookURL() == "" {
		return
	}

	fullName := b.ChannelKey
	if p, ok := d.channels[b.ChannelKey]; ok {
		fullName = p.displayName
	}

	title := b.Title
	if title == "" {
		title = "(no title reported)"
	}

	fields := []map[string]any{
		{"name": "Channel", "value": fmt.Sprintf("%s (`%s`)", fullName, b.ChannelKey), "inline": true},
		{"name": "Platform", "value": b.Platform, "inline": true},
		{"name": "Mechanism", "value": b.Mechanism, "inline": true},
	}

	// A platform that reported no start time makes the delay unknowable. Say
	// so rather than rendering a delay measured against the epoch.
	if b.StartedAt > 0 {
		fields = append(fields,
			map[string]any{"name": "Stream Started", "value": fmt.Sprintf("<t:%d:T> (<t:%d:R>)", b.StartedAt, b.StartedAt), "inline": true},
			map[string]any{"name": "Detected", "value": fmt.Sprintf("<t:%d:T> (<t:%d:R>)", b.DetectedAt, b.DetectedAt), "inline": true},
			map[string]any{"name": "Delay", "value": formatDetectionDelay(b.DetectedAt - b.StartedAt), "inline": true},
		)
	} else {
		fields = append(fields,
			map[string]any{"name": "Stream Started", "value": "not reported by platform", "inline": true},
			map[string]any{"name": "Detected", "value": fmt.Sprintf("<t:%d:T>", b.DetectedAt), "inline": true},
			map[string]any{"name": "Delay", "value": "unknown", "inline": true},
		)
	}

	fields = append(fields, map[string]any{
		"name": "Broadcast ID", "value": fmt.Sprintf("`%s`", b.BroadcastID), "inline": false,
	})

	embed := map[string]any{
		"title":       fmt.Sprintf("Live Detected: %s", fullName),
		"description": fmt.Sprintf("**%s**\n[%s](%s)", truncate(title, 240), b.URL, b.URL),
		"url":         b.URL,
		"color":       3447003, // Blue - informational, distinct from the green stream-start announce.
		"fields":      fields,
		"timestamp":   time.Unix(b.DetectedAt, 0).UTC().Format(time.RFC3339),
		"footer": map[string]string{
			"text": fmt.Sprintf("live detection (shadow mode) · Version: %s", d.Version),
		},
	}

	go d.sendTo(d.detectWebhookURL(), map[string]any{"embeds": []map[string]any{embed}})
}

// formatDetectionDelay renders a detection delay for the notification.
// Negative values are possible and are not an error: a platform's reported
// start time can be a second or two ahead of the clock we compare it against,
// and a push notification can arrive before the API admits the stream exists.
func formatDetectionDelay(seconds int64) string {
	if seconds < 0 {
		return fmt.Sprintf("%s (detected before reported start)", (time.Duration(-seconds) * time.Second).String())
	}
	return (time.Duration(seconds) * time.Second).String()
}

// detectWebhookURL resolves where detection notifications go: the dedicated
// detection webhook if configured, otherwise the admin webhook (which itself
// falls back to the main one). A soak produces a lot of these, so being able
// to route them to their own channel is worth the knob.
func (d *Client) detectWebhookURL() string {
	if d.DetectWebhookURL != "" {
		return d.DetectWebhookURL
	}
	return d.AdminWebhookURL
}

// NotifyLiveDetectDown alerts that a live-detection leg has stopped working.
//
// Fired once on the healthy->down transition, never per cycle: every notifier
// here is an unbounded `go d.send(...)`, so alerting on every failed poll
// during a long outage would spawn thousands of goroutines, hit Discord's rate
// limit, and bury the one message that mattered.
//
// Routed to the detection webhook rather than the main one, and without a
// ping: during a shadow-mode soak a detector leg failing is information, not
// an emergency - nothing downstream depends on it yet.
func (d *Client) NotifyLiveDetectDown(mechanism string, err error, failures int) {
	if d == nil || d.detectWebhookURL() == "" {
		return
	}
	detail := fmt.Sprintf("Detection leg **%s** is failing.\n**Error:** %v", mechanism, err)
	if failures > 0 {
		detail += fmt.Sprintf("\nConsecutive failures: %d", failures)
	}
	go d.sendTo(d.detectWebhookURL(), map[string]any{
		"embeds": []map[string]any{
			{
				"title":       "Live Detection Leg Down",
				"description": detail,
				"color":       15158332, // Red
				"timestamp":   time.Now().Format(time.RFC3339),
				"footer": map[string]string{
					"text": fmt.Sprintf("live detection (shadow mode) · Version: %s", d.Version),
				},
			},
		},
	})
}

// NotifyLiveDetectRecovered announces that a detection leg is working again,
// sent once after a prior down alert so the operator knows the outage closed.
func (d *Client) NotifyLiveDetectRecovered(mechanism string) {
	if d == nil || d.detectWebhookURL() == "" {
		return
	}
	go d.sendTo(d.detectWebhookURL(), map[string]any{
		"embeds": []map[string]any{
			{
				"title":       "Live Detection Leg Recovered",
				"description": fmt.Sprintf("Detection leg **%s** is producing results again.", mechanism),
				"color":       3066993, // Green
				"timestamp":   time.Now().Format(time.RFC3339),
				"footer": map[string]string{
					"text": fmt.Sprintf("live detection (shadow mode) · Version: %s", d.Version),
				},
			},
		},
	})
}

// NotifyLiveDetectRevoked reports that Twitch dropped an EventSub
// subscription. The reconciler recreates it, but the operator needs to hear
// about it: a revocation for notification_failures_exceeded means deliveries
// were being rejected, which usually points at something in front of the
// server (a Cloudflare challenge on Twitch's Go-http-client user agent) rather
// than at the server itself.
func (d *Client) NotifyLiveDetectRevoked(subType, status, broadcasterID string) {
	if d == nil || d.detectWebhookURL() == "" {
		return
	}
	go d.sendTo(d.detectWebhookURL(), map[string]any{
		"embeds": []map[string]any{
			{
				"title": "Twitch EventSub Subscription Revoked",
				"description": fmt.Sprintf(
					"Twitch revoked a **%s** subscription.\n**Reason:** %s\n**Broadcaster:** %s\n\nThe reconciler will try to recreate it. A `notification_failures_exceeded` reason means Twitch could not deliver to the callback.",
					subType, status, broadcasterID),
				"color":     16744448, // Orange
				"timestamp": time.Now().Format(time.RFC3339),
				"footer": map[string]string{
					"text": fmt.Sprintf("live detection (shadow mode) · Version: %s", d.Version),
				},
			},
		},
	})
}

// NotifyLiveDetectAuditMiss reports that the independent cross-check found a
// live broadcast detection had not seen.
//
// This is the alarm for the one failure that is otherwise invisible: if
// discovery never surfaces a channel's in-progress broadcast, every leg looks
// healthy and the only symptom is an absence of notifications - which is
// indistinguishable from a channel that simply did not stream that day.
func (d *Client) NotifyLiveDetectAuditMiss(channelKey, videoID string) {
	if d == nil || d.detectWebhookURL() == "" {
		return
	}
	fullName := channelKey
	if p, ok := d.channels[channelKey]; ok {
		fullName = p.displayName
	}
	go d.sendTo(d.detectWebhookURL(), map[string]any{
		"content": d.NotifyPing,
		"embeds": []map[string]any{
			{
				"title": "Live Detection Missed a Stream",
				"description": fmt.Sprintf(
					"The search cross-check found **%s** live on a broadcast detection had not seen.\n[%s](%s)\n\nDiscovery is not surfacing this channel's broadcasts; the delay reported for it (if any) is not trustworthy.",
					fullName, videoID, "https://www.youtube.com/watch?v="+videoID),
				"color":     15158332, // Red
				"timestamp": time.Now().Format(time.RFC3339),
				"footer": map[string]string{
					"text": fmt.Sprintf("live detection (shadow mode) · Version: %s", d.Version),
				},
			},
		},
	})
}

// NotifyLiveDetectCallbackBlocked alerts that an inbound callback URL is being
// answered by something other than this server.
//
// This is the loudest alert in the detector, and it pings, because it is the
// only failure that is invisible from both ends. A bot challenge in front of
// the server answers with a decoy page and a 2xx status, so Twitch records the
// notification as delivered and discards it - no retry, no revocation, no
// delivery-failure counter - while nothing reaches this process to be logged.
// Without this alert the only symptom is that every detection quietly arrives
// via the slower polling leg.
func (d *Client) NotifyLiveDetectCallbackBlocked(blocked []string) {
	if d == nil || d.detectWebhookURL() == "" {
		return
	}
	go d.sendTo(d.detectWebhookURL(), map[string]any{
		"content": d.NotifyPing,
		"embeds": []map[string]any{
			{
				"title": "Live Detection Callback Is Being Intercepted",
				"description": fmt.Sprintf(
					"A probe of the push callback URL was answered by something other than this server:\n%s\n\n"+
						"Push notifications are being silently discarded - a bot challenge returns a 2xx, so the sender treats the delivery as successful and never retries. "+
						"Detection still works via polling, but at minutes rather than seconds.\n\n"+
						"Fix: add an edge rule that skips bot protection for the `/livedetect/` paths. They are safe to exempt - every push is HMAC-verified over the raw body and size-limited.",
					"• "+strings.Join(blocked, "\n• ")),
				"color":     15158332, // Red
				"timestamp": time.Now().Format(time.RFC3339),
				"footer": map[string]string{
					"text": fmt.Sprintf("live detection (shadow mode) · Version: %s", d.Version),
				},
			},
		},
	})
}
