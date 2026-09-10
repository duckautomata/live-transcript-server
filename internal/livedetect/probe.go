package livedetect

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"live-transcript-server/internal/metrics"
)

// MechanismCallbackProbe labels the reachability self-check.
const MechanismCallbackProbe = "callback-probe"

// callbackProbeInterval is how often the inbound path is re-checked. Edge
// configuration changes under you - a bot-protection feature switched on, a
// rule edited - so this is a standing check, not a startup one.
const callbackProbeInterval = time.Hour

// runCallbackProbe verifies that the public callback URLs actually reach this
// process, and alerts loudly when they do not.
//
// This exists because a bot challenge in front of the server is the one failure
// that is invisible from both ends. Cloudflare's AI Labyrinth (and similar
// protections) answer a suspicious request with a decoy page and a 2xx status
// rather than an error - so Twitch records the notification as successfully
// delivered and discards it, no retry, no revocation, no delivery-failure
// counter. Nothing reaches this process, so nothing here can log it, and the
// only symptom is detections that quietly always come from the polling leg.
//
// The probe closes that gap by asking the question from outside: send a request
// to our own public URL and check whether OUR handler answered it.
func (d *Detector) runCallbackProbe() {
	// Let the HTTP server bind and the edge settle before the first check.
	if !d.sleep(30 * time.Second) {
		return
	}
	for {
		d.probeCallbacksOnce()
		if !d.sleep(callbackProbeInterval) {
			return
		}
	}
}

// probeCallbacksOnce checks each enabled callback path.
func (d *Detector) probeCallbacksOnce() {
	type target struct {
		name string
		url  string
	}
	var targets []target
	if d.EventSubEnabled() {
		targets = append(targets, target{name: "twitch-eventsub", url: d.twitchCallbackURL()})
	}
	if d.WebSubEnabled() {
		targets = append(targets, target{name: "youtube-websub", url: d.webSubCallbackURL()})
	}
	if len(targets) == 0 {
		return
	}

	var blocked []string
	for _, t := range targets {
		reached, detail := d.probeOne(t.url)
		if reached {
			metrics.LiveDetectWebhooks.WithLabelValues(t.name, "probe-ok").Inc()
			slog.Debug("callback probe reached the handler", "func", "Detector.probeCallbacksOnce",
				"callback", t.name, "detail", detail)
			continue
		}
		metrics.LiveDetectWebhooks.WithLabelValues(t.name, "probe-intercepted").Inc()
		slog.Error("callback probe did NOT reach this process; something in front of the server answered",
			"func", "Detector.probeCallbacksOnce", "callback", t.name, "url", t.url, "detail", detail)
		blocked = append(blocked, fmt.Sprintf("%s (%s)", t.name, detail))
	}

	// Latched like every other alert: once per outage, not once per hour.
	d.mu.Lock()
	h := d.legHealthLocked(MechanismCallbackProbe)
	var alert, recovered bool
	if len(blocked) > 0 {
		h.failures++
		h.lastErr = strings.Join(blocked, "; ")
		h.lastErrAt = time.Now()
		alert = !h.alerted
		h.alerted = alert || h.alerted
	} else {
		h.lastSuccess = time.Now()
		h.failures = 0
		recovered = h.alerted
		h.alerted = false
	}
	d.mu.Unlock()

	if alert {
		d.alerts.NotifyLiveDetectCallbackBlocked(blocked)
	}
	if recovered {
		slog.Info("callback probe recovered", "func", "Detector.probeCallbacksOnce")
		d.alerts.NotifyLiveDetectRecovered(MechanismCallbackProbe)
	}
	if len(blocked) == 0 {
		metrics.LiveDetectLastSuccess.WithLabelValues(MechanismCallbackProbe).SetToCurrentTime()
	}
}

// probeOne sends a deliberately unsigned request to a callback URL and reports
// whether our own handler answered.
//
// An unsigned request is the ideal probe: the handler rejects it with 403
// having already set the marker header, so a healthy result is unmistakable and
// nothing is mutated. What matters is not the status but WHO answered - a
// decoy page carries a plausible status and no marker.
func (d *Detector) probeOne(url string) (reached bool, detail string) {
	ctx, cancel := context.WithTimeout(d.ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return false, "could not build probe request: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	// Identify the probe so the handler answers without walking the real push
	// path, which would log a parse failure for this throwaway body.
	req.Header.Set(HeaderCallbackProbe, "1")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// A transport failure is not proof of interception - the probe has to
		// leave the container, reach the edge and come back, and that hairpin
		// can fail for reasons unrelated to bot protection. Report it as
		// inconclusive rather than crying wolf.
		return true, "probe could not complete (inconclusive): " + err.Error()
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.Header.Get(HeaderCallbackMarker) != "" {
		return true, fmt.Sprintf("status %d, handler marker present", resp.StatusCode)
	}
	// cf-ray tells us the request did traverse Cloudflare, which distinguishes
	// "the edge answered it" from "DNS sent us somewhere else entirely".
	via := "no cf-ray header"
	if ray := resp.Header.Get("Cf-Ray"); ray != "" {
		via = "answered at the Cloudflare edge (cf-ray " + ray + ")"
	}
	return false, fmt.Sprintf("status %d with no handler marker; %s", resp.StatusCode, via)
}
