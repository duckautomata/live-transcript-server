package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"live-transcript-server/internal/config"
	"live-transcript-server/internal/livedetect"
	"live-transcript-server/internal/model"
)

// captureDetectWebhook routes the app's live-detection alerts to a test
// webhook and returns a channel of the decoded payloads. The announcer's
// operator feed was resolved when the App was built (empty in these tests),
// so only the Discord client's own detection alerts arrive here. Posts are
// sent from a goroutine, so tests must receive rather than assert immediately.
func captureDetectWebhook(t *testing.T, app *App) chan map[string]any {
	t.Helper()
	payloads := make(chan map[string]any, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode detect webhook payload: %v", err)
		}
		payloads <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	app.Discord.DetectWebhookURL = srv.URL
	app.Discord.NotifyPing = "<@operator>"
	return payloads
}

// waitDetectAlert returns the next detection alert's ping and single embed.
func waitDetectAlert(t *testing.T, payloads chan map[string]any) (string, map[string]any) {
	t.Helper()
	var payload map[string]any
	select {
	case payload = <-payloads:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a detection alert")
	}
	embeds, ok := payload["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("payload embeds = %#v, want exactly one embed", payload["embeds"])
	}
	embed, ok := embeds[0].(map[string]any)
	if !ok {
		t.Fatalf("embed = %#v, want object", embeds[0])
	}
	content, _ := payload["content"].(string)
	return content, embed
}

func expectNoDetectAlert(t *testing.T, payloads chan map[string]any) {
	t.Helper()
	select {
	case p := <-payloads:
		t.Fatalf("unexpected detection alert: %#v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

// The 2026-10-11 incident: a stream detected two hours after it started, with
// every leg healthy throughout. The winning claim must page the operator, once,
// and must otherwise do exactly what any detection does.
func TestLateLiveDetectionPagesTheOperator(t *testing.T) {
	app, _ := setupDetectApp(t)
	alerts := captureDetectWebhook(t, app)
	app.QueueIncoming = true
	ctx := context.Background()

	late := livedetect.Broadcast{
		Platform:   livedetect.PlatformYouTube,
		ChannelKey: "doki",
		ID:         "late-1",
		URL:        livedetect.YouTubeWatchURL("late-1"),
		StartedAt:  time.Now().Add(-2 * time.Hour),
	}
	if err := app.ObserveLive(ctx, late, livedetect.MechanismYouTubeDiscover); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}

	ping, embed := waitDetectAlert(t, alerts)
	if ping != "<@operator>" {
		t.Errorf("content = %q, want the operator ping: a late detection is a missed stream", ping)
	}
	if got := embed["title"]; got != "Live Detection Was Late" {
		t.Errorf("title = %v", got)
	}
	desc, _ := embed["description"].(string)
	for _, want := range []string{
		"**Dokibird** went live on YouTube 2h0m", // +0-1 s, however the clock ticks
		"[late-1](https://www.youtube.com/watch?v=late-1)",
		"**Detected by:** " + livedetect.MechanismYouTubeDiscover,
		"**Watched as scheduled:** no",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("description = %q\nmissing %q", desc, want)
		}
	}

	// Every other leg that goes on to see it loses the claim and stays quiet.
	if err := app.ObserveLive(ctx, late, livedetect.MechanismYouTubeState); err != nil {
		t.Fatalf("ObserveLive (replay): %v", err)
	}
	expectNoDetectAlert(t, alerts)

	// The alert only reports. Whether a late stream should still be queued and
	// announced is a separate policy decision; until one is made it is treated
	// like any other detection.
	det, err := app.Store.GetDetection(ctx, livedetect.PlatformYouTube, "late-1")
	if err != nil || det == nil {
		t.Fatalf("detection missing: %v %v", det, err)
	}
	if det.Mechanism != livedetect.MechanismYouTubeDiscover {
		t.Errorf("mechanism = %q, want the winning leg", det.Mechanism)
	}
	urls, err := app.Store.GetIncomingStreams(ctx, "doki")
	if err != nil {
		t.Fatalf("incoming: %v", err)
	}
	if len(urls) != 1 || urls[0] != late.URL {
		t.Errorf("incoming = %v, want the late stream queued as usual", urls)
	}
}

// Healthy detections land within seconds, and a missing start time makes the
// delay unknown - neither may page anyone.
func TestTimelyOrUnknownDelayDoesNotPage(t *testing.T) {
	app, _ := setupDetectApp(t)
	alerts := captureDetectWebhook(t, app)
	ctx := context.Background()

	for _, b := range []livedetect.Broadcast{
		{
			Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "on-time",
			URL: livedetect.YouTubeWatchURL("on-time"), StartedAt: time.Now().Add(-5 * time.Second),
			SawScheduled: true,
		},
		{
			// Slow but under the threshold: a surprise go-live on a slow
			// discovery cadence.
			Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "slow",
			URL: livedetect.YouTubeWatchURL("slow"), StartedAt: time.Now().Add(-9 * time.Minute),
		},
		{
			// No start time from the platform: unknown, never late.
			Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "no-start",
			URL: livedetect.YouTubeWatchURL("no-start"),
		},
	} {
		if err := app.ObserveLive(ctx, b, livedetect.MechanismYouTubeState); err != nil {
			t.Fatalf("ObserveLive(%s): %v", b.ID, err)
		}
		if det, _ := app.Store.GetDetection(ctx, livedetect.PlatformYouTube, b.ID); det == nil {
			t.Fatalf("detection %s was not recorded", b.ID)
		}
	}
	expectNoDetectAlert(t, alerts)
}

// A claim the search audit rescued was paged by the audit when it found the
// broadcast, with the diagnosis in it; a "late" ping seconds later about the
// same stream would only teach the operator that pings come in pairs. It is
// still recorded, and still logged. An operator's rescue is paged as usual:
// whoever ran the check is not necessarily the operator the feed is for.
func TestAuditRescuedLateDetectionIsNotPagedTwice(t *testing.T) {
	app, _ := setupDetectApp(t)
	alerts := captureDetectWebhook(t, app)
	ctx := context.Background()
	started := time.Now().Add(-2 * time.Hour)

	rescued := livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "rescued",
		URL: livedetect.YouTubeWatchURL("rescued"), StartedAt: started,
	}
	if err := app.ObserveLive(ctx, rescued, livedetect.MechanismYouTubeAudit); err != nil {
		t.Fatalf("ObserveLive(audit): %v", err)
	}
	expectNoDetectAlert(t, alerts)
	det, err := app.Store.GetDetection(ctx, livedetect.PlatformYouTube, "rescued")
	if err != nil || det == nil || det.Mechanism != livedetect.MechanismYouTubeAudit {
		t.Fatalf("detection = %+v, %v; want it recorded under the audit", det, err)
	}

	checked := livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "checked",
		URL: livedetect.YouTubeWatchURL("checked"), StartedAt: started,
	}
	if err := app.ObserveLive(ctx, checked, livedetect.MechanismYouTubeManual); err != nil {
		t.Fatalf("ObserveLive(manual): %v", err)
	}
	_, embed := waitDetectAlert(t, alerts)
	if desc, _ := embed["description"].(string); !strings.Contains(desc, "**Detected by:** "+livedetect.MechanismYouTubeManual) {
		t.Errorf("description = %q, want the operator's rescue named", desc)
	}
}

// Ten minutes is a floor, not the threshold. Discovery's cadence is
// configurable without an upper bound - and the startup quota projection tells
// a deployment with many channels to slow it - so a fixed ten minutes would
// page every surprise go-live such a deployment finds on schedule.
func TestLateDetectionThresholdFollowsTheDiscoveryCadence(t *testing.T) {
	app, _ := setupDetectApp(t)
	alerts := captureDetectWebhook(t, app)
	ctx := context.Background()

	// The defaults: two-minute discovery, one-minute Helix polls.
	for _, platform := range []string{livedetect.PlatformYouTube, livedetect.PlatformTwitch} {
		if got := app.lateDetectionThreshold(platform); got != lateDetectionAlertAfter {
			t.Errorf("%s threshold at the default cadence = %s, want %s", platform, got, lateDetectionAlertAfter)
		}
	}

	slow, err := livedetect.New(config.LiveDetectConfig{
		Enabled: true,
		YouTube: config.LiveDetectYouTubeConfig{Enabled: true, ApiKey: "k", DiscoverySeconds: 900},
	}, []config.ChannelConfig{{Name: "doki", YouTubeChannelId: testYTChannelID}}, app, nil)
	if err != nil {
		t.Fatalf("livedetect.New: %v", err)
	}
	t.Cleanup(func() { slow.Close() })
	app.LiveDetect = slow

	if got, want := app.lateDetectionThreshold(livedetect.PlatformYouTube), 15*time.Minute+lateDetectionMargin; got != want {
		t.Errorf("youtube threshold with 15-minute discovery = %s, want %s", got, want)
	}
	if got := app.lateDetectionThreshold(livedetect.PlatformTwitch); got != lateDetectionAlertAfter {
		t.Errorf("twitch threshold with twitch off = %s, want the floor", got)
	}

	onSchedule := livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "on-schedule",
		URL: livedetect.YouTubeWatchURL("on-schedule"), StartedAt: time.Now().Add(-14 * time.Minute),
	}
	if err := app.ObserveLive(ctx, onSchedule, livedetect.MechanismYouTubeDiscover); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}
	expectNoDetectAlert(t, alerts)

	late := livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "late-even-so",
		URL: livedetect.YouTubeWatchURL("late-even-so"), StartedAt: time.Now().Add(-45 * time.Minute),
	}
	if err := app.ObserveLive(ctx, late, livedetect.MechanismYouTubeDiscover); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}
	if _, embed := waitDetectAlert(t, alerts); embed["title"] != "Live Detection Was Late" {
		t.Errorf("title = %v", embed["title"])
	}
}

func TestDetectionDelay(t *testing.T) {
	if _, ok := detectionDelay(model.DetectedBroadcast{DetectedAt: 1_700_000_000}); ok {
		t.Error("a zero start time must read as unknown, not as a delay")
	}
	delay, ok := detectionDelay(model.DetectedBroadcast{StartedAt: 1_700_000_000, DetectedAt: 1_700_007_349})
	if !ok || delay != 7349*time.Second {
		t.Errorf("delay = %v, %v; want 2h2m29s", delay, ok)
	}
	if delay <= lateDetectionAlertAfter {
		t.Errorf("the incident's %v delay must be past the %v alert threshold", delay, lateDetectionAlertAfter)
	}
}
