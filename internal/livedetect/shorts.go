package livedetect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// shortsProbeTimeout bounds one classification request.
const shortsProbeTimeout = 8 * time.Second

// ErrShortUnknown reports that the probe could not tell whether a video is a
// short. Callers announce such a video as an ordinary upload: of the two
// possible mistakes, calling a short "a video" is the one nobody minds.
var ErrShortUnknown = errors.New("could not determine whether the video is a short")

// ShortsProbe tells a YouTube short from an ordinary upload.
//
// The Data API has no field for it. What it has is a behaviour of the site:
// https://www.youtube.com/shorts/{id} answers 200 for a short and redirects to
// /watch?v={id} for anything else. This is the one place the detector touches
// youtube.com rather than the API, and it is allowed to because it is a
// classification with a safe fallback, not a detection: a blocked or odd
// answer degrades to "announce it as a video", never to "never announce it".
// It runs once per new upload - a few requests a day.
type ShortsProbe struct {
	httpClient *http.Client
	// BaseURL is injectable for tests.
	BaseURL string
}

// NewShortsProbe constructs a probe that does not follow redirects: the
// redirect IS the answer.
func NewShortsProbe() *ShortsProbe {
	return &ShortsProbe{
		httpClient: &http.Client{
			Timeout: shortsProbeTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		BaseURL: "https://www.youtube.com",
	}
}

// IsShort reports whether the video is a short. It returns ErrShortUnknown
// (wrapped) whenever the answer was anything other than the two documented
// shapes, so a consent interstitial or a bot check cannot be misread as either
// answer.
func (p *ShortsProbe) IsShort(ctx context.Context, videoID string) (bool, error) {
	if p == nil || videoID == "" {
		return false, ErrShortUnknown
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/shorts/"+videoID, nil)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrShortUnknown, err)
	}
	// A browser-like agent gets the ordinary answer; a bare Go agent is more
	// likely to be handed a challenge page, which is exactly the unknown case.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrShortUnknown, err)
	}
	defer resp.Body.Close()
	// The body is irrelevant; drain a little so the connection can be reused,
	// but never the whole page.
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusOK:
		return true, nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		loc := resp.Header.Get("Location")
		if strings.Contains(loc, "/watch") {
			return false, nil
		}
		return false, fmt.Errorf("%w: redirected to %s", ErrShortUnknown, redirectHost(loc))
	default:
		return false, fmt.Errorf("%w: status %d", ErrShortUnknown, resp.StatusCode)
	}
}

// redirectHost keeps only the host of a redirect target for the log line.
func redirectHost(loc string) string {
	loc = strings.TrimPrefix(strings.TrimPrefix(loc, "https://"), "http://")
	if i := strings.IndexAny(loc, "/?#"); i >= 0 {
		loc = loc[:i]
	}
	if loc == "" {
		return "(relative url)"
	}
	return loc
}
