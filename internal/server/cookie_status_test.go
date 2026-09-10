package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"live-transcript-server/internal/model"
	"live-transcript-server/internal/store"
)

func cookieStatus(t *testing.T, app *App) *model.CookieStatus {
	t.Helper()
	st, err := app.Store.GetCookieStatus(context.Background(), store.DefaultWorkerID)
	if err != nil {
		t.Fatalf("GetCookieStatus: %v", err)
	}
	return st
}

func TestCookieStatus_AlertsOnceThenRecovers(t *testing.T) {
	app, _ := setupTestApp(t, []string{"chan"})
	ctx := context.Background()

	app.recordCookieStatus(ctx, model.CookieStateOK, "", 1000)
	if got := cookieStatus(t, app); got.Alerted {
		t.Fatalf("a healthy first report must not alert, got %+v", got)
	}

	app.recordCookieStatus(ctx, model.CookieStateRotated, "rotated mid-request", 2000)
	got := cookieStatus(t, app)
	if !got.Alerted {
		t.Fatal("going degraded must alert")
	}
	if got.Since != 2000 {
		t.Fatalf("since should mark the transition, got %d", got.Since)
	}

	// Repeated degraded heartbeats must not re-alert, and must not restart
	// the clock: the operator gets one ping per outage.
	for _, at := range []int64{2060, 2120, 2180} {
		app.recordCookieStatus(ctx, model.CookieStateRotated, "rotated mid-request", at)
	}
	got = cookieStatus(t, app)
	if got.Since != 2000 {
		t.Fatalf("since must survive repeated reports of the same state, got %d", got.Since)
	}
	if !got.Alerted {
		t.Fatal("alerted must stay set for the duration of the outage")
	}

	app.recordCookieStatus(ctx, model.CookieStateOK, "", 3000)
	got = cookieStatus(t, app)
	if got.Alerted {
		t.Fatal("recovery must clear the alerted flag so the next outage alerts again")
	}
	if got.State != model.CookieStateOK {
		t.Fatalf("state = %q, want ok", got.State)
	}
}

func TestCookieStatus_SurvivesServerRestartWithoutRespamming(t *testing.T) {
	// The alerted flag is persisted precisely so a redeploy mid-outage does
	// not re-ping about an outage the operator already knows about. Simulate
	// the redeploy by re-reading the row and feeding the same state again.
	app, _ := setupTestApp(t, []string{"chan"})
	ctx := context.Background()

	app.recordCookieStatus(ctx, model.CookieStateAbsent, "no usable account cookies", 1000)
	before := cookieStatus(t, app)
	if !before.Alerted {
		t.Fatal("expected the first degraded report to alert")
	}

	app.recordCookieStatus(ctx, model.CookieStateAbsent, "no usable account cookies", 5000)
	after := cookieStatus(t, app)
	if after.Since != before.Since {
		t.Fatalf("since changed across a restart: %d -> %d", before.Since, after.Since)
	}
	if !after.Alerted {
		t.Fatal("alerted must persist across a restart")
	}
}

func TestCookieStatus_UnreportedAndNAAreNotFailures(t *testing.T) {
	app, _ := setupTestApp(t, []string{"chan"})
	ctx := context.Background()

	// An older worker omits the field entirely; a worker with cookies
	// disabled sends "na". Neither is evidence of a problem, and neither may
	// create a row that later reads as a state change.
	app.recordCookieStatus(ctx, "", "", 1000)
	app.recordCookieStatus(ctx, model.CookieStateNA, "", 1000)

	if got := cookieStatus(t, app); got != nil {
		t.Fatalf("expected no cookie row, got %+v", got)
	}
}

func TestCookieStatus_UnknownStateIsTreatedAsHealthy(t *testing.T) {
	// A newer worker reporting a state this server does not know must not be
	// able to page the operator.
	app, _ := setupTestApp(t, []string{"chan"})
	ctx := context.Background()

	app.recordCookieStatus(ctx, "some-future-state", "", 1000)
	got := cookieStatus(t, app)
	if got == nil {
		t.Fatal("the state should still be recorded")
	}
	if got.Alerted {
		t.Fatal("an unknown state must not alert")
	}
}

func TestCookieDegraded(t *testing.T) {
	for state, want := range map[string]bool{
		model.CookieStateOK:      false,
		model.CookieStateNA:      false,
		model.CookieStateAbsent:  true,
		model.CookieStateRotated: true,
		"":                       false,
		"unknown":                false,
	} {
		if got := model.CookieDegraded(state); got != want {
			t.Errorf("CookieDegraded(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestWorkerStatusHandler_AcceptsBothWireVersions(t *testing.T) {
	// Worker and server deploy independently, so /status must accept a body
	// from an old worker (no cookie fields) and from a new one (with them).
	app, mux := setupTestApp(t, []string{"chan"})

	for _, tc := range []struct {
		name        string
		body        string
		wantCookies bool
	}{
		{"old worker", `{"version":"1.0.4","build_time":"b","keys":["chan"]}`, false},
		{
			"new worker",
			`{"version":"1.1.0","build_time":"b","keys":["chan"],"cookie_state":"rotated","cookie_reason":"why"}`,
			true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/status", strings.NewReader(tc.body))
			req.Header.Set("X-API-Key", "test-api-key")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			got := cookieStatus(t, app)
			if tc.wantCookies && got == nil {
				t.Fatal("expected cookie health to be recorded")
			}
			if !tc.wantCookies && got != nil {
				t.Fatalf("an old worker must not create a cookie row, got %+v", got)
			}
		})
	}
}

// The reference worker (live-transcript-worker, threads + local whisper) posts
// this exact body from status_reporter.py and has no cookie support enabled.
// It shares this server with the cloud worker, so the cookie feature must be
// completely inert for it -- no row, no alert, and no change to the public
// status payload that the browser client parses.
func TestReferenceWorkerPayloadIsInert(t *testing.T) {
	app, mux := setupTestApp(t, []string{"doki"})

	body := `{"version": "local", "build_time": "unknown", "keys": ["doki"]}`
	req := httptest.NewRequest(http.MethodPost, "/status", strings.NewReader(body))
	req.Header.Set("X-API-Key", "test-api-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := cookieStatus(t, app); got != nil {
		t.Fatalf("a cookie-less worker must not create a cookie row, got %+v", got)
	}

	// The worker heartbeat itself must still be recorded as before.
	ws, err := app.Store.GetWorkerStatusByKey(context.Background(), "doki")
	if err != nil || ws == nil {
		t.Fatalf("worker status not recorded: %v (%+v)", err, ws)
	}

	// And GET /status must not grow a "cookies" key for clients that predate it.
	statusReq := httptest.NewRequest(http.MethodGet, "/status", nil)
	statusRec := httptest.NewRecorder()
	mux.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("GET /status = %d", statusRec.Code)
	}
	if strings.Contains(statusRec.Body.String(), "cookies") {
		t.Fatalf("public status payload gained a cookies key with no cookie report: %s", statusRec.Body.String())
	}
}
