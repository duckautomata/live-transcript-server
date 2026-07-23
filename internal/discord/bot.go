package discord

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"live-transcript-server/internal/config"
)

// urlRegex extracts the first http(s) URL from a chunk of text. We only parse
// the message Content (not embeds) so untrusted text inside an embed cannot
// inject a URL or a channel-name match.
var urlRegex = regexp.MustCompile(`https?://[^\s<>"']+`)

// Tunables for the gateway health watchdog.
const (
	// heartbeatStaleThreshold is how long the gateway may go without a
	// heartbeat ACK before we treat the connection as dead and force a rebuild.
	// discordgo refreshes LastHeartbeatAck every ~41s and its own watchdog only
	// fires after ~5 missed acks (~3.4m), so 5m is a safe ceiling that still
	// reacts long before a missed stream announcement would matter.
	heartbeatStaleThreshold = 5 * time.Minute

	// watchdogInterval is how often we poll the gateway's health.
	watchdogInterval = 90 * time.Second
)

// StreamSink accepts a stream URL announced on Discord for a channel key. The
// server implements it: its implementation stores the URL and bumps the
// channel's admin change counter, so the bot must not do a separate bump.
type StreamSink interface {
	QueueIncomingStream(ctx context.Context, channelKey, url string) error
}

// Bot listens for Pingcord stream-start messages on a Discord channel and
// queues the announced URL for the matching server key.
type Bot struct {
	session    *discordgo.Session
	sink       StreamSink
	alerts     *Client
	channelIDs map[string]struct{}
	channelMap map[string]string

	// watchdogOnce guards the watchdog goroutine so repeated Start calls
	// (e.g. a retry after a failed Open) can never launch it twice.
	watchdogOnce sync.Once

	// stop signals the watchdog goroutine to exit; closed once by Close.
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	// mu guards alerted, the offline-alert debounce flag. It is set when an
	// offline alert is sent and cleared when the gateway recovers, so we alert
	// (and announce recovery) only on the healthy<->stale transitions rather
	// than on every watchdog tick.
	mu      sync.Mutex
	alerted bool
}

// NewBot constructs a bot from config. Returns (nil, nil) if the bot is not
// configured, so callers can no-op without special-casing.
func NewBot(cfg config.DiscordBotConfig, sink StreamSink, alerts *Client) (*Bot, error) {
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

	bot := &Bot{
		session:    session,
		sink:       sink,
		alerts:     alerts,
		channelIDs: channelIDs,
		channelMap: cfg.ChannelMap,
		stop:       make(chan struct{}),
	}
	session.AddHandler(bot.onMessage)
	// Lifecycle handlers give us a trail of gateway connect/disconnect/resume
	// events. Without these a silent disconnect leaves no log at all.
	session.AddHandler(bot.onConnect)
	session.AddHandler(bot.onDisconnect)
	session.AddHandler(bot.onResumed)
	session.AddHandler(bot.onReady)
	return bot, nil
}

// Start opens the Discord gateway connection. The watchdog is started even
// when the initial Open fails: a zero LastHeartbeatAck reads as stale, so the
// checkHealth->forceReconnect path performs the retry the startup alert
// promises.
func (b *Bot) Start() error {
	if b == nil {
		return nil
	}
	if err := b.session.Open(); err != nil {
		b.alerts.NotifyDiscordBotStartupError(err)
		b.startWatchdog()
		return fmt.Errorf("open discord session: %w", err)
	}
	slog.Info("discord bot connected", "func", "Bot.Start", "listening_channels", len(b.channelIDs), "mapped_keys", len(b.channelMap))

	// Supervise the gateway: discordgo auto-reconnects on most errors, but can
	// silently wedge (reconnect/resume into a dead event stream) without ever
	// surfacing an error. The watchdog detects that and forces a fresh session.
	b.startWatchdog()
	return nil
}

// startWatchdog launches the watchdog goroutine at most once per Bot, no
// matter how many times Start is called.
func (b *Bot) startWatchdog() {
	b.watchdogOnce.Do(func() {
		b.wg.Add(1)
		go b.runWatchdog()
	})
}

// Close shuts down the Discord gateway connection.
func (b *Bot) Close() error {
	if b == nil {
		return nil
	}
	// Stop the watchdog first so it cannot reopen the session mid-shutdown.
	b.stopOnce.Do(func() { close(b.stop) })
	b.wg.Wait()
	return b.session.Close()
}

func (b *Bot) onMessage(_ *discordgo.Session, m *discordgo.MessageCreate) {
	// discordgo dispatches handlers in their own goroutines, so an unrecovered
	// panic here would take down the entire process. Contain it.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("recovered from panic in discord message handler", "func", "Bot.onMessage", "panic", r)
		}
	}()

	// Only listen on configured channels. An empty list is treated as
	// "no channel allowed" to avoid accidentally listening server-wide.
	if _, ok := b.channelIDs[m.ChannelID]; !ok {
		return
	}

	authorID, authorName := "", ""
	if m.Author != nil {
		authorID = m.Author.ID
		authorName = m.Author.Username
	}

	key, url, ok := parsePingcordMessage(m.Message, b.channelMap)
	slog.Debug("discord message received",
		"func", "Bot.onMessage",
		"messageId", m.ID,
		"channelId", m.ChannelID,
		"guildId", m.GuildID,
		"authorId", authorID,
		"authorName", authorName,
		"content", m.Content,
		"matched", ok,
		"parsedKey", key,
		"parsedUrl", url,
	)

	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.sink.QueueIncomingStream(ctx, key, url); err != nil {
		slog.Error("failed to store incoming stream", "func", "Bot.onMessage", "key", key, "url", url, "err", err)
		return
	}
	slog.Info("incoming stream queued", "func", "Bot.onMessage", "key", key, "url", url, "messageId", m.ID)
}

func (b *Bot) onConnect(_ *discordgo.Session, _ *discordgo.Connect) {
	slog.Info("discord gateway connected", "func", "Bot.onConnect")
}

func (b *Bot) onDisconnect(_ *discordgo.Session, _ *discordgo.Disconnect) {
	slog.Warn("discord gateway disconnected", "func", "Bot.onDisconnect")
}

func (b *Bot) onResumed(_ *discordgo.Session, _ *discordgo.Resumed) {
	slog.Info("discord gateway session resumed", "func", "Bot.onResumed")
}

func (b *Bot) onReady(_ *discordgo.Session, r *discordgo.Ready) {
	slog.Info("discord gateway ready", "func", "Bot.onReady", "sessionId", r.SessionID, "guilds", len(r.Guilds))
}

// runWatchdog periodically checks gateway health until Close is called.
func (b *Bot) runWatchdog() {
	defer b.wg.Done()
	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			b.checkHealth()
		case <-b.stop:
			return
		}
	}
}

// checkHealth uses discordgo's heartbeat-ACK timestamp as a liveness signal.
// discordgo updates LastHeartbeatAck on every ACK (~41s), so a stale value
// means the gateway is no longer alive. On staleness we alert once and force a
// reconnect; when the connection recovers we announce it once.
func (b *Bot) checkHealth() {
	if b.session == nil {
		return
	}
	b.session.RLock()
	lastAck := b.session.LastHeartbeatAck
	b.session.RUnlock()

	stale := time.Since(lastAck)
	if stale <= heartbeatStaleThreshold {
		// Healthy. If we had previously alerted, announce recovery exactly once.
		b.mu.Lock()
		recovered := b.alerted
		b.alerted = false
		b.mu.Unlock()
		if recovered {
			slog.Info("discord bot gateway recovered", "func", "Bot.checkHealth", "last_ack", lastAck.Format(time.RFC3339))
			b.alerts.NotifyDiscordBotRecovered()
		}
		return
	}

	slog.Error("discord bot gateway is stale; forcing reconnect",
		"func", "Bot.checkHealth",
		"last_ack", lastAck.Format(time.RFC3339),
		"stale_for", stale.Round(time.Second).String(),
	)

	// Alert once per outage (on the healthy->stale transition).
	b.mu.Lock()
	firstAlert := !b.alerted
	b.alerted = true
	b.mu.Unlock()
	if firstAlert {
		b.alerts.NotifyDiscordBotOffline(lastAck.Unix())
	}

	b.forceReconnect()
}

// forceReconnect tears down the wedged session and opens a fresh one. This is
// the same Close-then-Open pattern discordgo uses internally when its own
// heartbeat watchdog fires, so it is safe to race with discordgo's reconnect:
// whichever opens first wins and the other observes ErrWSAlreadyOpen.
func (b *Bot) forceReconnect() {
	if err := b.session.Close(); err != nil {
		slog.Warn("error closing stale discord session", "func", "Bot.forceReconnect", "err", err)
	}
	if err := b.session.Open(); err != nil {
		slog.Error("failed to reopen discord session; will retry on next watchdog tick", "func", "Bot.forceReconnect", "err", err)
		return
	}
	slog.Info("discord session reopened after staleness", "func", "Bot.forceReconnect")
}

// parsePingcordMessage looks at a Discord message's Content for (a) the first
// http(s) URL and (b) the first channel-name key from channelMap that appears
// anywhere in the text. Returns (key, url, true) only when both are found.
// Channel names are matched case-insensitively. Embeds are intentionally
// ignored so attacker-controlled embed text cannot trigger a match.
func parsePingcordMessage(m *discordgo.Message, channelMap map[string]string) (string, string, bool) {
	if m == nil || m.Content == "" {
		return "", "", false
	}
	text := m.Content

	url := urlRegex.FindString(text)
	if url == "" {
		return "", "", false
	}
	// Trim trailing punctuation that can ride along with a URL captured from prose.
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
