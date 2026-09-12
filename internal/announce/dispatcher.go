package announce

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/model"
)

// Store is the persistence the dispatcher needs. *store.Store implements it.
type Store interface {
	ListNotificationEvents(ctx context.Context, channelKey string) ([]model.NotificationEvent, error)
	ClaimNotificationSend(ctx context.Context, id int64, trigger string, now int64) (allowed bool, remaining int64, err error)
	RecordNotificationResult(ctx context.Context, id int64, delivered bool, errText string, now int64) error
	InsertNotificationLog(ctx context.Context, e model.NotificationLogEntry) error
}

// Config wires a Dispatcher.
type Config struct {
	Store Store
	// Channels is every configured channel's presentation, keyed by channel
	// key.
	Channels map[string]Channel
	// TranscriptBaseURL is the base for {transcript}, without a trailing
	// slash; the channel key is appended.
	TranscriptBaseURL string
	// OperatorWebhookURL receives the default-look rendering of every
	// observation, with no pings and a diagnostic footer. It is the
	// operator's own feed, not an audience webhook. Empty disables it.
	OperatorWebhookURL string
	Version            string
	// HTTPClient is injectable for tests; nil gets a default.
	HTTPClient *http.Client
	// Background runs deliveries off the caller's goroutine. It returns false
	// when the process is shutting down and the work was not started. Nil
	// means a plain goroutine.
	Background func(fn func()) bool
	// Shutdown, when non-nil, is cancelled when the process starts shutting
	// down. Deliveries in flight get shutdownGrace more seconds to finish,
	// then are cancelled, so a slow Discord cannot hold shutdown past the
	// container's kill window.
	Shutdown context.Context
	// OnLogged is called after a log row is written, so the admin page can be
	// woken. Nil is fine.
	OnLogged func(channelKey string)
}

// Dispatcher matches observations to notification events and delivers them.
type Dispatcher struct {
	store      Store
	channels   map[string]Channel
	transcript string
	operator   string
	version    string
	sender     *Sender
	background func(fn func()) bool
	shutdown   context.Context
	onLogged   func(channelKey string)
	now        func() time.Time
}

// Delivery bounds.
const (
	// ruleTimeout bounds one rule's delivery to all of its webhooks. Webhooks
	// post concurrently, so this is a few attempts' worth, not a sum.
	ruleTimeout = 60 * time.Second
	// operatorTimeout bounds the operator feed post. It shares nothing with
	// the audience rules: a rate-limited operator webhook must never delay a
	// ping.
	operatorTimeout = 30 * time.Second
	// listTimeout bounds the local database read that finds the rules.
	listTimeout = 10 * time.Second
	// shutdownGrace is how long an in-flight delivery may continue after the
	// process starts shutting down.
	shutdownGrace = 5 * time.Second
	// maxConcurrentWebhooks bounds the fan-out within one rule.
	maxConcurrentWebhooks = 4
)

// New constructs a dispatcher.
func New(cfg Config) *Dispatcher {
	d := &Dispatcher{
		store:      cfg.Store,
		channels:   cfg.Channels,
		transcript: strings.TrimRight(cfg.TranscriptBaseURL, "/"),
		operator:   strings.TrimSpace(cfg.OperatorWebhookURL),
		version:    cfg.Version,
		sender:     NewSender(cfg.HTTPClient),
		background: cfg.Background,
		shutdown:   cfg.Shutdown,
		onLogged:   cfg.OnLogged,
		now:        time.Now,
	}
	if d.channels == nil {
		d.channels = map[string]Channel{}
	}
	if d.background == nil {
		d.background = func(fn func()) bool { go fn(); return true }
	}
	return d
}

// SetHTTPClient swaps the client every webhook post goes through. It exists
// for tests, which point it at an httptest server through a rewriting
// transport - the URL validation deliberately refuses anything but Discord.
func (d *Dispatcher) SetHTTPClient(c *http.Client) {
	if d == nil {
		return
	}
	d.sender = NewSender(c)
}

// ChannelInfo returns a channel's presentation, falling back to its key.
func (d *Dispatcher) ChannelInfo(channelKey string) Channel {
	if d == nil {
		return Channel{Key: channelKey, DisplayName: channelKey}
	}
	if ch, ok := d.channels[channelKey]; ok {
		return ch
	}
	return Channel{Key: channelKey, DisplayName: channelKey}
}

// TranscriptURL is the {transcript} link for a channel.
func (d *Dispatcher) TranscriptURL(channelKey string) string {
	if d == nil || d.transcript == "" {
		return ""
	}
	return d.transcript + "/" + channelKey + "/"
}

// OperatorFeedConfigured reports whether the operator feed has somewhere to
// post, for the admin page.
func (d *Dispatcher) OperatorFeedConfigured() bool {
	return d != nil && d.operator != ""
}

// renderContext builds the render context for a payload.
func (d *Dispatcher) renderContext(p Payload) RenderContext {
	return RenderContext{
		Payload:       p,
		Channel:       d.ChannelInfo(p.ChannelKey),
		TranscriptURL: d.TranscriptURL(p.ChannelKey),
	}
}

// Preview renders what a rule would send for a payload, without sending.
func (d *Dispatcher) Preview(ev model.NotificationEvent, p Payload) Message {
	return Render(ev, d.renderContext(p))
}

// Dispatch announces an observation: every enabled rule for the payload's
// channel whose triggers include the payload's trigger is rendered and posted,
// subject to its cooldown, and the operator feed gets the default look. It
// returns immediately; delivery runs in the background so the detection path
// never waits on Discord. Nil-receiver-safe.
func (d *Dispatcher) Dispatch(p Payload) {
	if d == nil {
		return
	}
	d.run(func() { d.dispatch(p) })
}

// run executes fn in the background, or inline when no background task can
// be started because the process is shutting down. Every caller has already
// taken a durable claim - the ledger row, the cooldown - so dropping fn would
// lose an announcement forever, while running it inline only delays a
// shutdown that deliveryContext bounds anyway.
func (d *Dispatcher) run(fn func()) {
	if d.background(fn) {
		return
	}
	slog.Warn("running announcement work inline during shutdown", "func", "Dispatcher.run")
	fn()
}

// deliveryContext bounds one piece of delivery work, and additionally cuts
// it short shortly after shutdown begins.
func (d *Dispatcher) deliveryContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	if d.shutdown == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(d.shutdown, func() {
		time.AfterFunc(shutdownGrace, cancel)
	})
	return ctx, func() {
		stop()
		cancel()
	}
}

func (d *Dispatcher) dispatch(p Payload) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("recovered from panic in announcement dispatch", "func", "Dispatcher.dispatch", "panic", r)
		}
	}()

	// The operator feed is informational and shares nothing with the audience
	// rules: its own task, its own deadline.
	if d.operator != "" {
		d.run(func() {
			ctx, cancel := d.deliveryContext(operatorTimeout)
			defer cancel()
			d.postOperatorFeed(ctx, p)
		})
	}

	ctx, cancel := d.deliveryContext(listTimeout)
	events, err := d.store.ListNotificationEvents(ctx, p.ChannelKey)
	cancel()
	if err != nil {
		slog.Error("failed to list notification events", "func", "Dispatcher.dispatch",
			"key", p.ChannelKey, "trigger", p.Trigger, "err", err)
		return
	}
	for _, ev := range events {
		if !ev.Enabled || !slices.Contains(ev.Triggers, string(p.Trigger)) {
			continue
		}
		// Each rule gets its own task and deadline, so one rule's stalled
		// webhook cannot delay another rule's ping.
		d.run(func() {
			ctx, cancel := d.deliveryContext(ruleTimeout)
			defer cancel()
			d.deliver(ctx, ev, p)
		})
	}
}

// deliver runs one rule for one payload: claim the cooldown, render, post to
// every webhook, record what happened.
func (d *Dispatcher) deliver(ctx context.Context, ev model.NotificationEvent, p Payload) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("recovered from panic in announcement delivery", "func", "Dispatcher.deliver", "panic", r)
		}
	}()

	now := d.now()
	entry := model.NotificationLogEntry{
		ChannelKey:  p.ChannelKey,
		EventID:     ev.ID,
		UserID:      ev.UserID,
		EventName:   ev.Name,
		Trigger:     string(p.Trigger),
		Platform:    p.Platform,
		BroadcastID: p.ID,
		Title:       p.Title,
		URL:         p.URL,
		Webhooks:    len(ev.Webhooks),
		SentAt:      now.Unix(),
	}

	allowed, remaining, err := d.store.ClaimNotificationSend(ctx, ev.ID, string(p.Trigger), now.Unix())
	if err != nil {
		slog.Error("failed to claim notification cooldown", "func", "Dispatcher.deliver",
			"key", p.ChannelKey, "event", ev.ID, "err", err)
		return
	}
	if !allowed {
		entry.Status = model.NotificationStatusSuppressed
		entry.Detail = fmt.Sprintf("within the minimum gap between %s notifications (%s left)",
			p.Trigger.Label(), (time.Duration(remaining) * time.Second).Round(time.Second))
		slog.Info("announcement suppressed by cooldown", "func", "Dispatcher.deliver",
			"key", p.ChannelKey, "event", ev.ID, "name", ev.Name, "trigger", p.Trigger, "remaining_s", remaining)
		d.log(ctx, entry)
		return
	}

	msg := Render(ev, d.renderContext(p))
	if msg.IsEmpty() {
		entry.Status = model.NotificationStatusFailed
		entry.Detail = "the template rendered to an empty message"
		_ = d.store.RecordNotificationResult(ctx, ev.ID, false, entry.Detail, now.Unix())
		d.log(ctx, entry)
		return
	}

	delivered, failures := d.post(ctx, ev.Webhooks, msg, false)
	entry.Delivered = delivered
	switch {
	case delivered == len(ev.Webhooks):
		entry.Status = model.NotificationStatusSent
	case delivered > 0:
		entry.Status = model.NotificationStatusPartial
	default:
		entry.Status = model.NotificationStatusFailed
	}
	entry.Detail = strings.Join(failures, "; ")

	if err := d.store.RecordNotificationResult(ctx, ev.ID, delivered > 0, entry.Detail, d.now().Unix()); err != nil {
		slog.Error("failed to record notification result", "func", "Dispatcher.deliver", "event", ev.ID, "err", err)
	}
	slog.Info("announcement dispatched", "func", "Dispatcher.deliver",
		"key", p.ChannelKey, "event", ev.ID, "name", ev.Name, "trigger", p.Trigger,
		"status", entry.Status, "delivered", delivered, "webhooks", len(ev.Webhooks), "detail", entry.Detail)
	d.log(ctx, entry)
}

// post sends msg to every webhook concurrently (bounded) and reports how many
// accepted it, with a description of each failure in webhook order: the
// webhook's name when it has one, plus its masked URL, never the token.
func (d *Dispatcher) post(ctx context.Context, hooks []model.Webhook, msg Message, suppressMentions bool) (delivered int, failures []string) {
	results := make([]error, len(hooks))
	sem := make(chan struct{}, maxConcurrentWebhooks)
	var wg sync.WaitGroup
	for i, h := range hooks {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = d.sender.Send(ctx, u, msg, suppressMentions)
		}(i, h.URL)
	}
	wg.Wait()

	for i, err := range results {
		if err == nil {
			delivered++
			continue
		}
		failures = append(failures, describeFailure(hooks[i], err))
	}
	return delivered, failures
}

// describeFailure labels a delivery error with the webhook's name (if any)
// and masked URL.
func describeFailure(h model.Webhook, err error) string {
	var de *DeliveryError
	if errors.As(err, &de) {
		return h.Label(de.Webhook) + ": " + de.Detail()
	}
	return h.Label(MaskWebhookURL(h.URL)) + ": " + err.Error()
}

// postOperatorFeed renders the default look for the operator's own feed. No
// pings, and the footer carries the detection diagnostics that used to be the
// whole notification: which mechanism won and how far behind the platform's
// own start time it was.
func (d *Dispatcher) postOperatorFeed(ctx context.Context, p Payload) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("recovered from panic in operator feed post", "func", "Dispatcher.postOperatorFeed", "panic", r)
		}
	}()
	if d.operator == "" {
		return
	}
	rc := d.renderContext(p)
	rc.FooterOverride = d.operatorFooter(p)
	msg := Render(DefaultEvent(), rc)
	if err := d.sender.Send(ctx, d.operator, msg, true); err != nil {
		slog.Error("failed to post to the operator feed", "func", "Dispatcher.postOperatorFeed",
			"key", p.ChannelKey, "trigger", p.Trigger, "err", err)
	}
}

func (d *Dispatcher) operatorFooter(p Payload) string {
	parts := []string{"live detection"}
	if p.Mechanism != "" {
		parts = append(parts, "via "+p.Mechanism)
	}
	if p.Trigger == TriggerLive && !p.EventTime.IsZero() && !p.DetectedAt.IsZero() {
		delay := p.DetectedAt.Sub(p.EventTime).Round(time.Second)
		switch {
		case delay < 0:
			parts = append(parts, fmt.Sprintf("detected %s before the reported start", -delay))
		default:
			parts = append(parts, "delay "+delay.String())
		}
		if p.Platform == PlatformYouTube {
			if p.SawScheduled {
				parts = append(parts, "watched as scheduled")
			} else {
				parts = append(parts, "found by discovery")
			}
		}
	}
	if d.version != "" {
		parts = append(parts, "v"+strings.TrimPrefix(d.version, "v"))
	}
	return strings.Join(parts, " · ")
}

// TestResult is the outcome of one admin-initiated test send.
type TestResult struct {
	// Webhook is the target's label: its name if it has one, and its masked
	// URL. Never the token.
	Webhook   string `json:"webhook"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	Delivered int    `json:"-"`
}

// SendTest posts a rule's rendering of a sample payload to one webhook with
// every mention suppressed, records it in the log as a test, and reports the
// result synchronously so the admin page can show it. The rule need not be
// saved: the draft in the editor is what gets tested.
func (d *Dispatcher) SendTest(ctx context.Context, ev model.NotificationEvent, p Payload, hook model.Webhook) TestResult {
	res := TestResult{Webhook: hook.Label(MaskWebhookURL(hook.URL))}
	msg := Render(ev, d.renderContext(p))
	if msg.IsEmpty() {
		res.Error = "the template rendered to an empty message"
	} else if err := d.sender.Send(ctx, hook.URL, msg, true); err != nil {
		res.Error = describeFailure(hook, err)
	} else {
		res.OK = true
		res.Delivered = 1
	}

	entry := model.NotificationLogEntry{
		ChannelKey:  p.ChannelKey,
		EventID:     ev.ID,
		UserID:      ev.UserID,
		EventName:   ev.Name,
		Trigger:     string(p.Trigger),
		Platform:    p.Platform,
		BroadcastID: p.ID,
		Title:       p.Title,
		URL:         p.URL,
		Status:      model.NotificationStatusTest,
		Webhooks:    1,
		Delivered:   res.Delivered,
		SentAt:      d.now().Unix(),
	}
	if res.OK {
		entry.Detail = "test sent to " + res.Webhook + " with pings suppressed"
	} else {
		entry.Detail = "test failed: " + res.Error
	}
	d.log(ctx, entry)
	return res
}

func (d *Dispatcher) log(ctx context.Context, entry model.NotificationLogEntry) {
	if entry.Status != model.NotificationStatusTest {
		metrics.Announcements.WithLabelValues(entry.ChannelKey, entry.Trigger, entry.Status).Inc()
	}
	if err := d.store.InsertNotificationLog(ctx, entry); err != nil {
		slog.Error("failed to write notification log", "func", "Dispatcher.log", "key", entry.ChannelKey, "err", err)
		return
	}
	if d.onLogged != nil {
		d.onLogged(entry.ChannelKey)
	}
}
