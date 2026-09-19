package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"live-transcript-server/internal/announce"
	"live-transcript-server/internal/livedetect"
	"live-transcript-server/internal/model"
)

// A deployed build previews only what the channel has actually detected.
// Nothing is borrowed from the stand-in video: a channel with nothing yet
// previews blank details, and a live Twitch stream EventSub claimed without
// a title previews with a blank title. The one labelled exception is an
// offline Twitch stream, which is previewed as it will look live - Twitch
// serves a "404" placeholder as the preview of an offline channel - with the
// page told which parts are examples; a test send of it carries neither.
func TestNotificationsPreviewOnDeployedBuildNeverShowsStandIn(t *testing.T) {
	app, mux, ws := notifDetectApp(t)
	if app.localBuild() {
		t.Fatalf("test app has version %q; this test needs a deployed one", app.Version)
	}
	ctx := context.Background()

	draft := model.NotificationEvent{
		Name: "Draft", Enabled: true, Triggers: []string{"live", "upload", "short"},
		Content: announce.DefaultContent, EmbedEnabled: true, Embed: announce.DefaultEmbed(),
	}
	preview := func(t *testing.T, trigger string) NotificationPreviewResponse {
		t.Helper()
		rec := notifReq(t, mux, http.MethodPost, notifBase+"/preview",
			notificationDraftRequest{Event: draft, Trigger: trigger})
		if rec.Code != http.StatusOK {
			t.Fatalf("preview %s: status=%d body=%s", trigger, rec.Code, rec.Body.String())
		}
		var resp NotificationPreviewResponse
		notifDecode(t, rec, &resp)
		if strings.Contains(rec.Body.String(), announce.SampleVideoID) || strings.Contains(rec.Body.String(), announce.SampleVideoTitle) {
			t.Errorf("preview %s leaks the stand-in video: %s", trigger, rec.Body.String())
		}
		return resp
	}
	sampleStr := func(resp NotificationPreviewResponse, key string) string {
		s, _ := resp.Sample[key].(string)
		return s
	}

	t.Run("nothing detected previews blank", func(t *testing.T) {
		for _, trigger := range []string{"live", "upload", "short"} {
			resp := preview(t, trigger)
			if sampleStr(resp, "source") != "none" || sampleStr(resp, "id") != "" || sampleStr(resp, "title") != "" || sampleStr(resp, "url") != "" {
				t.Errorf("%s sample = %#v, want source=none and blank details", trigger, resp.Sample)
			}
			if resp.Embed["image"] != nil {
				t.Errorf("%s embed has an image with nothing detected: %v", trigger, resp.Embed["image"])
			}
			if desc := notifNested(resp.Embed, "description"); strings.Contains(desc, "*") {
				t.Errorf("%s description = %q, want no title line", trigger, desc)
			}
		}
	})

	t.Run("a Twitch stream without a title previews untitled", func(t *testing.T) {
		started := time.Now().Add(-time.Minute)
		if err := app.ObserveLive(ctx, livedetect.Broadcast{
			Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "s1",
			URL: "https://twitch.tv/dokibird", StartedAt: started,
		}, livedetect.MechanismTwitchEventSub); err != nil {
			t.Fatalf("ObserveLive: %v", err)
		}

		resp := preview(t, "live")
		if sampleStr(resp, "source") != "recent" || sampleStr(resp, "platform") != "twitch" || sampleStr(resp, "id") != "s1" || sampleStr(resp, "title") != "" {
			t.Errorf("sample = %#v, want the untitled Twitch detection", resp.Sample)
		}
		if ended, _ := resp.Sample["ended"].(bool); ended {
			t.Error("sample says ended while the stream is live")
		}
		if got := notifNested(resp.Embed, "image", "url"); !strings.HasPrefix(got, "https://static-cdn.jtvnw.net/previews-ttv/live_user_dokibird-1280x720.jpg?t=") {
			t.Errorf("embed.image.url = %q, want the live Twitch preview", got)
		}
		desc := notifNested(resp.Embed, "description")
		if !strings.HasPrefix(desc, "[Open on Twitch](https://twitch.tv/dokibird)") {
			t.Errorf("description = %q, want it to start at the links with the title line gone", desc)
		}
		if got := notifNested(resp.Embed, "timestamp"); got != started.UTC().Format(time.RFC3339) {
			t.Errorf("embed.timestamp = %q, want the platform start time", got)
		}

		// A Twitch broadcast is not a video: the other triggers stay blank.
		if resp := preview(t, "upload"); sampleStr(resp, "source") != "none" || sampleStr(resp, "platform") != "youtube" {
			t.Errorf("upload sample = %#v, want none (a Twitch stream cannot stand in)", resp.Sample)
		}
	})

	t.Run("an offline Twitch stream previews as it will look live, examples labelled", func(t *testing.T) {
		if err := app.ObserveEnded(ctx, livedetect.PlatformTwitch, "s1"); err != nil {
			t.Fatalf("ObserveEnded: %v", err)
		}
		resp := preview(t, "live")
		if ended, _ := resp.Sample["ended"].(bool); !ended {
			t.Errorf("sample = %#v, want ended", resp.Sample)
		}
		// The sample still describes the detection as it is: no title.
		if sampleStr(resp, "title") != "" || sampleStr(resp, "exampleTitle") != announce.ExampleTitle {
			t.Errorf("sample = %#v, want a blank title and the example flagged", resp.Sample)
		}
		if ex, _ := resp.Sample["exampleImage"].(bool); !ex {
			t.Errorf("sample = %#v, want exampleImage so the page can draw the frame", resp.Sample)
		}
		// Rendered as if live: the Twitch preview URL is in place for the page
		// to stand in for, and the example title is where the title goes.
		if got := notifNested(resp.Embed, "image", "url"); !strings.HasPrefix(got, "https://static-cdn.jtvnw.net/previews-ttv/live_user_dokibird-1280x720.jpg?t=") {
			t.Errorf("embed.image.url = %q, want the Twitch preview slot", got)
		}
		if desc := notifNested(resp.Embed, "description"); !strings.HasPrefix(desc, "**"+announce.ExampleTitle+"**") {
			t.Errorf("description = %q, want the example title line", desc)
		}
		if got := notifNested(resp.Embed, "url"); got != "https://twitch.tv/dokibird" {
			t.Errorf("embed.url = %q; the link still works after the stream", got)
		}
	})

	t.Run("a test send of that offline stream carries neither example", func(t *testing.T) {
		rec := notifReq(t, mux, http.MethodPost, notifBase+"/test",
			notificationDraftRequest{Event: draft, Trigger: "live", WebhookURL: notifWebhookURL})
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		post := ws.next(t)
		e := notifEmbed(t, post.Body)
		if e["image"] != nil {
			t.Errorf("test embed.image = %v, want none: Twitch has no frame for an offline channel", e["image"])
		}
		desc := notifNested(e, "description")
		if strings.Contains(desc, announce.ExampleTitle) || strings.Contains(desc, "*") {
			t.Errorf("test description = %q, want the title line simply gone", desc)
		}
		if body := rec.Body.String(); strings.Contains(body, announce.ExampleTitle) {
			t.Errorf("test result mentions the example title: %s", body)
		}
	})

	t.Run("each video trigger previews its own latest video", func(t *testing.T) {
		published := time.Now().Add(-2 * time.Hour)
		if err := app.ObserveVideo(ctx, livedetect.VideoEvent{
			Kind: livedetect.VideoShort, Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "short1",
			URL: "https://www.youtube.com/shorts/short1", Title: "A short", PublishedAt: published,
		}); err != nil {
			t.Fatalf("ObserveVideo: %v", err)
		}
		resp := preview(t, "short")
		if sampleStr(resp, "source") != "recent" || sampleStr(resp, "id") != "short1" || sampleStr(resp, "title") != "A short" || sampleStr(resp, "url") != "https://www.youtube.com/shorts/short1" {
			t.Errorf("short sample = %#v", resp.Sample)
		}
		if got := notifNested(resp.Embed, "image", "url"); got != "https://i.ytimg.com/vi/short1/maxresdefault.jpg" {
			t.Errorf("short embed.image.url = %q", got)
		}
		if at, _ := resp.Sample["eventTime"].(float64); int64(at) != published.Unix() {
			t.Errorf("short eventTime = %v, want the publish time %d", at, published.Unix())
		}
		if resp := preview(t, "upload"); sampleStr(resp, "source") != "none" {
			t.Errorf("upload sample = %#v, want none: a short is not an upload", resp.Sample)
		}
	})

	t.Run("only a local build falls back to the stand-in", func(t *testing.T) {
		app.Version = LocalVersion
		t.Cleanup(func() { app.Version = "test-version" })
		rec := notifReq(t, mux, http.MethodPost, notifBase+"/preview",
			notificationDraftRequest{Event: draft, Trigger: "upload"})
		var resp NotificationPreviewResponse
		notifDecode(t, rec, &resp)
		if sampleStr(resp, "source") != "sample" || sampleStr(resp, "id") != announce.SampleVideoID {
			t.Errorf("local upload sample = %#v, want the stand-in", resp.Sample)
		}
		// A real detection still wins over the stand-in, blanks and all.
		rec = notifReq(t, mux, http.MethodPost, notifBase+"/preview",
			notificationDraftRequest{Event: draft, Trigger: "live"})
		notifDecode(t, rec, &resp)
		if sampleStr(resp, "source") != "recent" || sampleStr(resp, "id") != "s1" || sampleStr(resp, "title") != "" {
			t.Errorf("local live sample = %#v, want the channel's own detection", resp.Sample)
		}
	})

	t.Run("video triggers borrow the newest YouTube broadcast, whatever came after it", func(t *testing.T) {
		if err := app.ObserveLive(ctx, livedetect.Broadcast{
			Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "ytlive1",
			URL: livedetect.YouTubeWatchURL("ytlive1"), Title: "A YouTube stream", StartedAt: time.Now(),
		}, livedetect.MechanismYouTubeState); err != nil {
			t.Fatalf("ObserveLive: %v", err)
		}
		if err := app.ObserveLive(ctx, livedetect.Broadcast{
			Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "s2",
			URL: "https://twitch.tv/dokibird", Title: "Back on Twitch", StartedAt: time.Now(),
		}, livedetect.MechanismTwitchPoll); err != nil {
			t.Fatalf("ObserveLive: %v", err)
		}
		if resp := preview(t, "live"); sampleStr(resp, "id") != "s2" || sampleStr(resp, "title") != "Back on Twitch" {
			t.Errorf("live sample = %#v, want the newest broadcast", resp.Sample)
		}
		resp := preview(t, "upload")
		if sampleStr(resp, "source") != "recent" || sampleStr(resp, "id") != "ytlive1" || sampleStr(resp, "title") != "A YouTube stream" {
			t.Errorf("upload sample = %#v, want the newest YouTube broadcast", resp.Sample)
		}
		if at, _ := resp.Sample["eventTime"].(float64); at != 0 {
			t.Errorf("upload eventTime = %v, want blank: a stream start is not a publish time", at)
		}
		if resp := preview(t, "short"); sampleStr(resp, "id") != "short1" {
			t.Errorf("short sample = %#v, want the short itself over the broadcast", resp.Sample)
		}
	})
}

// notifDescriptionDraft is a draft whose embed puts the first line of the
// video description between the title and the links.
func notifDescriptionDraft() model.NotificationEvent {
	embed := announce.DefaultEmbed()
	embed.Description = "**{title}**\n\n{description:1}\n\n[Open on {platform}]({url})"
	return model.NotificationEvent{
		Name: "Draft", Enabled: true, Triggers: []string{"live", "upload"},
		Content: announce.DefaultContent, EmbedEnabled: true, Embed: embed,
	}
}

// notifPreviewBlanks reads sample.blanks out of a preview as name -> why.
func notifPreviewBlanks(t *testing.T, resp NotificationPreviewResponse) map[string]string {
	t.Helper()
	list, ok := resp.Sample["blanks"].([]any)
	if !ok {
		t.Fatalf("sample.blanks = %#v, want an array (never null, never absent)", resp.Sample["blanks"])
	}
	out := map[string]string{}
	for _, item := range list {
		obj, _ := item.(map[string]any)
		name, _ := obj["name"].(string)
		why, _ := obj["why"].(string)
		if name == "" || why == "" || len(obj) != 2 {
			t.Fatalf("sample.blanks entry = %#v, want exactly {name, why}", item)
		}
		out[name] = why
	}
	return out
}

// A preview renders the description the ledger recorded, and tells the editor
// why a {description} came up blank when it did: a Twitch stream never has
// one ("platform"), a YouTube video might simply carry none ("missing"). A
// draft that does not use the placeholder is told nothing, and neither is a
// channel with no detection at all, where everything is blank anyway.
func TestNotificationsPreviewDescription(t *testing.T) {
	app, mux, _ := notifDetectApp(t)
	ctx := context.Background()
	const description = "The pitch for tonight.\n\nMerch: https://example.test/merch\nThanks for watching!"

	preview := func(t *testing.T, draft model.NotificationEvent, trigger string) NotificationPreviewResponse {
		t.Helper()
		rec := notifReq(t, mux, http.MethodPost, notifBase+"/preview", notificationDraftRequest{Event: draft, Trigger: trigger})
		if rec.Code != http.StatusOK {
			t.Fatalf("preview %s: status=%d body=%s", trigger, rec.Code, rec.Body.String())
		}
		// Both additions are always there, in the shape the editor reads.
		var raw struct {
			Sample map[string]json.RawMessage `json:"sample"`
		}
		notifDecode(t, rec, &raw)
		if got := string(raw.Sample["blanks"]); !strings.HasPrefix(got, "[") {
			t.Errorf("sample.blanks = %s, want an array", got)
		}
		if got := string(raw.Sample["shortened"]); got != "true" && got != "false" {
			t.Errorf("sample.shortened = %s, want a boolean", got)
		}
		var resp NotificationPreviewResponse
		notifDecode(t, rec, &resp)
		return resp
	}

	t.Run("nothing detected reports no blanks", func(t *testing.T) {
		resp := preview(t, notifDescriptionDraft(), "live")
		if blanks := notifPreviewBlanks(t, resp); len(blanks) != 0 {
			t.Errorf("blanks = %v, want none: with no detection everything is blank and the editor says so", blanks)
		}
		if got := notifNested(resp.Embed, "description"); got != "[Open on ]()" {
			t.Errorf("description = %q, want the title and description lines gone", got)
		}
	})

	t.Run("a YouTube stream renders its first line", func(t *testing.T) {
		if err := app.ObserveLive(ctx, livedetect.Broadcast{
			Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "ytlive1",
			URL: livedetect.YouTubeWatchURL("ytlive1"), Title: "A YouTube stream", Description: description, StartedAt: time.Now(),
		}, livedetect.MechanismYouTubeState); err != nil {
			t.Fatalf("ObserveLive: %v", err)
		}
		resp := preview(t, notifDescriptionDraft(), "live")
		want := "**A YouTube stream**\n\nThe pitch for tonight.\n\n[Open on YouTube](" + livedetect.YouTubeWatchURL("ytlive1") + ")"
		if got := notifNested(resp.Embed, "description"); got != want {
			t.Errorf("description = %q, want %q", got, want)
		}
		if blanks := notifPreviewBlanks(t, resp); len(blanks) != 0 {
			t.Errorf("blanks = %v, want none", blanks)
		}
		if shortened, _ := resp.Sample["shortened"].(bool); shortened {
			t.Error("sample.shortened = true for a one-line description")
		}
		// The sample says where the details came from; the description itself
		// only ever travels rendered.
		if sample, _ := json.Marshal(resp.Sample); strings.Contains(string(sample), "Merch") || strings.Contains(string(sample), "pitch") {
			t.Errorf("sample carries the description text: %s", sample)
		}

		// The same row stands in for a video trigger, description and all.
		resp = preview(t, notifDescriptionDraft(), "upload")
		if got := notifNested(resp.Embed, "description"); !strings.Contains(got, "\n\nThe pitch for tonight.\n\n") {
			t.Errorf("upload description = %q, want the borrowed broadcast's first line", got)
		}
	})

	t.Run("a YouTube video without one is missing it", func(t *testing.T) {
		if err := app.ObserveVideo(ctx, livedetect.VideoEvent{
			Kind: livedetect.VideoUpload, Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "up1",
			URL: livedetect.YouTubeWatchURL("up1"), Title: "An upload", Description: " \n\t ", PublishedAt: time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatalf("ObserveVideo: %v", err)
		}
		resp := preview(t, notifDescriptionDraft(), "upload")
		if blanks := notifPreviewBlanks(t, resp); len(blanks) != 1 || blanks["{description}"] != "missing" {
			t.Errorf("blanks = %v, want {description}: missing", blanks)
		}
		want := "**An upload**\n\n[Open on YouTube](" + livedetect.YouTubeWatchURL("up1") + ")"
		if got := notifNested(resp.Embed, "description"); got != want {
			t.Errorf("description = %q, want %q", got, want)
		}
	})

	t.Run("a Twitch stream never has one", func(t *testing.T) {
		if err := app.ObserveLive(ctx, livedetect.Broadcast{
			Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "s1",
			URL: "https://twitch.tv/dokibird", Title: "Back on Twitch", StartedAt: time.Now(),
		}, livedetect.MechanismTwitchPoll); err != nil {
			t.Fatalf("ObserveLive: %v", err)
		}
		resp := preview(t, notifDescriptionDraft(), "live")
		if blanks := notifPreviewBlanks(t, resp); len(blanks) != 1 || blanks["{description}"] != "platform" {
			t.Errorf("blanks = %v, want {description}: platform", blanks)
		}
		// The line is gone, and the gap it stood in with it.
		if got, want := notifNested(resp.Embed, "description"), "**Back on Twitch**\n\n[Open on Twitch](https://twitch.tv/dokibird)"; got != want {
			t.Errorf("description = %q, want %q", got, want)
		}

		// A draft that never asks for the description has nothing to explain.
		plain := notifDescriptionDraft()
		plain.Embed = announce.DefaultEmbed()
		if blanks := notifPreviewBlanks(t, preview(t, plain, "live")); len(blanks) != 0 {
			t.Errorf("blanks = %v for a draft without the placeholder, want none", blanks)
		}
		// Nor does one whose only use of it is switched off with the embed.
		off := notifDescriptionDraft()
		off.EmbedEnabled = false
		off.Content = "{channel} is live"
		if blanks := notifPreviewBlanks(t, preview(t, off, "live")); len(blanks) != 0 {
			t.Errorf("blanks = %v with the embed off, want none", blanks)
		}
		// The message body counts as much as the embed.
		off.Content = "{channel} is live\n> {description}"
		resp = preview(t, off, "live")
		if blanks := notifPreviewBlanks(t, resp); blanks["{description}"] != "platform" {
			t.Errorf("blanks = %v for a description in the message, want platform", blanks)
		}
		if resp.Content != "Dokibird is live" {
			t.Errorf("content = %q, want the quoted description line gone", resp.Content)
		}
	})
}

// "All of it" on a description near YouTube's maximum does not fit an embed.
// The description is what gives way, the author's links survive, and the
// editor is told so it can say what happened.
func TestNotificationsPreviewReportsAShortenedDescription(t *testing.T) {
	app, mux, _ := notifDetectApp(t)
	huge := strings.TrimSpace(strings.Repeat("lorem ipsum dolor ", 270)) // ~4860 runes
	if err := app.ObserveLive(context.Background(), livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "ytlive1",
		URL: livedetect.YouTubeWatchURL("ytlive1"), Title: "A YouTube stream", Description: huge, StartedAt: time.Now(),
	}, livedetect.MechanismYouTubeState); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}

	draft := notifDescriptionDraft()
	draft.Embed.Description = "**{title}**\n\n{description}\n\n[Open on {platform}]({url})"
	rec := notifReq(t, mux, http.MethodPost, notifBase+"/preview", notificationDraftRequest{Event: draft, Trigger: "live"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp NotificationPreviewResponse
	notifDecode(t, rec, &resp)
	if shortened, _ := resp.Sample["shortened"].(bool); !shortened {
		t.Errorf("sample.shortened = %v, want true", resp.Sample["shortened"])
	}
	desc := notifNested(resp.Embed, "description")
	if n := utf8.RuneCountInString(desc); n > announce.MaxEmbedDescription || n < announce.MaxEmbedDescription-20 {
		t.Errorf("description is %d runes, want it to fill the %d available", n, announce.MaxEmbedDescription)
	}
	if !strings.HasSuffix(desc, "…\n\n[Open on YouTube]("+livedetect.YouTubeWatchURL("ytlive1")+")") {
		t.Errorf("description ends %q, want the shortened description and then the link intact", desc[len(desc)-80:])
	}

	// The first line alone fits, and nothing is reported.
	rec = notifReq(t, mux, http.MethodPost, notifBase+"/preview", notificationDraftRequest{Event: notifDescriptionDraft(), Trigger: "live"})
	notifDecode(t, rec, &resp)
	if shortened, _ := resp.Sample["shortened"].(bool); shortened {
		t.Error("sample.shortened = true for {description:1}")
	}
}

// A test send goes out exactly as the detection stands, so it carries the
// same description the preview showed - with the links in the message body
// wrapped so Discord does not unfurl each one, and raw in the embed.
func TestNotificationsTestSendRendersTheDescription(t *testing.T) {
	app, mux, ws := notifDetectApp(t)
	if err := app.ObserveLive(context.Background(), livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "ytlive1",
		URL: livedetect.YouTubeWatchURL("ytlive1"), Title: "A YouTube stream", StartedAt: time.Now(),
		Description: "The pitch: https://example.test/pitch\n\nSecond paragraph.",
	}, livedetect.MechanismYouTubeState); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}

	draft := notifDescriptionDraft()
	draft.Content = "{description:1}"
	rec := notifReq(t, mux, http.MethodPost, notifBase+"/test",
		notificationDraftRequest{Event: draft, Trigger: "live", WebhookURL: notifWebhookURL})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var res announce.TestResult
	notifDecode(t, rec, &res)
	if !res.OK {
		t.Fatalf("result = %+v, want ok", res)
	}
	// The result keeps its shape: nothing about the preview leaks into it.
	var raw map[string]json.RawMessage
	notifDecode(t, rec, &raw)
	for key := range raw {
		if key != "webhook" && key != "ok" && key != "error" {
			t.Errorf("test result has an unexpected field %q: %s", key, rec.Body.String())
		}
	}

	post := ws.next(t)
	if content, _ := post.Body["content"].(string); content != "The pitch: <https://example.test/pitch>" {
		t.Errorf("content = %q, want the first line with its link wrapped", content)
	}
	want := "**A YouTube stream**\n\nThe pitch: https://example.test/pitch\n\n[Open on YouTube](" + livedetect.YouTubeWatchURL("ytlive1") + ")"
	if got := notifNested(notifEmbed(t, post.Body), "description"); got != want {
		t.Errorf("embed.description = %q, want %q", got, want)
	}
	ws.none(t)
}

// The description is for rendering only. The detection rows are encoded
// verbatim by the public detection feed and the admin page, and up to 5000
// characters a row is pure bloat there - so it never serializes.
func TestDetectionFeedsNeverCarryTheDescription(t *testing.T) {
	app, mux, _ := notifDetectApp(t)
	ctx := context.Background()
	const secretText = "DistinctiveDescriptionText"
	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "ytlive1",
		URL: livedetect.YouTubeWatchURL("ytlive1"), Title: "A YouTube stream", Description: secretText + " live", StartedAt: time.Now(),
	}, livedetect.MechanismYouTubeState); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}
	if err := app.ObserveVideo(ctx, livedetect.VideoEvent{
		Kind: livedetect.VideoUpload, Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "up1",
		URL: livedetect.YouTubeWatchURL("up1"), Title: "An upload", Description: secretText + " video", PublishedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("ObserveVideo: %v", err)
	}
	// It is in the ledger...
	if got, err := app.Store.GetDetection(ctx, livedetect.PlatformYouTube, "ytlive1"); err != nil || got == nil || got.Description != secretText+" live" {
		t.Fatalf("ledger row = (%+v, %v), want the description recorded", got, err)
	}

	// ...and on neither feed.
	pub := userReq(t, mux, http.MethodGet, "/doki/livedetect", "", nil)
	if pub.Code != http.StatusOK {
		t.Fatalf("GET livedetect: status=%d body=%s", pub.Code, pub.Body.String())
	}
	var feed LiveDetectResponse
	notifDecode(t, pub, &feed)
	if len(feed.Detections) != 1 || len(feed.Videos) != 1 || feed.Detections[0].Title != "A YouTube stream" {
		t.Fatalf("public feed = %+v, want both rows", feed)
	}
	if body := pub.Body.String(); strings.Contains(body, secretText) || strings.Contains(strings.ToLower(body), `"description"`) {
		t.Errorf("the public detection feed carries the description: %s", body)
	}

	admin := adminReq(t, mux, http.MethodGet, notifAdminBase, notifAdminKey, nil)
	if admin.Code != http.StatusOK {
		t.Fatalf("GET admin notifications: status=%d body=%s", admin.Code, admin.Body.String())
	}
	if body := admin.Body.String(); !strings.Contains(body, "An upload") || strings.Contains(body, secretText) {
		t.Errorf("the admin page's video ledger should list the upload without its description: %s", body)
	}
}

// YouTube documents 5000 characters, and a promise is not a guarantee: the
// ledger keeps every row for a month, so whatever arrives is bounded at the
// sink - trimmed, then cut - before it is stored or announced.
func TestObserveClampsTheDescription(t *testing.T) {
	app, _, _ := notifDetectApp(t)
	ctx := context.Background()
	oversized := "  \n" + strings.Repeat("é", 6000) + "\n  " // multi-byte: the bound is characters, not bytes

	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "ytlive1",
		URL: livedetect.YouTubeWatchURL("ytlive1"), Title: "A YouTube stream", Description: oversized, StartedAt: time.Now(),
	}, livedetect.MechanismYouTubeState); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}
	got, err := app.Store.GetDetection(ctx, livedetect.PlatformYouTube, "ytlive1")
	if err != nil || got == nil {
		t.Fatalf("ledger row = (%+v, %v)", got, err)
	}
	if got.Description != strings.Repeat("é", 5000) {
		t.Errorf("stored description is %d runes (starts %.10q), want exactly 5000 with the padding trimmed",
			utf8.RuneCountInString(got.Description), got.Description)
	}

	if err := app.ObserveVideo(ctx, livedetect.VideoEvent{
		Kind: livedetect.VideoUpload, Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "up1",
		URL: livedetect.YouTubeWatchURL("up1"), Title: "An upload", Description: oversized, PublishedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("ObserveVideo: %v", err)
	}
	vids, err := app.Store.GetRecentVideoDetections(ctx, "doki", 5)
	if err != nil || len(vids) != 1 {
		t.Fatalf("video ledger = (%+v, %v)", vids, err)
	}
	if n := utf8.RuneCountInString(vids[0].Description); n != 5000 {
		t.Errorf("stored video description is %d runes, want 5000", n)
	}

	// One that fits is stored as it came, inner line breaks and all.
	if got := clampDescription(" line one\n\nline two \n"); got != "line one\n\nline two" {
		t.Errorf("clampDescription = %q, want only the ends trimmed", got)
	}
}

// The preview and the test send were pinned above; this is the announcement
// itself. A saved rule fires on a real detection, and what reaches the
// webhook carries the description of THAT detection, first line only as the
// rule asked, with the ledger and the payload agreeing on the text.
func TestAnnouncementCarriesTheDescription(t *testing.T) {
	app, mux, ws := notifDetectApp(t)
	rule := notifRule("With description", []string{"live", "upload"})
	rule.Content = "{channel} is live\n> {description:1}"
	rule.Embed.Description = "**{title}**\n\n{description:2}\n\n[Open on {platform}]({url})"
	notifCreate(t, mux, "doki", rule)

	if err := app.ObserveLive(context.Background(), livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "ytlive9",
		URL: livedetect.YouTubeWatchURL("ytlive9"), Title: "A YouTube stream", StartedAt: time.Now(),
		Description: "  The pitch: https://example.test/pitch\n\nSecond line.\nThird line.  ",
	}, livedetect.MechanismYouTubeState); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}
	post := ws.next(t)
	if content, _ := post.Body["content"].(string); content != "Dokibird is live\n> The pitch: <https://example.test/pitch>" {
		t.Errorf("content = %q, want the first line of the description, its link wrapped", content)
	}
	want := "**A YouTube stream**\n\nThe pitch: https://example.test/pitch\n\nSecond line.\n\n[Open on YouTube](" + livedetect.YouTubeWatchURL("ytlive9") + ")"
	if got := notifNested(notifEmbed(t, post.Body), "description"); got != want {
		t.Errorf("embed.description = %q, want %q", got, want)
	}

	// An upload goes the same way, through the other sink.
	if err := app.ObserveVideo(context.Background(), livedetect.VideoEvent{
		Kind: livedetect.VideoUpload, Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "ytvid9",
		URL: livedetect.YouTubeWatchURL("ytvid9"), Title: "A video", PublishedAt: time.Now(),
		Description: "About the video.\nMore.",
	}); err != nil {
		t.Fatalf("ObserveVideo: %v", err)
	}
	post = ws.next(t)
	if content, _ := post.Body["content"].(string); content != "Dokibird is live\n> About the video." {
		t.Errorf("upload content = %q, want the video's own first line", content)
	}
}

// When the author's own text leaves no room for the description, the preview
// must not claim it was "shortened, everything else kept": it was left out,
// and the editor is told so with a reason of its own.
func TestNotificationsPreviewReportsADescriptionWithNoRoom(t *testing.T) {
	app, mux, _ := notifDetectApp(t)
	if err := app.ObserveLive(context.Background(), livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "ytlive1",
		URL: livedetect.YouTubeWatchURL("ytlive1"), Title: "A YouTube stream", StartedAt: time.Now(),
		Description: strings.TrimSpace(strings.Repeat("lorem ipsum dolor ", 50)),
	}, livedetect.MechanismYouTubeState); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}

	draft := notifDescriptionDraft()
	draft.Embed.Description = strings.Repeat("x", announce.MaxEmbedDescription-len("\n{description}\nEND")) + "\n{description}\nEND"
	rec := notifReq(t, mux, http.MethodPost, notifBase+"/preview", notificationDraftRequest{Event: draft, Trigger: "live"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp NotificationPreviewResponse
	notifDecode(t, rec, &resp)
	if shortened, _ := resp.Sample["shortened"].(bool); shortened {
		t.Error("sample.shortened = true, but nothing of the description is there to have been shortened")
	}
	if blanks := notifPreviewBlanks(t, resp); blanks["{description}"] != "room" {
		t.Errorf("sample.blanks = %v, want {description} blank for want of room", blanks)
	}
	if desc := notifNested(resp.Embed, "description"); strings.Contains(desc, "lorem") || !strings.HasSuffix(desc, "x\nEND") {
		t.Errorf("description ends %q, want the author's text whole and no description", desc[len(desc)-20:])
	}
}

// A draft is previewed even when it does not validate, and previews are not
// rate limited. A body of nothing but {description}, repeated up to the request
// limit, must therefore be cut down to what a rule may hold BEFORE it is
// rendered: expanded in full it is hundreds of megabytes per request.
func TestNotificationsPreviewClipsAnOverLongDraft(t *testing.T) {
	ev := model.NotificationEvent{
		Content: strings.Repeat("{description}", 19000),
		Embed: model.EmbedTemplate{
			Title:       strings.Repeat("t", announce.MaxEmbedTitle+50),
			Description: strings.Repeat("あ", announce.MaxEmbedDescription+50),
			Footer:      strings.Repeat("f", announce.MaxEmbedFooter+50),
			URL:         "https://a.test/" + strings.Repeat("u", announce.MaxTemplateURLLength),
			Image:       strings.Repeat("{description}", 19000),
			Thumbnail:   "{thumbnail}",
		},
	}
	clipDraftTemplates(&ev)
	if n := utf8.RuneCountInString(ev.Content); n != announce.MaxContentLength {
		t.Errorf("content clipped to %d runes, want %d", n, announce.MaxContentLength)
	}
	if n := utf8.RuneCountInString(ev.Embed.Title); n != announce.MaxEmbedTitle {
		t.Errorf("title clipped to %d runes, want %d", n, announce.MaxEmbedTitle)
	}
	if n := utf8.RuneCountInString(ev.Embed.Description); n != announce.MaxEmbedDescription {
		t.Errorf("description clipped to %d runes (counted in runes, not bytes), want %d", n, announce.MaxEmbedDescription)
	}
	if n := utf8.RuneCountInString(ev.Embed.Footer); n != announce.MaxEmbedFooter {
		t.Errorf("footer clipped to %d runes, want %d", n, announce.MaxEmbedFooter)
	}
	if ev.Embed.URL != "" || ev.Embed.Image != "" {
		t.Errorf("over-long URL templates must be dropped, got url=%d bytes image=%d bytes", len(ev.Embed.URL), len(ev.Embed.Image))
	}
	if ev.Embed.Thumbnail != "{thumbnail}" {
		t.Errorf("thumbnail = %q, a template within the limit must be left alone", ev.Embed.Thumbnail)
	}

	// Through the handler: the over-long draft still previews, at the size of
	// a real rule.
	app, mux, _ := notifDetectApp(t)
	if err := app.ObserveLive(context.Background(), livedetect.Broadcast{
		Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "ytlive1",
		URL: livedetect.YouTubeWatchURL("ytlive1"), Title: "A YouTube stream", StartedAt: time.Now(),
		Description: strings.TrimSpace(strings.Repeat("lorem ipsum dolor ", 270)),
	}, livedetect.MechanismYouTubeState); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}
	draft := notifDescriptionDraft()
	draft.Content = strings.Repeat("{description}", 15000)
	rec := notifReq(t, mux, http.MethodPost, notifBase+"/preview", notificationDraftRequest{Event: draft, Trigger: "live"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%.200s", rec.Code, rec.Body.String())
	}
	var resp NotificationPreviewResponse
	notifDecode(t, rec, &resp)
	if n := utf8.RuneCountInString(resp.Content); n > announce.MaxContentLength {
		t.Errorf("previewed content is %d runes, over Discord's limit", n)
	}
}
