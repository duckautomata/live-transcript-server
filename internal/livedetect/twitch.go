package livedetect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Twitch API endpoints. Overridable on the client so tests can point at an
// httptest server, matching internal/archive's shape.
const (
	twitchHelixURL = "https://api.twitch.tv/helix"
	twitchIDURL    = "https://id.twitch.tv"
)

// EventSub secret length bounds. Twitch requires the transport secret to be an
// ASCII string of 10-100 characters and rejects the subscription otherwise.
const (
	EventSubSecretMinLen = 10
	EventSubSecretMaxLen = 100
)

// errTwitchUnauthorized signals a 401 so the caller can re-mint the app token
// and retry exactly once.
var errTwitchUnauthorized = errors.New("twitch: unauthorized")

// TwitchClient talks to the Twitch Helix API using an app access token.
//
// App access tokens have NO refresh token: the documented way to refresh one
// is to make the same client_credentials request again. So "refresh" here
// means "re-mint", and the 401 path does exactly that, once, before giving up.
type TwitchClient struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	// HelixURL and IDURL are injectable for tests.
	HelixURL string
	IDURL    string

	// mu guards the cached app token. It is deliberately held across the mint
	// request: minting is single-flight, happens roughly twice a quarter, and
	// correctness matters more than concurrency here.
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewTwitchClient constructs a Helix client. Empty credentials are allowed and
// leave it unconfigured; see Configured.
func NewTwitchClient(clientID, clientSecret string) *TwitchClient {
	return &TwitchClient{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		HelixURL:     twitchHelixURL,
		IDURL:        twitchIDURL,
	}
}

// Configured reports whether both credentials are present.
func (c *TwitchClient) Configured() bool {
	return c != nil && c.clientID != "" && c.clientSecret != ""
}

// ClientID exposes the configured client id for callers that must send it as
// a header alongside a token they already hold.
func (c *TwitchClient) ClientID() string { return c.clientID }

type twitchTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// token returns a valid app access token, minting one if the cached token is
// missing or within an hour of expiry.
func (c *TwitchClient) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Until(c.expiresAt) > time.Hour {
		return c.token, nil
	}

	form := url.Values{}
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	form.Set("grant_type", "client_credentials")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.IDURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build twitch token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("mint twitch app token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The body can echo the client id; report only the status.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("mint twitch app token: status %d", resp.StatusCode)
	}

	var tr twitchTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&tr); err != nil {
		return "", fmt.Errorf("decode twitch token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("twitch token response had no access_token")
	}

	c.token = tr.AccessToken
	// expires_in is ~58 days in practice but is never assumed: always read it.
	c.expiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.token, nil
}

// invalidate drops the cached token so the next call mints a fresh one.
func (c *TwitchClient) invalidate() {
	c.mu.Lock()
	c.token = ""
	c.expiresAt = time.Time{}
	c.mu.Unlock()
}

// do performs a Helix request, retrying exactly once on 401 with a freshly
// minted token. body is passed as bytes rather than an io.Reader precisely so
// the retry can replay it - a consumed reader would send an empty body the
// second time.
func (c *TwitchClient) do(ctx context.Context, method, path string, body []byte, out any) error {
	for attempt := range 2 {
		tok, err := c.accessToken(ctx)
		if err != nil {
			return err
		}

		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.HelixURL+path, rdr)
		if err != nil {
			return fmt.Errorf("build twitch request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Client-Id", c.clientID)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("call twitch %s %s: %w", method, path, err)
		}

		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			c.invalidate()
			continue
		}

		err = decodeTwitchResponse(resp, out)
		resp.Body.Close()
		return err
	}
	return errTwitchUnauthorized
}

// decodeTwitchResponse turns a Helix response into an error or a decoded body.
// Response bodies are never included in errors: they can echo request content.
func decodeTwitchResponse(resp *http.Response, out any) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusUnauthorized {
			return errTwitchUnauthorized
		}
		return fmt.Errorf("twitch returned status %d", resp.StatusCode)
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode twitch response: %w", err)
	}
	return nil
}

// TwitchUser is one entry from GET /helix/users.
type TwitchUser struct {
	ID          string `json:"id"`
	Login       string `json:"login"`
	DisplayName string `json:"display_name"`
}

// GetUsers resolves logins to numeric user IDs.
//
// Logins that do not exist are silently OMITTED from the response rather than
// erroring, so the result is a map and the caller is expected to notice which
// of its logins are missing. Never index the response positionally.
func (c *TwitchClient) GetUsers(ctx context.Context, logins []string) (map[string]TwitchUser, error) {
	if len(logins) == 0 {
		return map[string]TwitchUser{}, nil
	}
	q := url.Values{}
	for _, l := range logins {
		if l != "" {
			q.Add("login", strings.ToLower(l))
		}
	}

	var resp struct {
		Data []TwitchUser `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/users?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}

	out := make(map[string]TwitchUser, len(resp.Data))
	for _, u := range resp.Data {
		out[strings.ToLower(u.Login)] = u
	}
	return out, nil
}

// TwitchStream is one entry from GET /helix/streams.
type TwitchStream struct {
	ID           string `json:"id"`
	UserID       string `json:"user_id"`
	UserLogin    string `json:"user_login"`
	UserName     string `json:"user_name"`
	GameName     string `json:"game_name"`
	Type         string `json:"type"`
	Title        string `json:"title"`
	StartedAt    string `json:"started_at"`
	ThumbnailURL string `json:"thumbnail_url"`
}

// GetStreams returns the live streams among the given logins. Up to 100 logins
// fit in one request, so every configured channel is covered by a single call
// costing one rate-limit point.
//
// A login that is offline is simply ABSENT from the response - the endpoint is
// level-triggered on presence. Absence is not proof of being offline though:
// Helix responses are edge-cached and a genuinely live channel can be missing
// from one poll, which is why end detection requires several consecutive
// absences rather than one.
func (c *TwitchClient) GetStreams(ctx context.Context, logins []string) ([]TwitchStream, error) {
	if len(logins) == 0 {
		return nil, nil
	}
	q := url.Values{}
	for _, l := range logins {
		if l != "" {
			q.Add("user_login", strings.ToLower(l))
		}
	}
	q.Set("first", "100")

	var resp struct {
		Data []TwitchStream `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/streams?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// EventSub subscription types and the transport we use.
const (
	EventSubTypeStreamOnline  = "stream.online"
	EventSubTypeStreamOffline = "stream.offline"
	eventSubVersion           = "1"
)

// EventSubTransport describes where Twitch should deliver notifications.
type EventSubTransport struct {
	Method   string `json:"method"`
	Callback string `json:"callback"`
	// Secret is write-only: Twitch never returns it on a read, so losing it
	// means every existing subscription must be deleted and recreated.
	Secret string `json:"secret,omitempty"`
}

// EventSubSubscription is one subscription as Twitch reports it.
type EventSubSubscription struct {
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	Type      string            `json:"type"`
	Version   string            `json:"version"`
	Condition map[string]string `json:"condition"`
	Transport EventSubTransport `json:"transport"`
	CreatedAt string            `json:"created_at"`
	Cost      int               `json:"cost"`
}

// EventSub subscription statuses we act on.
const (
	EventSubStatusEnabled             = "enabled"
	EventSubStatusVerificationPending = "webhook_callback_verification_pending"
	EventSubStatusVerificationFailed  = "webhook_callback_verification_failed"
)

// CreateEventSubSubscription registers a webhook subscription for one
// broadcaster. A 409 means an identical subscription already exists, which is
// success for our purposes - reconciliation adopts it.
func (c *TwitchClient) CreateEventSubSubscription(ctx context.Context, subType, broadcasterID, callback, secret string) (*EventSubSubscription, error) {
	body, err := json.Marshal(map[string]any{
		"type":      subType,
		"version":   eventSubVersion,
		"condition": map[string]string{"broadcaster_user_id": broadcasterID},
		"transport": EventSubTransport{Method: "webhook", Callback: callback, Secret: secret},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal eventsub subscription: %w", err)
	}

	var resp struct {
		Data []EventSubSubscription `json:"data"`
	}
	err = c.do(ctx, http.MethodPost, "/eventsub/subscriptions", body, &resp)
	if err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, errors.New("twitch returned no subscription")
	}
	return &resp.Data[0], nil
}

// ListEventSubSubscriptions returns every subscription for this client id,
// following pagination.
//
// The scope is the CLIENT ID, not this deployment: a dev box or a twitch-cli
// session sharing the credentials shows up here too. Callers must never delete
// a subscription solely because they did not create it - see the callback-host
// check in the reconciler.
func (c *TwitchClient) ListEventSubSubscriptions(ctx context.Context) ([]EventSubSubscription, error) {
	var out []EventSubSubscription
	cursor := ""
	for range 20 { // bounded: 20 pages is far past any plausible real count
		path := "/eventsub/subscriptions"
		if cursor != "" {
			path += "?after=" + url.QueryEscape(cursor)
		}
		var resp struct {
			Data       []EventSubSubscription `json:"data"`
			Pagination struct {
				Cursor string `json:"cursor"`
			} `json:"pagination"`
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		if resp.Pagination.Cursor == "" {
			break
		}
		cursor = resp.Pagination.Cursor
	}
	return out, nil
}

// DeleteEventSubSubscription removes one subscription by id.
func (c *TwitchClient) DeleteEventSubSubscription(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/eventsub/subscriptions?id="+url.QueryEscape(id), nil, nil)
}
