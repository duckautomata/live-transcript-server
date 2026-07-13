package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// KeyResponse mirrors the archive server's membership-key model
// (docks/archive-membership-keys-api.md). ExpiresAt is an RFC3339 string
// computed by the archive server at read time.
type KeyResponse struct {
	Key       string `json:"key"`
	ExpiresAt string `json:"expiresAt"`
}

// membershipEnabled reports whether membership-key management is available for
// the given channel. It requires the archive server to be configured globally
// and the channel to have an archive-side name mapped. Fail closed: any missing
// piece disables the feature.
func (app *App) membershipEnabled(cs *ChannelState) bool {
	return app.ArchiveURL != "" && app.ArchiveKey != "" && cs.MembersName != ""
}

// archiveRequest performs a single request against the archive server's
// membership API. membersName is always derived from config (never client
// input); it is path-escaped before being interpolated into the URL. The
// shared X-API-Key is attached here so it never leaves the server.
//
// On a non-2xx response it returns an error that includes the status code but
// not the response body, so archive internals are not surfaced to callers.
func (app *App) archiveRequest(ctx context.Context, method, membersName string) (*http.Response, error) {
	endpoint := fmt.Sprintf("%s/membership/%s", app.ArchiveURL, url.PathEscape(membersName))
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build archive request: %w", err)
	}
	req.Header.Set("X-API-Key", app.ArchiveKey)

	resp, err := app.ArchiveClient.Do(req)
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

// listMembershipKeys returns all membership keys for the channel's archive-side
// name. It maps to the archive's GET /membership/{channelName}, which is
// side-effect-free.
func (app *App) listMembershipKeys(ctx context.Context, membersName string) ([]KeyResponse, error) {
	resp, err := app.archiveRequest(ctx, http.MethodGet, membersName)
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

// createMembershipKey creates (or rotates) a membership key for the channel's
// archive-side name via the archive's POST /membership/{channelName}. The
// archive enforces a 2-key cap and prunes older keys itself.
func (app *App) createMembershipKey(ctx context.Context, membersName string) (KeyResponse, error) {
	resp, err := app.archiveRequest(ctx, http.MethodPost, membersName)
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

// deleteMembershipKeys deletes all membership keys for the channel's
// archive-side name via the archive's DELETE /membership/{channelName}. The
// archive API has no per-key delete, so this is all-or-nothing.
func (app *App) deleteMembershipKeys(ctx context.Context, membersName string) error {
	resp, err := app.archiveRequest(ctx, http.MethodDelete, membersName)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	resp.Body.Close()
	return nil
}
