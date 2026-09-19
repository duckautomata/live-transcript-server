package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"live-transcript-server/internal/announce"
	"live-transcript-server/internal/livedetect"
	"live-transcript-server/internal/model"
)

// notifGameContent is a message body with the Twitch category on a line of its
// own, the way an author would write it.
const notifGameContent = "{channel} is live: {title}\n🎮 {game}"

// EventSub wins nearly every Twitch claim and its payload has neither a title
// nor a category. The one lookup made for the title brings the category back
// with it, so the announcement renders {game}, the ledger records it, and a
// preview of that detection afterwards says the same thing.
func TestObserveLiveAnnouncesTheLookedUpGame(t *testing.T) {
	app, mux, ws := notifDetectApp(t)
	ctx := context.Background()

	asked := 0
	app.TwitchStreamLookup = func(context.Context, string) livedetect.TwitchStreamInfo {
		asked++
		return livedetect.TwitchStreamInfo{Title: "Building a castle", Game: "Minecraft"}
	}
	rule := notifRule("Go live", []string{"live"})
	rule.Content = notifGameContent
	notifCreate(t, mux, "doki", rule)

	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "s1",
		URL: "https://twitch.tv/dokibird", StartedAt: time.Now(),
	}, livedetect.MechanismTwitchEventSub); err != nil {
		t.Fatalf("ObserveLive: %v", err)
	}
	if asked != 1 {
		t.Fatalf("lookup calls = %d, want exactly one for both details", asked)
	}

	const want = "Dokibird is live: Building a castle\n🎮 Minecraft"
	post := ws.next(t)
	if content, _ := post.Body["content"].(string); content != want {
		t.Errorf("announced content = %q, want %q", content, want)
	}
	notifWaitLog(t, app, 1)

	det, err := app.Store.GetDetection(ctx, "twitch", "s1")
	if err != nil || det == nil {
		t.Fatalf("detection missing: %v %v", det, err)
	}
	if det.Title != "Building a castle" || det.Game != "Minecraft" {
		t.Errorf("ledger row = %+v, want the looked-up title and game", det)
	}
	// Unlike the description, the category is a few words, so it travels on
	// the detection feed beside the title.
	pub := userReq(t, mux, http.MethodGet, "/doki/livedetect", "", nil)
	var feed LiveDetectResponse
	notifDecode(t, pub, &feed)
	if pub.Code != http.StatusOK || len(feed.Detections) != 1 || feed.Detections[0].Game != "Minecraft" {
		t.Errorf("public feed: status=%d body=%s, want the one detection with its game", pub.Code, pub.Body.String())
	}

	rec := notifReq(t, mux, http.MethodPost, notifBase+"/preview", notificationDraftRequest{Event: rule, Trigger: "live"})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp NotificationPreviewResponse
	notifDecode(t, rec, &resp)
	if resp.Content != want {
		t.Errorf("preview content = %q, want what was announced: %q", resp.Content, want)
	}
	if blanks := notifPreviewBlanks(t, resp); len(blanks) != 0 {
		t.Errorf("blanks = %v, want none", blanks)
	}
}

// When the lookup has nothing to offer, the poll leg's next sighting of the
// same broadcast loses the claim but knows the category, so it lands in the
// ledger - once. A streamer who switches games an hour in does not rewrite
// what the stream went live as.
func TestObserveLiveBackfillsGameFromALaterObservation(t *testing.T) {
	app, _ := setupDetectApp(t) // no stream lookup installed
	ctx := context.Background()
	started := time.Now().Add(-time.Minute)
	changes := func() int64 { return app.Channels["doki"].AdminChangeCounter.Load() }

	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "s1",
		URL: "https://twitch.tv/dokibird", StartedAt: started,
	}, livedetect.MechanismTwitchEventSub); err != nil {
		t.Fatalf("ObserveLive (eventsub): %v", err)
	}
	if det, _ := app.Store.GetDetection(ctx, "twitch", "s1"); det == nil || det.Game != "" {
		t.Fatalf("eventsub detection = %+v, want no game yet", det)
	}

	// Title and category arrive together: both are filled, and the admin page
	// is woken once for the pair.
	before := changes()
	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "s1",
		URL: "https://twitch.tv/dokibird", Title: "Building a castle", Game: "Minecraft", StartedAt: started,
	}, livedetect.MechanismTwitchPoll); err != nil {
		t.Fatalf("ObserveLive (poll): %v", err)
	}
	det, _ := app.Store.GetDetection(ctx, "twitch", "s1")
	if det.Title != "Building a castle" || det.Game != "Minecraft" {
		t.Errorf("row after the poll leg = %+v, want the title and game backfilled", det)
	}
	if det.Mechanism != livedetect.MechanismTwitchEventSub {
		t.Errorf("mechanism = %q; the backfill must not rewrite who won", det.Mechanism)
	}
	if got := changes() - before; got != 1 {
		t.Errorf("admin change counter moved by %d, want 1 for the one backfill", got)
	}

	// The category changes mid-stream: the ledger keeps the first one, and
	// nothing is reported as having changed.
	before = changes()
	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "s1",
		URL: "https://twitch.tv/dokibird", Title: "Building a castle", Game: "Just Chatting", StartedAt: started,
	}, livedetect.MechanismTwitchPoll); err != nil {
		t.Fatalf("ObserveLive (poll again): %v", err)
	}
	if det, _ = app.Store.GetDetection(ctx, "twitch", "s1"); det.Game != "Minecraft" {
		t.Errorf("game after a category change = %q, want the first one kept", det.Game)
	}
	if got := changes() - before; got != 0 {
		t.Errorf("admin change counter moved by %d for a no-op observation, want 0", got)
	}

	// The poll leg winning the claim outright records the category directly.
	if err := app.ObserveLive(ctx, livedetect.Broadcast{
		Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "s2",
		URL: "https://twitch.tv/dokibird", Title: "Round two", Game: "Just Chatting", StartedAt: time.Now(),
	}, livedetect.MechanismTwitchPoll); err != nil {
		t.Fatalf("ObserveLive (poll wins): %v", err)
	}
	if det, _ := app.Store.GetDetection(ctx, "twitch", "s2"); det == nil || det.Game != "Just Chatting" {
		t.Errorf("poll-claimed row = %+v, want its game recorded at the claim", det)
	}
}

// A preview tells the editor why a {game} came up blank: a YouTube video never
// has one ("platform"), a Twitch stream might have been claimed without one
// ("missing"). Either way the line it stood alone on is gone, and nothing is
// ever made up to fill it.
func TestNotificationsPreviewGame(t *testing.T) {
	app, mux, _ := notifDetectApp(t)
	ctx := context.Background()

	draft := model.NotificationEvent{
		Name: "Draft", Enabled: true, Triggers: []string{"live", "upload"},
		Content: notifGameContent, EmbedEnabled: true, Embed: announce.DefaultEmbed(),
	}
	preview := func(t *testing.T, draft model.NotificationEvent, trigger string) NotificationPreviewResponse {
		t.Helper()
		rec := notifReq(t, mux, http.MethodPost, notifBase+"/preview", notificationDraftRequest{Event: draft, Trigger: trigger})
		if rec.Code != http.StatusOK {
			t.Fatalf("preview %s: status=%d body=%s", trigger, rec.Code, rec.Body.String())
		}
		var resp NotificationPreviewResponse
		notifDecode(t, rec, &resp)
		return resp
	}

	t.Run("a YouTube stream never has one", func(t *testing.T) {
		if err := app.ObserveLive(ctx, livedetect.Broadcast{
			Platform: livedetect.PlatformYouTube, ChannelKey: "doki", ID: "ytlive1",
			URL: livedetect.YouTubeWatchURL("ytlive1"), Title: "A YouTube stream", StartedAt: time.Now(),
		}, livedetect.MechanismYouTubeState); err != nil {
			t.Fatalf("ObserveLive: %v", err)
		}
		for _, trigger := range []string{"live", "upload"} {
			resp := preview(t, draft, trigger)
			if blanks := notifPreviewBlanks(t, resp); len(blanks) != 1 || blanks["{game}"] != "platform" {
				t.Errorf("%s blanks = %v, want {game}: platform", trigger, blanks)
			}
			if resp.Content != "Dokibird is live: A YouTube stream" {
				t.Errorf("%s content = %q, want the game line gone", trigger, resp.Content)
			}
		}

		// Each platform-limited placeholder is explained on its own terms: this
		// video could have had a description and has none, and could never have
		// had a game.
		both := draft
		both.Content = notifGameContent + "\n> {description:1}"
		blanks := notifPreviewBlanks(t, preview(t, both, "live"))
		if len(blanks) != 2 || blanks["{description}"] != "missing" || blanks["{game}"] != "platform" {
			t.Errorf("blanks = %v, want {description}: missing and {game}: platform", blanks)
		}
	})

	t.Run("a Twitch stream claimed without one is missing it", func(t *testing.T) {
		if err := app.ObserveLive(ctx, livedetect.Broadcast{
			Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "s1",
			URL: "https://twitch.tv/dokibird", Title: "Back on Twitch", StartedAt: time.Now(),
		}, livedetect.MechanismTwitchEventSub); err != nil {
			t.Fatalf("ObserveLive: %v", err)
		}
		resp := preview(t, draft, "live")
		if blanks := notifPreviewBlanks(t, resp); len(blanks) != 1 || blanks["{game}"] != "missing" {
			t.Errorf("blanks = %v, want {game}: missing", blanks)
		}
		if resp.Content != "Dokibird is live: Back on Twitch" {
			t.Errorf("content = %q, want the game line gone and no example in its place", resp.Content)
		}

		// A draft that never asks for the game has nothing to explain.
		plain := draft
		plain.Content = announce.DefaultContent
		if blanks := notifPreviewBlanks(t, preview(t, plain, "live")); len(blanks) != 0 {
			t.Errorf("blanks = %v for a draft without the placeholder, want none", blanks)
		}
	})

	t.Run("the poll leg's category fills it in", func(t *testing.T) {
		if err := app.ObserveLive(ctx, livedetect.Broadcast{
			Platform: livedetect.PlatformTwitch, ChannelKey: "doki", ID: "s1",
			URL: "https://twitch.tv/dokibird", Title: "Back on Twitch", Game: "Just Chatting", StartedAt: time.Now(),
		}, livedetect.MechanismTwitchPoll); err != nil {
			t.Fatalf("ObserveLive: %v", err)
		}
		resp := preview(t, draft, "live")
		if blanks := notifPreviewBlanks(t, resp); len(blanks) != 0 {
			t.Errorf("blanks = %v, want none", blanks)
		}
		if resp.Content != "Dokibird is live: Back on Twitch\n🎮 Just Chatting" {
			t.Errorf("content = %q, want the game rendered", resp.Content)
		}

		// The video triggers borrow only a YouTube broadcast, so the Twitch
		// category never leaks into one.
		resp = preview(t, draft, "upload")
		if blanks := notifPreviewBlanks(t, resp); blanks["{game}"] != "platform" {
			t.Errorf("upload blanks = %v, want {game}: platform", blanks)
		}
	})
}
