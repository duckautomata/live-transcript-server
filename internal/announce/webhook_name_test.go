package announce

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"live-transcript-server/internal/model"
)

// A webhook's name is how the delivery log refers to it; the masked URL rides
// along so two webhooks with the same name can still be told apart. The token
// never appears.
func TestDescribeFailureUsesWebhookName(t *testing.T) {
	url := notifWebhookURL(notifWebhookA)
	de := &DeliveryError{Webhook: MaskWebhookURL(url), Status: http.StatusNotFound, Reason: "webhook no longer exists"}

	named := describeFailure(model.Webhook{Name: "#announcements", URL: url}, de)
	if !strings.HasPrefix(named, "#announcements (webhook "+notifWebhookA+"/") || !strings.Contains(named, "HTTP 404 webhook no longer exists") {
		t.Errorf("named failure = %q", named)
	}
	unnamed := describeFailure(model.Webhook{URL: url}, de)
	if !strings.HasPrefix(unnamed, "webhook "+notifWebhookA+"/") || strings.Contains(unnamed, "(") {
		t.Errorf("unnamed failure = %q, want the masked URL alone", unnamed)
	}
	plain := describeFailure(model.Webhook{Name: "members", URL: url}, errors.New("boom"))
	if !strings.HasPrefix(plain, "members (webhook "+notifWebhookA+"/") || !strings.HasSuffix(plain, ": boom") {
		t.Errorf("non-delivery error = %q", plain)
	}
	for _, s := range []string{named, unnamed, plain} {
		notifAssertNoToken(t, "failure text", s)
	}
}

// Normalize keeps names with their URLs, trims both, drops a row that has a
// name but no URL, dedupes by URL keeping the first name, and rejects an
// over-long name.
func TestNormalizeWebhookNames(t *testing.T) {
	a, b := notifWebhookURL(notifWebhookA), notifWebhookURL(notifWebhookB)
	ev := notifEvent(1, "rule", []Trigger{TriggerLive}, a)
	ev.Webhooks = []model.Webhook{
		{Name: "  #announcements ", URL: " " + a + " "},
		{Name: "unfinished row", URL: ""},
		{Name: "dupe of a", URL: a},
		{Name: "", URL: b},
	}
	if err := Normalize(&ev); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	want := []model.Webhook{{Name: "#announcements", URL: a}, {Name: "", URL: b}}
	if len(ev.Webhooks) != len(want) {
		t.Fatalf("webhooks = %+v, want %+v", ev.Webhooks, want)
	}
	for i := range want {
		if ev.Webhooks[i] != want[i] {
			t.Errorf("webhook %d = %+v, want %+v", i, ev.Webhooks[i], want[i])
		}
	}

	ev.Webhooks = []model.Webhook{{Name: strings.Repeat("n", MaxWebhookNameLength+1), URL: a}}
	err := Normalize(&ev)
	if !IsValidationError(err) || !strings.Contains(err.Error(), "webhook name") {
		t.Errorf("over-long name: err = %v, want a validation problem about the name", err)
	}
}

// A test send is labelled with the webhook's name in its result and its log
// entry, and the sample it renders is the stand-in video.
func TestSendTestLabelsWebhookByName(t *testing.T) {
	ev := notifEvent(5, "rule", []Trigger{TriggerLive}, notifWebhookURL(notifWebhookA))
	store := newNotifStore(ev)
	discord := newNotifDiscord(t)
	d := newNotifDispatcher(t, store, discord, nil)

	p := SamplePayload(TriggerLive, notifChannel(), notifNow)
	if !p.IsSample() || p.ID != SampleVideoID || p.Title != SampleVideoTitle {
		t.Fatalf("sample payload = %+v", p)
	}
	res := d.SendTest(context.Background(), ev, p, model.Webhook{Name: "staff", URL: notifWebhookURL(notifWebhookB)})
	if !res.OK || !strings.HasPrefix(res.Webhook, "staff (webhook "+notifWebhookB+"/") {
		t.Errorf("result = %+v", res)
	}
	logs := store.logEntries()
	if len(logs) != 1 || !strings.Contains(logs[0].Detail, "staff (webhook "+notifWebhookB+"/") || logs[0].BroadcastID != SampleVideoID {
		t.Errorf("log = %+v", logs)
	}
	notifAssertNoToken(t, "result", res.Webhook)
	notifAssertNoToken(t, "log detail", logs[0].Detail)
}
