package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"live-transcript-server/internal/announce"
	"live-transcript-server/internal/discord"
	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/model"
	"live-transcript-server/internal/store"
)

// maxNotificationBody bounds a notification request body. A rule is a few
// kilobytes at most; the limit only stops a hostile admin key from making the
// server buffer something large.
const maxNotificationBody = 256 << 10

// testSendTimeout bounds an admin test send, which runs synchronously so the
// page can show the result. One webhook, a bounded rate-limit wait.
const testSendTimeout = 30 * time.Second

// NotificationsResponse is the aggregated state returned by
// GET /{channel}/admin/notifications: the rules, the delivery log, the recent
// non-live observations, and the static vocabulary the editor needs
// (triggers, placeholders, defaults, limits) so the page and the server can
// never disagree about what a template may say.
type NotificationsResponse struct {
	Events       []model.NotificationEvent    `json:"events"`
	Log          []model.NotificationLogEntry `json:"log"`
	Videos       []model.DetectedVideo        `json:"videos"`
	Triggers     []announce.TriggerInfo       `json:"triggers"`
	Placeholders []announce.Placeholder       `json:"placeholders"`
	Defaults     NotificationDefaults         `json:"defaults"`
	Limits       NotificationLimits           `json:"limits"`
	// ChannelName is what {channel} expands to, so the editor can show it.
	ChannelName   string `json:"channelName"`
	TranscriptURL string `json:"transcriptUrl"`
	// OperatorFeed reports whether the operator's own feed (detectWebhookUrl)
	// is configured; every observation is mirrored there in the default look.
	OperatorFeed bool `json:"operatorFeed"`
	// QueueIncoming mirrors liveDetect.queueIncoming for the status card.
	QueueIncoming bool `json:"queueIncoming"`
}

// NotificationDefaults is what a new rule starts from.
type NotificationDefaults struct {
	Content         string              `json:"content"`
	EmbedEnabled    bool                `json:"embedEnabled"`
	Embed           model.EmbedTemplate `json:"embed"`
	CooldownSeconds int64               `json:"cooldownSeconds"`
}

// NotificationLimits are the Discord limits the editor enforces client-side
// before the server does.
type NotificationLimits struct {
	Name             int `json:"name"`
	Content          int `json:"content"`
	EmbedTitle       int `json:"embedTitle"`
	EmbedDescription int `json:"embedDescription"`
	EmbedFooter      int `json:"embedFooter"`
	Webhooks         int `json:"webhooks"`
	CooldownSeconds  int `json:"cooldownSeconds"`
}

// defaultCooldownSeconds is the suggested minimum gap for a new rule: long
// enough to swallow a stream restart, short enough not to hide a second
// stream in the same evening.
const defaultCooldownSeconds = 30 * 60

func notificationDefaults() NotificationDefaults {
	return NotificationDefaults{
		Content:         announce.DefaultContent,
		EmbedEnabled:    true,
		Embed:           announce.DefaultEmbed(),
		CooldownSeconds: defaultCooldownSeconds,
	}
}

func notificationLimits() NotificationLimits {
	return NotificationLimits{
		Name:             announce.MaxEventNameLength,
		Content:          announce.MaxContentLength,
		EmbedTitle:       announce.MaxEmbedTitle,
		EmbedDescription: announce.MaxEmbedDescription,
		EmbedFooter:      announce.MaxEmbedFooter,
		Webhooks:         announce.MaxWebhooksPerEvent,
		CooldownSeconds:  announce.MaxCooldownSeconds,
	}
}

func (app *App) getAdminNotificationsHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	ctx := r.Context()

	events, err := app.Store.ListNotificationEvents(ctx, cs.Key)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to list notification events", "key", cs.Key, "func", "getAdminNotificationsHandler", "err", err)
		return
	}
	if events == nil {
		events = []model.NotificationEvent{}
	}

	// The log and the video ledger are diagnostic: a read failure must not
	// take the whole tab down.
	log, err := app.Store.ListNotificationLog(ctx, cs.Key, 50)
	if err != nil {
		slog.Error("failed to list notification log", "key", cs.Key, "func", "getAdminNotificationsHandler", "err", err)
	}
	if log == nil {
		log = []model.NotificationLogEntry{}
	}
	videos, err := app.Store.GetRecentVideoDetections(ctx, cs.Key, 20)
	if err != nil {
		slog.Error("failed to list video detections", "key", cs.Key, "func", "getAdminNotificationsHandler", "err", err)
	}
	if videos == nil {
		videos = []model.DetectedVideo{}
	}

	writeJSON(w, NotificationsResponse{
		Events:        events,
		Log:           log,
		Videos:        videos,
		Triggers:      announce.Triggers,
		Placeholders:  announce.Placeholders,
		Defaults:      notificationDefaults(),
		Limits:        notificationLimits(),
		ChannelName:   app.Announcer.ChannelInfo(cs.Key).DisplayName,
		TranscriptURL: app.Announcer.TranscriptURL(cs.Key),
		OperatorFeed:  app.Announcer.OperatorFeedConfigured(),
		QueueIncoming: app.QueueIncoming,
	})
}

// decodeNotificationEvent reads a rule from the request body and normalizes
// it. The channel, id and delivery trail are never taken from the body: the
// channel comes from the route (so a rule cannot be written into another
// channel) and the trail belongs to the dispatcher.
func decodeNotificationEvent(w http.ResponseWriter, r *http.Request, cs *ChannelState) (*model.NotificationEvent, bool) {
	var ev model.NotificationEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxNotificationBody)).Decode(&ev); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return nil, false
	}
	ev.ChannelKey = cs.Key
	ev.ID = 0
	ev.LastSentAt, ev.SentCount, ev.LastError, ev.LastErrorAt, ev.CreatedAt, ev.UpdatedAt = 0, 0, "", 0, 0, 0

	if err := announce.Normalize(&ev); err != nil {
		if announce.IsValidationError(err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			metrics.Http400Errors.Inc()
			return nil, false
		}
		http.Error(w, "Invalid notification event", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return nil, false
	}
	return &ev, true
}

// notificationAuditFields describes a rule for the admin audit log. Webhook
// URLs are masked: the audit webhook is a different audience from the rule's
// own webhooks, and the token must not travel between them.
func notificationAuditFields(ev *model.NotificationEvent) []discord.AdminField {
	masked := make([]string, 0, len(ev.Webhooks))
	for _, h := range ev.Webhooks {
		masked = append(masked, h.Label(announce.MaskWebhookURL(h.URL)))
	}
	return []discord.AdminField{
		{Name: "Event", Value: ev.Name, Inline: true},
		{Name: "ID", Value: strconv.FormatInt(ev.ID, 10), Inline: true},
		{Name: "Enabled", Value: yesNo(ev.Enabled), Inline: true},
		{Name: "Triggers", Value: strings.Join(ev.Triggers, ", "), Inline: true},
		{Name: "Min. gap", Value: (time.Duration(ev.CooldownSeconds) * time.Second).String(), Inline: true},
		{Name: "Embed", Value: yesNo(ev.EmbedEnabled), Inline: true},
		{Name: "Webhooks", Value: strings.Join(masked, "\n")},
	}
}

// postAdminNotificationHandler creates a rule.
func (app *App) postAdminNotificationHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	ev, ok := decodeNotificationEvent(w, r, cs)
	if !ok {
		return
	}
	now := time.Now().Unix()
	id, err := app.Store.CreateNotificationEvent(r.Context(), *ev, now)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to create notification event", "key", cs.Key, "func", "postAdminNotificationHandler", "err", err)
		return
	}
	ev.ID, ev.CreatedAt, ev.UpdatedAt = id, now, now

	app.bumpAdminChange(cs.Key)
	app.notifyAdminAction(r, cs, "Created notification event", notificationAuditFields(ev)...)
	slog.Info("admin created notification event", "key", cs.Key, "func", "postAdminNotificationHandler",
		"id", id, "name", ev.Name, "triggers", ev.Triggers, "webhooks", len(ev.Webhooks), "cooldown_s", ev.CooldownSeconds)
	// The content type has to be set before the status is written, or the
	// page receives JSON labelled as text and cannot read the new id.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(ev); err != nil {
		slog.Error("failed to write notification event response", "key", cs.Key, "func", "postAdminNotificationHandler", "err", err)
	}
}

// notificationID parses the {id} path value.
func notificationID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid notification event id", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return 0, false
	}
	return id, true
}

// putAdminNotificationHandler replaces a rule's editable fields. The delivery
// trail survives the edit.
func (app *App) putAdminNotificationHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	id, ok := notificationID(w, r)
	if !ok {
		return
	}
	ev, ok := decodeNotificationEvent(w, r, cs)
	if !ok {
		return
	}
	ev.ID = id

	err := app.Store.UpdateNotificationEvent(r.Context(), *ev, time.Now().Unix())
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "notification event not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to update notification event", "key", cs.Key, "func", "putAdminNotificationHandler", "id", id, "err", err)
		return
	}

	// Re-read so the response carries the trail the body could not.
	stored, err := app.Store.GetNotificationEvent(r.Context(), cs.Key, id)
	if err != nil || stored == nil {
		slog.Error("failed to re-read notification event after update", "key", cs.Key, "func", "putAdminNotificationHandler", "id", id, "err", err)
		stored = ev
	}

	app.bumpAdminChange(cs.Key)
	app.notifyAdminAction(r, cs, "Edited notification event", notificationAuditFields(stored)...)
	slog.Info("admin edited notification event", "key", cs.Key, "func", "putAdminNotificationHandler",
		"id", id, "name", stored.Name, "enabled", stored.Enabled, "triggers", stored.Triggers, "webhooks", len(stored.Webhooks))
	writeJSON(w, stored)
}

// deleteAdminNotificationLogHandler empties the channel's delivery log. The
// log is a record for the admin page only; nothing reads it back, so this
// cannot change what gets sent.
func (app *App) deleteAdminNotificationLogHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	removed, err := app.Store.ClearNotificationLog(r.Context(), cs.Key)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to clear notification log", "key", cs.Key, "func", "deleteAdminNotificationLogHandler", "err", err)
		return
	}
	app.bumpAdminChange(cs.Key)
	app.notifyAdminAction(r, cs, "Cleared notification deliveries",
		discord.AdminField{Name: "Entries Removed", Value: strconv.FormatInt(removed, 10), Inline: true},
	)
	slog.Info("admin cleared notification log", "key", cs.Key, "func", "deleteAdminNotificationLogHandler", "removed", removed)
	w.WriteHeader(http.StatusNoContent)
}

// Detection history that is still doing a job is kept when the admin clears
// it: an ended broadcast is held for a while against the platform's stale
// cache reporting it live again, and a video stays for the whole announcement
// recency window, since a restart re-reads the uploads playlist and the ledger
// row is what stops the video being announced twice.
const (
	detectionClearEndedGrace = time.Hour
	detectionClearVideoGrace = 6 * time.Hour
)

// deleteAdminDetectionsHandler clears the channel's detection history as far
// as that is safe (see the graces above).
func (app *App) deleteAdminDetectionsHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	now := time.Now()
	broadcasts, videos, err := app.Store.ClearDetectionHistory(r.Context(), cs.Key,
		now.Add(-detectionClearEndedGrace).Unix(), now.Add(-detectionClearVideoGrace).Unix())
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to clear detection history", "key", cs.Key, "func", "deleteAdminDetectionsHandler", "err", err)
		return
	}
	app.bumpAdminChange(cs.Key)
	app.notifyAdminAction(r, cs, "Cleared detection history",
		discord.AdminField{Name: "Broadcasts Removed", Value: strconv.FormatInt(broadcasts, 10), Inline: true},
		discord.AdminField{Name: "Videos Removed", Value: strconv.FormatInt(videos, 10), Inline: true},
	)
	slog.Info("admin cleared detection history", "key", cs.Key, "func", "deleteAdminDetectionsHandler",
		"broadcasts", broadcasts, "videos", videos)
	w.WriteHeader(http.StatusNoContent)
}

// deleteAdminNotificationHandler removes a rule.
func (app *App) deleteAdminNotificationHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	id, ok := notificationID(w, r)
	if !ok {
		return
	}

	// Read first so the audit record can say what was deleted.
	before, err := app.Store.GetNotificationEvent(r.Context(), cs.Key, id)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to look up notification event", "key", cs.Key, "func", "deleteAdminNotificationHandler", "id", id, "err", err)
		return
	}
	if before == nil {
		http.Error(w, "notification event not found", http.StatusNotFound)
		return
	}

	rows, err := app.Store.DeleteNotificationEvent(r.Context(), cs.Key, id)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		metrics.Http500Errors.Inc()
		slog.Error("failed to delete notification event", "key", cs.Key, "func", "deleteAdminNotificationHandler", "id", id, "err", err)
		return
	}
	if rows == 0 {
		http.Error(w, "notification event not found", http.StatusNotFound)
		return
	}

	app.bumpAdminChange(cs.Key)
	app.notifyAdminAction(r, cs, "Deleted notification event", notificationAuditFields(before)...)
	slog.Info("admin deleted notification event", "key", cs.Key, "func", "deleteAdminNotificationHandler", "id", id, "name", before.Name)
	w.WriteHeader(http.StatusNoContent)
}

// notificationDraftRequest is the body of preview and test: a rule as the
// editor currently has it (saved or not), and which trigger to render it for.
type notificationDraftRequest struct {
	Event   model.NotificationEvent `json:"event"`
	Trigger string                  `json:"trigger"`
	// WebhookURL is the test target. It need not be one of the rule's own
	// webhooks - an admin can point a test at a private channel - but it
	// must be a Discord webhook URL like any other. WebhookName is its label
	// for the result and the log, if it has one.
	WebhookURL  string `json:"webhookUrl"`
	WebhookName string `json:"webhookName"`
}

// NotificationPreviewResponse is what a rule would send: the rendered content
// and embed exactly as they would go to Discord, plus the sample the
// placeholders were filled from.
type NotificationPreviewResponse struct {
	Trigger string         `json:"trigger"`
	Content string         `json:"content"`
	Embed   map[string]any `json:"embed,omitempty"`
	Sample  map[string]any `json:"sample"`
}

// decodeNotificationDraft reads a preview/test body. The draft is normalized
// for its side effects (trimming, color canonicalization) but validation
// problems are returned rather than fatal: a preview of a half-finished rule
// is exactly what the editor wants to show.
func decodeNotificationDraft(w http.ResponseWriter, r *http.Request, cs *ChannelState) (*notificationDraftRequest, announce.Trigger, bool) {
	var req notificationDraftRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxNotificationBody)).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return nil, "", false
	}
	req.Event.ChannelKey = cs.Key
	if err := announce.Normalize(&req.Event); err != nil && !announce.IsValidationError(err) {
		http.Error(w, "Invalid notification event", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return nil, "", false
	}

	trigger := announce.Trigger(strings.ToLower(strings.TrimSpace(req.Trigger)))
	if trigger == "" {
		if len(req.Event.Triggers) > 0 {
			trigger = announce.Trigger(req.Event.Triggers[0])
		} else {
			trigger = announce.TriggerLive
		}
	}
	if !announce.KnownTrigger(string(trigger)) {
		http.Error(w, "unknown trigger", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return nil, "", false
	}
	return &req, trigger, true
}

// previewPayload builds the payload previews and tests are rendered from: the
// channel's most recent detection for the trigger, with anything it lacks
// left blank. Only a local build, where no detection will ever be recorded,
// substitutes the stand-in video for a channel that has nothing yet; a
// deployment previews blank details rather than someone else's stream.
func (app *App) previewPayload(ctx context.Context, cs *ChannelState, trigger announce.Trigger) announce.Payload {
	// The live trigger previews the newest broadcast on either platform; the
	// video triggers can only borrow a YouTube one, so for them the newest
	// YouTube broadcast is the one worth passing along.
	var live *model.DetectedBroadcast
	if dets, err := app.Store.GetRecentDetections(ctx, cs.Key, 20); err == nil {
		for i := range dets {
			if trigger == announce.TriggerLive || dets[i].Platform == announce.PlatformYouTube {
				live = &dets[i]
				break
			}
		}
	}
	var video *model.DetectedVideo
	if trigger != announce.TriggerLive {
		if vids, err := app.Store.GetRecentVideoDetections(ctx, cs.Key, 50); err == nil {
			for i := range vids {
				if vids[i].Kind == string(trigger) {
					video = &vids[i]
					break
				}
			}
		}
	}
	ch := app.Announcer.ChannelInfo(cs.Key)
	p := announce.PreviewPayload(trigger, ch, live, video, time.Now())
	if p.ID == "" && app.localBuild() {
		p = announce.SamplePayload(trigger, ch, time.Now())
	}
	return p
}

// sampleDescription tells the editor where a preview's details came from, so
// it can say why a field is blank: "recent" is the channel's own detection,
// "sample" the stand-in video a local build falls back to, and "none" means
// the channel has nothing for this trigger yet.
func sampleDescription(p announce.Payload) map[string]any {
	source := "recent"
	switch {
	case p.IsSample():
		source = "sample"
	case p.ID == "":
		source = "none"
	}
	var eventTime int64
	if !p.EventTime.IsZero() {
		eventTime = p.EventTime.Unix()
	}
	return map[string]any{
		"platform":  p.Platform,
		"id":        p.ID,
		"url":       p.URL,
		"title":     p.Title,
		"eventTime": eventTime,
		"ended":     p.Ended,
		"source":    source,
	}
}

// postAdminNotificationPreviewHandler renders a draft without sending it.
// Read-only: no audit record, no change counter bump.
func (app *App) postAdminNotificationPreviewHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	req, trigger, ok := decodeNotificationDraft(w, r, cs)
	if !ok {
		return
	}
	payload := app.previewPayload(r.Context(), cs, trigger)
	msg := app.Announcer.Preview(req.Event, payload)
	writeJSON(w, NotificationPreviewResponse{
		Trigger: string(trigger),
		Content: msg.Content,
		Embed:   msg.Embed,
		Sample:  sampleDescription(payload),
	})
}

// postAdminNotificationTestHandler posts a draft's rendering to one webhook
// with every mention suppressed. It is the one admin-initiated path to an
// audience webhook, so it is confirmed in the UI, logged as a test, and
// recorded in the audit trail with the webhook masked.
func (app *App) postAdminNotificationTestHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	req, trigger, ok := decodeNotificationDraft(w, r, cs)
	if !ok {
		return
	}
	webhookURL := strings.TrimSpace(req.WebhookURL)
	if !announce.ValidWebhookURL(webhookURL) {
		http.Error(w, "webhookUrl must be a Discord webhook URL (https://discord.com/api/webhooks/…)", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
		return
	}

	hook := model.Webhook{Name: strings.TrimSpace(req.WebhookName), URL: webhookURL}
	if len([]rune(hook.Name)) > announce.MaxWebhookNameLength {
		hook.Name = ""
	}

	ctx, cancel := context.WithTimeout(r.Context(), testSendTimeout)
	defer cancel()
	payload := app.previewPayload(ctx, cs, trigger)
	result := app.Announcer.SendTest(ctx, req.Event, payload, hook)

	app.bumpAdminChange(cs.Key)
	app.notifyAdminAction(r, cs, "Sent notification test",
		discord.AdminField{Name: "Event", Value: req.Event.Name, Inline: true},
		discord.AdminField{Name: "Trigger", Value: string(trigger), Inline: true},
		discord.AdminField{Name: "Webhook", Value: result.Webhook, Inline: true},
		discord.AdminField{Name: "Delivered", Value: yesNo(result.OK), Inline: true},
		discord.AdminField{Name: "Result", Value: firstNonEmpty(result.Error, "ok, pings suppressed")},
	)
	slog.Info("admin sent notification test", "key", cs.Key, "func", "postAdminNotificationTestHandler",
		"name", req.Event.Name, "trigger", trigger, "webhook", result.Webhook, "ok", result.OK, "err", result.Error)
	writeJSON(w, result)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
