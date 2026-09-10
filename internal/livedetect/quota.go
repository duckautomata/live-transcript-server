package livedetect

import (
	"sync"
	"time"
)

// YouTube Data API quota facts. The daily allocation is per PROJECT and resets
// at midnight Pacific, and search.list sits in its own bucket rather than
// drawing on the shared units - so exhausting one must never stop the other.
const (
	youtubeDailyUnits       = 10000
	youtubeDailySearchCalls = 100
)

// quotaPacificOffsetHours is Pacific Standard Time relative to UTC. The daily
// reset is defined in Pacific time; using a fixed -8 rather than loading a
// location keeps this dependency-free at the cost of being an hour early
// during daylight saving, which is the safe direction (we resume polling an
// hour late rather than an hour early into an exhausted quota).
const quotaPacificOffsetHours = -8

// quotaGovernor tracks YouTube quota spend against a self-imposed budget.
//
// Two independent buckets, because the API has two: the shared unit budget
// used by videos.list and playlistItems.list, and search.list's own daily call
// count. A 403 quotaExceeded from one must trip only that bucket - tripping
// both on a search exhaustion would stop all detection while 90% of the unit
// budget sat unused.
//
// Reserve is taken per HTTP ATTEMPT, not per logical call: the API charges at
// least one unit for every request including invalid ones, so a retried call
// that booked a single unit would under-count and quietly overrun the budget.
type quotaGovernor struct {
	unitBudget   int
	searchBudget int

	mu            sync.Mutex
	day           int
	unitsSpent    int
	searchSpent   int
	unitsBlocked  bool
	searchBlocked bool
}

// newQuotaGovernor budgets a fraction of the daily allocation, leaving
// headroom for retries and for a restart re-seeding its watchlist.
func newQuotaGovernor(unitBudget, searchBudget int) *quotaGovernor {
	if unitBudget <= 0 || unitBudget > youtubeDailyUnits {
		unitBudget = defaultYouTubeUnitBudget
	}
	if searchBudget <= 0 || searchBudget > youtubeDailySearchCalls {
		searchBudget = defaultSearchAuditCalls
	}
	return &quotaGovernor{unitBudget: unitBudget, searchBudget: searchBudget}
}

// quotaDay identifies the API's quota day: the calendar date in Pacific time.
func quotaDay(now time.Time) int {
	pacific := now.UTC().Add(time.Duration(quotaPacificOffsetHours) * time.Hour)
	y, m, d := pacific.Date()
	return y*10000 + int(m)*100 + d
}

// rollover resets the counters when the quota day changes. Callers must hold mu.
func (g *quotaGovernor) rollover(now time.Time) {
	if d := quotaDay(now); d != g.day {
		g.day = d
		g.unitsSpent = 0
		g.searchSpent = 0
		g.unitsBlocked = false
		g.searchBlocked = false
	}
}

// ReserveUnits books n units against the shared budget, reporting whether the
// spend is allowed. A refusal is not an error: the caller backs off to a
// slower tier rather than failing.
func (g *quotaGovernor) ReserveUnits(now time.Time, n int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollover(now)
	if g.unitsBlocked || g.unitsSpent+n > g.unitBudget {
		return false
	}
	g.unitsSpent += n
	return true
}

// ReserveSearch books one search.list call against its separate bucket.
func (g *quotaGovernor) ReserveSearch(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollover(now)
	if g.searchBlocked || g.searchSpent+1 > g.searchBudget {
		return false
	}
	g.searchSpent++
	return true
}

// BlockUnits trips the shared-unit breaker until the next quota day. Called on
// a 403 quotaExceeded from videos.list or playlistItems.list: the quota
// genuinely does not reset before midnight Pacific, so retrying is pure spam.
func (g *quotaGovernor) BlockUnits(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollover(now)
	g.unitsBlocked = true
}

// BlockSearch trips the search bucket's breaker until the next quota day.
func (g *quotaGovernor) BlockSearch(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollover(now)
	g.searchBlocked = true
}

// QuotaSnapshot reports spend for the admin page and logs.
type QuotaSnapshot struct {
	UnitsSpent    int  `json:"unitsSpent"`
	UnitBudget    int  `json:"unitBudget"`
	SearchSpent   int  `json:"searchSpent"`
	SearchBudget  int  `json:"searchBudget"`
	UnitsBlocked  bool `json:"unitsBlocked"`
	SearchBlocked bool `json:"searchBlocked"`
}

// Snapshot returns the current spend.
func (g *quotaGovernor) Snapshot(now time.Time) QuotaSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollover(now)
	return QuotaSnapshot{
		UnitsSpent:    g.unitsSpent,
		UnitBudget:    g.unitBudget,
		SearchSpent:   g.searchSpent,
		SearchBudget:  g.searchBudget,
		UnitsBlocked:  g.unitsBlocked,
		SearchBlocked: g.searchBlocked,
	}
}
