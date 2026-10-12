package livedetect

import (
	"context"
	"testing"
	"time"
)

// The scheduled discovery loop itself. Its first pass runs after
// discoveryStartDelay however many requests it serves in the meantime - a check
// must not move the schedule - and it goes on serving requests after a
// scheduled pass. A regression here silently ends scheduled discovery, the only
// floor for an unscheduled go-live, while every other leg still looks healthy:
// the same silent failure class as FdwrQid9ulg.
func TestDiscoveryLoopKeepsItsScheduleAroundRequests(t *testing.T) {
	api, baseURL := newMembersFakeAPI(t)
	api.setVideo(dokiVideo(manualDokiVideo, "upcoming", time.Time{}))
	d := membersDetector(t, &recordingSink{}, baseURL)
	d.discoveryStartDelay = 300 * time.Millisecond
	d.spawn(d.runYouTubeDiscovery)
	ctx := context.Background()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("the discovery loop to start", d.ytDiscoveryRunning.Load)

	// Served inside the start delay.
	if _, err := d.CheckYouTubeVideo(ctx, "doki", manualDokiVideo); err != nil {
		t.Fatalf("check before the first pass: %v", err)
	}
	waitFor("the first scheduled pass", func() bool { return api.count(UploadsPlaylistID(membersDoki)) >= 1 })

	// Still serving after it.
	if _, err := d.CheckYouTubeVideo(ctx, "doki", manualDokiVideo); err != nil {
		t.Fatalf("check after the first pass: %v", err)
	}
	if !d.ytDiscoveryRunning.Load() {
		t.Error("the discovery loop stopped")
	}
}

// awaitDiscovery's one decision: a request that is not a pass leaves the
// schedule exactly as it was, and a manual pass restarts it a full interval
// out rather than letting the scheduled pass follow moments later and pay for
// the same playlists twice.
func TestAwaitDiscoveryRestartsTheScheduleOnlyAfterAPass(t *testing.T) {
	_, baseURL := newMembersFakeAPI(t)
	d := membersDetector(t, &recordingSink{}, baseURL)
	ctx := context.Background()
	type result struct {
		next time.Time
		ok   bool
	}
	done := make(chan result, 1)
	await := func(next time.Time) {
		go func() {
			n, ok := d.awaitDiscovery(next)
			done <- result{n, ok}
		}()
	}
	noop := func(time.Time) error { return nil }

	// Far enough out that the request is always served before it falls due.
	next := time.Now().Add(500 * time.Millisecond)
	await(next)
	if err := d.onDiscoveryGoroutine(ctx, discoveryRequest{run: noop}); err != nil {
		t.Fatalf("request: %v", err)
	}
	select {
	case r := <-done:
		if !r.ok || !r.next.Equal(next) {
			t.Errorf("after a check: next = %s, ok = %v; want the schedule untouched", r.next, r.ok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduled pass never fell due after a check was served")
	}

	await(time.Now().Add(500 * time.Millisecond))
	before := time.Now()
	if err := d.onDiscoveryGoroutine(ctx, discoveryRequest{isPass: true, run: noop}); err != nil {
		t.Fatalf("pass: %v", err)
	}
	select {
	case r := <-done:
		t.Fatalf("awaitDiscovery returned %+v; a manual pass must push the scheduled one a full interval out", r)
	case <-time.After(time.Second):
	}
	d.Close()
	select {
	case r := <-done:
		if r.ok {
			t.Error("ok after Close; the loop must stop")
		}
		if want := before.Add(d.ytDiscoveryInterval()); r.next.Before(want) {
			t.Errorf("next = %s after a manual pass, want at least %s", r.next, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("awaitDiscovery did not return on Close")
	}
}
