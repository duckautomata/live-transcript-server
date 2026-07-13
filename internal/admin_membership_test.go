package internal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeArchive is a stand-in for the archive server's membership API. It records
// the requests it receives so tests can assert the live server proxied them
// correctly (path, X-API-Key), and lets each test control the response.
type fakeArchive struct {
	mu       sync.Mutex
	calls    []archiveCall
	status   int    // response status (default 200)
	listBody string // JSON body returned for GET
	postBody string // JSON body returned for POST
	server   *httptest.Server
}

type archiveCall struct {
	method string
	path   string
	apiKey string
}

func newFakeArchive(t *testing.T) *fakeArchive {
	t.Helper()
	fa := &fakeArchive{
		status:   http.StatusOK,
		listBody: `[{"key":"abc123","expiresAt":"2026-08-11T00:00:00Z"}]`,
		postBody: `{"key":"new-key","expiresAt":"2026-08-11T00:00:00Z"}`,
	}
	fa.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fa.mu.Lock()
		fa.calls = append(fa.calls, archiveCall{method: r.Method, path: r.URL.Path, apiKey: r.Header.Get("X-API-Key")})
		status := fa.status
		fa.mu.Unlock()

		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(fa.listBody))
		case http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(fa.postBody))
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(fa.server.Close)
	return fa
}

func (fa *fakeArchive) lastCall() (archiveCall, bool) {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.calls) == 0 {
		return archiveCall{}, false
	}
	return fa.calls[len(fa.calls)-1], true
}

func (fa *fakeArchive) callCount() int {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	return len(fa.calls)
}

// setupMembershipApp builds an app whose "doki" channel has membership enabled
// (pointed at the fake archive) and whose "mint" channel does not (no
// membersName), so tests can cover both the enabled and disabled paths.
func setupMembershipApp(t *testing.T, archiveURL, archiveKey string) (*App, *http.ServeMux) {
	t.Helper()
	db, err := InitDB(":memory:", DatabaseConfig{SkipWarmup: true})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	cfg := Config{
		Credentials: struct {
			ApiKey string `yaml:"apiKey"`
		}{ApiKey: "test"},
		ArchiveURL: archiveURL,
		ArchiveKey: archiveKey,
		Channels: []ChannelConfig{
			{Name: "doki", NumPastStreams: 1, AdminKey: "admin-doki", MembersName: "DokiArchive"},
			{Name: "mint", NumPastStreams: 1, AdminKey: "admin-mint"}, // no membersName
		},
		Storage: StorageConfig{Type: "local"},
	}
	app := NewApp(cfg, db, t.TempDir(), "v", "b")
	t.Cleanup(func() { app.Close() })
	mux := http.NewServeMux()
	app.RegisterRoutes(mux)
	return app, mux
}

func TestMembershipList(t *testing.T) {
	fa := newFakeArchive(t)
	_, mux := setupMembershipApp(t, fa.server.URL, "archive-secret")

	rec := adminReq(t, mux, http.MethodGet, "/doki/admin/membership", "admin-doki", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status=%d want 200, body=%s", rec.Code, rec.Body.String())
	}
	var keys []KeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(keys) != 1 || keys[0].Key != "abc123" {
		t.Fatalf("unexpected keys: %+v", keys)
	}
	// The archive must have been called for the configured members name with
	// the shared X-API-Key.
	call, ok := fa.lastCall()
	if !ok {
		t.Fatal("archive was not called")
	}
	if call.method != http.MethodGet || call.path != "/membership/DokiArchive" {
		t.Errorf("archive call = %s %s, want GET /membership/DokiArchive", call.method, call.path)
	}
	if call.apiKey != "archive-secret" {
		t.Errorf("archive X-API-Key = %q, want archive-secret", call.apiKey)
	}
}

func TestMembershipCreate(t *testing.T) {
	fa := newFakeArchive(t)
	_, mux := setupMembershipApp(t, fa.server.URL, "archive-secret")

	rec := adminReq(t, mux, http.MethodPost, "/doki/admin/membership", "admin-doki", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status=%d want 200, body=%s", rec.Code, rec.Body.String())
	}
	var key KeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &key); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if key.Key != "new-key" {
		t.Errorf("key = %q, want new-key", key.Key)
	}
	call, _ := fa.lastCall()
	if call.method != http.MethodPost || call.path != "/membership/DokiArchive" {
		t.Errorf("archive call = %s %s, want POST /membership/DokiArchive", call.method, call.path)
	}
}

func TestMembershipDelete(t *testing.T) {
	fa := newFakeArchive(t)
	_, mux := setupMembershipApp(t, fa.server.URL, "archive-secret")

	rec := adminReq(t, mux, http.MethodDelete, "/doki/admin/membership", "admin-doki", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status=%d want 204, body=%s", rec.Code, rec.Body.String())
	}
	call, _ := fa.lastCall()
	if call.method != http.MethodDelete || call.path != "/membership/DokiArchive" {
		t.Errorf("archive call = %s %s, want DELETE /membership/DokiArchive", call.method, call.path)
	}
}

func TestMembershipAuth(t *testing.T) {
	fa := newFakeArchive(t)
	_, mux := setupMembershipApp(t, fa.server.URL, "archive-secret")

	// Wrong admin key -> 403 and the archive must never be contacted.
	rec := adminReq(t, mux, http.MethodGet, "/doki/admin/membership", "wrong", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("wrong key: status=%d want 403", rec.Code)
	}
	// Cross-channel key -> 403.
	rec = adminReq(t, mux, http.MethodGet, "/doki/admin/membership", "admin-mint", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-channel key: status=%d want 403", rec.Code)
	}
	if fa.callCount() != 0 {
		t.Errorf("archive was called %d times on unauthorized requests, want 0", fa.callCount())
	}
}

func TestMembershipDisabledChannel(t *testing.T) {
	fa := newFakeArchive(t)
	_, mux := setupMembershipApp(t, fa.server.URL, "archive-secret")

	// "mint" has no membersName -> feature disabled -> 404, archive untouched.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		rec := adminReq(t, mux, method, "/mint/admin/membership", "admin-mint", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s disabled channel: status=%d want 404", method, rec.Code)
		}
	}
	if fa.callCount() != 0 {
		t.Errorf("archive was called %d times for a disabled channel, want 0", fa.callCount())
	}
}

func TestMembershipDisabledWhenArchiveKeyMissing(t *testing.T) {
	fa := newFakeArchive(t)
	// Archive URL set but no archive key -> feature fails closed everywhere.
	_, mux := setupMembershipApp(t, fa.server.URL, "")

	rec := adminReq(t, mux, http.MethodGet, "/doki/admin/membership", "admin-doki", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing archive key: status=%d want 404", rec.Code)
	}
	if fa.callCount() != 0 {
		t.Errorf("archive called %d times with no key configured, want 0", fa.callCount())
	}
}

func TestMembershipArchiveError(t *testing.T) {
	fa := newFakeArchive(t)
	fa.mu.Lock()
	fa.status = http.StatusInternalServerError
	fa.mu.Unlock()
	_, mux := setupMembershipApp(t, fa.server.URL, "archive-secret")

	rec := adminReq(t, mux, http.MethodGet, "/doki/admin/membership", "admin-doki", nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("archive error: status=%d want 502", rec.Code)
	}
}

func TestMembershipEnabledFlagInInfo(t *testing.T) {
	fa := newFakeArchive(t)
	_, mux := setupMembershipApp(t, fa.server.URL, "archive-secret")

	// doki -> enabled
	rec := adminReq(t, mux, http.MethodGet, "/doki/admin/info", "admin-doki", nil)
	var info AdminInfoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode doki info: %v", err)
	}
	if !info.MembershipEnabled {
		t.Error("doki membershipEnabled = false, want true")
	}

	// mint -> disabled
	rec = adminReq(t, mux, http.MethodGet, "/mint/admin/info", "admin-mint", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode mint info: %v", err)
	}
	if info.MembershipEnabled {
		t.Error("mint membershipEnabled = true, want false")
	}
}
