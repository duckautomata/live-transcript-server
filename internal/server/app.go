// Package server is the application core: it owns the App composition root,
// per-channel state, the HTTP route table and handlers, stream lifecycle, and
// background maintenance. Transport-independent building blocks live in their
// own packages (store, storage, media, ws, notify, discord, archive); this
// package wires them together and speaks HTTP.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"live-transcript-server/internal/announce"
	"live-transcript-server/internal/archive"
	"live-transcript-server/internal/config"
	"live-transcript-server/internal/discord"
	"live-transcript-server/internal/livedetect"
	"live-transcript-server/internal/media"
	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/notify"
	"live-transcript-server/internal/storage"
	"live-transcript-server/internal/store"
	"live-transcript-server/internal/ws"

	"github.com/gorilla/websocket"
)

// ChannelState holds the per-channel configuration and runtime state.
type ChannelState struct {
	Key             string
	AdminKey        string
	MembersName     string // archive-server channel name; empty disables membership-key admin
	BaseMediaFolder string
	NumPastStreams  int
	Hub             *ws.Hub

	// AdminChangeCounter versions the admin-visible state of the channel for
	// the GET /{channel}/admin/poll long poll. Bumped (via bumpAdminChange)
	// on incoming/restart/stream changes; seeded from the clock so a client
	// holding a pre-restart counter resyncs immediately.
	AdminChangeCounter atomic.Int64
}

// App holds the application-wide dependencies and configuration.
type App struct {
	ApiKey string
	// AdminKey gates the site-wide admin page and API (handlers_site_admin.go);
	// empty disables them.
	AdminKey   string
	Store      *store.Store
	Storage    storage.Storage
	Media      media.Processor
	Discord    *discord.Client
	DiscordBot *discord.Bot
	// LiveDetect observes YouTube and Twitch for channels going live. It is
	// nil when unconfigured, and every method is nil-receiver-safe.
	LiveDetect *livedetect.Detector
	// Announcer turns detections into the admin-configured public Discord
	// announcements (and the operator's own feed). Always constructed.
	Announcer *announce.Dispatcher
	// Accounts governs the self-service accounts behind notification events
	// (see handlers_auth.go); authLimits are the request limiters on the
	// account endpoints.
	Accounts   config.AccountsConfig
	authLimits authLimits
	// QueueIncoming is liveDetect.queueIncoming: whether a detected live
	// broadcast is queued for the worker, or only observed and announced.
	QueueIncoming bool
	// TwitchTitleLookup fills in the title of a Twitch broadcast that was
	// detected without one (EventSub carries none). Called after the ledger
	// claim and the queue write, before the announcement. Nil disables it;
	// tests set it to a stub so nothing reaches Helix.
	TwitchTitleLookup func(ctx context.Context, channelKey string) string
	Archive           *archive.Client
	Notifier          *notify.Notifier
	Upgrader          websocket.Upgrader
	Channels          map[string]*ChannelState
	MaxConn           int
	MaxClipSize       int
	TempDir           string
	// Vods tracks in-flight full-VOD builds so concurrent admin requests for
	// the same stream collapse into a single build. See vod.go.
	Vods *vodRegistry

	IncomingStreamTTL time.Duration
	Version           string
	BuildTime         string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// bgMu guards bgClosed, which gates every deferred background task started
	// from a request handler. A sync.WaitGroup must not take a positive delta
	// from zero once Wait has begun, so the flag and the Add have to be set
	// under the same lock Close takes before waiting.
	bgMu     sync.Mutex
	bgClosed bool
}

// LocalVersion is the version string of a binary built and run by hand, with
// no VERSION set: every deployed image carries a real version (or "dev" /
// "unknown" from the Dockerfile). A few conveniences that must never reach a
// deployment - the stand-in video behind a blank notification preview - key
// off it.
const LocalVersion = "local"

// localBuild reports whether this process is a hand-built local binary
// rather than a deployment.
func (app *App) localBuild() bool {
	return app.Version == LocalVersion
}

// NewApp wires the application together. It performs no environment side
// effects (no mkdirs, no DB writes) - those live in Init so construction can
// never half-succeed and tests can build an App without touching the world.
func NewApp(cfg config.Config, st *store.Store, tempDir, version, buildTime string) (*App, error) {
	ttlMinutes := cfg.Discord.Bot.StreamTTLMinutes
	if ttlMinutes <= 0 {
		ttlMinutes = 24 * 60 // 24 hours
	}

	app := &App{
		ApiKey:   cfg.Credentials.ApiKey,
		AdminKey: cfg.Credentials.AdminKey,
		Store:    st,
		Media:    media.FFmpeg{},
		Discord:  discord.NewClient(cfg.Discord, version, cfg.Channels),
		Archive:  archive.NewClient(cfg.ArchiveURL, cfg.ArchiveKey),
		Upgrader: websocket.Upgrader{
			ReadBufferSize:    1024,
			WriteBufferSize:   1024,
			EnableCompression: true,
			CheckOrigin:       func(r *http.Request) bool { return true },
		},
		Notifier:          notify.New(),
		Channels:          make(map[string]*ChannelState),
		Vods:              newVodRegistry(),
		MaxConn:           10_000, // through testing, assuming a steady flow of connections, 10k connections will use 200 millicores
		MaxClipSize:       40,
		TempDir:           tempDir,
		IncomingStreamTTL: time.Duration(ttlMinutes) * time.Minute,
		Version:           version,
		BuildTime:         buildTime,
		QueueIncoming:     cfg.LiveDetect.QueueIncoming,
		Accounts:          cfg.Accounts,
		authLimits:        newAuthLimits(cfg.Accounts),
	}
	app.ctx, app.cancel = context.WithCancel(context.Background())

	// The announcer is the only thing allowed to post to audience webhooks.
	// It runs deliveries through goBackground so shutdown waits for a post in
	// flight, and wakes the admin page whenever it writes a log row.
	app.Announcer = announce.New(announce.Config{
		Store:              st,
		Channels:           announceChannels(cfg.Channels),
		TranscriptBaseURL:  discord.TranscriptBaseURL(cfg.Discord.TranscriptBaseURL, version),
		OperatorWebhookURL: app.Discord.OperatorWebhookURL(),
		Version:            version,
		Background:         app.goBackground,
		Shutdown:           app.ctx,
		OnLogged:           app.bumpAdminChange,
	})

	for _, cc := range cfg.Channels {
		cs := &ChannelState{
			Key:             cc.Name,
			AdminKey:        cc.AdminKey,
			MembersName:     cc.MembersName,
			BaseMediaFolder: filepath.Join(tempDir, cc.Name),
			NumPastStreams:  cc.NumPastStreams,
			Hub:             ws.NewHub(cc.Name, app.MaxConn),
		}
		cs.AdminChangeCounter.Store(time.Now().UnixMilli())
		app.Channels[cc.Name] = cs
	}

	mediaStore, err := storage.New(app.ctx, cfg.Storage, tempDir)
	if err != nil {
		return nil, fmt.Errorf("initialize storage: %w", err)
	}
	app.Storage = mediaStore

	bot, err := discord.NewBot(cfg.Discord.Bot, app, app.Discord)
	if err != nil {
		return nil, fmt.Errorf("construct discord bot: %w", err)
	}
	app.DiscordBot = bot

	// A misconfigured detector must never stop the server from starting: it is
	// a shadow-mode observer, and the transcript service is the actual product.
	// New disables the unusable legs and reports why; we log and carry on.
	detector, err := livedetect.New(cfg.LiveDetect, cfg.Channels, app, app.Discord)
	if err != nil {
		slog.Error("live detection is degraded or disabled", "func", "NewApp", "err", err)
	}
	app.LiveDetect = detector
	app.TwitchTitleLookup = detector.LookupTwitchTitle

	return app, nil
}

// announceChannels builds the announcement presentation of every channel,
// with the same defaults the Discord client applies: the display name falls
// back to the key, and the Twitch login to the lowercased display name.
func announceChannels(channels []config.ChannelConfig) map[string]announce.Channel {
	out := make(map[string]announce.Channel, len(channels))
	for _, cc := range channels {
		ch := announce.Channel{
			Key:         cc.Name,
			DisplayName: cc.DisplayName,
			TwitchLogin: strings.ToLower(strings.TrimSpace(cc.TwitchLogin)),
		}
		if ch.DisplayName == "" {
			ch.DisplayName = cc.Name
		}
		if ch.TwitchLogin == "" {
			ch.TwitchLogin = strings.ToLower(ch.DisplayName)
		}
		out[cc.Name] = ch
	}
	return out
}

// Init performs the environment side effects the app needs before serving:
// temp/media directories and worker-status seeding.
func (app *App) Init(ctx context.Context) error {
	if err := os.MkdirAll(app.TempDir, 0755); err != nil {
		return fmt.Errorf("create temp folder: %w", err)
	}
	for _, cs := range app.Channels {
		if err := os.MkdirAll(cs.BaseMediaFolder, 0755); err != nil {
			return fmt.Errorf("create base media folder for %s: %w", cs.Key, err)
		}
	}

	// Reset worker status to remove stale entries, then seed a row per channel.
	if err := app.Store.ResetWorkerStatus(ctx); err != nil {
		return fmt.Errorf("reset worker status: %w", err)
	}
	now := time.Now().Unix()
	for _, cs := range app.Channels {
		if err := app.Store.UpsertWorkerStatus(ctx, cs.Key, "N/A", app.BuildTime, now); err != nil {
			return fmt.Errorf("initialize worker status for %s: %w", cs.Key, err)
		}
	}
	return nil
}

// Close stops all background tasks, waits for per-channel writers, and closes
// the database.
func (app *App) Close() error {
	if app.cancel != nil {
		app.cancel()
	}
	// Refuse new background work before waiting, so nothing can Add after the
	// Wait has started.
	app.bgMu.Lock()
	app.bgClosed = true
	app.bgMu.Unlock()

	app.wg.Wait()
	for _, cs := range app.Channels {
		cs.Hub.Wait()
	}
	if app.Store != nil {
		return app.Store.Close()
	}
	return nil
}

// goBackground starts a tracked background task, unless the app is shutting
// down. It returns whether the task was started.
//
// The lock is what makes this safe rather than merely likely-safe: Close sets
// bgClosed under the same mutex before calling wg.Wait, so an Add can never
// race a Wait that has already begun.
func (app *App) goBackground(fn func()) bool {
	app.bgMu.Lock()
	if app.bgClosed {
		app.bgMu.Unlock()
		return false
	}
	app.wg.Add(1)
	app.bgMu.Unlock()

	go func() {
		defer app.wg.Done()
		fn()
	}()
	return true
}

// QueueIncomingStream records an announced stream URL for a channel and wakes
// the admin/worker long polls. It implements discord.StreamSink, so the
// Discord bot (and any future announcement source) queues work through this
// single entry point.
func (app *App) QueueIncomingStream(ctx context.Context, channelKey, url string) error {
	if _, ok := app.Channels[channelKey]; !ok {
		return fmt.Errorf("unknown channel %q", channelKey)
	}
	if err := app.Store.UpsertIncomingStream(ctx, channelKey, url, time.Now().Unix()); err != nil {
		return err
	}
	app.bumpAdminChange(channelKey)
	return nil
}

// bumpAdminChange advances the channel's admin change counter and wakes every
// parked long poll (both GET /events and GET /{channel}/admin/poll - they
// share the app-wide signal; a spurious wakeup costs one cheap recheck).
// Call it after any write an admin page viewer should see promptly: incoming
// queue changes, restart flag changes, and stream state changes. Deliberately
// NOT bumped: worker status heartbeats and client connect/disconnect - they
// churn constantly and the page has a slow fallback refresh for them.
func (app *App) bumpAdminChange(channelKey string) {
	cs, ok := app.Channels[channelKey]
	if !ok {
		return
	}
	cs.AdminChangeCounter.Add(1)
	app.Notifier.Notify()
}

// membershipEnabled reports whether membership-key management is available for
// the given channel. It requires the archive server to be configured globally
// and the channel to have an archive-side name mapped. Fail closed: any
// missing piece disables the feature.
func (app *App) membershipEnabled(cs *ChannelState) bool {
	return app.Archive.Configured() && cs.MembersName != ""
}

// notifyAdminAction posts an audit record of a completed admin operation to
// Discord, tagged with the channel it acted on and the endpoint that did it.
//
// Call it only on the success path - a handler that failed returns before
// reaching it. Read-only admin endpoints (info, poll, membership list) do not
// call it: the admin page polls them continuously and would flood the webhook.
func (app *App) notifyAdminAction(r *http.Request, cs *ChannelState, action string, fields ...discord.AdminField) {
	fields = append(fields,
		discord.AdminField{Name: "Endpoint", Value: fmt.Sprintf("%s %s", r.Method, r.URL.Path)},
	)
	app.Discord.NotifyAdminAction(cs.Key, action, fields...)
}

// yesNo renders a bool for a human-readable notification field.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// isClientGone reports whether err is a request-context cancellation ,
// typically a client disconnect - rather than a real server failure.
func isClientGone(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// report500 records a server-side error: bumps the 500 metric, notifies
// Discord, and logs at Error level. If the error is a client disconnect,
// it logs at Info level and skips the alert. The caller is responsible
// for writing the HTTP response and any cleanup.
func (app *App) report500(r *http.Request, err error, msg string, attrs ...any) {
	if isClientGone(err) {
		slog.Info("client canceled request", append(attrs, "path", r.URL.Path, "err", err)...)
		return
	}
	metrics.Http500Errors.Inc()
	app.Discord.Notify500Error(fmt.Errorf("%s: %w", msg, err), r.URL.Path)
	slog.Error(msg, append(attrs, "err", err)...)
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("failed to write JSON response", "err", err)
	}
}
