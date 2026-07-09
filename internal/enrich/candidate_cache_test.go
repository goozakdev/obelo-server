package enrich

import (
	"strconv"
	"testing"
	"time"
)

// candidate_cache_test.go unit-tests the per-session candidate cache internals
// (TTL expiry, invalidation, bounded eviction, disabled mode) with an injected
// clock — the end-to-end behavior is proven through the black-box HTTP tests
// (artwork_candidate_cache_test.go). Prior art: table-free focused unit tests.

func cands(url string) []ArtworkCandidate {
	return []ArtworkCandidate{{URL: url, Source: "tmdb"}}
}

// withClock installs a manually-advanced clock so TTL expiry is deterministic
// (no sleeping). t is a pointer the closure reads on every now() call.
func withClock(c *candidateCache, now *time.Time) {
	c.now = func() time.Time { return *now }
}

func TestCandidateCacheHitThenExpiry(t *testing.T) {
	clock := time.Unix(0, 0)
	c := newCandidateCache(2 * time.Minute)
	withClock(c, &clock)

	key := titleCandidateKey("t1", "poster")
	c.put(key, cands("a.jpg"))

	// Within the TTL: a hit that returns the stored list without a provider call.
	if got, ok := c.get(key); !ok || len(got) != 1 || got[0].URL != "a.jpg" {
		t.Fatalf("get within TTL = %v, %v; want the cached list", got, ok)
	}

	// Exactly at the TTL boundary the entry is expired (expiresAt is exclusive).
	clock = clock.Add(2 * time.Minute)
	if _, ok := c.get(key); ok {
		t.Fatalf("get at TTL boundary hit; want a miss (re-query)")
	}
}

func TestCandidateCacheInvalidate(t *testing.T) {
	clock := time.Unix(0, 0)
	c := newCandidateCache(time.Minute)
	withClock(c, &clock)

	key := titleCandidateKey("t1", "poster")
	c.put(key, cands("a.jpg"))
	c.invalidate(key)
	if _, ok := c.get(key); ok {
		t.Fatalf("get after invalidate hit; want a miss")
	}
}

func TestCandidateCacheDisabledIsNoOp(t *testing.T) {
	c := newCandidateCache(0) // TTL 0 → disabled
	if c.enabled() {
		t.Fatalf("cache with TTL 0 reports enabled")
	}
	key := titleCandidateKey("t1", "poster")
	c.put(key, cands("a.jpg"))
	if _, ok := c.get(key); ok {
		t.Fatalf("disabled cache returned a hit; want always-miss")
	}
}

func TestCandidateCacheBounded(t *testing.T) {
	clock := time.Unix(0, 0)
	c := newCandidateCache(time.Hour)
	withClock(c, &clock)
	c.max = 4

	// Insert more entries than capacity, advancing the clock so each has a
	// distinct storedAt (oldest-first eviction is deterministic).
	for i := 0; i < 10; i++ {
		clock = clock.Add(time.Second)
		c.put(titleCandidateKey("t"+strconv.Itoa(i), "poster"), cands("u"+strconv.Itoa(i)))
	}

	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > c.max {
		t.Fatalf("cache holds %d entries; want <= max %d (bounded)", n, c.max)
	}

	// The most-recent insert survived; the oldest was evicted.
	if _, ok := c.get(titleCandidateKey("t9", "poster")); !ok {
		t.Errorf("newest entry evicted; want it retained")
	}
	if _, ok := c.get(titleCandidateKey("t0", "poster")); ok {
		t.Errorf("oldest entry survived; want it evicted first")
	}
}

// TestCandidateCacheKeysDisjoint: a title and an entity with the same id + role
// occupy different cache slots (never collide), and role is part of the key.
func TestCandidateCacheKeysDisjoint(t *testing.T) {
	if titleCandidateKey("x", "poster") == entityCandidateKey("artist", "x", "poster") {
		t.Errorf("title and entity keys collide")
	}
	if titleCandidateKey("x", "poster") == titleCandidateKey("x", "background") {
		t.Errorf("keys ignore role")
	}
}
