package internal

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// urlRegex extracts the first http(s) URL from a chunk of text. Pingcord posts
// either a custom-template body (`{channel} {link}`) or an embed with the link
// in the URL/description, so we look across all text fields for a match.
var urlRegex = regexp.MustCompile(`https?://[^\s<>"']+`)

// DiscordBot listens for Pingcord stream-start messages on a Discord channel
// and queues the announced URL for the matching server key.
type DiscordBot struct {
	session    *discordgo.Session
	app        *App
	channelIDs map[string]struct{}
	channelMap map[string]string
}

// NewDiscordBot constructs a bot from config. Returns (nil, nil) if the bot is
// not configured, so callers can no-op without special-casing.
func NewDiscordBot(cfg DiscordBotConfig, app *App) (*DiscordBot, error) {
	if cfg.Token == "" {
		return nil, nil
	}
	if len(cfg.ChannelMap) == 0 {
		return nil, fmt.Errorf("discord.bot.channelMap is empty; bot would never match a message")
	}

	session, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}
	session.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent

	channelIDs := make(map[string]struct{}, len(cfg.ChannelIDs))
	for _, id := range cfg.ChannelIDs {
		if id != "" {
			channelIDs[id] = struct{}{}
		}
	}

	bot := &DiscordBot{
		session:    session,
		app:        app,
		channelIDs: channelIDs,
		channelMap: cfg.ChannelMap,
	}
	session.AddHandler(bot.onMessage)
	return bot, nil
}

// Start opens the Discord gateway connection.
func (b *DiscordBot) Start() error {
	if b == nil {
		return nil
	}
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("open discord session: %w", err)
	}
	slog.Info("discord bot connected", "func", "DiscordBot.Start", "listening_channels", len(b.channelIDs), "mapped_keys", len(b.channelMap))
	return nil
}

// Close shuts down the Discord gateway connection.
func (b *DiscordBot) Close() error {
	if b == nil {
		return nil
	}
	return b.session.Close()
}

func (b *DiscordBot) onMessage(_ *discordgo.Session, m *discordgo.MessageCreate) {
	// Only listen on configured channels. An empty list is treated as
	// "no channel allowed" to avoid accidentally listening server-wide.
	if _, ok := b.channelIDs[m.ChannelID]; !ok {
		return
	}

	key, url, ok := parsePingcordMessage(m.Message, b.channelMap)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.app.UpsertIncomingStream(ctx, key, url, time.Now().Unix()); err != nil {
		slog.Error("failed to store incoming stream", "func", "DiscordBot.onMessage", "key", key, "url", url, "err", err)
		return
	}
	slog.Info("incoming stream queued", "func", "DiscordBot.onMessage", "key", key, "url", url, "messageId", m.ID)
}

// parsePingcordMessage flattens a Discord message into a single string and
// looks for (a) the first http(s) URL and (b) the first channel-name key from
// channelMap that appears anywhere in the text. Returns (key, url, true) only
// when both are found. Channel names are matched case-insensitively to handle
// minor formatting drift in Pingcord templates.
func parsePingcordMessage(m *discordgo.Message, channelMap map[string]string) (string, string, bool) {
	if m == nil {
		return "", "", false
	}

	var b strings.Builder
	b.WriteString(m.Content)
	for _, embed := range m.Embeds {
		b.WriteByte(' ')
		b.WriteString(embed.Title)
		b.WriteByte(' ')
		b.WriteString(embed.Description)
		b.WriteByte(' ')
		b.WriteString(embed.URL)
		if embed.Author != nil {
			b.WriteByte(' ')
			b.WriteString(embed.Author.Name)
			b.WriteByte(' ')
			b.WriteString(embed.Author.URL)
		}
		for _, f := range embed.Fields {
			b.WriteByte(' ')
			b.WriteString(f.Name)
			b.WriteByte(' ')
			b.WriteString(f.Value)
		}
	}
	text := b.String()
	if text == "" {
		return "", "", false
	}

	url := urlRegex.FindString(text)
	if url == "" {
		return "", "", false
	}
	// Discord wraps suppressed embeds in <...>; trim trailing punctuation that
	// can ride along with a URL captured from prose.
	url = strings.TrimRight(url, ".,);]>")

	lowerText := strings.ToLower(text)
	// Deterministic order: longest names first so "Mint Fantôme" wins over a
	// hypothetical "Mint" prefix entry.
	names := make([]string, 0, len(channelMap))
	for name := range channelMap {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int { return len(b) - len(a) })

	for _, name := range names {
		if name == "" {
			continue
		}
		if strings.Contains(lowerText, strings.ToLower(name)) {
			return channelMap[name], url, true
		}
	}
	return "", "", false
}
