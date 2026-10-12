package livedetect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"time"

	"live-transcript-server/internal/metrics"
)

// Operator-requested YouTube detection: the admin page's "Check video" and
// "Run discovery now".
//
// Both exist because the operator's only remedy for a missed go-live used to
// be restarting the server (FdwrQid9ulg, 2026-10-11). A restart did clear the
// negative cache that hid the stream, but it also erased the in-memory state
// that explained the miss, reset the quota counters and disconnected every
// viewer. These do the useful part of a restart - forget what hid one video,
// look again now - and nothing else.
//
// Both run ON THE DISCOVERY GOROUTINE. ytMembersAbsent is a plain map only
// that goroutine touches, and a check has to read and write it exactly as
// discovery does. Doing that from the HTTP handler's goroutine would race
// discovery, and a concurrent map write is a fatal runtime error no recover
// can catch: it would take the whole transcript server down with it. So a
// request is a closure handed to the discovery loop over ytRequests and run
// between passes, and its answer comes back on a reply channel. Serialising
// with discovery also means a check can never interleave with a pass seeding
// the same id.

const (
	// manualRequestTimeout bounds how long a caller waits for the discovery
	// loop. A pass already in progress holds the loop for up to its 20-second
	// context before it can pick a request up, and a check then makes at most
	// two calls of its own inside a 15-second one, so this leaves headroom
	// over the worst case while staying well inside a proxy's patience.
	manualRequestTimeout = 45 * time.Second
	// manualDiscoveryMinInterval spaces manual discovery passes, across every
	// channel. A pass costs up to two units per channel and an impatient
	// operator's repeated clicks would buy nothing a pass a minute does not:
	// the playlists it reads lag a go-live by longer than that anyway.
	manualDiscoveryMinInterval = 60 * time.Second
	// manualPassesPerDay caps the manual discovery passes ONE channel's admin
	// may run per quota day. The spacing alone does not bound spend: a pass a
	// minute, scripted, is twice the default schedule's rate and keeps the
	// scheduled pass from ever running, and with six channels that alone is
	// more than the day's budget - after which every state poll is refused
	// and nothing on YouTube is detected until the quota day ends. A manual
	// pass replaces the next scheduled one (the schedule restarts from it),
	// so its extra cost is at most a pass's worth each. Counted per channel
	// so one channel's script cannot use up another channel's incident
	// response; incident response needs one or two.
	manualPassesPerDay = 6
	// manualChecksPerDay caps the video checks ONE channel's admin may run per
	// quota day. A check costs at most three units - its own videos.list
	// call, the members-only playlist read, and the one state poll it brings
	// forward, which is free whenever it rides along on a call the loop was
	// making anyway - and it never arms the ten-minute hot boost (see
	// watchlist.SeedDueNow), so a channel's worst day is about 60 units. The
	// cap is there to stop a runaway script or a stuck key, not to ration
	// incident response, which needs a handful; per channel so one tenant's
	// script cannot lock every other channel out of it.
	manualChecksPerDay = 20
	// manualCheckCallTimeout bounds both API calls one check makes.
	manualCheckCallTimeout = 15 * time.Second
)

// Errors the manual controls return. The admin endpoints map each to an HTTP
// status, so they are sentinels and types rather than strings to match on.
var (
	// ErrDetectionDisabled is live detection being off in the config, or
	// having no usable leg at all.
	ErrDetectionDisabled = errors.New("live detection is disabled in the server config")
	// ErrYouTubeNotRunning is the YouTube leg being disabled, not started or
	// shutting down: there is no discovery goroutine to run the request on.
	ErrYouTubeNotRunning = errors.New("youtube detection is not running")
	// ErrNoYouTubeChannel is a channel without a youtubeChannelId.
	ErrNoYouTubeChannel = errors.New("this channel is not watched on YouTube; it has no youtubeChannelId")
	// ErrInvalidVideoID is input that cannot be a YouTube video id.
	ErrInvalidVideoID = errors.New("not a YouTube video id")
	// ErrVideoNotFound is videos.list declining to return the id: the video
	// is private, deleted, blocked, or the id is wrong.
	ErrVideoNotFound = errors.New("YouTube did not return this video; it is private, deleted, or the id is wrong")
	// ErrManualTimeout is the discovery loop not answering in time.
	ErrManualTimeout = errors.New("timed out waiting for the discovery loop")
	// ErrYouTubeAPI wraps a Data API call that failed outright.
	ErrYouTubeAPI = errors.New("the YouTube Data API call failed")

	// errManualPanicked is what a caller gets back from a request that
	// panicked on the discovery goroutine. The panic is logged there.
	errManualPanicked = errors.New("the request failed on the discovery goroutine; see the server log")
)

// TooSoonError refuses a manual request a limit does not allow yet: the
// manual discovery spacing, a daily cap, or the unit budget.
type TooSoonError struct {
	// Reason says which limit, for the operator.
	Reason string
	// RetryAfter is how long until the request would be allowed.
	RetryAfter time.Duration
}

func (e *TooSoonError) Error() string {
	return fmt.Sprintf("%s; try again in %s", e.Reason, waitText(e.RetryAfter))
}

// waitText renders a wait for the admin page: "42s", "3m", "5h 12m". The
// message goes to a person, and Go's "5h0m0s" reads like a timestamp.
func waitText(d time.Duration) string {
	// Rounded before choosing the unit, so 59m50s reads "1h", not "60m".
	if s := d.Round(time.Second); s < time.Minute {
		return fmt.Sprintf("%ds", max(int(s/time.Second), 1))
	}
	d = d.Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

// OtherChannelError refuses a check for a video that is not the asking
// channel's own.
//
// The guard matters because applyYouTubeVideo trusts the channel key an id
// was seeded under (watchlist.ChannelOf) over the video's own channel. Without
// it, one channel's admin could seed another channel's stream under their own
// key, and detection would announce it - and with queueIncoming, queue it -
// as theirs.
type OtherChannelError struct {
	VideoID string
	// ChannelID is the video's own channel, "" when YouTube did not say.
	ChannelID string
	// ChannelKey is the configured channel that owns ChannelID, "" when none
	// does. Kept for the log; the message leaves it out.
	ChannelKey string
}

func (e *OtherChannelError) Error() string {
	if e.ChannelID == "" {
		return fmt.Sprintf("YouTube did not say which channel video %s belongs to, so it cannot be checked for this one", e.VideoID)
	}
	return fmt.Sprintf("video %s belongs to YouTube channel %s, which is not this channel's", e.VideoID, e.ChannelID)
}

// ytVideoIDPattern matches a YouTube video id: 11 characters of base64url
// alphabet.
var ytVideoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// ValidYouTubeVideoID reports whether s has the shape of a YouTube video id.
// It says nothing about whether the video exists.
func ValidYouTubeVideoID(s string) bool {
	return ytVideoIDPattern.MatchString(s)
}

// VideoCheck is what CheckYouTubeVideo learned about one video and did with it.
type VideoCheck struct {
	VideoID    string `json:"videoId"`
	ChannelKey string `json:"channelKey"`
	URL        string `json:"url"`
	// State is the video's state as YouTube reports it right now: upcoming,
	// live, ended, or unknown when the answer could not be classified.
	State string `json:"state"`
	Title string `json:"title"`
	// ScheduledStart and ActualStart are unix seconds, zero when YouTube
	// reported none.
	ScheduledStart int64 `json:"scheduledStart,omitempty"`
	ActualStart    int64 `json:"actualStart,omitempty"`
	// MembersOnly is how detection will treat the video: true keeps it off
	// the worker's queue unless liveDetect.queueMembersOnly is set.
	MembersOnly bool `json:"membersOnly"`
	// MembersOnlyChecked says the channel's members-only playlist was read
	// for this check. False means it was skipped - the channel has none, or
	// the read failed - and MembersOnly is whatever detection already held.
	MembersOnlyChecked bool `json:"membersOnlyChecked"`
	// Unretired says the id was in a negative cache, hidden from discovery,
	// although it has not ended, and the check cleared it. True is the
	// signature of the failure behind FdwrQid9ulg, and worth reporting. An
	// ended video found there is the cache doing its job - finished videos
	// are what it is for - and reads false.
	Unretired bool `json:"unretired"`
	// WasTracked says the id was already on the watchlist before the check.
	WasTracked bool `json:"wasTracked"`
	// Tracked says it is on the watchlist now, due for an immediate state
	// poll. The state loop, not the check, does the detecting.
	Tracked bool `json:"tracked"`
	// ChecksRemaining is how many more checks today's cap allows this
	// channel.
	ChecksRemaining int `json:"checksRemaining"`
}

// DiscoverySummary is what one discovery pass did. Every pass builds one; a
// manual pass hands it back to the operator.
type DiscoverySummary struct {
	// Channels is how many YouTube channels the pass covered. A pass always
	// covers every configured channel, whichever admin page asked for it.
	Channels int `json:"channels"`
	// PlaylistsRead counts the playlists that answered, uploads and
	// members-only alike.
	PlaylistsRead int `json:"playlistsRead"`
	// Failures counts playlist reads that failed. A members-only playlist
	// that does not exist is not one: that is most channels' normal answer.
	Failures int `json:"failures"`
	// BudgetRefused says the unit budget stopped the pass before it read
	// every playlist it meant to.
	BudgetRefused bool `json:"budgetRefused"`
	// UnitsSpent is the quota the pass booked.
	UnitsSpent int `json:"unitsSpent"`
	// Found and FoundMembersOnly are the ids the pass seeded, by channel key
	// and in page order.
	Found            map[string][]string `json:"found,omitempty"`
	FoundMembersOnly map[string][]string `json:"foundMembersOnly,omitempty"`
}

// noteFound records ids a pass seeded for a channel.
func (s *DiscoverySummary) noteFound(key string, ids []string, membersOnly bool) {
	if len(ids) == 0 {
		return
	}
	m := &s.Found
	if membersOnly {
		m = &s.FoundMembersOnly
	}
	if *m == nil {
		*m = map[string][]string{}
	}
	(*m)[key] = append((*m)[key], ids...)
}

// discoveryRequest is one piece of operator work for the discovery goroutine.
type discoveryRequest struct {
	// run does the work. It is called on the discovery goroutine, so it may
	// touch ytMembersAbsent like any other discovery code.
	run func(now time.Time) error
	// isPass marks a full discovery pass. The loop's own schedule restarts
	// from it rather than running another pass moments later and paying for
	// the same playlists twice.
	isPass bool
	// reply carries run's error back. Buffered to one so the loop never
	// blocks on a caller that has already given up.
	reply chan error
}

// awaitDiscovery waits until the next scheduled pass is due, serving operator
// requests in the meantime. It returns when that pass is due - later than
// asked if a request ran a pass of its own - and false if the detector is
// shutting down.
//
// It replaces a plain sleep. With one, a request would wait out the whole
// discovery interval (two minutes by default) before the loop looked at it.
func (d *Detector) awaitDiscovery(next time.Time) (time.Time, bool) {
	for {
		wait := time.Until(next)
		if wait <= 0 {
			return next, true
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
			return next, true
		case <-d.ctx.Done():
			t.Stop()
			return next, false
		case req := <-d.ytRequests:
			t.Stop()
			d.serveDiscoveryRequest(req)
			if req.isPass {
				now := time.Now()
				next = now.Add(d.discoveryWait(now))
			}
		}
	}
}

// serveDiscoveryRequest runs one request and always answers it.
//
// A panic is recovered HERE rather than left to spawn's recover, which would
// end the discovery goroutine and with it discovery itself, for the rest of
// the process's life, over one bad request. The caller hears about it either
// way.
func (d *Detector) serveDiscoveryRequest(req discoveryRequest) {
	err := errManualPanicked
	defer func() {
		if r := recover(); r != nil {
			slog.Error("recovered from panic in an operator request on the discovery goroutine",
				"func", "Detector.serveDiscoveryRequest", "panic", r)
		}
		req.reply <- err
	}()
	err = req.run(time.Now())
}

// onDiscoveryGoroutine hands req to the discovery loop and waits for its
// answer, the request's context, shutdown (StopManualRequests or Close) or
// manualRequestTimeout, whichever comes first.
//
// A caller that gives up after the loop picked the request up leaves it to
// finish on its own. That is harmless: everything a request does is safe to
// complete unobserved, and the reply channel's buffer means the loop never
// waits for anyone to read it.
func (d *Detector) onDiscoveryGoroutine(ctx context.Context, req discoveryRequest) error {
	req.reply = make(chan error, 1)
	timer := time.NewTimer(manualRequestTimeout)
	defer timer.Stop()

	select {
	case d.ytRequests <- req:
	case <-timer.C:
		return ErrManualTimeout
	case <-ctx.Done():
		return ctx.Err()
	case <-d.manualStop:
		return ErrYouTubeNotRunning
	case <-d.ctx.Done():
		return ErrYouTubeNotRunning
	}

	select {
	case err := <-req.reply:
		return err
	case <-timer.C:
		return ErrManualTimeout
	case <-ctx.Done():
		return ctx.Err()
	case <-d.manualStop:
		return ErrYouTubeNotRunning
	case <-d.ctx.Done():
		return ErrYouTubeNotRunning
	}
}

// StopManualRequests releases every caller waiting on the discovery loop and
// refuses new ones, as the server begins to shut down. Nil-receiver-safe and
// idempotent.
//
// It exists because the HTTP server is shut down BEFORE the detector is
// closed - the detector must outlive the handlers that call it - and
// http.Server.Shutdown waits for in-flight handlers without cancelling their
// requests. A check parked behind a discovery pass against a slow API could
// otherwise hold Shutdown for its whole grace period, and Docker's SIGKILL
// would land before the detector and the database were closed. The request
// itself may still be running on the discovery goroutine; Close cancels it.
func (d *Detector) StopManualRequests() {
	if d == nil {
		return
	}
	d.manualStopOnce.Do(func() { close(d.manualStop) })
}

// manualAvailable reports why the manual controls cannot run, or nil when
// they can. Checked before anything else so a disabled leg answers at once.
func (d *Detector) manualAvailable() error {
	if d == nil || !d.Enabled() {
		return ErrDetectionDisabled
	}
	if !d.cfg.YouTube.Enabled || !d.ytDiscoveryRunning.Load() {
		return ErrYouTubeNotRunning
	}
	select {
	case <-d.manualStop:
		return ErrYouTubeNotRunning
	default:
	}
	return nil
}

// WatchesYouTube reports whether a channel is watched on YouTube by an enabled
// YouTube leg, which is what the admin page's manual controls need before it
// offers them. Nil-receiver-safe.
func (d *Detector) WatchesYouTube(channelKey string) bool {
	return d != nil && d.cfg.YouTube.Enabled && d.youtubeChannelFor(channelKey) != ""
}

// youtubeChannelFor reverses the channel id -> channel key map. Config gives
// each channel at most one YouTube channel id, so the answer is unique.
func (d *Detector) youtubeChannelFor(channelKey string) string {
	for channelID, key := range d.ytTargets {
		if key == channelKey {
			return channelID
		}
	}
	return ""
}

// RunDiscoveryNow runs one full discovery pass now, on behalf of channelKey's
// admin - the same pass the loop runs on its schedule, over every channel,
// spending the same units through the governor - and reports what it did. The
// loop's next scheduled pass is then a full interval away.
//
// At most one manual pass a minute across every channel, and
// manualPassesPerDay per channel per quota day; a refusal is a *TooSoonError.
func (d *Detector) RunDiscoveryNow(ctx context.Context, channelKey string) (DiscoverySummary, error) {
	if err := d.manualAvailable(); err != nil {
		return DiscoverySummary{}, err
	}
	if err := d.reserveManualDiscovery(channelKey, time.Now()); err != nil {
		return DiscoverySummary{}, err
	}

	var summary DiscoverySummary
	err := d.onDiscoveryGoroutine(ctx, discoveryRequest{
		isPass: true,
		run: func(time.Time) error {
			summary = d.youtubeDiscoverOnce()
			return nil
		},
	})
	if err != nil {
		return DiscoverySummary{}, err
	}
	slog.Info("manual youtube discovery pass complete", "func", "Detector.RunDiscoveryNow",
		"key", channelKey, "channels", summary.Channels, "playlists_read", summary.PlaylistsRead,
		"failures", summary.Failures, "budget_refused", summary.BudgetRefused,
		"units", summary.UnitsSpent, "found", summary.Found, "found_members_only", summary.FoundMembersOnly)
	return summary, nil
}

// reserveManualDiscovery books a manual pass for channelKey against
// manualDiscoveryMinInterval and the channel's manualPassesPerDay, or says how
// long until one is allowed.
func (d *Detector) reserveManualDiscovery(channelKey string, now time.Time) error {
	d.manualMu.Lock()
	defer d.manualMu.Unlock()
	d.manualRolloverLocked(now)
	if d.manualPasses[channelKey] >= manualPassesPerDay {
		return &TooSoonError{
			Reason:     fmt.Sprintf("this channel has run discovery by hand the %d times allowed per quota day", manualPassesPerDay),
			RetryAfter: nextQuotaDayStart(now).Sub(now),
		}
	}
	if !d.lastManualDiscovery.IsZero() {
		if wait := d.lastManualDiscovery.Add(manualDiscoveryMinInterval).Sub(now); wait > 0 {
			return &TooSoonError{Reason: "discovery was already run by hand in the last minute", RetryAfter: wait}
		}
	}
	d.lastManualDiscovery = now
	d.manualPasses[channelKey]++
	return nil
}

// reserveManualCheck books one video check for channelKey against the
// channel's cap for today and returns how many it has left.
//
// A check is counted when it is accepted, not when it succeeds: one YouTube
// refuses (not found, another channel's video) has spent its units all the
// same, and one that times out may still be running. Counting refusals is
// also what bounds a script probing other channels' ids, each of which costs
// a videos.list call.
func (d *Detector) reserveManualCheck(channelKey string, now time.Time) (remaining int, err error) {
	d.manualMu.Lock()
	defer d.manualMu.Unlock()
	d.manualRolloverLocked(now)
	if d.manualChecks[channelKey] >= manualChecksPerDay {
		return 0, &TooSoonError{
			Reason:     fmt.Sprintf("this channel has used all %d video checks allowed per quota day", manualChecksPerDay),
			RetryAfter: nextQuotaDayStart(now).Sub(now),
		}
	}
	d.manualChecks[channelKey]++
	return manualChecksPerDay - d.manualChecks[channelKey], nil
}

// manualRolloverLocked starts the per-channel manual counts afresh when the
// quota day changes, so the caps reset with the budget they protect. Callers
// must hold manualMu.
func (d *Detector) manualRolloverLocked(now time.Time) {
	if day := quotaDay(now); day != d.manualDay || d.manualChecks == nil || d.manualPasses == nil {
		d.manualDay = day
		d.manualChecks = map[string]int{}
		d.manualPasses = map[string]int{}
	}
}

// CheckYouTubeVideo looks up one video an operator names for a channel and,
// if it is that channel's, puts it on the watchlist due for an immediate
// state poll - clearing it out of both negative caches first, so nothing that
// hid it can go on hiding it.
//
// It does not detect anything itself. The seed wakes the state loop, which
// polls the id within seconds and takes the normal path: a live broadcast is
// claimed, queued and announced like any other (booked under
// MechanismYouTubeManual, as the operator's rescue), and an upcoming one is
// watched on the ladder until it starts. Calling the sink from here would be
// a second, untested way into the ledger.
//
// Costs one unit for videos.list, one more to read the members-only playlist
// unless the channel is known to have none, and the one state poll it brings
// forward, which is free when it rides along on a call the loop was making
// anyway. At most manualChecksPerDay a quota day per channel.
func (d *Detector) CheckYouTubeVideo(ctx context.Context, channelKey, videoID string) (VideoCheck, error) {
	if err := d.manualAvailable(); err != nil {
		return VideoCheck{}, err
	}
	if !ValidYouTubeVideoID(videoID) {
		return VideoCheck{}, ErrInvalidVideoID
	}
	channelID := d.youtubeChannelFor(channelKey)
	if channelID == "" {
		return VideoCheck{}, ErrNoYouTubeChannel
	}
	remaining, err := d.reserveManualCheck(channelKey, time.Now())
	if err != nil {
		return VideoCheck{}, err
	}

	var check VideoCheck
	err = d.onDiscoveryGoroutine(ctx, discoveryRequest{
		run: func(now time.Time) error {
			var err error
			check, err = d.checkVideo(channelID, channelKey, videoID, now)
			return err
		},
	})
	if err != nil {
		return VideoCheck{}, err
	}
	check.ChecksRemaining = remaining
	return check, nil
}

// checkVideo is CheckYouTubeVideo's work. It runs on the discovery goroutine.
func (d *Detector) checkVideo(channelID, channelKey, videoID string, now time.Time) (VideoCheck, error) {
	if !d.gov.ReserveUnits(now, 1) {
		return VideoCheck{}, &TooSoonError{
			Reason:     "the YouTube unit budget is spent for today",
			RetryAfter: nextQuotaDayStart(now).Sub(now),
		}
	}
	metrics.LiveDetectQuotaUnits.Inc()

	ctx, cancel := d.pollCtx(manualCheckCallTimeout)
	defer cancel()

	videos, err := d.youtube.VideosList(ctx, []string{videoID})
	if err != nil {
		if errors.Is(err, errYouTubeQuota) {
			d.gov.BlockUnits(now)
			return VideoCheck{}, &TooSoonError{
				Reason:     "YouTube reports the API quota exhausted",
				RetryAfter: nextQuotaDayStart(now).Sub(now),
			}
		}
		return VideoCheck{}, fmt.Errorf("%w: %v", ErrYouTubeAPI, err)
	}
	var v *YTVideo
	for i := range videos {
		if videos[i].ID == videoID {
			v = &videos[i]
			break
		}
	}
	if v == nil {
		return VideoCheck{}, ErrVideoNotFound
	}
	if owner := d.ytTargets[v.ChannelID()]; owner != channelKey {
		return VideoCheck{}, &OtherChannelError{VideoID: videoID, ChannelID: v.ChannelID(), ChannelKey: owner}
	}

	state := v.State()
	wasTracked := d.watch.Known(videoID)
	// Cleared whatever the state, but only reported for a video that has not
	// ended. Only ended entries are ever retired, so an ended video found in
	// the cache is the cache working as meant; reporting it as "hidden" would
	// send the operator hunting a bug that is not there.
	unretired := d.watch.Unretire(videoID, now) && state != StateEnded
	membersOnly, membersChecked := d.checkMembersOnly(ctx, channelID, channelKey, videoID, now)

	// Due now, but NOT boosted: see SeedDueNow. The check has just read the
	// video's state, so one poll settles it and the ladder does the rest; a
	// boost cost about 200 units a check, re-armed by every re-check.
	//
	// A video already live is a go-live detection missed, and the claim that
	// poll makes is booked as the operator's rescue. The flags go in with the
	// seed rather than after it, for the reason SeedMembersOnly gives: the
	// seed wakes the state loop, and a poll in between could claim a
	// members-only go-live as an ordinary one. Not being on the members-only
	// page does not clear an existing flag, which mirrors discovery: only the
	// uploads page says an id is public, and an older members-only video can
	// simply have scrolled off the 50-id page.
	rescuedBy := ""
	if state == StateLive {
		rescuedBy = MechanismYouTubeManual
	}
	d.watch.SeedDueNow(videoID, channelKey, now, membersOnly, rescuedBy)

	check := VideoCheck{
		VideoID:            videoID,
		ChannelKey:         channelKey,
		URL:                YouTubeWatchURL(videoID),
		State:              state.String(),
		Title:              v.Title(),
		MembersOnly:        d.watch.MembersOnly(videoID),
		MembersOnlyChecked: membersChecked,
		Unretired:          unretired,
		WasTracked:         wasTracked,
		Tracked:            d.watch.Known(videoID),
	}
	if t := v.ScheduledStartTime(); !t.IsZero() {
		check.ScheduledStart = t.Unix()
	}
	if t := v.StartedAt(); !t.IsZero() {
		check.ActualStart = t.Unix()
	}

	// Named like the discovery and drop lines, so one grep for the id or the
	// key finds the check among them.
	slog.Info("operator check seeded a youtube video", "func", "Detector.checkVideo",
		"key", channelKey, "videoId", videoID, "state", check.State,
		rfc3339Attr("scheduled", v.ScheduledStartTime()), rfc3339Attr("started", v.StartedAt()),
		"members_only", check.MembersOnly, "members_only_checked", membersChecked,
		"unretired", unretired, "was_tracked", wasTracked)
	return check, nil
}

// checkMembersOnly reports whether videoID is on its channel's members-only
// playlist, and whether that playlist was read at all.
//
// The read is discovery's own (readMembersOnlyPlaylist), backoff and 404/403
// rules included - which is why it must run on the discovery goroutine; the
// videos.list call just made with the same key is what lets a 403 there be
// read as "this playlist". Anything that stops the read answers "not
// members-only, not checked", and the video is seeded as public: the safe
// direction, as discovery's tie-break explains - a public stream treated as
// members-only loses its transcript.
func (d *Detector) checkMembersOnly(ctx context.Context, channelID, key, videoID string, now time.Time) (membersOnly, checked bool) {
	ids, read, err := d.readMembersOnlyPlaylist(ctx, channelID, key, now)
	switch read {
	case membersReadOK:
		return slices.Contains(ids, videoID), true
	case membersReadRefused:
		slog.Warn("unit budget refused the members-only read for an operator check; seeding the video as public",
			"func", "Detector.checkMembersOnly", "key", key, "videoId", videoID)
	case membersReadFailed:
		slog.Warn("could not read the members-only playlist for an operator check; seeding the video as public",
			"func", "Detector.checkMembersOnly", "key", key, "videoId", videoID, "err", err)
	}
	return false, false
}
