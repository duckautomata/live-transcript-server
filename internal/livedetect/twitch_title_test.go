package livedetect

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
)

// titleLookupServer stands in for Helix during a stream lookup. The channel
// endpoint answers according to channelMode: "ok" (a title and a category),
// "blank" (the broadcaster exists and has a category but no title), "unknown"
// (no such broadcaster) or "error"; the streams endpoint answers live with its
// own title and category unless streamsOffline is set, and with no category
// when streamsNoGame is.
type titleLookupServer struct {
	channelMode    atomic.Value // string
	streamsOffline atomic.Bool
	streamsNoGame  atomic.Bool
	channelCalls   atomic.Int32
	streamCalls    atomic.Int32
}

func (s *titleLookupServer) client(t *testing.T) *TwitchClient {
	t.Helper()
	return newTwitchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			tokenHandler(w)
		case "/channels":
			s.channelCalls.Add(1)
			if got := r.URL.Query().Get("broadcaster_id"); got != "123" {
				t.Errorf("channels asked for broadcaster_id=%q, want 123", got)
			}
			switch s.channelMode.Load() {
			case "blank":
				json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
					{"broadcaster_id": "123", "broadcaster_login": "dokibird", "title": "   ", "game_name": "Channel Game"},
				}})
			case "unknown":
				json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
			case "error":
				w.WriteHeader(http.StatusBadRequest)
			default:
				json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
					{"broadcaster_id": "123", "broadcaster_login": "dokibird", "title": "  From the channel  ", "game_name": "  Channel Game  "},
				}})
			}
		case "/streams":
			s.streamCalls.Add(1)
			if s.streamsOffline.Load() {
				json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
				return
			}
			game := " Stream Game "
			if s.streamsNoGame.Load() {
				game = ""
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"id": "1", "user_login": "DokiBird", "type": "live", "title": "From the stream", "game_name": game},
			}})
		default:
			http.NotFound(w, r)
		}
	})
}

// The channel endpoint is asked first: it carries the title the moment the
// streamer sets it, while /streams can still answer "offline" for a few
// seconds after stream.online - which used to come back as no title at all.
//
// The category comes back on the same answer: it never costs a second call.
func TestLookupTwitchStreamPrefersChannelInformation(t *testing.T) {
	srv := &titleLookupServer{}
	srv.channelMode.Store("ok")
	d := &Detector{
		twitch:        srv.client(t),
		twitchTargets: map[string]string{"dokibird": "doki"},
		twitchUserIDs: map[string]string{"123": "doki"},
	}

	want := TwitchStreamInfo{Title: "From the channel", Game: "Channel Game"}
	if got := d.LookupTwitchStream(context.Background(), "doki"); got != want {
		t.Errorf("lookup = %+v, want the channel's title and category, trimmed: %+v", got, want)
	}
	if srv.channelCalls.Load() != 1 || srv.streamCalls.Load() != 0 {
		t.Errorf("calls: channels=%d streams=%d, want the channel endpoint only", srv.channelCalls.Load(), srv.streamCalls.Load())
	}

	// A channel key without a Twitch login is nothing to look up.
	if got := d.LookupTwitchStream(context.Background(), "nope"); got != (TwitchStreamInfo{}) {
		t.Errorf("unknown key lookup = %+v, want nothing", got)
	}
	var none *Detector
	if got := none.LookupTwitchStream(context.Background(), "doki"); got != (TwitchStreamInfo{}) {
		t.Errorf("nil detector lookup = %+v, want nothing", got)
	}
	if srv.channelCalls.Load() != 1 || srv.streamCalls.Load() != 0 {
		t.Errorf("unknown keys must not call Helix: channels=%d streams=%d", srv.channelCalls.Load(), srv.streamCalls.Load())
	}
}

// Whatever the channel endpoint fails to say, the stream is asked next; only
// when neither knows does the announcement go out untitled.
func TestLookupTwitchStreamFallsBackToStreams(t *testing.T) {
	srv := &titleLookupServer{}
	d := &Detector{
		twitch:        srv.client(t),
		twitchTargets: map[string]string{"dokibird": "doki"},
		twitchUserIDs: map[string]string{"123": "doki"},
	}

	// The stream answers with a category of its own, which is the one that
	// goes with its title - even over a category the channel already gave.
	fromStream := TwitchStreamInfo{Title: "From the stream", Game: "Stream Game"}
	for _, mode := range []string{"blank", "unknown", "error"} {
		srv.channelMode.Store(mode)
		before := srv.streamCalls.Load()
		if got := d.LookupTwitchStream(context.Background(), "doki"); got != fromStream {
			t.Errorf("channel %s: lookup = %+v, want the stream's: %+v", mode, got, fromStream)
		}
		if srv.streamCalls.Load() != before+1 {
			t.Errorf("channel %s: streams endpoint not consulted", mode)
		}
	}

	// The channel has a category but a blank title, and the stream has a title
	// but no category: each supplies what the other lacks.
	srv.streamsNoGame.Store(true)
	srv.channelMode.Store("blank")
	if got, want := d.LookupTwitchStream(context.Background(), "doki"), (TwitchStreamInfo{Title: "From the stream", Game: "Channel Game"}); got != want {
		t.Errorf("blank channel title, stream without a category: lookup = %+v, want %+v", got, want)
	}
	// With no channel answer there is no category to fall back on.
	srv.channelMode.Store("unknown")
	if got, want := d.LookupTwitchStream(context.Background(), "doki"), (TwitchStreamInfo{Title: "From the stream"}); got != want {
		t.Errorf("no channel, stream without a category: lookup = %+v, want %+v", got, want)
	}
	srv.streamsNoGame.Store(false)

	// No broadcaster id was ever resolved (EventSub off): straight to streams.
	srv.channelMode.Store("ok")
	channelCalls := srv.channelCalls.Load()
	pollOnly := &Detector{twitch: srv.client(t), twitchTargets: map[string]string{"dokibird": "doki"}, twitchUserIDs: map[string]string{}}
	if got := pollOnly.LookupTwitchStream(context.Background(), "doki"); got != fromStream {
		t.Errorf("without a broadcaster id: lookup = %+v, want %+v", got, fromStream)
	}
	if srv.channelCalls.Load() != channelCalls {
		t.Error("without a broadcaster id the channel endpoint cannot be asked")
	}

	// Neither knows: nothing, not an error.
	srv.channelMode.Store("unknown")
	srv.streamsOffline.Store(true)
	if got := d.LookupTwitchStream(context.Background(), "doki"); got != (TwitchStreamInfo{}) {
		t.Errorf("lookup with nothing to find = %+v, want nothing", got)
	}

	// The channel knew the category and nobody knew a title: the category is
	// real, so it is kept rather than thrown away with the missing title.
	srv.channelMode.Store("blank")
	if got, want := d.LookupTwitchStream(context.Background(), "doki"), (TwitchStreamInfo{Game: "Channel Game"}); got != want {
		t.Errorf("blank channel title and no live stream: lookup = %+v, want %+v", got, want)
	}
}

// The poll leg is the one Twitch observation that knows the category at
// detection time, so the broadcast it reports carries it: that is what fills
// {game} when polling wins the claim, and what backfills the ledger when
// EventSub won it without one.
func TestTwitchPollCarriesTheGame(t *testing.T) {
	sink := &recordingSink{}
	d := newTestDetector(t, sink)
	d.twitch = newTwitchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			tokenHandler(w)
			return
		}
		if r.URL.Path != "/streams" {
			t.Errorf("path = %q, want the poll to ask /streams only", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "555", "user_login": "DokiBird", "type": "live", "title": "Building a castle",
				"game_name": "  Minecraft ", "started_at": "2026-01-01T19:00:00Z"},
		}})
	})

	d.twitchPollOnce()

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.live) != 1 {
		t.Fatalf("observed %d broadcasts, want 1: %+v", len(sink.live), sink.live)
	}
	b := sink.live[0]
	if b.Platform != PlatformTwitch || b.ChannelKey != "doki" || b.ID != "555" || b.Title != "Building a castle" {
		t.Errorf("broadcast = %+v", b)
	}
	if b.Game != "Minecraft" {
		t.Errorf("game = %q, want the stream's category, trimmed", b.Game)
	}
	if sink.mechs[0] != MechanismTwitchPoll {
		t.Errorf("mechanism = %q, want %q", sink.mechs[0], MechanismTwitchPoll)
	}
}

// GetChannel decodes the broadcaster's current settings and treats an
// unknown broadcaster as nothing rather than an error.
func TestGetChannel(t *testing.T) {
	var gotID string
	c := newTwitchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			tokenHandler(w)
			return
		}
		if r.URL.Path != "/channels" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotID = r.URL.Query().Get("broadcaster_id")
		if gotID == "404" {
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"broadcaster_id": gotID, "broadcaster_login": "dokibird", "title": "Tonight: games", "game_name": "Just Chatting"},
		}})
	})

	ch, err := c.GetChannel(context.Background(), "123")
	if err != nil || ch == nil {
		t.Fatalf("GetChannel = %+v, %v", ch, err)
	}
	if gotID != "123" || ch.Title != "Tonight: games" || ch.GameName != "Just Chatting" || ch.BroadcasterLogin != "dokibird" {
		t.Errorf("GetChannel asked %q, got %+v", gotID, ch)
	}
	if ch, err := c.GetChannel(context.Background(), "404"); err != nil || ch != nil {
		t.Errorf("unknown broadcaster = %+v, %v, want nil, nil", ch, err)
	}
	if ch, err := c.GetChannel(context.Background(), ""); err != nil || ch != nil {
		t.Errorf("empty id = %+v, %v, want nil, nil without a request", ch, err)
	}
}
