// Package archive is the HTTP client for the archive server's membership-key
// API. The server proxies admin-page requests through it so the shared
// X-API-Key never leaves the backend.
package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// KeyResponse mirrors the archive server's membership-key model
// (docks/archive-membership-keys-api.md). ExpiresAt is an RFC3339 string
// computed by the archive server at read time.
type KeyResponse struct {
	Key       string `json:"key"`
	ExpiresAt string `json:"expiresAt"`
}

// Client talks to the archive server's membership API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient constructs a client for the archive server at baseURL (trailing
// slashes are trimmed). Empty values are allowed and leave the client
// unconfigured; see Configured.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Configured reports whether the archive server connection is configured at
// all. Callers must still check the channel-level MembersName before enabling
// membership features for a channel.
func (c *Client) Configured() bool {
	return c.baseURL != "" && c.apiKey != ""
}

// request performs a single request against the archive server's membership
// API. membersName is always derived from config (never client input); it is
// path-escaped before being interpolated into the URL. The shared X-API-Key is
// attached here so it never leaves the server.
//
// On a non-2xx response it returns an error that includes the status code but
// not the response body, so archive internals are not surfaced to callers.
func (c *Client) request(ctx context.Context, method, membersName string) (*http.Response, error) {
	endpoint := fmt.Sprintf("%s/membership/%s", c.baseURL, url.PathEscape(membersName))
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build archive request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call archive %s %s: %w", method, endpoint, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain and close so the connection can be reused, but do not include
		// the body in the error returned to the caller.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("archive %s %s returned status %d", method, endpoint, resp.StatusCode)
	}
	return resp, nil
}

// ListKeys returns all membership keys for the channel's archive-side name. It
// maps to the archive's GET /membership/{channelName}, which is
// side-effect-free.
func (c *Client) ListKeys(ctx context.Context, membersName string) ([]KeyResponse, error) {
	resp, err := c.request(ctx, http.MethodGet, membersName)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var keys []KeyResponse
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		return nil, fmt.Errorf("decode archive key list: %w", err)
	}
	if keys == nil {
		keys = []KeyResponse{}
	}
	return keys, nil
}

// CreateKey creates (or rotates) a membership key for the channel's
// archive-side name via the archive's POST /membership/{channelName}. The
// archive enforces a 2-key cap and prunes older keys itself.
func (c *Client) CreateKey(ctx context.Context, membersName string) (KeyResponse, error) {
	resp, err := c.request(ctx, http.MethodPost, membersName)
	if err != nil {
		return KeyResponse{}, err
	}
	defer resp.Body.Close()

	var key KeyResponse
	if err := json.NewDecoder(resp.Body).Decode(&key); err != nil {
		return KeyResponse{}, fmt.Errorf("decode archive created key: %w", err)
	}
	return key, nil
}

// DeleteKeys deletes all membership keys for the channel's archive-side name
// via the archive's DELETE /membership/{channelName}. The archive API has no
// per-key delete, so this is all-or-nothing.
func (c *Client) DeleteKeys(ctx context.Context, membersName string) error {
	resp, err := c.request(ctx, http.MethodDelete, membersName)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	resp.Body.Close()
	return nil
}
