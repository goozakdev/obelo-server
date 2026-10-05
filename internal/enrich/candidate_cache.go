package enrich

import "time"

// DefaultCandidateCacheTTL is how long a provider candidate-list result is reused
// before the next request re-queries. A couple of minutes is enough to absorb
// artwork-tab toggling and reopening (which auto-search on activation) without a
// fresh provider hit each time, while staying short enough that a genuinely
// refreshed record surfaces new options soon (PRD artwork-management, slice 04).
const DefaultCandidateCacheTTL = 2 * time.Minute

// candidateCache is a small, bounded, TTL cache of provider candidate-list results
// keyed by (entity, role) — a pure performance optimization that protects the
// metadata providers' rate-limits when artwork tabs auto-search on every open
// (PRD artwork-management, slice 04). Correctness never depends on it: a miss
// falls through to the live provider query exactly as today, it holds only
// ephemeral provider results (never Uploaded/Local bytes), applying/uploading an
// image invalidates the affected entry, and a zero/negative TTL disables it
// entirely — every get misses and every put is a no-op — so the server behaves
// exactly as if the cache weren't there.
//
// It is a listCache (episode_cache.go), which owns the TTL, bound and eviction.
type candidateCache = listCache[[]ArtworkCandidate]

// newCandidateCache builds a candidate cache with the given TTL. A ttl <= 0
// yields a permanently-disabled cache (get always misses, put is a no-op), which
// is the "cache off, no behavior change" mode.
func newCandidateCache(ttl time.Duration) *candidateCache {
	return newListCache[[]ArtworkCandidate](ttl)
}

// titleCandidateKey / entityCandidateKey build the (entity, role) cache keys. A
// leaf Title and a browse parent (Show/Artist/Album) live in disjoint key spaces
// so their ids never collide, and both match the identifiers the pick/upload
// invalidation paths carry.
func titleCandidateKey(titleID, role string) string { return "title\x00" + titleID + "\x00" + role }

func entityCandidateKey(entityType, entityID, role string) string {
	return "entity\x00" + entityType + "\x00" + entityID + "\x00" + role
}
