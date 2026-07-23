package discord

import (
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
