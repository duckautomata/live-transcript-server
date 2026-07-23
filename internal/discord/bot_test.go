package discord

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"live-transcript-server/internal/config"
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
			// Embed text is intentionally ignored — only Content is parsed.
			name: "embed-only message is ignored",
			msg: &discordgo.Message{
				Embeds: []*discordgo.MessageEmbed{
					{
						Title: "Dokibird is now live",
						URL:   "https://www.youtube.com/watch?v=abc123",
					},
				},
			},
			wantOK: false,
		},
		{
			// Channel name must come from Content, not from an embed —
			// even when the Content URL doesn't itself reveal the streamer.
			name: "embed channel name does not feed the match",
			msg: &discordgo.Message{
				Content: "https://example.com/r/abc",
				Embeds: []*discordgo.MessageEmbed{
					{Title: "Dokibird is now live"},
				},
			},
			wantOK: false,
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

// newStatusTestBot builds a bot that never touches the network; state is
// driven by calling the lifecycle handlers directly.
func newStatusTestBot(t *testing.T) *Bot {
	t.Helper()
	bot, err := NewBot(config.DiscordBotConfig{
		Token:      "test-token",
		ChannelIDs: []string{"123"},
		ChannelMap: map[string]string{"Dokibird": "doki"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	return bot
}

func TestBotStatusNilBot(t *testing.T) {
	var b *Bot
	st := b.Status("doki")
	if st.State != BotStateOff {
		t.Errorf("state=%q want %q", st.State, BotStateOff)
	}
	if st.Detail == "" {
		t.Error("expected a detail explaining the bot is not configured")
	}
}

func TestBotStatusTransitions(t *testing.T) {
	bot := newStatusTestBot(t)

	// Never started: down, no error yet.
	st := bot.Status("doki")
	if st.State != BotStateDown {
		t.Fatalf("unstarted: state=%q want %q", st.State, BotStateDown)
	}
	if st.LastError != "" {
		t.Errorf("unstarted: lastError=%q want empty", st.LastError)
	}

	// Failed open: down, with the error surfaced in the detail.
	bot.recordGatewayError(errors.New("401: unauthorized"))
	st = bot.Status("doki")
	if st.State != BotStateDown {
		t.Fatalf("after open error: state=%q want %q", st.State, BotStateDown)
	}
	if !strings.Contains(st.Detail, "401: unauthorized") {
		t.Errorf("after open error: detail=%q want it to contain the error", st.Detail)
	}

	// Connected with a fresh heartbeat ack: ok.
	bot.onConnect(nil, nil)
	bot.session.Lock()
	bot.session.LastHeartbeatAck = time.Now()
	bot.session.Unlock()
	st = bot.Status("doki")
	if st.State != BotStateOK {
		t.Fatalf("connected: state=%q detail=%q want %q", st.State, st.Detail, BotStateOK)
	}
	if st.Detail != "" {
		t.Errorf("connected: detail=%q want empty", st.Detail)
	}
	if st.ListeningChannels != 1 {
		t.Errorf("listeningChannels=%d want 1", st.ListeningChannels)
	}

	// Connected but the gateway went silent: stale.
	bot.session.Lock()
	bot.session.LastHeartbeatAck = time.Now().Add(-heartbeatStaleThreshold - time.Minute)
	bot.session.Unlock()
	st = bot.Status("doki")
	if st.State != BotStateStale {
		t.Fatalf("stale ack: state=%q want %q", st.State, BotStateStale)
	}
	if !strings.Contains(st.Detail, "heartbeat") {
		t.Errorf("stale ack: detail=%q want a heartbeat explanation", st.Detail)
	}

	// Disconnected again: down, and the pre-connect error must NOT be blamed
	// because it predates the last successful connect.
	bot.onDisconnect(nil, nil)
	st = bot.Status("doki")
	if st.State != BotStateDown {
		t.Fatalf("disconnected: state=%q want %q", st.State, BotStateDown)
	}
	if strings.Contains(st.Detail, "401: unauthorized") {
		t.Errorf("disconnected: detail=%q blames an error from before the last connect", st.Detail)
	}
	if st.LastDisconnect == 0 {
		t.Error("disconnected: lastDisconnect not recorded")
	}

	// A fresh reconnect failure postdates the connect and is blamed again.
	bot.recordGatewayError(errors.New("dial tcp: timeout"))
	st = bot.Status("doki")
	if !strings.Contains(st.Detail, "dial tcp: timeout") {
		t.Errorf("after reopen error: detail=%q want it to contain the error", st.Detail)
	}
}

func TestBotStatusChannelMapped(t *testing.T) {
	bot := newStatusTestBot(t)
	if !bot.Status("doki").ChannelMapped {
		t.Error("doki is in the channel map; want ChannelMapped=true")
	}
	if bot.Status("unmapped").ChannelMapped {
		t.Error("unmapped key reported as mapped")
	}
}
