package announce

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// webhookPattern is the only shape of URL this package will ever POST to. It
// pins the host to Discord (including the PTB/canary and legacy domains) and
// the path to the webhook endpoint, so a rule can never be pointed at an
// arbitrary server - the admin key would otherwise be a way to make this
// process issue POSTs anywhere. The one query parameter allowed is
// thread_id, which is how a webhook posts into a forum channel's thread.
var webhookPattern = regexp.MustCompile(
	`^https://(?:(?:ptb|canary)\.)?discord(?:app)?\.com/api/(?:v\d+/)?webhooks/(\d{5,30})/([A-Za-z0-9_\-]{20,200})(?:\?thread_id=\d{5,30})?$`)

// ValidWebhookURL reports whether u is a Discord webhook URL.
func ValidWebhookURL(u string) bool {
	return webhookPattern.MatchString(strings.TrimSpace(u))
}

// MaskWebhookURL renders a webhook URL for logs, audit records and error
// messages with its token hidden. The webhook id is kept: it is not a secret
// (it is visible in the channel's integrations page) and it is what lets an
// operator tell two webhooks apart.
func MaskWebhookURL(u string) string {
	u = strings.TrimSpace(u)
	if m := webhookPattern.FindStringSubmatch(u); m != nil {
		return "webhook " + m[1] + "/••••"
	}
	// Not a webhook URL at all. Show enough to recognise the mistake and
	// nothing that could be a credential.
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			rest = rest[:j]
		}
		return u[:i+3] + rest + "/…"
	}
	if len(u) > 12 {
		return u[:12] + "…"
	}
	return u
}

// WebhookID extracts the numeric webhook id, for grouping in the UI.
func WebhookID(u string) string {
	if m := webhookPattern.FindStringSubmatch(strings.TrimSpace(u)); m != nil {
		return m[1]
	}
	return ""
}

// mentionsNone is the allowed_mentions policy for test sends and the operator
// feed, and for any message that carries no policy of its own: Discord applies
// allowed_mentions after parsing the content, so every "@Role" is still SHOWN
// but nobody is notified. Real announcements carry Message.Mentions, derived
// from the rule's template by MentionPolicy.
var mentionsNone = map[string]any{"parse": []string{}}

// Delivery limits. A webhook that keeps rate-limiting is a webhook shared with
// something noisy; waiting out one modest limit is worth it, waiting out a
// long one on the detection path is not.
const (
	maxAttempts        = 3
	maxRateLimitWait   = 15 * time.Second
	serverErrorBackoff = 2 * time.Second
	requestTimeout     = 15 * time.Second
)

// Sender posts messages to Discord webhooks.
type Sender struct {
	httpClient *http.Client
	// Sleep is injectable so tests do not wait out rate limits.
	sleep func(context.Context, time.Duration) error
}

// NewSender constructs a sender. A nil client gets a default with a timeout.
func NewSender(client *http.Client) *Sender {
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	return &Sender{httpClient: client, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DeliveryError is a failed webhook post, described without the URL's token.
type DeliveryError struct {
	Webhook string // masked
	Status  int    // 0 for transport failures
	Reason  string
}

func (e *DeliveryError) Error() string {
	return e.Webhook + ": " + e.Detail()
}

// Detail is the failure without the webhook identifier, for callers that
// label the webhook themselves.
func (e *DeliveryError) Detail() string {
	if e.Status > 0 {
		return fmt.Sprintf("HTTP %d %s", e.Status, e.Reason)
	}
	return e.Reason
}

// Send posts one rendered message to one webhook. suppressMentions selects the
// no-ping policy used by test sends and the operator feed.
//
// Retries are limited to what Discord asks for: a 429 is waited out (bounded)
// and retried, a 5xx is retried after a short pause, up to maxAttempts in
// total, and every other failure is final - a 401/404 means the webhook was
// deleted and retrying will not bring it back.
func (s *Sender) Send(ctx context.Context, webhookURL string, msg Message, suppressMentions bool) error {
	// Trimmed here as well as at validation: the target below is built from
	// this exact string, and a stray control character would otherwise put
	// the whole URL - token included - into a URL-parse error.
	webhookURL = strings.TrimSpace(webhookURL)
	if !ValidWebhookURL(webhookURL) {
		return &DeliveryError{Webhook: MaskWebhookURL(webhookURL), Reason: "not a Discord webhook URL"}
	}
	if msg.IsEmpty() {
		return &DeliveryError{Webhook: MaskWebhookURL(webhookURL), Reason: "message is empty"}
	}

	policy := msg.Mentions
	if suppressMentions || policy == nil {
		policy = mentionsNone
	}
	payload := map[string]any{"allowed_mentions": policy}
	if strings.TrimSpace(msg.Content) != "" {
		payload["content"] = msg.Content
	}
	if msg.Embed != nil {
		payload["embeds"] = []map[string]any{msg.Embed}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return &DeliveryError{Webhook: MaskWebhookURL(webhookURL), Reason: "encode payload: " + err.Error()}
	}

	// wait=true makes Discord answer with the created message (200) rather
	// than a bare 204, so an embed the API rejects surfaces as a 400 here
	// instead of vanishing. Added through the parsed URL so a thread_id the
	// admin supplied survives.
	parsed, err := url.Parse(webhookURL)
	if err != nil {
		return &DeliveryError{Webhook: MaskWebhookURL(webhookURL), Reason: "the webhook URL could not be used"}
	}
	query := parsed.Query()
	query.Set("wait", "true")
	parsed.RawQuery = query.Encode()
	target := parsed.String()
	masked := MaskWebhookURL(webhookURL)

	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		status, retryAfter, reason, err := s.post(ctx, target, body)
		if err != nil {
			last = &DeliveryError{Webhook: masked, Reason: err.Error()}
			if ctx.Err() != nil {
				return last
			}
			// A transport error may be transient; one more try.
			if attempt < maxAttempts {
				if err := s.sleep(ctx, serverErrorBackoff); err != nil {
					return last
				}
				continue
			}
			return last
		}
		switch {
		case status >= 200 && status < 300:
			return nil
		case status == http.StatusTooManyRequests:
			last = &DeliveryError{Webhook: masked, Status: status, Reason: "rate limited"}
			if retryAfter <= 0 {
				retryAfter = time.Second
			}
			if retryAfter > maxRateLimitWait {
				return &DeliveryError{Webhook: masked, Status: status,
					Reason: fmt.Sprintf("rate limited for %s, gave up", retryAfter.Round(time.Second))}
			}
			// No point waiting out a limit when no attempt follows.
			if attempt == maxAttempts {
				return last
			}
			if err := s.sleep(ctx, retryAfter); err != nil {
				return last
			}
		case status >= 500:
			last = &DeliveryError{Webhook: masked, Status: status, Reason: reason}
			if attempt == maxAttempts {
				return last
			}
			if err := s.sleep(ctx, serverErrorBackoff); err != nil {
				return last
			}
		default:
			return &DeliveryError{Webhook: masked, Status: status, Reason: reason}
		}
	}
	return last
}

// post performs one attempt and classifies the answer. The response body is
// read only to extract Discord's error message and retry hint; it is bounded
// and never logged whole.
func (s *Sender) post(ctx context.Context, target string, body []byte) (status int, retryAfter time.Duration, reason string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		// A *url.Error quotes the whole URL. Never let that text escape.
		return 0, 0, "", errors.New("request failed: the webhook URL could not be used")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "live-transcript-server (announce)")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, 0, "", scrubURLError(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, 0, "", nil
	}

	reason = describeDiscordError(resp.StatusCode, raw)
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter = parseRetryAfter(resp.Header, raw)
	}
	return resp.StatusCode, retryAfter, reason, nil
}

// scrubURLError strips the request URL from a transport error. *url.Error
// stringifies the whole URL, token included, and this error is stored on the
// rule and shown on the admin page.
func scrubURLError(err error) error {
	var uerr interface{ Unwrap() error }
	if errors.As(err, &uerr) {
		if inner := uerr.Unwrap(); inner != nil {
			return errors.New("request failed: " + inner.Error())
		}
	}
	return errors.New("request failed")
}

// describeDiscordError turns a non-2xx answer into a short operator-readable
// reason.
func describeDiscordError(status int, raw []byte) string {
	var body struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	}
	msg := ""
	if json.Unmarshal(raw, &body) == nil && body.Message != "" {
		msg = body.Message
	}
	switch status {
	case http.StatusUnauthorized, http.StatusNotFound:
		return "webhook no longer exists (deleted, or the token is wrong)"
	case http.StatusForbidden:
		return "webhook is not allowed to post here"
	case http.StatusBadRequest:
		if msg != "" {
			return "Discord rejected the message: " + truncate(msg, 200)
		}
		return "Discord rejected the message"
	case http.StatusTooManyRequests:
		return "rate limited"
	default:
		if msg != "" {
			return truncate(msg, 200)
		}
		return http.StatusText(status)
	}
}

// parseRetryAfter reads Discord's retry hint: retry_after in the 429 body is
// seconds as a float; Retry-After in the header is whole seconds.
func parseRetryAfter(h http.Header, raw []byte) time.Duration {
	var body struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(raw, &body) == nil && body.RetryAfter > 0 {
		return time.Duration(body.RetryAfter * float64(time.Second))
	}
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return 0
}
