package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestParsePingcordMessage(t *testing.T) {
	channelMap := map[string]string{
		"Dokibird":     "doki",
		"Mint Fantôme": "mint",
	}

	tests := []struct {
		name    string
		msg     *discordgo.Message
		wantKey string
		wantURL string
		wantOK  bool
	}{
		{
			name:    "plain content",
			msg:     &discordgo.Message{Content: "Dokibird https://twitch.tv/dokibird"},
			wantKey: "doki",
			wantURL: "https://twitch.tv/dokibird",
			wantOK:  true,
		},
		{
			name: "channel name in embed title, url in embed url",
			msg: &discordgo.Message{
				Embeds: []*discordgo.MessageEmbed{
					{
						Title: "Dokibird is now live",
						URL:   "https://www.youtube.com/watch?v=abc123",
					},
				},
			},
			wantKey: "doki",
			wantURL: "https://www.youtube.com/watch?v=abc123",
			wantOK:  true,
		},
		{
			name:    "name with non-ascii chars",
			msg:     &discordgo.Message{Content: "Mint Fantôme https://twitch.tv/mintfantome"},
			wantKey: "mint",
			wantURL: "https://twitch.tv/mintfantome",
			wantOK:  true,
		},
		{
			name:    "case-insensitive channel name match",
			msg:     &discordgo.Message{Content: "DOKIBIRD https://twitch.tv/dokibird"},
			wantKey: "doki",
			wantURL: "https://twitch.tv/dokibird",
			wantOK:  true,
		},
		{
			name:    "trailing punctuation stripped from url",
			msg:     &discordgo.Message{Content: "Dokibird is live at https://twitch.tv/dokibird."},
			wantKey: "doki",
			wantURL: "https://twitch.tv/dokibird",
			wantOK:  true,
		},
		{
			name:   "no url",
			msg:    &discordgo.Message{Content: "Dokibird is live"},
			wantOK: false,
		},
		{
			name:   "no matching channel",
			msg:    &discordgo.Message{Content: "SomeoneElse https://twitch.tv/someoneelse"},
			wantOK: false,
		},
		{
			name:   "empty message",
			msg:    &discordgo.Message{},
			wantOK: false,
		},
		{
			name:   "nil message",
			msg:    nil,
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotKey, gotURL, gotOK := parsePingcordMessage(tc.msg, channelMap)
			if gotOK != tc.wantOK {
				t.Fatalf("ok=%v want %v (key=%q url=%q)", gotOK, tc.wantOK, gotKey, gotURL)
			}
			if gotOK {
				if gotKey != tc.wantKey {
					t.Errorf("key=%q want %q", gotKey, tc.wantKey)
				}
				if gotURL != tc.wantURL {
					t.Errorf("url=%q want %q", gotURL, tc.wantURL)
				}
			}
		})
	}
}

func TestParsePingcordMessage_LongestNameWins(t *testing.T) {
	// "Mint Fantôme" should match before a hypothetical "Mint" prefix entry
	// when both could match the text.
	channelMap := map[string]string{
		"Mint":         "mintshort",
		"Mint Fantôme": "mint",
	}
	msg := &discordgo.Message{Content: "Mint Fantôme https://twitch.tv/mintfantome"}

	key, _, ok := parsePingcordMessage(msg, channelMap)
	if !ok {
		t.Fatal("expected match")
	}
	if key != "mint" {
		t.Errorf("expected longest-name match to win, got key=%q", key)
	}
}

func TestIncomingEndpoints(t *testing.T) {
	app, mux, _ := setupTestApp(t, []string{"doki"})
	ctx := context.Background()

	// Initially empty
	req := httptest.NewRequest(http.MethodGet, "/doki/incoming", nil)
	req.Header.Set("X-API-Key", app.ApiKey)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET empty: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "{\"urls\":[]}\n" {
		t.Errorf("GET empty body=%q want empty url list", got)
	}

	// Insert via direct DB call (bot path)
	if err := app.UpsertIncomingStream(ctx, "doki", "https://twitch.tv/dokibird", 1700000000); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := app.UpsertIncomingStream(ctx, "doki", "https://www.youtube.com/watch?v=abc", 1700000010); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// GET returns both URLs in received_at order
	req = httptest.NewRequest(http.MethodGet, "/doki/incoming", nil)
	req.Header.Set("X-API-Key", app.ApiKey)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: status=%d body=%s", rec.Code, rec.Body.String())
	}
	want := "{\"urls\":[\"https://twitch.tv/dokibird\",\"https://www.youtube.com/watch?v=abc\"]}\n"
	if rec.Body.String() != want {
		t.Errorf("GET body=%q want %q", rec.Body.String(), want)
	}

	// GET should be idempotent — call again, same result
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/doki/incoming", nil).WithContext(ctx))
	// (no api key — should fail auth)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 without api key, got %d", rec.Code)
	}

	// DELETE one URL
	delReq := httptest.NewRequest(http.MethodDelete, "/doki/incoming?url=https://twitch.tv/dokibird", nil)
	delReq.Header.Set("X-API-Key", app.ApiKey)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, delReq)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// DELETE same URL again -> 404
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, delReq)
	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE missing: status=%d want 404", rec.Code)
	}

	// GET shows only the remaining url
	req = httptest.NewRequest(http.MethodGet, "/doki/incoming", nil)
	req.Header.Set("X-API-Key", app.ApiKey)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	want = "{\"urls\":[\"https://www.youtube.com/watch?v=abc\"]}\n"
	if rec.Body.String() != want {
		t.Errorf("GET after delete body=%q want %q", rec.Body.String(), want)
	}

	// DELETE without ?url -> 400
	rec = httptest.NewRecorder()
	bad := httptest.NewRequest(http.MethodDelete, "/doki/incoming", nil)
	bad.Header.Set("X-API-Key", app.ApiKey)
	mux.ServeHTTP(rec, bad)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("DELETE no url: status=%d want 400", rec.Code)
	}

	// Unknown channel -> 404
	rec = httptest.NewRecorder()
	bad = httptest.NewRequest(http.MethodGet, "/unknown/incoming", nil)
	bad.Header.Set("X-API-Key", app.ApiKey)
	mux.ServeHTTP(rec, bad)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown channel: status=%d want 404", rec.Code)
	}
}

func TestCleanupExpiredIncomingStreams(t *testing.T) {
	app, _, _ := setupTestApp(t, []string{"doki"})
	ctx := context.Background()

	// Two URLs at different ages
	if err := app.UpsertIncomingStream(ctx, "doki", "old", 100); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := app.UpsertIncomingStream(ctx, "doki", "fresh", 1000); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	removed, err := app.CleanupExpiredIncomingStreams(ctx, 500)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed=%d want 1", removed)
	}

	urls, err := app.GetIncomingStreams(ctx, "doki")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(urls) != 1 || urls[0] != "fresh" {
		t.Errorf("urls=%v want [fresh]", urls)
	}
}
