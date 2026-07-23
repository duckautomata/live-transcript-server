package archive

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordedRequest captures what the archive server saw for one request.
type recordedRequest struct {
	method      string
	escapedPath string
	apiKey      string
}

// newTestServer returns a client pointed at an httptest archive server driven
// by handler, plus a pointer to the last recorded request.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*Client, *recordedRequest) {
	t.Helper()
	last := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last.method = r.Method
		last.escapedPath = r.URL.EscapedPath()
		last.apiKey = r.Header.Get("X-API-Key")
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	// Trailing slash must be trimmed by NewClient, or the request path would
	// contain a double slash.
	return NewClient(srv.URL+"/", "secret-key"), last
}

func TestConfigured(t *testing.T) {
	if !NewClient("http://archive", "key").Configured() {
		t.Error("expected configured with both url and key")
	}
	if NewClient("", "key").Configured() {
		t.Error("expected unconfigured with empty url")
	}
	if NewClient("http://archive", "").Configured() {
		t.Error("expected unconfigured with empty key")
	}
}

func TestListKeys(t *testing.T) {
	c, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"key":"abc","expiresAt":"2026-01-01T00:00:00Z"}]`)
	})

	keys, err := c.ListKeys(context.Background(), "Mint Fantôme")
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if last.method != http.MethodGet {
		t.Errorf("method=%q want GET", last.method)
	}
	// membersName must be path-escaped in the request URL.
	if last.escapedPath != "/membership/Mint%20Fant%C3%B4me" {
		t.Errorf("path=%q want escaped members name", last.escapedPath)
	}
	if last.apiKey != "secret-key" {
		t.Errorf("X-API-Key=%q want the configured key", last.apiKey)
	}
	if len(keys) != 1 || keys[0].Key != "abc" || keys[0].ExpiresAt != "2026-01-01T00:00:00Z" {
		t.Errorf("keys=%+v want the decoded key", keys)
	}
}

func TestListKeys_NullBodyBecomesEmptySlice(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `null`)
	})

	keys, err := c.ListKeys(context.Background(), "doki")
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if keys == nil {
		t.Fatal("keys is nil, want empty slice")
	}
	if len(keys) != 0 {
		t.Errorf("keys=%+v want empty", keys)
	}
}

func TestListKeys_ErrorOmitsResponseBody(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "super secret backend details")
	})

	_, err := c.ListKeys(context.Background(), "doki")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err=%q want status code included", err)
	}
	if strings.Contains(err.Error(), "super secret") {
		t.Errorf("err=%q must not leak the response body", err)
	}
}

func TestCreateKey(t *testing.T) {
	c, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"key":"new-key","expiresAt":"2026-02-01T00:00:00Z"}`)
	})

	key, err := c.CreateKey(context.Background(), "doki")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if last.method != http.MethodPost {
		t.Errorf("method=%q want POST", last.method)
	}
	if key.Key != "new-key" || key.ExpiresAt != "2026-02-01T00:00:00Z" {
		t.Errorf("key=%+v want the decoded key", key)
	}
}

func TestDeleteKeys(t *testing.T) {
	c, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// A body on success exercises the drain-and-close path.
		fmt.Fprint(w, `{"deleted":2}`)
	})

	if err := c.DeleteKeys(context.Background(), "doki"); err != nil {
		t.Fatalf("DeleteKeys: %v", err)
	}
	if last.method != http.MethodDelete {
		t.Errorf("method=%q want DELETE", last.method)
	}
	if last.escapedPath != "/membership/doki" {
		t.Errorf("path=%q want /membership/doki", last.escapedPath)
	}
}
