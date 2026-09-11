package announce

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"live-transcript-server/internal/model"
)

// Discord limits. Exceeding any of them makes the API reject the whole
// message, so rendering truncates rather than trusting the template.
// https://discord.com/developers/docs/resources/message#create-message
const (
	MaxContentLength     = 2000
	MaxEmbedTitle        = 256
	MaxEmbedDescription  = 4096
	MaxEmbedFooter       = 2048
	MaxEmbedTotal        = 6000
	MaxWebhooksPerEvent  = 10
	MaxWebhookNameLength = 64
	MaxEventNameLength   = 80
	MaxCooldownSeconds   = 7 * 24 * 60 * 60
	MaxTemplateURLLength = 512
)

// Placeholder documents one {placeholder} for the admin UI.
type Placeholder struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Placeholders is every placeholder a template may use, in display order.
var Placeholders = []Placeholder{
	{"{channel}", "The channel's display name, e.g. Dokibird."},
	{"{title}", "The stream or video title."},
	{"{url}", "Link to the stream or video."},
	{"{platform}", "YouTube or Twitch."},
	{"{headline}", "A short phrase for the trigger: Stream Started, Stream Scheduled, New Video, New Short."},
	{"{time}", "When it happens or happened, as a live Discord timestamp that reads like \"in 2 hours\" or \"5 minutes ago\"."},
	{"{timeFull}", "The same moment as a full date and time in each reader's own time zone."},
	{"{transcript}", "Link to this channel's live transcript page."},
	{"{thumbnail}", "The platform's preview image URL (for the embed image)."},
	{"{trigger}", "The trigger id: live, scheduled, upload or short."},
	{"{id}", "The platform's video or stream id."},
}

// Defaults for a new notification event. The embed mirrors the server's own
// stream-start announcement so a rule left untouched looks familiar.
const (
	DefaultContent          = ""
	DefaultEmbedTitle       = "{channel}'s {headline}"
	DefaultEmbedDescription = "**{title}**\n\n[Open on {platform}]({url}) | [Transcript]({transcript})"
	DefaultEmbedURL         = "{url}"
	DefaultEmbedColor       = "#2ECC71" // the same green as NotifyStreamStart
	DefaultEmbedImage       = "{thumbnail}"
	DefaultEmbedFooter      = ""
)

// DefaultEmbed is the embed template a new rule starts from.
func DefaultEmbed() model.EmbedTemplate {
	return model.EmbedTemplate{
		Title:       DefaultEmbedTitle,
		Description: DefaultEmbedDescription,
		URL:         DefaultEmbedURL,
		Color:       DefaultEmbedColor,
		Image:       DefaultEmbedImage,
		Footer:      DefaultEmbedFooter,
		Timestamp:   true,
	}
}

// DefaultEvent is a complete rule with the default look and every trigger,
// used for the operator feed and as the template a new rule starts from.
func DefaultEvent() model.NotificationEvent {
	return model.NotificationEvent{
		Name:         "Default",
		Enabled:      true,
		Triggers:     []string{string(TriggerLive), string(TriggerScheduled), string(TriggerUpload), string(TriggerShort)},
		Content:      DefaultContent,
		EmbedEnabled: true,
		Embed:        DefaultEmbed(),
	}
}

// Message is a rendered announcement: what actually goes on the wire.
type Message struct {
	Content string
	// Embed is nil when the rule has embeds disabled.
	Embed map[string]any
	// Mentions is the allowed_mentions policy derived from the rule's
	// TEMPLATE, never from the rendered text: only the roles and users the
	// admin wrote into the message may be pinged, so a stream title that
	// happens to contain "@everyone" is shown but notifies nobody. Nil means
	// no pings at all.
	Mentions map[string]any
}

// Mention syntax in a template.
var (
	roleMentionPattern = regexp.MustCompile(`<@&(\d+)>`)
	userMentionPattern = regexp.MustCompile(`<@!?(\d+)>`)
	everyonePattern    = regexp.MustCompile(`@(everyone|here)\b`)
)

// MentionPolicy builds Discord's allowed_mentions from a message template:
// the role and user ids it names explicitly, and @everyone/@here only if the
// template itself says so. Everything else in the rendered text - including
// whatever a platform-supplied title contains - is rendered but inert.
func MentionPolicy(template string) map[string]any {
	policy := map[string]any{"parse": []string{}}
	if roles := uniqueSubmatches(roleMentionPattern, template); len(roles) > 0 {
		policy["roles"] = roles
	}
	if users := uniqueSubmatches(userMentionPattern, template); len(users) > 0 {
		policy["users"] = users
	}
	if everyonePattern.MatchString(template) {
		policy["parse"] = []string{"everyone"}
	}
	return policy
}

// uniqueSubmatches returns the first capture group of every match, deduped,
// in order of first appearance. Discord caps each id list at 100.
func uniqueSubmatches(re *regexp.Regexp, s string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		if len(m) > 1 && !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
		if len(out) >= 100 {
			break
		}
	}
	return out
}

// IsEmpty reports whether nothing would be shown - Discord rejects a message
// with no content and no embeds.
func (m Message) IsEmpty() bool {
	return strings.TrimSpace(m.Content) == "" && m.Embed == nil
}

// RenderContext is everything a template can reference, computed once per
// payload so every field of a rule expands the same way.
type RenderContext struct {
	Payload       Payload
	Channel       Channel
	TranscriptURL string
	// FooterOverride replaces the template footer when set; the operator feed
	// uses it to carry detection diagnostics without touching the public
	// template.
	FooterOverride string
}

// values builds the placeholder table.
func (rc RenderContext) values() map[string]string {
	p := rc.Payload
	name := rc.Channel.DisplayName
	if name == "" {
		name = p.ChannelKey
	}
	// A missing title renders blank, nothing is invented in its place. Expand
	// takes the emphasis around an empty placeholder with it, so the default
	// "**{title}**" line disappears rather than posting four bare asterisks.
	v := map[string]string{
		"{channel}":    name,
		"{title}":      strings.TrimSpace(p.Title),
		"{url}":        p.URL,
		"{platform}":   PlatformLabel(p.Platform),
		"{headline}":   p.Trigger.Headline(),
		"{transcript}": rc.TranscriptURL,
		"{thumbnail}":  p.ThumbnailURL(rc.Channel),
		"{trigger}":    string(p.Trigger),
		"{id}":         p.ID,
		"{time}":       "",
		"{timeFull}":   "",
	}
	if !p.EventTime.IsZero() {
		unix := p.EventTime.Unix()
		v["{time}"] = fmt.Sprintf("<t:%d:R>", unix)
		v["{timeFull}"] = fmt.Sprintf("<t:%d:F>", unix)
	}
	return v
}

// placeholderPattern matches any {word}, together with a Discord emphasis
// marker hugging it on either side; unknown placeholders are left untouched
// so a template author can write literal braces without an escape syntax.
var placeholderPattern = regexp.MustCompile("(?:\\*\\*\\*|\\*\\*|\\*|___|__|_|~~|`)?\\{[A-Za-z]+\\}(?:\\*\\*\\*|\\*\\*|\\*|___|__|_|~~|`)?")

// Expand substitutes placeholders in a template.
//
// A placeholder that expands to nothing takes matching emphasis markers on
// both sides with it: "**{title}**" for a stream with no title renders as
// nothing at all, not as four bare asterisks. Anything else around a
// placeholder is left exactly as written.
func Expand(template string, values map[string]string) string {
	return placeholderPattern.ReplaceAllStringFunc(template, func(m string) string {
		open := strings.IndexByte(m, '{')
		closing := strings.IndexByte(m, '}')
		lead, key, trail := m[:open], m[open:closing+1], m[closing+1:]
		v, ok := values[key]
		if !ok {
			return m
		}
		if v == "" && lead != "" && lead == trail {
			return ""
		}
		return lead + v + trail
	})
}

// Render produces the message a rule would send for a payload.
func Render(ev model.NotificationEvent, rc RenderContext) Message {
	values := rc.values()
	msg := Message{
		Content:  truncate(strings.TrimSpace(Expand(ev.Content, values)), MaxContentLength),
		Mentions: MentionPolicy(ev.Content),
	}
	if !ev.EmbedEnabled {
		return msg
	}

	embed := map[string]any{}
	total := 0
	if t := truncate(strings.TrimSpace(Expand(ev.Embed.Title, values)), MaxEmbedTitle); t != "" {
		embed["title"] = t
		total += utf8.RuneCountInString(t)
	}
	if d := truncate(strings.TrimSpace(Expand(ev.Embed.Description, values)), MaxEmbedDescription); d != "" {
		embed["description"] = d
		total += utf8.RuneCountInString(d)
	}
	if u := expandURL(ev.Embed.URL, values); u != "" {
		embed["url"] = u
	}
	if c, ok := ParseColor(ev.Embed.Color); ok {
		embed["color"] = c
	}
	if img := expandURL(ev.Embed.Image, values); img != "" {
		embed["image"] = map[string]string{"url": img}
	}
	if thumb := expandURL(ev.Embed.Thumbnail, values); thumb != "" {
		embed["thumbnail"] = map[string]string{"url": thumb}
	}
	footer := ev.Embed.Footer
	if rc.FooterOverride != "" {
		footer = rc.FooterOverride
	}
	if f := truncate(strings.TrimSpace(Expand(footer, values)), MaxEmbedFooter); f != "" {
		embed["footer"] = map[string]string{"text": f}
		total += utf8.RuneCountInString(f)
	}
	if ev.Embed.Timestamp && !rc.Payload.EventTime.IsZero() {
		embed["timestamp"] = rc.Payload.EventTime.UTC().Format(time.RFC3339)
	}

	// The per-field limits leave the combined limit reachable; trim the
	// description, which is where a long title template lands.
	if total > MaxEmbedTotal {
		if d, ok := embed["description"].(string); ok {
			over := total - MaxEmbedTotal
			keep := utf8.RuneCountInString(d) - over
			if keep < 0 {
				keep = 0
			}
			embed["description"] = truncate(d, keep)
		}
	}

	// An embed with nothing visible - only a color, a link or a timestamp -
	// is one Discord rejects as an empty message. Leave it out so the rule is
	// logged as an empty render rather than failing at send time.
	visible := false
	for _, key := range []string{"title", "description", "image", "thumbnail", "footer"} {
		if _, ok := embed[key]; ok {
			visible = true
			break
		}
	}
	if !visible {
		return msg
	}
	msg.Embed = embed
	return msg
}

// expandURL expands a URL-valued template field and drops anything that is
// not an http(s) URL: Discord rejects the whole embed for a bad image URL, and
// a placeholder that rendered empty (no thumbnail for this platform) must
// simply omit the field.
func expandURL(template string, values map[string]string) string {
	u := strings.TrimSpace(Expand(template, values))
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return ""
	}
	if len(u) > 2048 {
		return ""
	}
	return u
}

// truncate shortens s to at most max runes, marking any cut with an ellipsis.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(runes[:max-1]) + "…"
}

// ParseColor reads a "#RRGGBB" (or "RRGGBB") color into Discord's integer
// form. Empty or malformed input reports false, which renders as no color.
func ParseColor(s string) (int, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(s, "#"))
	if len(s) != 6 {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

// ValidationError lists every problem with a rule so the admin page can show
// them all at once rather than one per save attempt.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return strings.Join(e.Problems, "; ")
}

// Normalize trims and dedupes a rule in place, then validates it. It returns a
// *ValidationError describing every problem, or nil when the rule is sendable.
// The rule's channel, id and delivery trail are not its concern.
func Normalize(ev *model.NotificationEvent) error {
	var problems []string

	ev.Name = strings.TrimSpace(ev.Name)
	if ev.Name == "" {
		problems = append(problems, "name is required")
	} else if utf8.RuneCountInString(ev.Name) > MaxEventNameLength {
		problems = append(problems, fmt.Sprintf("name must be at most %d characters", MaxEventNameLength))
	}

	// Webhooks: trimmed, deduped by URL, every one a Discord webhook URL.
	// Anything else is refused outright - this is the field that decides who
	// gets pinged, and a typo here must not become a POST to a random host.
	// Names are labels for the admin page and the log; a row with a name but
	// no URL is an unfinished row and is dropped.
	var hooks []model.Webhook
	seen := map[string]bool{}
	for _, raw := range ev.Webhooks {
		u := strings.TrimSpace(raw.URL)
		name := strings.TrimSpace(raw.Name)
		if u == "" {
			continue
		}
		if !ValidWebhookURL(u) {
			problems = append(problems, fmt.Sprintf("%s is not a Discord webhook URL (expected https://discord.com/api/webhooks/…)", MaskWebhookURL(u)))
			continue
		}
		if utf8.RuneCountInString(name) > MaxWebhookNameLength {
			problems = append(problems, fmt.Sprintf("webhook name %q must be at most %d characters", truncate(name, 20), MaxWebhookNameLength))
			continue
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		hooks = append(hooks, model.Webhook{Name: name, URL: u})
	}
	if len(hooks) == 0 {
		problems = append(problems, "at least one Discord webhook URL is required")
	}
	if len(hooks) > MaxWebhooksPerEvent {
		problems = append(problems, fmt.Sprintf("at most %d webhook URLs per event", MaxWebhooksPerEvent))
	}
	if hooks == nil {
		hooks = []model.Webhook{}
	}
	ev.Webhooks = hooks

	var triggers []string
	for _, raw := range ev.Triggers {
		t := strings.ToLower(strings.TrimSpace(raw))
		if t == "" {
			continue
		}
		if !KnownTrigger(t) {
			problems = append(problems, fmt.Sprintf("unknown trigger %q", raw))
			continue
		}
		if !slices.Contains(triggers, t) {
			triggers = append(triggers, t)
		}
	}
	if len(triggers) == 0 {
		problems = append(problems, "pick at least one trigger")
	}
	if triggers == nil {
		triggers = []string{}
	}
	ev.Triggers = triggers

	if utf8.RuneCountInString(ev.Content) > MaxContentLength {
		problems = append(problems, fmt.Sprintf("message must be at most %d characters", MaxContentLength))
	}

	e := &ev.Embed
	e.Title = strings.TrimSpace(e.Title)
	e.Description = strings.TrimRight(e.Description, " \t\r\n")
	e.URL = strings.TrimSpace(e.URL)
	e.Color = strings.TrimSpace(e.Color)
	e.Image = strings.TrimSpace(e.Image)
	e.Thumbnail = strings.TrimSpace(e.Thumbnail)
	e.Footer = strings.TrimSpace(e.Footer)
	if ev.EmbedEnabled {
		if utf8.RuneCountInString(e.Title) > MaxEmbedTitle {
			problems = append(problems, fmt.Sprintf("embed title must be at most %d characters", MaxEmbedTitle))
		}
		if utf8.RuneCountInString(e.Description) > MaxEmbedDescription {
			problems = append(problems, fmt.Sprintf("embed description must be at most %d characters", MaxEmbedDescription))
		}
		if utf8.RuneCountInString(e.Footer) > MaxEmbedFooter {
			problems = append(problems, fmt.Sprintf("embed footer must be at most %d characters", MaxEmbedFooter))
		}
		if e.Color != "" {
			if _, ok := ParseColor(e.Color); !ok {
				problems = append(problems, "embed color must look like #RRGGBB")
			} else if !strings.HasPrefix(e.Color, "#") {
				e.Color = "#" + e.Color
			}
			e.Color = strings.ToUpper(e.Color)
		}
		for _, f := range []struct{ name, value string }{{"link", e.URL}, {"image", e.Image}, {"thumbnail", e.Thumbnail}} {
			if f.value == "" {
				continue
			}
			if len(f.value) > MaxTemplateURLLength {
				problems = append(problems, fmt.Sprintf("embed %s URL is too long", f.name))
				continue
			}
			if !strings.HasPrefix(f.value, "http://") && !strings.HasPrefix(f.value, "https://") && !strings.HasPrefix(f.value, "{") {
				problems = append(problems, fmt.Sprintf("embed %s must be a URL or a placeholder like {url}", f.name))
			}
		}
		if e.Title == "" && e.Description == "" && e.Image == "" && e.Thumbnail == "" && e.Footer == "" {
			problems = append(problems, "the embed is enabled but empty - give it a title or description, or disable it")
		}
	}

	if !ev.EmbedEnabled && strings.TrimSpace(ev.Content) == "" {
		problems = append(problems, "the message is empty: add message text or enable the embed")
	}

	if ev.CooldownSeconds < 0 {
		problems = append(problems, "minimum time between notifications cannot be negative")
	} else if ev.CooldownSeconds > MaxCooldownSeconds {
		problems = append(problems, "minimum time between notifications must be at most 7 days")
	}

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// IsValidationError reports whether err is a rule-validation failure the
// caller should show to the admin rather than treat as a server fault.
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}

// PreviewPayload builds the payload previews and test sends are rendered
// from, out of the channel's own detections and nothing else: live is the
// most recent live broadcast, and each other trigger is the most recent video
// detected as that kind, falling back to the most recent YouTube broadcast
// (which is a video too) when there is none. A detail the detection lacks -
// a title EventSub never carried, a start time the platform did not report -
// is left blank, and with no detection at all only the trigger and channel
// are filled in. Nothing is ever invented: the result is what a real
// announcement of that detection would have said.
func PreviewPayload(trigger Trigger, ch Channel, live *model.DetectedBroadcast, video *model.DetectedVideo, now time.Time) Payload {
	p := Payload{Trigger: trigger, ChannelKey: ch.Key, DetectedAt: now}
	if live != nil && live.BroadcastID == "" {
		live = nil
	}
	if video != nil && video.VideoID == "" {
		video = nil
	}

	if trigger == TriggerLive {
		if live != nil {
			p.Platform = live.Platform
			p.ID = live.BroadcastID
			p.URL = live.URL
			p.Title = live.Title
			p.Mechanism = live.Mechanism
			p.Ended = live.EndedAt > 0
			if live.StartedAt > 0 {
				p.EventTime = time.Unix(live.StartedAt, 0)
			}
		}
		return p
	}

	// The remaining triggers are YouTube-only.
	p.Platform = PlatformYouTube
	switch {
	case video != nil:
		p.ID = video.VideoID
		p.URL = video.URL
		p.Title = video.Title
		at := video.PublishedAt
		if trigger == TriggerScheduled {
			at = video.ScheduledAt
		}
		if at > 0 {
			p.EventTime = time.Unix(at, 0)
		}
	case live != nil && live.Platform == PlatformYouTube:
		// A finished YouTube broadcast is a video with a real thumbnail and a
		// working link; only its time is unknown for this trigger.
		p.ID = live.BroadcastID
		p.URL = live.URL
		p.Title = live.Title
		p.Mechanism = live.Mechanism
	}
	return p
}

// The stand-in video a LOCAL build previews when the channel has no detection
// of its own yet: a real, public video, so the thumbnail loads and the links
// resolve while working on the page. A deployed build never shows it; see
// SamplePayload.
const (
	SampleVideoID    = "oqFfSRupK_E"
	SampleVideoTitle = "【NEW OUTFIT REVEAL】What time is it? #DOKIBEACHEPISODE【Dokibird】"
)

// SamplePayload builds the stand-in payload for a trigger. It exists for
// local development only, where no detection will ever be recorded and a
// blank preview would make the editor impossible to work on; the caller
// decides whether the build qualifies.
func SamplePayload(trigger Trigger, ch Channel, now time.Time) Payload {
	p := Payload{
		Trigger:    trigger,
		Platform:   PlatformYouTube,
		ChannelKey: ch.Key,
		ID:         SampleVideoID,
		URL:        "https://www.youtube.com/watch?v=" + SampleVideoID,
		Title:      SampleVideoTitle,
		EventTime:  now,
		DetectedAt: now,
		Mechanism:  "youtube-state-poll",
	}
	switch trigger {
	case TriggerScheduled:
		p.EventTime = now.Add(2 * time.Hour)
	case TriggerShort:
		p.URL = "https://www.youtube.com/shorts/" + SampleVideoID
	}
	return p
}

// IsSample reports whether a payload is the stand-in video rather than one
// of the channel's own detections.
func (p Payload) IsSample() bool {
	return p.ID == SampleVideoID
}
