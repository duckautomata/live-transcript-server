package discord

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"live-transcript-server/internal/config"
)

// newTestClient builds a Client pointed at an httptest webhook that forwards
// every decoded payload on the returned channel. Notify* methods post from a
// goroutine, so tests must receive from the channel (see waitPayload) instead
// of asserting immediately.
func newTestClient(t *testing.T, cfg config.DiscordConfig, version string, channels []config.ChannelConfig) (*Client, chan map[string]any) {
	t.Helper()
	payloads := make(chan map[string]any, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode webhook payload: %v", err)
		}
		payloads <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	cfg.WebhookURL = srv.URL
	return NewClient(cfg, version, channels), payloads
}

func waitPayload(t *testing.T, payloads chan map[string]any) map[string]any {
	t.Helper()
	select {
	case p := <-payloads:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for webhook payload")
		return nil
	}
}

// embedFrom extracts the single embed object from a webhook payload.
func embedFrom(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	embeds, ok := payload["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("payload embeds = %#v, want exactly one embed", payload["embeds"])
	}
	embed, ok := embeds[0].(map[string]any)
	if !ok {
		t.Fatalf("embed = %#v, want object", embeds[0])
	}
	return embed
}

func imageURL(t *testing.T, embed map[string]any) string {
	t.Helper()
	image, ok := embed["image"].(map[string]any)
	if !ok {
		t.Fatalf("embed image = %#v, want object", embed["image"])
	}
	url, _ := image["url"].(string)
	return url
}

func TestNewClient_PingPrefersUserOverRole(t *testing.T) {
	c := NewClient(config.DiscordConfig{NotifyUserID: "u1", NotifyRoleID: "r1"}, "test", nil)
	if c.NotifyPing != "<@u1>" {
		t.Errorf("ping=%q want user mention", c.NotifyPing)
	}
	c = NewClient(config.DiscordConfig{NotifyRoleID: "r1"}, "test", nil)
	if c.NotifyPing != "<@&r1>" {
		t.Errorf("ping=%q want role mention", c.NotifyPing)
	}
}

func TestNotifyStreamStart_TwitchLinkFromNumericStreamID(t *testing.T) {
	channels := []config.ChannelConfig{{Name: "doki", DisplayName: "Dokibird"}}
	c, payloads := newTestClient(t, config.DiscordConfig{}, "test", channels)

	c.NotifyStreamStart("doki", "123456789", "playing games", "1700000000")
	embed := embedFrom(t, waitPayload(t, payloads))

	// TwitchLogin defaults to the lowercased display name.
	if got := embed["url"]; got != "https://twitch.tv/dokibird" {
		t.Errorf("embed url=%v want twitch link", got)
	}
	if got := imageURL(t, embed); got != "https://static-cdn.jtvnw.net/previews-ttv/live_user_dokibird-1280x720.jpg" {
		t.Errorf("image url=%v want twitch preview", got)
	}
	// Configured display name appears in the title.
	if got := embed["title"]; got != "Dokibird's Stream Started" {
		t.Errorf("title=%v want display name in title", got)
	}
	desc, _ := embed["description"].(string)
	if !strings.Contains(desc, "[Stream Link](https://twitch.tv/dokibird)") {
		t.Errorf("description=%q missing twitch stream link", desc)
	}
	// Version "test" is not "dev", so the prod default transcript domain is used.
	if !strings.Contains(desc, "[Transcript](https://www.duck-automata.com/live-transcript/doki/)") {
		t.Errorf("description=%q missing default transcript link", desc)
	}
}

func TestNotifyStreamStart_ConfiguredTwitchLoginWins(t *testing.T) {
	// A display name with a space lowercases into a broken login; the explicit
	// TwitchLogin config overrides it.
	channels := []config.ChannelConfig{{Name: "mint", DisplayName: "Mint Fantôme", TwitchLogin: "mintfantome"}}
	c, payloads := newTestClient(t, config.DiscordConfig{}, "test", channels)

	c.NotifyStreamStart("mint", "42", "karaoke", "1700000000")
	embed := embedFrom(t, waitPayload(t, payloads))

	if got := embed["url"]; got != "https://twitch.tv/mintfantome" {
		t.Errorf("embed url=%v want configured twitch login", got)
	}
	if got := embed["title"]; got != "Mint Fantôme's Stream Started" {
		t.Errorf("title=%v want configured display name", got)
	}
}

func TestNotifyStreamStart_YouTubeLinkFromNonNumericStreamID(t *testing.T) {
	channels := []config.ChannelConfig{{Name: "doki", DisplayName: "Dokibird"}}
	c, payloads := newTestClient(t, config.DiscordConfig{}, "test", channels)

	c.NotifyStreamStart("doki", "abc123XYZ", "watchalong", "1700000000")
	embed := embedFrom(t, waitPayload(t, payloads))

	if got := embed["url"]; got != "https://www.youtube.com/watch?v=abc123XYZ" {
		t.Errorf("embed url=%v want youtube link", got)
	}
	if got := imageURL(t, embed); got != "https://i.ytimg.com/vi/abc123XYZ/maxresdefault.jpg" {
		t.Errorf("image url=%v want youtube thumbnail", got)
	}
}

func TestNotifyStreamStart_UnconfiguredChannelFallsBackToKey(t *testing.T) {
	c, payloads := newTestClient(t, config.DiscordConfig{}, "test", nil)

	c.NotifyStreamStart("other", "555", "surprise stream", "1700000000")
	embed := embedFrom(t, waitPayload(t, payloads))

	if got := embed["title"]; got != "other's Stream Started" {
		t.Errorf("title=%v want channel key as display name", got)
	}
	if got := embed["url"]; got != "https://twitch.tv/other" {
		t.Errorf("embed url=%v want lowercased key as twitch login", got)
	}
}

func TestNotifyStreamStart_TranscriptBaseURLOverride(t *testing.T) {
	cfg := config.DiscordConfig{TranscriptBaseURL: "https://example.com/lt"}
	c, payloads := newTestClient(t, cfg, "test", nil)

	c.NotifyStreamStart("doki", "1", "stream", "1700000000")
	embed := embedFrom(t, waitPayload(t, payloads))

	desc, _ := embed["description"].(string)
	if !strings.Contains(desc, "[Transcript](https://example.com/lt/doki/)") {
		t.Errorf("description=%q missing configured transcript link", desc)
	}
}

func TestNotify500Error_Throttled(t *testing.T) {
	c, payloads := newTestClient(t, config.DiscordConfig{}, "test", nil)

	c.Notify500Error(errors.New("boom"), "first")
	c.Notify500Error(errors.New("boom again"), "second")

	embed := embedFrom(t, waitPayload(t, payloads))
	if got := embed["title"]; got != "500 Internal Server Error" {
		t.Errorf("title=%v want 500 alert", got)
	}
	desc, _ := embed["description"].(string)
	if !strings.Contains(desc, "first") {
		t.Errorf("description=%q want the first (unthrottled) alert", desc)
	}

	// The second call landed inside the throttle window and must be dropped.
	select {
	case p := <-payloads:
		t.Fatalf("unexpected second webhook POST: %#v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

// fieldsFrom extracts an embed's fields as an ordered name/value slice.
func fieldsFrom(t *testing.T, embed map[string]any) [][2]string {
	t.Helper()
	raw, ok := embed["fields"].([]any)
	if !ok {
		t.Fatalf("embed fields = %#v, want array", embed["fields"])
	}
	out := make([][2]string, 0, len(raw))
	for _, r := range raw {
		f, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("field = %#v, want object", r)
		}
		name, _ := f["name"].(string)
		value, _ := f["value"].(string)
		out = append(out, [2]string{name, value})
	}
	return out
}

func TestNotifyAdminAction_ReportsActionChannelAndDetails(t *testing.T) {
	c, payloads := newTestClient(t, config.DiscordConfig{NotifyUserID: "u1"}, "test", nil)

	c.NotifyAdminAction("doki", "Deleted stream",
		AdminField{Name: "Stream ID", Value: "abc123", Inline: true},
		AdminField{Name: "Media Deleted", Value: "no"},
	)
	payload := waitPayload(t, payloads)
	embed := embedFrom(t, payload)

	if got := embed["title"]; got != "Admin: Deleted stream" {
		t.Errorf("title=%v want the action", got)
	}
	// An audit record is not an alert: it must never ping the operator.
	if got, ok := payload["content"]; ok && got != "" {
		t.Errorf("content=%v want no ping on an admin audit post", got)
	}

	fields := fieldsFrom(t, embed)
	want := [][2]string{
		{"Channel Key", "doki"},
		{"Stream ID", "abc123"},
		{"Media Deleted", "no"},
	}
	if len(fields) != len(want) {
		t.Fatalf("fields=%v want %v", fields, want)
	}
	for i, w := range want {
		if fields[i] != w {
			t.Errorf("field[%d]=%v want %v", i, fields[i], w)
		}
	}
}

func TestNotifyAdminAction_UsesDedicatedAdminWebhook(t *testing.T) {
	// Two webhooks: the admin audit must go only to the admin one.
	main, mainPayloads := newTestClient(t, config.DiscordConfig{}, "test", nil)
	admin, adminPayloads := newTestClient(t, config.DiscordConfig{}, "test", nil)

	c := NewClient(config.DiscordConfig{
		WebhookURL:      main.WebhookURL,
		AdminWebhookURL: admin.WebhookURL,
	}, "test", nil)

	c.NotifyAdminAction("doki", "Requested worker restart")
	embed := embedFrom(t, waitPayload(t, adminPayloads))
	if got := embed["title"]; got != "Admin: Requested worker restart" {
		t.Errorf("title=%v want the action", got)
	}

	select {
	case p := <-mainPayloads:
		t.Fatalf("admin audit leaked to the main webhook: %#v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestNotifyAdminAction_DisabledWithoutAnyWebhook(t *testing.T) {
	// No webhook configured at all: the call must be a silent no-op, not a
	// panic or a POST to an empty URL.
	c := NewClient(config.DiscordConfig{}, "test", nil)
	if c.AdminWebhookURL != "" {
		t.Fatalf("AdminWebhookURL=%q want empty", c.AdminWebhookURL)
	}
	c.NotifyAdminAction("doki", "Stopped current stream")
}

func TestNotifyAdminAction_TruncatesOverlongValues(t *testing.T) {
	c, payloads := newTestClient(t, config.DiscordConfig{}, "test", nil)

	c.NotifyAdminAction("doki", "Queued incoming stream",
		AdminField{Name: "URL", Value: strings.Repeat("x", maxFieldValueLen+50)},
	)
	fields := fieldsFrom(t, embedFrom(t, waitPayload(t, payloads)))

	if len(fields) != 2 {
		t.Fatalf("fields=%v want channel key + url", fields)
	}
	url := fields[1][1]
	if len([]rune(url)) != maxFieldValueLen {
		t.Errorf("url length=%d want %d", len([]rune(url)), maxFieldValueLen)
	}
	if !strings.HasSuffix(url, "…") {
		t.Errorf("url=%q want a truncation marker", url[len(url)-10:])
	}
}

func TestNotifyAdminAction_EmptyValueIsPlaceheld(t *testing.T) {
	// Discord rejects the whole payload on an empty field value.
	c, payloads := newTestClient(t, config.DiscordConfig{}, "test", nil)

	c.NotifyAdminAction("doki", "Created membership key", AdminField{Name: "Expires At", Value: ""})
	fields := fieldsFrom(t, embedFrom(t, waitPayload(t, payloads)))
	for _, f := range fields {
		if f[1] == "" {
			t.Errorf("field %q has an empty value", f[0])
		}
	}
}
