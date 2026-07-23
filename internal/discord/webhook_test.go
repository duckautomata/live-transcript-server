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
