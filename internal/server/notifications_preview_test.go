package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"live-transcript-server/internal/announce"
	"live-transcript-server/internal/livedetect"
	"live-transcript-server/internal/model"
)

// A deployed build previews only what the channel has actually detected.
// Nothing is borrowed from the stand-in video: a channel with nothing yet
// previews blank details, a Twitch stream EventSub claimed without a title
// previews with a blank title, and once that stream has ended the Twitch
// preview image - a "404" placeholder for an offline channel - is left out.
func TestNotificationsPreviewOnDeployedBuildNeverShowsStandIn(t *testing.T) {
	app, mux, _ := notifDetectApp(t)
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
		rec := adminReq(t, mux, http.MethodPost, notifBase+"/preview", notifAdminKey,
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

	t.Run("an ended Twitch stream previews without the placeholder image", func(t *testing.T) {
		if err := app.ObserveEnded(ctx, livedetect.PlatformTwitch, "s1"); err != nil {
			t.Fatalf("ObserveEnded: %v", err)
		}
		resp := preview(t, "live")
		if ended, _ := resp.Sample["ended"].(bool); !ended {
			t.Errorf("sample = %#v, want ended", resp.Sample)
		}
		if resp.Embed["image"] != nil {
			t.Errorf("embed.image = %v, want none for an ended Twitch stream", resp.Embed["image"])
		}
		if got := notifNested(resp.Embed, "url"); got != "https://twitch.tv/dokibird" {
			t.Errorf("embed.url = %q; the link still works after the stream", got)
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
		rec := adminReq(t, mux, http.MethodPost, notifBase+"/preview", notifAdminKey,
			notificationDraftRequest{Event: draft, Trigger: "upload"})
		var resp NotificationPreviewResponse
		notifDecode(t, rec, &resp)
		if sampleStr(resp, "source") != "sample" || sampleStr(resp, "id") != announce.SampleVideoID {
			t.Errorf("local upload sample = %#v, want the stand-in", resp.Sample)
		}
		// A real detection still wins over the stand-in, blanks and all.
		rec = adminReq(t, mux, http.MethodPost, notifBase+"/preview", notifAdminKey,
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
