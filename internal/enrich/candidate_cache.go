package enrich

import (
	"sync"
	"time"
)

// DefaultCandidateCacheTTL is how long a provider candidate-list result is reused
// before the next request re-queries. A couple of minutes is enough to absorb
// artwork-tab toggling and reopening (which auto-search on activation) without a
// fresh provider hit each time, while staying short enough that a genuinely
// refreshed record surfaces new options soon (PRD artwork-management, slice 04).
const DefaultCandidateCacheTTL = 2 * time.Minute

// candidateCacheMaxEntries bounds the per-session candidate cache so it can never
// grow without limit under an Admin browsing many items — an eviction backstop,
// not a tuning knob. When full, the oldest entry is evicted to admit a new one.
const candidateCacheMaxEntries = 256

// candidateCache is a small, bounded, TTL cache of provider candidate-list results
// keyed by (entity, role) — a pure performance optimization that protects the
// metadata providers' rate-limits when artwork tabs auto-search on every open
// (PRD artwork-management, slice 04). Correctness never depends on it: a miss
// falls through to the live provider query exactly as today, it holds only
// ephemeral provider results (never Uploaded/Local bytes), applying/uploading an
// image invalidates the affected entry, and a zero/negative TTL disables it
// entirely — every Get misses and every Put is a no-op — so the server behaves
// exactly as if the cache weren't there.
type candidateCache struct {
	ttl time.Duration
	max int
	now func() time.Time

	mu      sync.Mutex
	entries map[string]candidateCacheEntry
}

// candidateCacheEntry is one cached candidate list with the timestamps used for
// TTL expiry (expiresAt) and oldest-first eviction (storedAt).
type candidateCacheEntry struct {
	cands     []ArtworkCandidate
	storedAt  time.Time
	expiresAt time.Time
}

// newCandidateCache builds a candidate cache with the given TTL. A ttl <= 0
// yields a permanently-disabled cache (get always misses, put is a no-op), which
// is the "cache off, no behavior change" mode.
func newCandidateCache(ttl time.Duration) *candidateCache {
	return &candidateCache{
		ttl:     ttl,
		max:     candidateCacheMaxEntries,
		now:     time.Now,
		entries: map[string]candidateCacheEntry{},
	}
}

// enabled reports whether the cache does anything (a positive TTL). A disabled
// cache is safe to leave wired: every operation is a no-op.
func (c *candidateCache) enabled() bool { return c != nil && c.ttl > 0 }

// get returns the cached candidate list for key when present and unexpired. A
// miss (absent, expired, or cache disabled) returns (nil, false), signalling the
// caller to query the provider. An expired entry is dropped on read.
func (c *candidateCache) get(key string) ([]ArtworkCandidate, bool) {
	if !c.enabled() {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !c.now().Before(e.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	return e.cands, true
}

// put stores a provider candidate list under key with a fresh TTL. It is a no-op
// when the cache is disabled. When the cache is at capacity it first drops any
// expired entries, then evicts the oldest, so size stays bounded.
func (c *candidateCache) put(key string, cands []ArtworkCandidate) {
	if !c.enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.max {
		c.evictLocked(now)
	}
	c.entries[key] = candidateCacheEntry{
		cands:     cands,
		storedAt:  now,
		expiresAt: now.Add(c.ttl),
	}
}

// invalidate drops the entry for key so the next request re-queries the provider.
// Called when applying (picking) or uploading a new image for a role, so the grid
// reflects reality on the next tab open. A no-op when the cache is disabled or the
// key is absent.
func (c *candidateCache) invalidate(key string) {
	if !c.enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// evictLocked makes room for one new entry: it removes all expired entries and,
// if none were expired, the single oldest one. Caller holds c.mu.
func (c *candidateCache) evictLocked(now time.Time) {
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

// titleCandidateKey / entityCandidateKey build the (entity, role) cache keys. A
// leaf Title and a browse parent (Show/Artist/Album) live in disjoint key spaces
// so their ids never collide, and both match the identifiers the pick/upload
// invalidation paths carry.
func titleCandidateKey(titleID, role string) string { return "title\x00" + titleID + "\x00" + role }

func entityCandidateKey(entityType, entityID, role string) string {
	return "entity\x00" + entityType + "\x00" + entityID + "\x00" + role
}
