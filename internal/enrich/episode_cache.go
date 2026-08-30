package enrich

import (
	"strconv"
	"sync"
	"time"
)

// episode_cache.go is the short-lived, bounded cache behind the two EpisodeLister
// reads (SeriesSeasons / SeasonEpisodes).
//
// It exists for the file matcher (ADR-0044), whose season list loads its Slots
// PER SEASON: one provider round-trip on open and one more per season expanded.
// Without a cache, collapsing and re-expanding a season — the single most ordinary
// gesture on that screen — is a fresh provider call every time, and a ten-season
// Show costs a call per toggle against a rate-limited API for a list that changes
// perhaps once a year.
//
// Correctness never depends on it, exactly as with candidateCache: a miss falls
// through to the live query, a zero/negative TTL disables it entirely (every get
// misses, every put is a no-op), and a provider swap clears it, so a re-keyed or
// re-configured provider is never answered from the old one's results.

// DefaultEpisodeListCacheTTL is how long one series' season/episode listing is
// reused before the next request re-queries. Longer than the artwork candidate
// TTL because an episode list is far more stable than an artwork grid and the
// matcher re-reads it on every expand; short enough that a provider correction
// shows up within a sitting.
const DefaultEpisodeListCacheTTL = 5 * time.Minute

// episodeCacheMaxEntries bounds the cache — an eviction backstop, not a tuning
// knob. One entry per (series, season) an Admin has looked at this session.
const episodeCacheMaxEntries = 256

// listCache is a bounded TTL cache of one provider list keyed by a string. It is
// generic only so the season list and the episode list can share one
// implementation rather than drift into two.
type listCache[T any] struct {
	ttl time.Duration
	max int
	now func() time.Time

	mu      sync.Mutex
	entries map[string]listEntry[T]
}

type listEntry[T any] struct {
	value     T
	storedAt  time.Time
	expiresAt time.Time
}

func newListCache[T any](ttl time.Duration) *listCache[T] {
	return &listCache[T]{
		ttl: ttl, max: episodeCacheMaxEntries, now: time.Now,
		entries: map[string]listEntry[T]{},
	}
}

func (c *listCache[T]) enabled() bool { return c != nil && c.ttl > 0 }

// get returns the cached list for key when present and unexpired. A miss returns
// the zero value and false, which is the signal to query the provider.
func (c *listCache[T]) get(key string) (T, bool) {
	var zero T
	if !c.enabled() {
		return zero, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return zero, false
	}
	if !c.now().Before(e.expiresAt) {
		delete(c.entries, key)
		return zero, false
	}
	return e.value, true
}

// put stores a provider list under key with a fresh TTL, evicting expired entries
// (or the oldest) when the cache is full.
func (c *listCache[T]) put(key string, v T) {
	if !c.enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.max {
		c.evictLocked(now)
	}
	c.entries[key] = listEntry[T]{value: v, storedAt: now, expiresAt: now.Add(c.ttl)}
}

// clear drops everything. Called on a provider swap: the entries are one
// provider's answers, and serving them from its replacement would be wrong in the
// one direction that matters — silently, and for as long as the TTL lasts.
func (c *listCache[T]) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]listEntry[T]{}
}

// evictLocked makes room for one entry: expired ones first, else the oldest.
func (c *listCache[T]) evictLocked(now time.Time) {
	var (
		oldestKey string
		oldestAt  time.Time
		freed     bool
	)
	for k, e := range c.entries {
		if !now.Before(e.expiresAt) {
			delete(c.entries, k)
			freed = true
			continue
		}
		if oldestKey == "" || e.storedAt.Before(oldestAt) {
			oldestKey, oldestAt = k, e.storedAt
		}
	}
	if !freed && oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// seasonEpisodesKey is the (series, season) cache key. The NUL separator keeps
// "12" + season 3 from colliding with "123" + season -1 or any other pair that
// concatenates to the same string.
func seasonEpisodesKey(showExternalID string, season int) string {
	return showExternalID + "\x00" + strconv.Itoa(season)
}
