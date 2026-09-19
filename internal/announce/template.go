package announce

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
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

// Placeholder documents one {placeholder} for the editor.
type Placeholder struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Label is a friendlier caption for the editor's chip when the bare name
	// would mislead: {description} sits under a field that is itself called
	// "Card description".
	Label string `json:"label,omitempty"`
	// Lines marks a multi-line placeholder that accepts a line count,
	// {name:N}; the editor offers a chooser for it. Max and Default bound the
	// chooser only - by hand any N from 1 to 99 works.
	Lines *PlaceholderLines `json:"lines,omitempty"`
}

// PlaceholderLines is the line chooser the editor offers for a {name:N}
// placeholder: how many lines it goes up to, and where it starts.
type PlaceholderLines struct {
	Max     int `json:"max"`
	Default int `json:"default"`
}

// descriptionKey names the one placeholder whose value is platform text of
// arbitrary length. Rendering treats it as the elastic part of a message: it
// is what gives way when a field would not fit (see expandField).
const descriptionKey = "{description}"

// Placeholders is every placeholder a template may use, in display order.
var Placeholders = []Placeholder{
	{Name: "{channel}", Description: "The channel's display name, e.g. Dokibird."},
	{Name: "{title}", Description: "The stream or video title."},
	{
		Name:        descriptionKey,
		Description: "YouTube only: the video's description, as it read when the stream or video was detected. {description} is all of it, {description:1} just the first line, {description:3} the first 3 lines. Blank on Twitch, which has no descriptions.",
		Label:       "Video description",
		Lines:       &PlaceholderLines{Max: 20, Default: 3},
	},
	{Name: "{game}", Description: "Twitch only: the category being streamed, e.g. Minecraft or Just Chatting. Blank on YouTube."},
	{Name: "{url}", Description: "Link to the stream or video."},
	{Name: "{platform}", Description: "YouTube or Twitch."},
	{Name: "{headline}", Description: "A short phrase for the trigger: Stream Started, Stream Scheduled, New Video, New Short."},
	{Name: "{time}", Description: "When it happens or happened, as a live Discord timestamp that reads like \"in 2 hours\" or \"5 minutes ago\"."},
	{Name: "{timeShort}", Description: "The same moment as just a clock time in each reader's own time zone, e.g. 4:20 PM."},
	{Name: "{timeFull}", Description: "The same moment as a full date and time in each reader's own time zone."},
	{Name: "{transcript}", Description: "Link to this channel's live transcript page."},
	{Name: "{thumbnail}", Description: "The platform's preview image URL (for the embed image)."},
	{Name: "{trigger}", Description: "The trigger id: live, scheduled, upload or short."},
	{Name: "{id}", Description: "The platform's video or stream id."},
}

// lineAware is the set of placeholders that accept a line count, {name:N}. It
// is derived from Placeholders so the vocabulary served to the editor and the
// parser can never disagree about which names take one.
var lineAware = func() map[string]bool {
	names := map[string]bool{}
	for _, p := range Placeholders {
		if p.Lines != nil {
			names[p.Name] = true
		}
	}
	return names
}()

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
	// Shortened reports that a field would not have fit Discord's limits and
	// the video description was cut to make room (see expandField). It is for
	// the editor's preview, which tells the author; nothing on the wire
	// depends on it.
	Shortened bool
	// DescriptionDropped reports the other outcome of that fit: the author's
	// own text left no room for the description, so a field went out without
	// it. Also for the preview only.
	DescriptionDropped bool
}

// note records what fitting one field did to the description.
func (m *Message) note(fit descriptionFit) {
	m.Shortened = m.Shortened || fit == fitShortened
	m.DescriptionDropped = m.DescriptionDropped || fit == fitDropped
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
	// {game} is the same off Twitch: YouTube's category ids are a different
	// thing, and passing one off as the game would be inventing it.
	v := map[string]string{
		"{channel}":    name,
		"{title}":      strings.TrimSpace(p.Title),
		"{game}":       strings.TrimSpace(p.Game),
		"{url}":        p.URL,
		"{platform}":   PlatformLabel(p.Platform),
		"{headline}":   p.Trigger.Headline(),
		"{transcript}": rc.TranscriptURL,
		"{thumbnail}":  p.ThumbnailURL(rc.Channel),
		"{trigger}":    string(p.Trigger),
		"{id}":         p.ID,
		"{time}":       "",
		"{timeShort}":  "",
		"{timeFull}":   "",
		// Platform text the author cannot edit, cleaned once here so that every
		// use of it - whole, or its first lines - starts from the same text.
		descriptionKey: cleanDescription(p.Description),
	}
	if !p.EventTime.IsZero() {
		unix := p.EventTime.Unix()
		v["{time}"] = fmt.Sprintf("<t:%d:R>", unix)
		v["{timeShort}"] = fmt.Sprintf("<t:%d:t>", unix)
		v["{timeFull}"] = fmt.Sprintf("<t:%d:F>", unix)
	}
	return v
}

// placeholderPattern matches any {word} or {word:N} (a line count of one or
// two digits), together with a Discord emphasis marker hugging it on either
// side; unknown placeholders are left untouched so a template author can
// write literal braces without an escape syntax. Which names accept a count is
// decided after the match (see splitPlaceholder), so {title:2} matches here
// and is then left exactly as written.
var placeholderPattern = regexp.MustCompile("(?:\\*\\*\\*|\\*\\*|\\*|___|__|_|~~|`)?\\{[A-Za-z]+(?::[0-9]{1,2})?\\}(?:\\*\\*\\*|\\*\\*|\\*|___|__|_|~~|`)?")

// splitPlaceholder takes one placeholderPattern match apart: the emphasis
// hugging it, the {name} to look up, and the line count of a {name:N} form
// (zero for the bare form). ok is false for a count the grammar does not
// accept - on a placeholder that is not line-aware, or a count of zero. Such
// a match is left as written like any other typo, so it shows in the preview
// instead of quietly rendering something the author did not ask for.
func splitPlaceholder(m string) (lead, key string, lines int, trail string, ok bool) {
	open := strings.IndexByte(m, '{')
	closing := strings.IndexByte(m, '}')
	lead, trail = m[:open], m[closing+1:]
	name, count, counted := strings.Cut(m[open+1:closing], ":")
	key = "{" + name + "}"
	if counted {
		// The pattern only lets one or two digits through, so this cannot fail.
		lines, _ = strconv.Atoi(count)
		if lines == 0 || !lineAware[key] {
			return lead, key, 0, trail, false
		}
	}
	return lead, key, lines, trail, true
}

// noCap is an expander's descCap when the description is not being fitted.
const noCap = -1

// expander expands placeholders from one table of values. It exists so Render
// can bound the description and see what each template line came to without
// Expand's signature, or its behaviour, changing.
type expander struct {
	values map[string]string
	// descCap bounds, in runes, the value of every use of {description}; it is
	// applied after any line count. noCap leaves the value whole. expandField
	// searches for the largest cap that makes a field fit.
	descCap int
}

// replace expands one placeholderPattern match. known reports whether it
// named a placeholder at all, and blank whether that placeholder's final
// value came up empty - which is what expandLines decides a line's fate on.
func (x expander) replace(m string) (out string, known, blank bool) {
	lead, key, lines, trail, ok := splitPlaceholder(m)
	if !ok {
		return m, false, false
	}
	v, ok := x.values[key]
	if !ok {
		return m, false, false
	}
	if lines > 0 {
		v = firstLines(v, lines)
	}
	if key == descriptionKey && x.descCap != noCap {
		// A cut that kept nothing but its ellipsis is no description at all:
		// when the text opens with a link longer than the cap, a lone "…"
		// would otherwise count as a value and hold its line in place.
		if c := cutAtWord(v, x.descCap); c != v && c == "…" {
			v = ""
		} else {
			v = c
		}
	}
	// Judged on the FINAL value: a description capped away to nothing takes
	// its emphasis with it exactly as a missing one does.
	if v == "" && lead != "" && lead == trail {
		return "", true, true
	}
	return lead + v + trail, true, v == ""
}

// expand substitutes every placeholder and touches nothing else.
func (x expander) expand(template string) string {
	return placeholderPattern.ReplaceAllStringFunc(template, func(m string) string {
		out, _, _ := x.replace(m)
		return out
	})
}

// Expand substitutes placeholders in a template.
//
// A placeholder that expands to nothing takes matching emphasis markers on
// both sides with it: "**{title}**" for a stream with no title renders as
// nothing at all, not as four bare asterisks. Anything else around a
// placeholder is left exactly as written, the template's own line breaks
// included.
//
// A line-aware placeholder also takes a line count: {description:3} is the
// first 3 lines of text of {description}. A count anywhere else ({title:2}),
// or one that is not 1 to 99, is left as written.
func Expand(template string, values map[string]string) string {
	return expander{values: values, descCap: noCap}.expand(template)
}

// expandLines expands a multi-line template field and applies the one layout
// rule an author can rely on: a line whose placeholders all came up blank,
// and that has no words left on it, is removed. That is what lets one
// template serve both platforms: a "{description:1}" line, quoted or bulleted
// or not, is simply not there for a Twitch stream, rather than leaving a hole
// or a stray "> " behind. A line that still says something ("About:
// {description:1}", "<@&123> {title}") stays, and so does every line the
// author wrote without a placeholder, or with one that is unknown.
//
// A removed line takes one blank line with it when it stood apart: with a
// blank line (or nothing) above it and a blank line below, the one below goes
// too, so the paragraphs either side end up one blank line apart, not two.
//
// The template is walked line by line because the rule is about the lines the
// AUTHOR wrote; a value spanning several lines still sits on one template
// line. Splitting first changes nothing else, since no placeholder can span a
// line break.
func (x expander) expandLines(template string) string {
	lines := strings.Split(template, "\n")
	kept := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		emptied, filled := 0, 0
		out := placeholderPattern.ReplaceAllStringFunc(lines[i], func(m string) string {
			s, known, blank := x.replace(m)
			switch {
			case !known:
			case blank:
				emptied++
			default:
				filled++
			}
			return s
		})
		if emptied == 0 || filled > 0 || strings.ContainsFunc(emojiToken.ReplaceAllString(out, ""), isWordRune) {
			kept = append(kept, out)
			continue
		}
		above := len(kept) == 0 || strings.TrimSpace(kept[len(kept)-1]) == ""
		if above && i+1 < len(lines) && strings.TrimSpace(lines[i+1]) == "" {
			i++
		}
	}
	return strings.Join(kept, "\n")
}

// isWordRune reports whether a rune is something a reader would read as text.
// Punctuation, emoji and markdown markers are not: a line of only those, next
// to a placeholder that came up blank, is decoration for a value that is not
// there.
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// emojiToken is a Discord custom emoji (<:name:id>, <a:name:id>) or an emoji
// shortcode (:name:). Both are spelled with letters and digits, but a reader
// sees a picture, so they are decoration like any other emoji and must not
// hold a line in place: "<:twitch:123> {game}" goes on YouTube exactly as
// "🎮 {game}" does. Mentions are not touched - "<@&123> {title}" still pings
// with a blank title, so that line has to stay.
var emojiToken = regexp.MustCompile(`<a?:\w+:\d+>|:\w+:`)

// minDescriptionCap is the shortest description worth showing. Below it the
// fit drops the description altogether: a dozen characters and an ellipsis
// say nothing, and only look like a mistake.
const minDescriptionCap = 20

// expandField renders one length-limited field. shortened reports that the
// description had to be cut to make the field fit.
//
// The description is the elastic part of a message; the author's own text is
// not. A description runs to 5000 characters, and cutting the rendered field
// at its limit would take whatever the author put after it - the links, in
// the usual "title, description, links" layout - and keep a wall of platform
// text instead. So when a field that uses {description} overflows, it is the
// description that gives way: the largest cap on it that makes the whole
// field fit is found by binary search over real renders. Nothing is
// estimated; a render is only ever accepted after being measured, so the
// result fits by construction, and the search costs a dozen expansions only
// when a field overflows.
//
// A field that fits, and an overflowing field with no description to give,
// render exactly as they always have.
//
// fit says what became of the description, because the editor tells the
// author and the two outcomes read very differently: fitShortened is the
// promise kept (the description gave way, everything else is whole), while
// fitDropped means the author's own text left no room for it at all.
func expandField(template string, values map[string]string, limit int) (out string, fit descriptionFit) {
	if limit <= 0 {
		return "", fitWhole
	}
	render := func(descCap int) (string, bool) {
		s := strings.TrimSpace(expander{values: values, descCap: descCap}.expandLines(template))
		return s, utf8.RuneCountInString(s) <= limit
	}
	out, fits := render(noCap)
	if fits {
		return out, fitWhole
	}
	if values[descriptionKey] == "" || !templateUses(template, descriptionKey) {
		return truncate(out, limit), fitWhole
	}

	// Without the description at all: if even that overflows, the author's
	// own text is too long and the tail cut is all that is left.
	bare, fits := render(0)
	if !fits {
		return truncate(bare, limit), fitDropped
	}
	// Largest cap that fits. lo always holds a cap measured to fit; a longer
	// cap never renders shorter (cutAtWord's result only grows with its max),
	// which is what makes the search valid.
	lo, hi, best := 0, limit, bare
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if s, fits := render(mid); fits {
			lo, best = mid, s
		} else {
			hi = mid - 1
		}
	}
	// best == bare is a cap that cut the description down to nothing (it opens
	// with a link longer than the room there was).
	if lo < minDescriptionCap || best == bare {
		return bare, fitDropped
	}
	return best, fitShortened
}

// descriptionFit is what fitting a field did to the description.
type descriptionFit int

const (
	fitWhole     descriptionFit = iota // untouched, or not there to touch
	fitShortened                       // cut with an ellipsis so the field fits
	fitDropped                         // no room for any of it; left out
)

// templateUses reports whether a template field has a working use of a
// placeholder, bare or with a line count. It reads the template through the
// same parser as the expansion, so a form that would be left as written
// ({description:0}) does not count as a use.
func templateUses(template, name string) bool {
	for _, m := range placeholderPattern.FindAllString(template, -1) {
		if _, key, _, _, ok := splitPlaceholder(m); ok && key == name {
			return true
		}
	}
	return false
}

// UsesPlaceholder reports whether a rule's visible text uses a placeholder,
// as {name} or {name:N}: its message, or its embed title, description and
// footer when the embed is on. name is given with its braces, "{description}".
// It exists so the preview can say why a placeholder came up blank without
// the placeholder grammar being written down a second time outside this file.
func UsesPlaceholder(ev model.NotificationEvent, name string) bool {
	fields := []string{ev.Content}
	if ev.EmbedEnabled {
		fields = append(fields, ev.Embed.Title, ev.Embed.Description, ev.Embed.Footer)
	}
	for _, f := range fields {
		if templateUses(f, name) {
			return true
		}
	}
	return false
}

// Render produces the message a rule would send for a payload.
func Render(ev model.NotificationEvent, rc RenderContext) Message {
	values := rc.values()

	// Discord unfurls every bare link in message CONTENT into a preview card
	// of its own (embed text does not unfurl), and a description is mostly
	// links the author never typed and cannot edit. So the content, and only
	// the content, gets the description with its links wrapped in <>, which
	// Discord shows as the same link minus the card. Wrapped before anything
	// is measured, so the fit is of the text that is really sent.
	contentValues := values
	if values[descriptionKey] != "" {
		contentValues = maps.Clone(values)
		contentValues[descriptionKey] = angleWrapURLs(values[descriptionKey])
	}
	content, fit := expandField(ev.Content, contentValues, MaxContentLength)
	msg := Message{
		Content:  content,
		Mentions: MentionPolicy(ev.Content),
	}
	msg.note(fit)
	if !ev.EmbedEnabled {
		return msg
	}

	embed := map[string]any{}
	total := 0
	t, fit := expandField(ev.Embed.Title, values, MaxEmbedTitle)
	msg.note(fit)
	if t != "" {
		embed["title"] = t
		total += utf8.RuneCountInString(t)
	}
	footer := ev.Embed.Footer
	if rc.FooterOverride != "" {
		footer = rc.FooterOverride
	}
	f, fit := expandField(footer, values, MaxEmbedFooter)
	msg.note(fit)
	if f != "" {
		embed["footer"] = map[string]string{"text": f}
		total += utf8.RuneCountInString(f)
	}
	// The description goes last because the per-field limits leave the
	// combined limit reachable: it is fitted into what the title and footer
	// left of the total, so that squeeze too comes out of the video
	// description rather than off the end of the author's text.
	d, fit := expandField(ev.Embed.Description, values, min(MaxEmbedDescription, MaxEmbedTotal-total))
	msg.note(fit)
	if d != "" {
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
	if ev.Embed.Timestamp && !rc.Payload.EventTime.IsZero() {
		embed["timestamp"] = rc.Payload.EventTime.UTC().Format(time.RFC3339)
	}

	// The description was fitted into the combined limit above, so this never
	// fires. It stays as a backstop: Discord rejects the whole message over
	// the total, and that must not hinge on the arithmetic above being right.
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

// descriptionBreaks folds every kind of line break a description can carry
// into "\n", and tabs into a space. CRLF comes first so it becomes one break,
// not two.
var descriptionBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u2028", "\n", "\u2029", "\n", "\t", " ")

// invisibleRunes render as nothing without being Unicode spaces: the
// zero-width space, non-joiner and joiner, the word joiner, the byte-order
// mark, the braille blank and the Hangul filler. Creators use them to force
// an "empty" line where the platform would strip a real one.
const invisibleRunes = "\u200B\u200C\u200D\u2060\uFEFF\u2800\u3164"

// blockMarkupPattern is Discord markdown that does not stay on its own line:
// ">>>" quotes everything after it, and a "# ", "## ", "### " or "-# " line
// changes size. "#hashtag" (no space) and a single "> " are harmless.
var blockMarkupPattern = regexp.MustCompile(`^(?:>>>|#{1,3} |-# )`)

// cleanDescription makes a platform description fit to sit in the middle of
// an author's message. The text is the creator's, not the author's, and the
// author cannot edit it, so it is normalised once, before any template sees
// it:
//
//   - every kind of line break becomes "\n" and control characters go, so
//     lines can be counted and nothing unprintable reaches Discord;
//   - trailing space is trimmed, and a line with nothing visible on it is
//     truly empty: "the first line" must never be an invisible one;
//   - blank lines at either end go and a run of them becomes one, so a
//     description spaced out with five empty lines does not push the author's
//     own text off the screen;
//   - markdown that would bleed out of the description - block-quoting the
//     rest of the author's message, or shouting a line in heading size - is
//     defused with a zero-width space in front of the marker, which shows the
//     characters as typed.
//
// Both halves of that last point were checked with a real send, in the message
// and in the embed: the zero-width space does defuse "# ", "-# " and ">>>",
// and a code fence the creator never closed needs no guard at all - Discord
// shows the backticks as typed and formats the author's text after it as
// usual.
//
// Indentation at the start of a line is kept.
func cleanDescription(raw string) string {
	s := strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' {
			return -1
		}
		return r
	}, descriptionBreaks.Replace(raw))

	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRightFunc(line, unicode.IsSpace)
		if !strings.ContainsFunc(line, isVisibleRune) {
			// Blank. Dropped at the start and after another blank.
			if len(lines) == 0 || lines[len(lines)-1] == "" {
				continue
			}
			lines = append(lines, "")
			continue
		}
		if rest := strings.TrimLeftFunc(line, unicode.IsSpace); blockMarkupPattern.MatchString(rest) {
			line = line[:len(line)-len(rest)] + "\u200B" + rest
		}
		lines = append(lines, line)
	}
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return strings.Join(lines, "\n")
}

// HasDescription reports whether {description} renders anything for this
// payload. It asks the cleaned text, not the raw one: a description made of
// nothing but invisible characters is as blank as a missing one.
func (p Payload) HasDescription() bool {
	return cleanDescription(p.Description) != ""
}

// isVisibleRune reports whether a rune puts anything on the screen.
func isVisibleRune(r rune) bool {
	return !unicode.IsSpace(r) && !strings.ContainsRune(invisibleRunes, r)
}

// MaxDescriptionLineRunes bounds one line of a {description:N} expansion. An
// author who asks for "the first line" wants a teaser, and the first line of
// a description written as a single 1200-character paragraph is a wall. Bare
// {description} has no such bound: there the author asked for all of it.
const MaxDescriptionLineRunes = 300

// firstLines returns the first n lines of text of a multi-line value. Only
// lines with text on them count - a blank line between two kept lines is
// kept, but never counted and never left dangling at the end - so
// {description:2} of "pitch, blank, link" is the pitch and the link, spaced
// as the creator spaced them. Asking for more lines than there are returns
// everything. A cut between lines gets no ellipsis: nothing was cut mid-way.
//
// A line over MaxDescriptionLineRunes is shortened, and the output stops
// there, since nothing should follow an ellipsis.
func firstLines(v string, n int) string {
	var kept []string
	count := 0
	for _, line := range strings.Split(v, "\n") {
		if strings.TrimSpace(line) == "" {
			if len(kept) > 0 {
				kept = append(kept, line)
			}
			continue
		}
		if utf8.RuneCountInString(line) > MaxDescriptionLineRunes {
			// A line that is one enormous link cuts down to a bare ellipsis;
			// that says nothing, so the output just ends on the line before.
			if c := cutAtWord(line, MaxDescriptionLineRunes); c != "…" {
				kept = append(kept, c)
			}
			break
		}
		kept = append(kept, line)
		if count++; count >= n {
			break
		}
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	return strings.Join(kept, "\n")
}

// maxWordBackup is how long a cut-into word may be and still be dropped whole.
// Backing up to the previous space reads better than half a word, but text
// without spaces (Japanese, a long run of hashtags) is one enormous "word",
// and dropping that would throw away most of what the cut was allowed to
// keep.
const maxWordBackup = 40

// cutAtWord shortens s to at most max runes, ending on a word boundary where
// it reasonably can and marking the cut with an ellipsis. It is the only
// cutter used on description text, because descriptions are mostly links: a
// cut URL is still a clickable link, to a different address, and a cut
// "<https://..." loses the bracket that keeps Discord from unfurling it. So a
// word containing "://" is never cut into, whatever its length.
//
// The decision looks at the whole word, not just the part before the cut, so
// that the result never gets shorter as max grows - expandField's binary
// search depends on that.
func cutAtWord(s string, max int) string {
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
	cut := max - 1
	if !unicode.IsSpace(runes[cut]) {
		start, end := cut, cut
		for start > 0 && !unicode.IsSpace(runes[start-1]) {
			start--
		}
		for end < len(runes) && !unicode.IsSpace(runes[end]) {
			end++
		}
		if cut-start <= maxWordBackup || strings.Contains(string(runes[start:end]), "://") {
			cut = start
		}
	}
	kept := []rune(strings.TrimRightFunc(string(runes[:cut]), unicode.IsSpace))
	// The ellipsis must not touch a link either: Discord ends a bare link at
	// whitespace, so "https://x.com/a…" is a clickable link to an address that
	// does not exist. A link the cut ends on keeps a space before the
	// ellipsis, and goes altogether when there is no room for that space.
	for len(kept) > 0 {
		start := len(kept)
		for start > 0 && !unicode.IsSpace(kept[start-1]) {
			start--
		}
		if !strings.Contains(string(kept[start:]), "://") {
			break
		}
		if len(kept)+2 <= max {
			return string(kept) + " …"
		}
		kept = []rune(strings.TrimRightFunc(string(kept[:start]), unicode.IsSpace))
	}
	if len(kept) == 0 {
		return "…"
	}
	return string(kept) + "…"
}

// bareURLPattern finds the links in platform text. "<" and ">" end a link so
// that one already written as <https://...> is seen for what it is, and so
// does every kind of space, not only the ASCII ones: a Japanese description
// follows a link with U+3000, and a link that swallowed the text after it
// would be wrapped together with that text and point nowhere.
var bareURLPattern = regexp.MustCompile(`https?://[^\s<>\p{Z}\x{85}\x{FEFF}]+`)

// angleWrapURLs wraps every bare link in <>, which Discord renders as the
// same link without unfurling it into a preview card. Punctuation after a
// link stays outside it, and so does a closing parenthesis that belongs to
// the sentence rather than the address: "(https://x.com/a)" wraps the
// address alone and so does a markdown "[label](https://x.com/a)", which
// Discord accepts in that form, while the balanced parentheses of
// "https://en.wikipedia.org/wiki/Foo_(bar)" are part of the link.
func angleWrapURLs(s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range bareURLPattern.FindAllStringIndex(s, -1) {
		start := loc[0]
		if start > 0 && s[start-1] == '<' {
			continue
		}
		u := s[start:loc[1]]
		for {
			trimmed := strings.TrimRight(u, ".,;:!?'\"")
			if strings.HasSuffix(trimmed, ")") && strings.Count(trimmed, ")") > strings.Count(trimmed, "(") {
				trimmed = trimmed[:len(trimmed)-1]
			}
			if trimmed == u {
				break
			}
			u = trimmed
		}
		if strings.HasSuffix(u, "://") {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString("<" + u + ">")
		last = start + len(u)
	}
	b.WriteString(s[last:])
	return b.String()
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
	// The embed fields are bounded whether or not the embed is on: a rule
	// keeps its embed template while the embed is disabled, and what is
	// stored must never be more than what could be sent.
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
	if ev.EmbedEnabled && e.Title == "" && e.Description == "" && e.Image == "" && e.Thumbnail == "" && e.Footer == "" {
		problems = append(problems, "the embed is enabled but empty - give it a title or description, or disable it")
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
			p.Description = live.Description
			p.Game = live.Game
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
		p.Description = video.Description
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
		p.Description = live.Description
		p.Mechanism = live.Mechanism
	}
	return p
}

// ExampleTitle is what the editor's preview shows in place of the title of an
// offline Twitch stream that was recorded without one, so the preview still
// looks the way the announcement will. It is unmistakably an example, and the
// editor labels it as one; it never reaches a webhook, not even a test send.
const ExampleTitle = "Example stream title"

// The stand-in video a LOCAL build previews when the channel has no detection
// of its own yet: a real, public video, so the thumbnail loads and the links
// resolve while working on the page. A deployed build never shows it; see
// SamplePayload.
const (
	SampleVideoID    = "oqFfSRupK_E"
	SampleVideoTitle = "【NEW OUTFIT REVEAL】What time is it? #DOKIBEACHEPISODE【Dokibird】"
)

// SampleVideoDescription is the stand-in's description. It is shaped like the
// real thing - a one-line pitch, a blank line, then bare links and credits -
// so that the first line, the first few lines and the whole of it are
// visibly different in the editor, and the links exercise what rendering does
// to them.
const SampleVideoDescription = "The new outfit is finally here! Come hang out while we take a first look at it together.\n" +
	"\n" +
	"Watch it again: https://www.youtube.com/watch?v=" + SampleVideoID + "\n" +
	"More streams and videos: https://www.youtube.com/@Dokibird\n" +
	"Live transcripts of every stream are made by fans, for fans.\n" +
	"\n" +
	"Thank you for watching!"

// SamplePayload builds the stand-in payload for a trigger. It exists for
// local development only, where no detection will ever be recorded and a
// blank preview would make the editor impossible to work on; the caller
// decides whether the build qualifies.
func SamplePayload(trigger Trigger, ch Channel, now time.Time) Payload {
	p := Payload{
		Trigger:     trigger,
		Platform:    PlatformYouTube,
		ChannelKey:  ch.Key,
		ID:          SampleVideoID,
		URL:         "https://www.youtube.com/watch?v=" + SampleVideoID,
		Title:       SampleVideoTitle,
		Description: SampleVideoDescription,
		EventTime:   now,
		DetectedAt:  now,
		Mechanism:   "youtube-state-poll",
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
