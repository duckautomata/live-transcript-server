package livedetect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// newTwitchTestClient points a client at an httptest server standing in for
// both id.twitch.tv and api.twitch.tv.
func newTwitchTestClient(t *testing.T, handler http.HandlerFunc) *TwitchClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewTwitchClient("test-client-id", "test-client-secret")
	c.HelixURL = srv.URL
	c.IDURL = srv.URL
	return c
}

// tokenHandler answers the client_credentials mint.
func tokenHandler(w http.ResponseWriter) {
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": "app-token-1", "expires_in": 5000000, "token_type": "bearer",
	})
}

func TestTwitchConfigured(t *testing.T) {
	if !NewTwitchClient("a", "b").Configured() {
		t.Error("both credentials present should be configured")
	}
	if NewTwitchClient("", "b").Configured() {
		t.Error("a missing client id must be unconfigured")
	}
	if NewTwitchClient("a", "").Configured() {
		t.Error("a missing secret must be unconfigured")
	}
	var nilClient *TwitchClient
	if nilClient.Configured() {
		t.Error("a nil client must be unconfigured, not panic")
	}
}

func TestGetStreamsSendsCredentialsAndBatchesLogins(t *testing.T) {
	var gotAuth, gotClientID string
	var gotLogins []string

	c := newTwitchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			tokenHandler(w)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotClientID = r.Header.Get("Client-Id")
		gotLogins = r.URL.Query()["user_login"]
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "111", "user_login": "dokibird", "type": "live",
					"title": "hi", "started_at": "2026-01-01T19:00:00Z"},
			},
		})
	})

	streams, err := c.GetStreams(context.Background(), []string{"DokiBird", "mintfantome"})
	if err != nil {
		t.Fatalf("GetStreams: %v", err)
	}
	if gotAuth != "Bearer app-token-1" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotClientID != "test-client-id" {
		t.Errorf("Client-Id = %q; Helix rejects a request without it", gotClientID)
	}
	// Logins are repeated params (never comma-joined) and lowercased.
	if len(gotLogins) != 2 || gotLogins[0] != "dokibird" || gotLogins[1] != "mintfantome" {
		t.Errorf("user_login params = %v", gotLogins)
	}
	if len(streams) != 1 || streams[0].ID != "111" {
		t.Fatalf("unexpected streams: %+v", streams)
	}
}

// An offline channel is simply absent from the response, which is what makes
// the endpoint level-triggered.
func TestGetStreamsOmitsOfflineChannels(t *testing.T) {
	c := newTwitchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			tokenHandler(w)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
	})

	streams, err := c.GetStreams(context.Background(), []string{"offline"})
	if err != nil {
		t.Fatalf("GetStreams: %v", err)
	}
	if len(streams) != 0 {
		t.Fatalf("expected no streams, got %+v", streams)
	}
}

// App access tokens have no refresh token: the recovery from a 401 is to mint
// a new one and replay the SAME request exactly once.
func TestTwitchRetriesOnceOn401(t *testing.T) {
	var mints, helixCalls atomic.Int32

	c := newTwitchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			mints.Add(1)
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "token", "expires_in": 5000000, "token_type": "bearer",
			})
			return
		}
		// Fail the first Helix call, succeed the second.
		if helixCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
	})

	if _, err := c.GetStreams(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("GetStreams should have recovered from a 401: %v", err)
	}
	if got := helixCalls.Load(); got != 2 {
		t.Errorf("helix calls = %d, want exactly 2 (one retry, never a loop)", got)
	}
	if got := mints.Load(); got != 2 {
		t.Errorf("token mints = %d, want 2 (initial plus the forced re-mint)", got)
	}
}

func TestTwitchGivesUpAfterASecond401(t *testing.T) {
	var helixCalls atomic.Int32
	c := newTwitchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			tokenHandler(w)
			return
		}
		helixCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})

	if _, err := c.GetStreams(context.Background(), []string{"x"}); err == nil {
		t.Fatal("expected an error after a persistent 401")
	}
	if got := helixCalls.Load(); got != 2 {
		t.Errorf("helix calls = %d, want 2; a persistent 401 must never loop", got)
	}
}

// Logins Twitch does not know are omitted from the response rather than
// erroring, so the caller must never index positionally.
func TestGetUsersOmitsUnknownLogins(t *testing.T) {
	c := newTwitchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			tokenHandler(w)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "1234", "login": "dokibird", "display_name": "Dokibird"}},
		})
	})

	users, err := c.GetUsers(context.Background(), []string{"dokibird", "doesnotexist"})
	if err != nil {
		t.Fatalf("GetUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("got %d users, want 1", len(users))
	}
	if users["dokibird"].ID != "1234" {
		t.Errorf("unexpected user: %+v", users["dokibird"])
	}
	if _, ok := users["doesnotexist"]; ok {
		t.Error("an unknown login must be absent from the map")
	}
}

func TestCreateEventSubSubscriptionBody(t *testing.T) {
	var body map[string]any
	c := newTwitchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			tokenHandler(w)
			return
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"id": "sub-1", "status": EventSubStatusVerificationPending, "type": EventSubTypeStreamOnline,
			}},
		})
	})

	sub, err := c.CreateEventSubSubscription(context.Background(),
		EventSubTypeStreamOnline, "1234", "https://example.test/cb", "a-secret-value")
	if err != nil {
		t.Fatalf("CreateEventSubSubscription: %v", err)
	}
	if sub.Status != EventSubStatusVerificationPending {
		t.Errorf("status = %q", sub.Status)
	}
	// version must be the string "1", and the condition keys on
	// broadcaster_user_id rather than a login.
	if body["version"] != "1" {
		t.Errorf("version = %v, want the string \"1\"", body["version"])
	}
	cond := body["condition"].(map[string]any)
	if cond["broadcaster_user_id"] != "1234" {
		t.Errorf("condition = %v", cond)
	}
	tr := body["transport"].(map[string]any)
	if tr["method"] != "webhook" || tr["callback"] != "https://example.test/cb" || tr["secret"] != "a-secret-value" {
		t.Errorf("transport = %v", tr)
	}
}

func TestCallbackHost(t *testing.T) {
	// Subscription cleanup is scoped by callback host so two deployments
	// sharing a client id cannot delete each other's subscriptions.
	if got := callbackHost("https://api.example.test/livedetect/twitch/eventsub"); got != "api.example.test" {
		t.Errorf("callbackHost = %q", got)
	}
	if got := callbackHost("https://OTHER.example.test/cb"); got != "other.example.test" {
		t.Errorf("callbackHost should normalise case, got %q", got)
	}
	if got := callbackHost("://nonsense"); got != "" {
		t.Errorf("an unparseable url should yield an empty host, got %q", got)
	}
}
