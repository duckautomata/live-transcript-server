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
	"live-transcript-server/internal/livedetect"
	"live-transcript-server/internal/metrics"
	"live-transcript-server/internal/model"
	"live-transcript-server/internal/store"
)

// Notification events are the "Pingcord-like" public Discord announcements
// driven by live detection. They belong to accounts on the live-transcript
// site (handlers_auth.go): each account creates its own rules for a channel,
// with its own webhooks, and can see nothing of anyone else's. The operator
// keeps a read-only view - names, owners, triggers, the delivery log - on the
// admin page, with every webhook masked, plus a moderation delete.

// maxNotificationBody bounds a notification request body. A rule is a few
// kilobytes at most; the limit only stops a client from making the server
// buffer something large.
const maxNotificationBody = 256 << 10

// testSendTimeout bounds a test send, which runs synchronously so the page
// can show the result. One webhook, a bounded rate-limit wait.
const testSendTimeout = 30 * time.Second

// Per-account bounds. Rules are cheap to hold but each fires a webhook post
// per detection, and a test send posts wherever the account points it.
const (
	maxEventsPerUserPerChannel = 100
	testSendsPerHour           = 30
	// ruleWritesPerHour bounds creates, edits and deletes per account: each
	// posts an audit record to the operator's webhook and wakes the admin page.
	ruleWritesPerHour = 60
)

// allowRuleWrite applies the per-account write bound, answering 429 itself.
func (app *App) allowRuleWrite(w http.ResponseWriter, u *authedUser) bool {
	if ok, wait := app.authLimits.writes.Allow(strconv.FormatInt(u.User.ID, 10)); !ok {
		tooMany(w, wait, "Too many changes in a short time. Try again in "+humanDuration(wait)+".")
		return false
	}
	return true
}

// NotificationsResponse is the aggregated state returned by
// GET /{channel}/notifications for the signed-in account: its rules and
// delivery trail on this channel, plus the static vocabulary the editor
// needs (triggers, placeholders, defaults, limits) so the site and the server
// can never disagree about what a template may say.
type NotificationsResponse struct {
	Events       []model.NotificationEvent    `json:"events"`
	Log          []model.NotificationLogEntry `json:"log"`
	Triggers     []announce.TriggerInfo       `json:"triggers"`
	Placeholders []announce.Placeholder       `json:"placeholders"`
	Defaults     NotificationDefaults         `json:"defaults"`
	Limits       NotificationLimits           `json:"limits"`
	// ChannelName is what {channel} expands to, so the editor can show it.
	ChannelName   string `json:"channelName"`
	TranscriptURL string `json:"transcriptUrl"`
}

// NotificationDefaults is what a new rule starts from.
type NotificationDefaults struct {
	Content         string              `json:"content"`
	EmbedEnabled    bool                `json:"embedEnabled"`
	Embed           model.EmbedTemplate `json:"embed"`
	CooldownSeconds int64               `json:"cooldownSeconds"`
}

// NotificationLimits are the limits the editor enforces client-side before
// the server does.
type NotificationLimits struct {
	Name             int `json:"name"`
	Content          int `json:"content"`
	EmbedTitle       int `json:"embedTitle"`
	EmbedDescription int `json:"embedDescription"`
	EmbedFooter      int `json:"embedFooter"`
	Webhooks         int `json:"webhooks"`
	CooldownSeconds  int `json:"cooldownSeconds"`
	EventsPerChannel int `json:"eventsPerChannel"`
}

// defaultCooldownSeconds is the suggested minimum gap for a new rule: long
// enough to swallow a stream that drops and comes straight back, short
// enough to announce a second stream soon after the first.
const defaultCooldownSeconds = 5 * 60

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
		EventsPerChannel: maxEventsPerUserPerChannel,
	}
}

// ---------------------------------------------------------------------------
// Public: live detection status and recent detections
// ---------------------------------------------------------------------------

// LiveDetectResponse is what the site shows about live detection for a
// channel. It carries no error text from the legs - that is operator
// material on the admin page - only whether each is working.
type LiveDetectResponse struct {
	Enabled bool `json:"enabled"`
	// State summarizes the legs: "ok", "degraded", "down", "disabled" (live
	// detection is off) or "unwatched" (on, but not for this channel).
	State string `json:"state"`
	// Watching lists the platforms this channel is watched on.
	Watching []string    `json:"watching"`
	Legs     []PublicLeg `json:"legs"`
	// QueueIncoming says whether a detected stream is also transcribed.
	QueueIncoming bool                      `json:"queueIncoming"`
	Detections    []model.DetectedBroadcast `json:"detections"`
	Videos        []model.DetectedVideo     `json:"videos"`
}

// PublicLeg is one detection mechanism's health, without its diagnostics.
type PublicLeg struct {
	Mechanism   string `json:"mechanism"`
	State       string `json:"state"`
	LastSuccess int64  `json:"lastSuccess,omitempty"`
}

func (app *App) getLiveDetectHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	ctx := r.Context()
	// Detections change once per broadcast; a short cache keeps a busy page
	// from re-reading the ledger on every visit.
	w.Header().Set("Cache-Control", "public, max-age=15")

	status := app.LiveDetect.Status()
	resp := LiveDetectResponse{
		Enabled:       status.Enabled,
		Watching:      app.LiveDetect.Watching(cs.Key),
		Legs:          []PublicLeg{},
		QueueIncoming: app.QueueIncoming,
		Detections:    []model.DetectedBroadcast{},
		Videos:        []model.DetectedVideo{},
	}
	if resp.Watching == nil {
		resp.Watching = []string{}
	}
	resp.State = summarizeLegs(status, len(resp.Watching) > 0)
	for _, leg := range status.Legs {
		resp.Legs = append(resp.Legs, PublicLeg{Mechanism: leg.Mechanism, State: leg.State, LastSuccess: leg.LastSuccess})
	}

	// Both ledgers are diagnostic reads: a failure logs and shows an empty
	// list rather than taking the page down.
	if dets, err := app.Store.GetRecentDetections(ctx, cs.Key, 20); err != nil {
		slog.Error("failed to list detections", "key", cs.Key, "func", "getLiveDetectHandler", "err", err)
	} else if dets != nil {
		resp.Detections = dets
	}
	if vids, err := app.Store.GetRecentVideoDetections(ctx, cs.Key, 20); err != nil {
		slog.Error("failed to list video detections", "key", cs.Key, "func", "getLiveDetectHandler", "err", err)
	} else if vids != nil {
		resp.Videos = vids
	}
	writeJSON(w, resp)
}

// summarizeLegs folds the legs into one word for the status card.
func summarizeLegs(status livedetect.Status, watched bool) string {
	if !status.Enabled {
		return "disabled"
	}
	if !watched {
		return "unwatched"
	}
	worst := "ok"
	for _, leg := range status.Legs {
		switch leg.State {
		case "down":
			return "down"
		case "degraded":
			worst = "degraded"
		}
	}
	return worst
}

// ---------------------------------------------------------------------------
// Account-facing: the signed-in account's rules on a channel
// ---------------------------------------------------------------------------

func (app *App) getNotificationsHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState, u *authedUser) {
	ctx := r.Context()

	events, err := app.Store.ListNotificationEventsForUser(ctx, u.User.ID, cs.Key)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		slog.Error("failed to list notification events", "key", cs.Key, "func", "getNotificationsHandler", "err", err)
		return
	}
	if events == nil {
		events = []model.NotificationEvent{}
	}
	log, err := app.Store.ListNotificationLogForUser(ctx, u.User.ID, cs.Key, 50)
	if err != nil {
		slog.Error("failed to list notification log", "key", cs.Key, "func", "getNotificationsHandler", "err", err)
	}
	if log == nil {
		log = []model.NotificationLogEntry{}
	}

	writeJSON(w, NotificationsResponse{
		Events:        events,
		Log:           log,
		Triggers:      announce.Triggers,
		Placeholders:  announce.Placeholders,
		Defaults:      notificationDefaults(),
		Limits:        notificationLimits(),
		ChannelName:   app.Announcer.ChannelInfo(cs.Key).DisplayName,
		TranscriptURL: app.Announcer.TranscriptURL(cs.Key),
	})
}

// decodeNotificationEvent reads a rule from the request body and normalizes
// it. The channel, owner, id and delivery trail are never taken from the
// body: the channel comes from the route, the owner from the session (so a
// rule cannot be written into another account), and the trail belongs to
// the dispatcher.
func decodeNotificationEvent(w http.ResponseWriter, r *http.Request, cs *ChannelState, u *authedUser) (*model.NotificationEvent, bool) {
	var ev model.NotificationEvent
	if !decodeJSONBody(w, r, maxNotificationBody, &ev) {
		return nil, false
	}
	ev.ChannelKey = cs.Key
	ev.UserID = u.User.ID
	ev.Owner = ""
	ev.ID = 0
	ev.LastSentAt, ev.SentCount, ev.LastError, ev.LastErrorAt, ev.CreatedAt, ev.UpdatedAt = 0, 0, "", 0, 0, 0

	if err := announce.Normalize(&ev); err != nil {
		if announce.IsValidationError(err) {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return nil, false
		}
		writeJSONError(w, http.StatusBadRequest, "Invalid notification event")
		return nil, false
	}
	return &ev, true
}

// notificationAuditFields describes a rule for the operator's audit log.
// Webhook URLs are masked: the audit webhook is a different audience from the
// rule's own webhooks, and the token must not travel between them.
func notificationAuditFields(ev *model.NotificationEvent, owner string) []discord.AdminField {
	masked := make([]string, 0, len(ev.Webhooks))
	for _, h := range ev.Webhooks {
		masked = append(masked, h.Label(announce.MaskWebhookURL(h.URL)))
	}
	return []discord.AdminField{
		{Name: "Account", Value: owner, Inline: true},
		{Name: "Event", Value: ev.Name, Inline: true},
		{Name: "ID", Value: strconv.FormatInt(ev.ID, 10), Inline: true},
		{Name: "Enabled", Value: yesNo(ev.Enabled), Inline: true},
		{Name: "Triggers", Value: strings.Join(ev.Triggers, ", "), Inline: true},
		{Name: "Min. gap", Value: (time.Duration(ev.CooldownSeconds) * time.Second).String(), Inline: true},
		{Name: "Embed", Value: yesNo(ev.EmbedEnabled), Inline: true},
		{Name: "Webhooks", Value: strings.Join(masked, "\n")},
	}
}

// postNotificationHandler creates a rule for the signed-in account.
func (app *App) postNotificationHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState, u *authedUser) {
	if !app.allowRuleWrite(w, u) {
		return
	}
	ev, ok := decodeNotificationEvent(w, r, cs, u)
	if !ok {
		return
	}
	count, err := app.Store.CountNotificationEventsForUser(r.Context(), u.User.ID, cs.Key)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		slog.Error("failed to count notification events", "key", cs.Key, "func", "postNotificationHandler", "err", err)
		return
	}
	if count >= maxEventsPerUserPerChannel {
		writeJSONError(w, http.StatusConflict,
			"You already have "+strconv.Itoa(maxEventsPerUserPerChannel)+" events on this channel. Delete one to add another.")
		return
	}

	now := time.Now().Unix()
	id, err := app.Store.CreateNotificationEvent(r.Context(), *ev, now)
	if errors.Is(err, store.ErrNotFound) {
		// The account was deleted under this session.
		unauthorized(w, "Your account no longer exists.")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		slog.Error("failed to create notification event", "key", cs.Key, "func", "postNotificationHandler", "err", err)
		return
	}
	ev.ID = id
	ev.CreatedAt, ev.UpdatedAt = now, now

	app.bumpAdminChange(cs.Key)
	slog.Info("notification event created", "key", cs.Key, "func", "postNotificationHandler",
		"user", u.User.Username, "id", id, "name", ev.Name, "triggers", ev.Triggers, "webhooks", len(ev.Webhooks))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(ev); err != nil {
		slog.Error("failed to write JSON response", "err", err)
	}
}

// notificationID parses the {id} path value.
func notificationID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "Invalid notification event id")
		return 0, false
	}
	return id, true
}

// putNotificationHandler replaces one of the account's own rules. A rule
// that is not theirs does not exist as far as they can tell.
func (app *App) putNotificationHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState, u *authedUser) {
	id, ok := notificationID(w, r)
	if !ok {
		return
	}
	if !app.allowRuleWrite(w, u) {
		return
	}
	ev, ok := decodeNotificationEvent(w, r, cs, u)
	if !ok {
		return
	}
	ev.ID = id

	err := app.Store.UpdateNotificationEvent(r.Context(), *ev, time.Now().Unix())
	if errors.Is(err, store.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "Notification event not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		slog.Error("failed to update notification event", "key", cs.Key, "func", "putNotificationHandler", "id", id, "err", err)
		return
	}

	stored, err := app.Store.GetNotificationEventForUser(r.Context(), u.User.ID, cs.Key, id)
	if err != nil || stored == nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		slog.Error("failed to re-read notification event", "key", cs.Key, "func", "putNotificationHandler", "id", id, "err", err)
		return
	}

	app.bumpAdminChange(cs.Key)
	slog.Info("notification event updated", "key", cs.Key, "func", "putNotificationHandler",
		"user", u.User.Username, "id", id, "name", stored.Name, "enabled", stored.Enabled)
	writeJSON(w, stored)
}

// deleteNotificationHandler removes one of the account's own rules.
func (app *App) deleteNotificationHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState, u *authedUser) {
	id, ok := notificationID(w, r)
	if !ok {
		return
	}
	if !app.allowRuleWrite(w, u) {
		return
	}
	before, err := app.Store.GetNotificationEventForUser(r.Context(), u.User.ID, cs.Key, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		slog.Error("failed to look up notification event", "key", cs.Key, "func", "deleteNotificationHandler", "id", id, "err", err)
		return
	}
	if before == nil {
		writeJSONError(w, http.StatusNotFound, "Notification event not found")
		return
	}
	rows, err := app.Store.DeleteNotificationEventForUser(r.Context(), u.User.ID, cs.Key, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Database error")
		slog.Error("failed to delete notification event", "key", cs.Key, "func", "deleteNotificationHandler", "id", id, "err", err)
		return
	}
	if rows == 0 {
		writeJSONError(w, http.StatusNotFound, "Notification event not found")
		return
	}
	app.bumpAdminChange(cs.Key)
	slog.Info("notification event deleted", "key", cs.Key, "func", "deleteNotificationHandler",
		"user", u.User.Username, "id", id, "name", before.Name)
	w.WriteHeader(http.StatusNoContent)
}

// notificationDraftRequest is the body of preview and test: a rule as the
// editor currently has it (saved or not), and which trigger to render it for.
type notificationDraftRequest struct {
	Event   model.NotificationEvent `json:"event"`
	Trigger string                  `json:"trigger"`
	// WebhookURL is the test target. It need not be one of the rule's own
	// webhooks - a test can go to a private channel - but it must be a
	// Discord webhook URL like any other. WebhookName is its label for the
	// result and the log, if it has one.
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
	if !decodeJSONBody(w, r, maxNotificationBody, &req) {
		return nil, "", false
	}
	req.Event.ChannelKey = cs.Key
	if err := announce.Normalize(&req.Event); err != nil && !announce.IsValidationError(err) {
		writeJSONError(w, http.StatusBadRequest, "Invalid notification event")
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
		writeJSONError(w, http.StatusBadRequest, "Unknown trigger")
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

// postNotificationPreviewHandler renders a draft without sending it.
// Read-only: no audit record, no change counter bump.
//
// An offline Twitch stream is previewed as it will look live. Twitch serves
// no frame for an offline channel, and the row may predate the title lookup,
// so a strictly honest preview would have a hole where the image goes and no
// title line - which only raises the question of why, when both will be
// there at go-live. The editor is told which parts are examples so it can
// label them; a test send (postNotificationTestHandler) gets none of this
// and goes out exactly as the detection stands.
func (app *App) postNotificationPreviewHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState, _ *authedUser) {
	req, trigger, ok := decodeNotificationDraft(w, r, cs)
	if !ok {
		return
	}
	payload := app.previewPayload(r.Context(), cs, trigger)
	sample := sampleDescription(payload)
	if payload.Platform == announce.PlatformTwitch && payload.Ended {
		if payload.Title == "" {
			payload.Title = announce.ExampleTitle
			sample["exampleTitle"] = announce.ExampleTitle
		}
		payload.Ended = false
		sample["exampleImage"] = true
	}
	msg := app.Announcer.Preview(req.Event, payload)
	writeJSON(w, NotificationPreviewResponse{
		Trigger: string(trigger),
		Content: msg.Content,
		Embed:   msg.Embed,
		Sample:  sample,
	})
}

// postNotificationTestHandler posts a draft's rendering to one webhook with
// every mention suppressed. It is the one account-initiated path to an
// audience webhook, so it is confirmed on the page, limited per account and
// logged as a test with the webhook masked. Like every action an account
// takes on its own events it goes to the server log, not the operator's
// audit webhook.
func (app *App) postNotificationTestHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState, u *authedUser) {
	if ok, wait := app.authLimits.test.Allow(strconv.FormatInt(u.User.ID, 10)); !ok {
		tooMany(w, wait, "Too many test sends. Try again in "+humanDuration(wait)+".")
		return
	}
	req, trigger, ok := decodeNotificationDraft(w, r, cs)
	if !ok {
		return
	}
	webhookURL := strings.TrimSpace(req.WebhookURL)
	if !announce.ValidWebhookURL(webhookURL) {
		writeJSONError(w, http.StatusBadRequest, "webhookUrl must be a Discord webhook URL (https://discord.com/api/webhooks/…)")
		return
	}

	hook := model.Webhook{Name: strings.TrimSpace(req.WebhookName), URL: webhookURL}
	if len([]rune(hook.Name)) > announce.MaxWebhookNameLength {
		hook.Name = ""
	}

	ctx, cancel := context.WithTimeout(r.Context(), testSendTimeout)
	defer cancel()
	payload := app.previewPayload(ctx, cs, trigger)
	// A test is of a draft: the log entry is stamped with the account, never
	// with whatever event id the body happened to carry.
	req.Event.UserID = u.User.ID
	req.Event.ID = 0
	result := app.Announcer.SendTest(ctx, req.Event, payload, hook)

	app.bumpAdminChange(cs.Key)
	slog.Info("notification test sent", "key", cs.Key, "func", "postNotificationTestHandler", "user", u.User.Username,
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

// ---------------------------------------------------------------------------
// Operator: the admin page's read-only view and moderation
// ---------------------------------------------------------------------------

// AdminNotificationsResponse is the operator's view: every account's rules
// on the channel with webhooks masked and owners named, the whole delivery
// log, and the video ledger.
type AdminNotificationsResponse struct {
	Events []model.NotificationEvent    `json:"events"`
	Log    []model.NotificationLogEntry `json:"log"`
	Videos []model.DetectedVideo        `json:"videos"`
	// Accounts is how many accounts exist on the site in total.
	Accounts int `json:"accounts"`
	// OperatorFeed reports whether the operator's own feed (detectWebhookUrl)
	// is configured; every observation is mirrored there in the default look.
	OperatorFeed bool `json:"operatorFeed"`
	// QueueIncoming mirrors liveDetect.queueIncoming for the status card.
	QueueIncoming bool `json:"queueIncoming"`
}

func (app *App) getAdminNotificationsHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	ctx := r.Context()

	// Every rule on the channel, including those of disabled accounts: the
	// operator's view says which ones are parked rather than hiding them.
	events, err := app.Store.ListChannelNotificationEvents(ctx, cs.Key)
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
	accounts, err := app.Store.CountUsers(ctx)
	if err != nil {
		slog.Error("failed to count accounts", "func", "getAdminNotificationsHandler", "err", err)
	}

	// Name the owners, and mask every webhook: the operator's page shows
	// whose rule it is and where it posts, never the credential.
	ids := make([]int64, 0, len(events)+len(log))
	for _, ev := range events {
		ids = append(ids, ev.UserID)
	}
	for _, e := range log {
		ids = append(ids, e.UserID)
	}
	names, err := app.Store.Usernames(ctx, ids)
	if err != nil {
		slog.Error("failed to resolve account names", "func", "getAdminNotificationsHandler", "err", err)
	}
	for i := range events {
		events[i].Owner = ownerLabel(names, events[i].UserID)
		for j := range events[i].Webhooks {
			events[i].Webhooks[j].URL = announce.MaskWebhookURL(events[i].Webhooks[j].URL)
		}
	}
	for i := range log {
		log[i].Owner = ownerLabel(names, log[i].UserID)
	}

	writeJSON(w, AdminNotificationsResponse{
		Events:        events,
		Log:           log,
		Videos:        videos,
		Accounts:      accounts,
		OperatorFeed:  app.Announcer.OperatorFeedConfigured(),
		QueueIncoming: app.QueueIncoming,
	})
}

// ownerLabel names a rule's owner for the operator: the username, "legacy
// admin" for a rule from before accounts existed, or the id of an account
// that has since been deleted.
func ownerLabel(names map[int64]store.UserLabel, userID int64) string {
	if userID == 0 {
		return "legacy admin"
	}
	if u, ok := names[userID]; ok {
		if u.Disabled {
			return u.Username + " (disabled)"
		}
		return u.Username
	}
	return "deleted account #" + strconv.FormatInt(userID, 10)
}

// deleteAdminNotificationLogHandler empties the channel's delivery log. The
// log is a record for the pages only; nothing reads it back, so this cannot
// change what gets sent.
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

// deleteAdminNotificationHandler removes any rule on the channel, whoever
// owns it: the operator's moderation tool, and the only way a rule from
// before accounts existed can go.
func (app *App) deleteAdminNotificationHandler(w http.ResponseWriter, r *http.Request, cs *ChannelState) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid notification event id", http.StatusBadRequest)
		metrics.Http400Errors.Inc()
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
	names, _ := app.Store.Usernames(r.Context(), []int64{before.UserID})

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
	app.notifyAdminAction(r, cs, "Deleted notification event (admin)", notificationAuditFields(before, ownerLabel(names, before.UserID))...)
	slog.Info("admin deleted notification event", "key", cs.Key, "func", "deleteAdminNotificationHandler",
		"id", id, "name", before.Name, "owner", ownerLabel(names, before.UserID))
	w.WriteHeader(http.StatusNoContent)
}
