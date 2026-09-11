package livedetect

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
)

// titleLookupServer stands in for Helix during a title lookup. The channel
// endpoint answers according to channelMode: "ok" (a title), "blank" (the
// broadcaster exists but has no title), "unknown" (no such broadcaster) or
// "error"; the streams endpoint answers live with its own title unless
// streamsOffline is set.
type titleLookupServer struct {
	channelMode    atomic.Value // string
	streamsOffline atomic.Bool
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
					{"broadcaster_id": "123", "broadcaster_login": "dokibird", "title": "   "},
				}})
			case "unknown":
				json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
			case "error":
				w.WriteHeader(http.StatusBadRequest)
			default:
				json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
					{"broadcaster_id": "123", "broadcaster_login": "dokibird", "title": "  From the channel  "},
				}})
			}
		case "/streams":
			s.streamCalls.Add(1)
			if s.streamsOffline.Load() {
				json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"id": "1", "user_login": "DokiBird", "type": "live", "title": "From the stream"},
			}})
		default:
			http.NotFound(w, r)
		}
	})
}

// The channel endpoint is asked first: it carries the title the moment the
// streamer sets it, while /streams can still answer "offline" for a few
// seconds after stream.online - which used to come back as no title at all.
func TestLookupTwitchTitlePrefersChannelInformation(t *testing.T) {
	srv := &titleLookupServer{}
	srv.channelMode.Store("ok")
	d := &Detector{
		twitch:        srv.client(t),
		twitchTargets: map[string]string{"dokibird": "doki"},
		twitchUserIDs: map[string]string{"123": "doki"},
	}

	if got := d.LookupTwitchTitle(context.Background(), "doki"); got != "From the channel" {
		t.Errorf("title = %q, want the channel's, trimmed", got)
	}
	if srv.channelCalls.Load() != 1 || srv.streamCalls.Load() != 0 {
		t.Errorf("calls: channels=%d streams=%d, want the channel endpoint only", srv.channelCalls.Load(), srv.streamCalls.Load())
	}

	// A channel key without a Twitch login is nothing to look up.
	if got := d.LookupTwitchTitle(context.Background(), "nope"); got != "" {
		t.Errorf("unknown key title = %q, want empty", got)
	}
	var none *Detector
	if got := none.LookupTwitchTitle(context.Background(), "doki"); got != "" {
		t.Errorf("nil detector title = %q, want empty", got)
	}
	if srv.channelCalls.Load() != 1 || srv.streamCalls.Load() != 0 {
		t.Errorf("unknown keys must not call Helix: channels=%d streams=%d", srv.channelCalls.Load(), srv.streamCalls.Load())
	}
}

// Whatever the channel endpoint fails to say, the stream is asked next; only
// when neither knows does the announcement go out untitled.
func TestLookupTwitchTitleFallsBackToStreams(t *testing.T) {
	srv := &titleLookupServer{}
	d := &Detector{
		twitch:        srv.client(t),
		twitchTargets: map[string]string{"dokibird": "doki"},
		twitchUserIDs: map[string]string{"123": "doki"},
	}

	for _, mode := range []string{"blank", "unknown", "error"} {
		srv.channelMode.Store(mode)
		before := srv.streamCalls.Load()
		if got := d.LookupTwitchTitle(context.Background(), "doki"); got != "From the stream" {
			t.Errorf("channel %s: title = %q, want the stream's", mode, got)
		}
		if srv.streamCalls.Load() != before+1 {
			t.Errorf("channel %s: streams endpoint not consulted", mode)
		}
	}

	// No broadcaster id was ever resolved (EventSub off): straight to streams.
	srv.channelMode.Store("ok")
	channelCalls := srv.channelCalls.Load()
	pollOnly := &Detector{twitch: srv.client(t), twitchTargets: map[string]string{"dokibird": "doki"}, twitchUserIDs: map[string]string{}}
	if got := pollOnly.LookupTwitchTitle(context.Background(), "doki"); got != "From the stream" {
		t.Errorf("without a broadcaster id: title = %q", got)
	}
	if srv.channelCalls.Load() != channelCalls {
		t.Error("without a broadcaster id the channel endpoint cannot be asked")
	}

	// Neither knows: untitled, not an error.
	srv.channelMode.Store("unknown")
	srv.streamsOffline.Store(true)
	if got := d.LookupTwitchTitle(context.Background(), "doki"); got != "" {
		t.Errorf("title with nothing to find = %q, want empty", got)
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
