package enrich

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Store is the persistence the enrich service needs. *store.DB satisfies it; the
// narrow interface keeps the seam explicit and the service testable.
type Store interface {
	LibraryByID(id string) (store.Library, error)
	TitlesForEnrichment(libraryID string, sel store.EnrichSelect, now time.Time) ([]store.Title, error)
	LockedFields(titleID string) (map[string]bool, error)
	WriteTitleEnrichment(titleID string, e store.TitleEnrichment, locks map[string]bool) error
	// SetTitleEnrichmentStatus settles a Title AND records why in one statement
	// (ADR-0050). reason is one of store.EnrichmentReason*, or the empty string where
	// the outcome has no diagnosis to give — which is a real value, not a skip: it
	// wipes whatever the last failure said, and a reason that outlives its failure is
	// worse than none.
	SetTitleEnrichmentStatus(titleID, status, reason string) error
	// SetTitleEnrichmentRetry is the TRANSIENT-failure twin of the settle above: it
	// records 'failed' with a scheduled retry instead of parking the Title, so an
	// item lost to a provider outage comes back on its own (ADR-0048). It carries no
	// reason and clears none: an outage is not a diagnosis.
	SetTitleEnrichmentRetry(titleID string, attempts int, retryAt time.Time) error

	// Single-Title match correction (issue 05): an Admin re-points a Title's
	// external metadata id, then it re-enriches just that Title. SetTitleExternalMatch
	// writes the external id WITHOUT touching identity_key; TitleForEnrichmentByID
	// reads the one Title back to re-resolve it. The store.RecordOrigin says whose
	// choice the record is — the item's own or its parent's Cascade (ADR-0046).
	SetTitleExternalMatch(titleID string, m store.ExternalMatch, origin store.RecordOrigin) error
	TitleForEnrichmentByID(titleID string) (store.Title, error)

	// TV/Music browse-parent entities (issue 03): the pass walks Shows → Seasons →
	// Episodes (leaves) and Artists → Albums → Tracks (leaves), enriching the
	// parents via the generic entity tables and the leaves via the Title tables.
	ListAllShows(libraryID string) ([]store.Show, error)
	SeasonsForShow(showID string) ([]store.Season, error)
	EpisodesForSeason(seasonID string) ([]store.Title, error)
	ListAllArtists(libraryID string) ([]store.Artist, error)
	AlbumsForArtist(artistID string) ([]store.Album, error)
	AlbumByID(albumID string) (store.Album, error)
	TracksForAlbum(albumID string) ([]store.Title, error)
	// TrackContextForTitle walks a Track UP to its Album — the one direction the
	// pass never needs and a single-Track re-enrich cannot do without (issue 14).
	// A pass arrives at a Track through its Album and has the whole album in hand;
	// a re-enrich arrives with a Title row, and store.Title carries no album_id. It
	// is how ADR-0050's tracklist tier reaches the one path that was missing it.
	// store.ErrNotFound for a Title that is not a Track (no album linkage).
	TrackContextForTitle(titleID string) (store.TrackContext, error)
	WriteEntityEnrichment(entityType, entityID string, e store.EntityEnrichmentWrite, locks map[string]bool) error
	SetEntityEnrichmentStatus(entityType, entityID, status string) error
	SetEntityEnrichmentRetry(entityType, entityID string, attempts int, retryAt time.Time) error
	EntityEnrichmentByID(entityType, entityID string) (store.EntityEnrichment, error)

	// Parent-entity Fix-info + Locked fields (issue item-editing/02): an Admin pins
	// a durable Enrichment override on a Show/Artist/Album (SetEntityExternalMatch)
	// and the parent enrich path honors its hand-set Locked fields (EntityLockedFields).
	// LibraryOfEntity gives the single-parent re-enrich the Library to serialize on.
	SetEntityExternalMatch(entityType, entityID string, pin store.EntityRecordPin) error
	EntityLockedFields(entityType, entityID string) (map[string]bool, error)
	LibraryOfEntity(entityType, entityID string) (string, error)

	// Fix-label image picker (issue item-editing/03): after the service downloads a
	// chosen provider image into the artwork cache, these write it as the role's
	// image and Lock the role (so re-enrichment keeps the hand-picked image; local
	// artwork still wins). The leaf + parent analogues.
	PickTitleArtwork(titleID, role, path, artworkID string) error
	PickEntityArtwork(entityType, entityID, role, path, artworkID string) error

	// Upload*Artwork records an Admin-uploaded image as the role's 'uploaded' row
	// and Locks the role (ADR-0026, upload-is-select). The bytes are written to the
	// artwork cache first; these persist the row (replacing any prior upload) and
	// return the replaced upload's path (empty if none) for orphan cleanup.
	UploadTitleArtwork(titleID, role, path, artworkID string) (replacedPath string, err error)
	UploadEntityArtwork(entityType, entityID, role, path, artworkID string) (replacedPath string, err error)

	// Cast headshots (cast-photos/01): a cast Credit's downloaded headshot is stored
	// as a `person` entity_artwork row keyed by the person ref, so one actor's photo
	// is cached ONCE across every Title. PersonArtworkByRef is the cross-title dedupe
	// check (skip the download when the ref already has a cached row); UpsertPersonArtwork
	// records the freshly-downloaded path.
	PersonArtworkByRef(personRef, role string) (store.Artwork, error)
	UpsertPersonArtwork(personRef, role, path string) error
}

// personProfileRole is the entity_artwork role a cast headshot is stored + served
// under (the only person image role in this slice — a person has no poster/backdrop).
const personProfileRole = "profile"

// Mode selects how much a pass re-enriches, mirroring the scanner's modes.
type Mode int

const (
	// ModeNew enriches only Titles never successfully enriched (status 'pending') —
	// the default and the auto-after-scan path.
	ModeNew Mode = iota
	// ModeFull re-enriches every visible Title (still unlocked-only) — a refresh.
	ModeFull
	// ModeRecheck enriches everything ModeNew would, PLUS the settled non-answers
	// (ADR-0051): 'unmatched' items, and 'failed' ones with no scheduled retry.
	// It is the mode for "the question changed" — a matching improvement shipped,
	// and the rows it was written for will otherwise never be asked again.
	//
	// It re-asks; it does NOT reset. No status is lowered to 'pending' and nothing
	// is cleared in advance, so an item that is still unmatched afterwards is simply
	// written unmatched again with a fresh reason (ADR-0050). And it never widens to
	// ModeFull: a 'matched' parent still short-circuits to its stored id, so a
	// recheck over a healthy Library costs zero provider calls.
	ModeRecheck
)

// settledNonAnswer reports whether an item's Enrichment SETTLED without a record
// and nothing is coming back for it on its own — the population ModeRecheck
// re-asks (ADR-0051). Two states qualify:
//
//   - 'unmatched' — the provider answered and had no record.
//   - 'failed' with no scheduled retry — a permanent refusal, parked.
//
// A 'failed' item WITH a retry in the future is deliberately excluded: that is
// in-flight work the server already owns (ADR-0048), and re-asking it early
// would collapse the distinction between "no record" and "could not reach the
// provider" that the retry column exists to keep. It is also not a resolution
// improvement's problem — the same question is already scheduled.
//
// This is the Go twin of store.settledNonAnswerClause; they change together.
func settledNonAnswer(status, retryAt string) bool {
	return status == "unmatched" || (status == "failed" && retryAt == "")
}

// providerSnapshot bundles the MetadataProvider with its derived per-kind
// Enablement so the two are always swapped as a unit — an in-flight pass that
// captures the current snapshot never observes a provider paired with a stale
// enablement, or vice versa.
type providerSnapshot struct {
	provider   MetadataProvider
	enablement Enablement
	// config is the effective per-Library ProviderConfig this snapshot was built from
	// (ADR-0027) — carried so the pass can honor per-item Enrichment-override
	// precedence: a Title pinned to a specific provider's record keeps resolving via
	// THAT provider while it stays reachable, even when the Library's Authoritative
	// provider changed underneath it, and an override whose provider a policy change
	// made unreachable is filed to the attention list rather than silently dropped
	// (issue 06). The zero value (the global/fixed-provider path) means "no per-Library
	// config" — every pin then rides the chain unchanged, the pre-policy behavior.
	config ProviderConfig
}

// Service runs Enrichment passes. It owns a Store, the ArtworkFetcher network
// seam, and the artwork cache directory. Its provider + per-kind Enablement live
// behind an atomically-swappable snapshot (see SetProvider), so a future
// settings-driven reconfiguration can rebuild and hot-swap them at runtime
// without reconstructing the Service. Enablement is per media kind: Video gates
// the Movie/TV kinds (TMDB needs a key) and Music gates the Music kind
// (MusicBrainz + Cover Art Archive need none). A kind that is off makes no
// outbound calls and its candidates are recorded 'disabled' (ADR-0001
// offline-first).
type Service struct {
	store   Store
	fetcher ArtworkFetcher

	// current holds the live GLOBAL provider + enablement snapshot, swapped
	// atomically by SetProvider and read (never mutated in place) at pass time.
	// Every provider lookup and enablement check reads the CURRENT snapshot, so a
	// runtime swap takes effect on the next read — and, because provider +
	// enablement travel together in one pointer, never half-applied. When a
	// per-Library resolver is installed (resolveLibrary below) this is the base the
	// resolver layers each Library's Enrichment policy over.
	current atomic.Pointer[providerSnapshot]

	// resolveLibrary, when installed, returns a Library's EFFECTIVE provider +
	// enablement — its Enrichment policy (ADR-0027) layered over the global config,
	// cached per Library. nil means no per-Library policy layer: every Library uses
	// the global snapshot (the pre-policy behavior, and the state of a Service
	// constructed directly in a test or given a fixed injected provider). The
	// Manager installs it via EnablePerLibraryResolution once the global config is
	// known (after its first Reload).
	resolveLibrary func(ctx context.Context, libraryID string) (providerSnapshot, error)

	cacheDir string

	// candidates is the short-lived, bounded per-session cache of provider
	// candidate-list results keyed by (entity, role), so artwork tabs that
	// auto-search on open don't re-hit the metadata providers on every toggle
	// (PRD artwork-management, slice 04). A pure optimization: a miss falls through
	// to the live query, applying/uploading an image invalidates the entry, and a
	// zero TTL disables it with no behavior change.
	candidates *candidateCache

	// now is the Service's clock, injectable so a test can drive the retry window
	// (ADR-0048) without sleeping. nil means time.Now — read through clock(), never
	// directly, so a Service built by a struct literal in a test still works.
	now func() time.Time

	// slotGroups / slotLists cache the two EpisodeLister reads (a series' season
	// list, and one season's episodes). The file matcher loads a Show's Slots one
	// season at a time (ADR-0044), so collapsing and re-expanding a season would
	// otherwise be a provider round-trip per toggle. Cleared on a provider swap;
	// see episode_cache.go.
	slotGroups *listCache[[]SeasonSummary]
	slotLists  *listCache[[]EpisodeCandidate]

	// tracklists caches an Album's resolved tracklist (ADR-0050) so the pass and a
	// Cascade over the same album inside one sitting cost one provider call between
	// them, at the host ADR-0049 watched shed load. Keyed by the whole request, not
	// the album; cleared on a provider swap. See album_tracklist.go.
	tracklists *listCache[[]TrackCandidate]

	// editionLists caches a release-group's editions (ADR-0052) so an Admin toggling
	// the edition section while they decide costs ONE provider call, not one per
	// look. Keyed by the release-group alone — the answer does not depend on the
	// album asking — and cleared on a provider swap. See album_editions.go.
	editionLists *listCache[[]ReleaseEdition]

	// Per-Library pass serialization: a Library is enriched by at most one pass at
	// a time, so the auto-after-scan trigger and a manual/scheduled pass can never
	// run concurrently over the same Library (which would double-fetch artwork and
	// race the 'pending' selection). Different Libraries still enrich in parallel.
	mu     sync.Mutex
	libMus map[string]*sync.Mutex
}

// NewService builds an enrich service. Pass the real composed provider (from
// BuildProvider) + HTTPArtworkFetcher in production (app.New) and fakes in tests.
// enablement is the per-kind on/off snapshot BuildProvider derives (or, for an
// injected fixed provider, the config-derived one); cacheDir is
// config.ArtworkCacheDir (already ensured to exist by the caller). candidateTTL
// is the artwork-picker candidate cache lifetime (0 disables the cache with no
// behavior change). The provider + enablement can later be swapped atomically via
// SetProvider.
func NewService(s Store, provider MetadataProvider, fetcher ArtworkFetcher, enablement Enablement, cacheDir string, candidateTTL time.Duration) *Service {
	svc := &Service{
		store: s, fetcher: fetcher,
		cacheDir: cacheDir, libMus: map[string]*sync.Mutex{},
		candidates:   newCandidateCache(candidateTTL),
		slotGroups:   newListCache[[]SeasonSummary](DefaultEpisodeListCacheTTL),
		slotLists:    newListCache[[]EpisodeCandidate](DefaultEpisodeListCacheTTL),
		tracklists:   newListCache[[]TrackCandidate](DefaultAlbumTracklistCacheTTL),
		editionLists: newListCache[[]ReleaseEdition](DefaultAlbumEditionCacheTTL),
	}
	svc.current.Store(&providerSnapshot{provider: provider, enablement: enablement})
	return svc
}

// SetProvider atomically swaps the Service's provider + per-kind Enablement as a
// unit. It is the runtime-reconfiguration seam: a future settings change rebuilds
// the provider (BuildProvider) and calls this to hot-swap it into the running
// Service with no restart. The swap is a single atomic pointer store, so an
// in-flight pass either sees the whole old snapshot or the whole new one — never
// a half-applied mix. The next lookup / enablement check reads the new snapshot.
func (s *Service) SetProvider(provider MetadataProvider, enablement Enablement) {
	s.current.Store(&providerSnapshot{provider: provider, enablement: enablement})
	// The cached episode listings are the OLD provider's answers (and its
	// language's). Serving them from the new one would be a silent wrong answer
	// lasting a whole TTL, which is exactly the class of bug a cache must not
	// introduce, so a swap empties them.
	s.slotGroups.clear()
	s.slotLists.clear()
	// Same reasoning for the album tracklists: they name the OLD provider's recording
	// ids, and pinning a track to an id the new provider never chose is the silent
	// wrong answer a cache must not introduce.
	s.tracklists.clear()
	// And the edition listings, for the same reason at one tier up: they name the OLD
	// provider's release ids, and offering an Admin a release the new provider never
	// listed is a pin that cannot resolve.
	s.editionLists.clear()
}

// snapshot returns the current GLOBAL provider + enablement snapshot. Callers read
// it once per use so a concurrent SetProvider swap is picked up on the next read.
func (s *Service) snapshot() providerSnapshot { return *s.current.Load() }

// clock returns the Service's time source (time.Now unless a test replaced it).
// Every read of "now" in the retry path goes through here so one pass measures
// scheduling and due-ness against a single, substitutable clock.
func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// SetClock replaces the Service's time source. Test-only seam for the retry
// backoff (ADR-0048): production never calls it, and nothing else in the Service
// reads the clock, so a fixed clock changes only when retries come due.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// snapshotFor returns the EFFECTIVE provider + enablement snapshot for a Library:
// its Enrichment policy (ADR-0027) resolved over the global config when a
// per-Library resolver is installed, else the global snapshot (identical to the
// pre-policy behavior — an empty policy resolves byte-for-byte to the global one).
// A Library-scoped pass resolves once at its start and threads the result through,
// so a mid-pass global/policy change never half-applies to an in-flight pass.
func (s *Service) snapshotFor(ctx context.Context, libraryID string) (providerSnapshot, error) {
	if s.resolveLibrary != nil {
		return s.resolveLibrary(ctx, libraryID)
	}
	return s.snapshot(), nil
}

// EnrichmentEnabledForLibrary reports whether ANY kind currently enriches for the
// given Library, honoring its Enrichment policy — so a Library switched off
// (enrich_enabled=false) reports false even while the server is globally enabled,
// and (a later slice) a Library leading with a globally-disabled-but-keyed
// provider reports true. The background triggers gate on it so a switched-off
// Library does no background work (ADR-0001). A resolution error falls back to the
// global snapshot so a transient DB hiccup degrades to the server-wide answer
// rather than silently disabling enrichment.
func (s *Service) EnrichmentEnabledForLibrary(ctx context.Context, libraryID string) bool {
	snap, err := s.snapshotFor(ctx, libraryID)
	if err != nil {
		return s.EnrichmentEnabled()
	}
	return snap.enablement.Video || snap.enablement.Music
}

// EnrichmentEnabled reports whether ANY kind is currently enriching in the live
// snapshot. The app's background triggers (auto-after-scan, the scheduled sweep)
// gate on it at pass time — the worker runs unconditionally, but a pass is only
// enqueued when something is enabled, so a runtime enable (via a settings save +
// Manager.Reload) starts background enrichment with no restart, and a fully
// unconfigured server enqueues nothing (ADR-0001 offline-first).
func (s *Service) EnrichmentEnabled() bool {
	e := s.snapshot().enablement
	return e.Video || e.Music
}

// libLock returns the per-Library mutex, creating it on first use.
func (s *Service) libLock(libraryID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.libMus[libraryID]
	if !ok {
		m = &sync.Mutex{}
		s.libMus[libraryID] = m
	}
	return m
}

// Progress is a snapshot a pass reports through its onProgress callback after
// each Title, so a caller (the app worker / the manual handler) can fan it out as
// a realtime event. It carries cumulative counts; Done is how many of Total have
// been processed so far.
type Progress struct {
	LibraryID string
	Total     int
	Done      int
	Matched   int
	Unmatched int
	Failed    int
	Disabled  int
	// Retrying counts the leaves whose lookup failed TRANSIENTLY and are scheduled
	// to be tried again (ADR-0048). Split out from Failed on purpose: the two need
	// different words in front of an operator — "8 failed" invites them to go and
	// fix eight things, "8 will be retried" tells them to wait — and collapsing them
	// would have made the fix invisible on every surface that reports a pass.
	Retrying int
}

// Result summarizes a completed pass.
type Result struct {
	Total     int
	Matched   int
	Unmatched int
	Failed    int
	Disabled  int
	// Retrying is the Progress counter of the same name: leaves left scheduled for
	// another attempt rather than parked (ADR-0048).
	Retrying int
}

// EnrichLibrary runs one Enrichment pass over a Library's visible Titles. It
// resolves each Title through the provider, downloads any referenced artwork, and
// writes the unlocked fields — recording per-Title status. A per-Title provider
// error is logged and recorded 'failed'; the pass continues (one bad lookup never
// starves the rest). When enrichment is disabled the pass makes no outbound calls
// and marks each candidate 'disabled'. Identity is never touched (ADR-0002).
//
// Returns store.ErrNotFound for an unknown Library (the handler maps it to 404).
func (s *Service) EnrichLibrary(ctx context.Context, libraryID string, mode Mode) (Result, error) {
	return s.EnrichLibraryProgress(ctx, libraryID, mode, nil)
}

// EnrichLibraryProgress is EnrichLibrary with an optional progress callback,
// invoked at the start (Done=0) and after each Title with cumulative counts, so
// the caller can publish realtime progress (events.EnrichProgress). onProgress
// may be nil. The pass holds the per-Library lock for its duration so it never
// races a concurrent pass over the same Library.
func (s *Service) EnrichLibraryProgress(ctx context.Context, libraryID string, mode Mode, onProgress func(Progress)) (Result, error) {
	lib, err := s.store.LibraryByID(libraryID)
	if err != nil {
		return Result{}, err // ErrNotFound flows through to the handler
	}

	lock := s.libLock(libraryID)
	lock.Lock()
	defer lock.Unlock()

	// Resolve the Library's EFFECTIVE provider + enablement ONCE (its Enrichment
	// policy layered over the global config, ADR-0027) and thread it through the
	// whole pass, so every leaf/parent sees one consistent snapshot even if the
	// global config or the Library's policy changes mid-pass.
	snap, err := s.snapshotFor(ctx, libraryID)
	if err != nil {
		return Result{}, err
	}

	// Phase A — gather the playable leaf Titles to enrich, processing the browse
	// parents (Show/Season, Artist/Album) as a side effect along the way. For a
	// Movie Library there are no parents; the leaves are the Movies themselves.
	// The Result counts LEAVES only (movies/episodes/tracks), uniform across kinds;
	// parent enrichment is a decoration recorded in the entity tables, not counted.
	var (
		leaves []leafWork
	)
	switch lib.Kind {
	case "tv":
		leaves, err = s.collectTVLeaves(ctx, snap, libraryID, mode)
	case "music":
		leaves, err = s.collectMusicLeaves(ctx, snap, libraryID, mode)
	default:
		sel := store.EnrichPending
		switch mode {
		case ModeFull:
			sel = store.EnrichAll
		case ModeRecheck:
			sel = store.EnrichRecheck
		}
		var titles []store.Title
		titles, err = s.store.TitlesForEnrichment(libraryID, sel, s.clock())
		for _, t := range titles {
			leaves = append(leaves, leafWork{title: t, ref: refFor(t)})
		}
	}
	if err != nil {
		return Result{}, err
	}

	// Phase B — enrich each leaf, emitting cumulative progress.
	res := Result{Total: len(leaves)}
	emit := func(done int) {
		if onProgress != nil {
			onProgress(Progress{
				LibraryID: libraryID, Total: res.Total, Done: done,
				Matched: res.Matched, Unmatched: res.Unmatched,
				Failed: res.Failed, Disabled: res.Disabled, Retrying: res.Retrying,
			})
		}
	}
	emit(0)
	for i, lw := range leaves {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := s.processLeaf(ctx, snap, lw, &res); err != nil {
			return res, err
		}
		emit(i + 1)
	}
	return res, nil
}

// ResolveIdentity looks an external id up in the provider and returns the source's
// canonical title + year — so an Admin's by-id identity fix can fill in the title
// and year from the id alone, instead of being typed. matched is false (with no
// error) when enrichment is disabled for the kind, the id does not resolve, or the
// provider has no title — the caller then falls back to whatever was supplied.
// Unlike MatchTitle this reads only; it writes nothing and never touches a Title.
func (s *Service) ResolveIdentity(ctx context.Context, ref TitleRef) (title string, year int, matched bool, err error) {
	snap := s.snapshot()
	if !snap.enablement.enabledFor(ref.Kind) {
		return "", 0, false, nil
	}
	meta, err := snap.provider.Lookup(ctx, ref)
	if errors.Is(err, ErrNoMatch) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	name := strings.TrimSpace(meta.Name)
	if !meta.Matched || name == "" {
		return "", 0, false, nil
	}
	return name, meta.Year, true, nil
}

// MatchTitle re-points a single Title's external metadata match and re-enriches
// JUST that Title immediately (PRD stories 22, 25). It writes the supplied
// external id(s) and refreshes the unlocked descriptive fields/artwork from the
// provider, but NEVER touches identity_key, season/episode numbers, or watch
// state (ADR-0002/0014) — this is the metadata match, distinct from an identity
// fix-match. With enrichment disabled the re-enrich is a no-op that records the
// Title 'disabled' (ADR-0001). Returns store.ErrNotFound for an unknown Title
// (the handler maps it to 404). The Title leaves the attention surface on a
// successful match (its status becomes 'matched').
//
// This is an ADMIN-FACING entry point: the record it writes is the Admin's choice
// made ON THIS TITLE (store.OriginChosen), so it outranks any later Cascade from
// the Title's parent (ADR-0046). A Cascade must NOT come through here — it goes
// through matchTitle with store.OriginCascaded, because the record it writes is
// the parent's and has to follow the parent.
func (s *Service) MatchTitle(ctx context.Context, titleID string, m store.ExternalMatch) error {
	return s.matchTitle(ctx, titleID, m, store.OriginChosen)
}

// matchTitle is MatchTitle with the record's origin spelled out — the one seam
// through which a Cascade writes a leaf child.
func (s *Service) matchTitle(ctx context.Context, titleID string, m store.ExternalMatch, origin store.RecordOrigin) error {
	if err := s.store.SetTitleExternalMatch(titleID, m, origin); err != nil {
		return err // ErrNotFound flows through to the handler
	}
	t, err := s.store.TitleForEnrichmentByID(titleID)
	if err != nil {
		return err
	}
	// Serialize against a concurrent pass over the same Library so the single-Title
	// re-enrich never races a full pass writing the same Title (per-Library lock).
	lock := s.libLock(t.LibraryID)
	lock.Lock()
	defer lock.Unlock()

	// Resolve the Title's Library policy so a switched-off Library records the
	// re-enrich 'disabled' (no call), consistent with a full pass over it.
	snap, err := s.snapshotFor(ctx, t.LibraryID)
	if err != nil {
		return err
	}

	// singleLeafWork is refFor plus ADR-0050's album tier — the same tier a library
	// pass applies, through the same function, so a Track re-enriched alone reaches
	// the record a pass would have given it instead of searching where a pass looked
	// up (issue 14). It also carries the tier's outcome, which is what makes this path
	// record `album-unmatched`/`not-in-tracklist` rather than a search reason.
	//
	// A Music leaf (Track) carries sparseTitle so a provider's canonical recording
	// name only fills a MISSING tag title — embedded tags are the Music display/
	// identity authority (ADR-0002), exactly as the album full-pass treats tracks.
	// Without this, applying a Track override would overwrite the tag title with
	// MusicBrainz's canonical name. A Movie/Episode display title is unaffected.
	var res Result
	return s.processLeaf(ctx, snap, s.singleLeafWork(ctx, snap, t), &res)
}

// SearchCandidateLimit caps a provider search result page so a broad query stays
// usable in the Edit-item picker (issue item-editing/01). The service truncates
// to this many; the real providers may also page internally.
const SearchCandidateLimit = 12

// SearchCandidates searches the authoritative provider for the entity kind and
// returns the Enrichment-override picker's candidates (ADR-0019). It short-circuits
// with ErrSearchUnavailable when the kind's enrichment is off (an unconfigured
// provider — the Edit-item box reports why instead of hanging), and caps the
// result to SearchCandidateLimit. A blank query yields no candidates (nil, nil);
// an unreachable provider surfaces its error to the handler. It writes nothing —
// this is a read, like ResolveIdentity.
func (s *Service) SearchCandidates(ctx context.Context, kind, query string, opts SearchOptions) ([]Candidate, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	snap := s.snapshot()
	if !snap.enablement.enabledFor(kind) {
		return nil, ErrSearchUnavailable
	}
	cands, err := snap.provider.Search(ctx, kind, query, opts)
	if err != nil {
		return nil, err
	}
	if len(cands) > SearchCandidateLimit {
		cands = cands[:SearchCandidateLimit]
	}
	return cands, nil
}

// SeriesSeasons lists the seasons of a series an Admin has picked, and
// SeasonEpisodes lists one season's episodes — the data behind choosing WHICH
// provider episode decorates a file.
//
// This is what makes an episode fixable when the provider numbers a series
// differently from the files on disk. Pinning the right series alone never helped:
// the lookup still asked for the season/episode parsed from the filename, so a file
// the provider counts in the next season stayed unmatchable no matter what was
// picked. Reads only — the pin is written by the apply.
//
// ErrSearchUnavailable when the kind's enrichment is off or the configured
// provider cannot list episodes, so the picker says why instead of hanging.
func (s *Service) SeriesSeasons(ctx context.Context, showExternalID string) ([]SeasonSummary, error) {
	lister, err := s.episodeLister()
	if err != nil {
		return nil, err
	}
	if cached, ok := s.slotGroups.get(showExternalID); ok {
		return cached, nil
	}
	out, err := lister.SeriesSeasons(ctx, showExternalID)
	if err != nil {
		return nil, err
	}
	s.slotGroups.put(showExternalID, out)
	return out, nil
}

func (s *Service) SeasonEpisodes(ctx context.Context, showExternalID string, season int) ([]EpisodeCandidate, error) {
	lister, err := s.episodeLister()
	if err != nil {
		return nil, err
	}
	key := seasonEpisodesKey(showExternalID, season)
	if cached, ok := s.slotLists.get(key); ok {
		return cached, nil
	}
	out, err := lister.SeasonEpisodes(ctx, showExternalID, season)
	if err != nil {
		return nil, err
	}
	s.slotLists.put(key, out)
	return out, nil
}

// Why episode listing is unavailable, when it is. ErrSearchUnavailable answers
// "not now" for both reasons at once, which is enough for a picker that can only
// hang or say so — and not enough for the file matcher, where a provider that
// cannot list episodes is a FIRST-CLASS state (bare numbered Slots, ADR-0044)
// rather than an error, and the screen has to explain which of the two it is
// before an Admin can do anything about it.
const (
	// EpisodeListingDisabled: video Enrichment is off for this server — switched
	// off, unconfigured, or consent not granted. Nothing about the provider.
	EpisodeListingDisabled = "disabled"
	// EpisodeListingUnsupported: Enrichment is on, but the Authoritative provider
	// does not implement EpisodeLister (only TMDB does). Turning enrichment on
	// again will not help; changing provider will.
	EpisodeListingUnsupported = "unsupported"
)

// EpisodeListingUnavailable reports why a provider episode list cannot be fetched
// — EpisodeListingDisabled or EpisodeListingUnsupported — or "" when one can. It
// asks the same two questions episodeLister asks, in the same order, and is the
// only way to tell them apart from outside the package.
func (s *Service) EpisodeListingUnavailable() string {
	snap := s.snapshot()
	if !snap.enablement.enabledFor("episode") {
		return EpisodeListingDisabled
	}
	if _, ok := snap.provider.(EpisodeLister); !ok {
		return EpisodeListingUnsupported
	}
	return ""
}

// episodeLister resolves the configured provider to the optional EpisodeLister
// capability, gated on video enrichment being on at all (an episode list is a
// video notion). A provider that doesn't implement it is ErrSearchUnavailable.
func (s *Service) episodeLister() (EpisodeLister, error) {
	snap := s.snapshot()
	if !snap.enablement.enabledFor("episode") {
		return nil, ErrSearchUnavailable
	}
	lister, ok := snap.provider.(EpisodeLister)
	if !ok {
		return nil, ErrSearchUnavailable
	}
	return lister, nil
}

// EpisodePicker is everything the "which episode is this?" chooser needs: the
// series' season list (sent once), the season being shown, and its episodes.
type EpisodePicker struct {
	// Seasons is populated only when the caller did not name a season — the client
	// fetches the list once, then pages episodes within it.
	Seasons  []SeasonSummary
	Season   int
	Episodes []EpisodeCandidate
}

// EpisodePickerData lists the episodes of `showExternalID` for the season the
// Admin is looking at. A nil season means "the one this file is already filed
// under", which is the season they are most likely to want: the common shape of
// this problem is a right-season/wrong-episode or an end-of-season run that the
// provider counts in the next season.
//
// The store read also owns the ErrNotFound for an unknown Title, so the HTTP layer
// needs no separate lookup just to learn the season.
func (s *Service) EpisodePickerData(ctx context.Context, titleID, showExternalID string, season *int) (EpisodePicker, error) {
	t, err := s.store.TitleForEnrichmentByID(titleID)
	if err != nil {
		return EpisodePicker{}, err // ErrNotFound flows through
	}
	out := EpisodePicker{Season: t.SeasonNumber}
	if season != nil {
		out.Season = *season
	} else {
		seasons, sErr := s.SeriesSeasons(ctx, showExternalID)
		if sErr != nil {
			return EpisodePicker{}, sErr
		}
		out.Seasons = seasons
		// The file's own season is the best default only when the chosen series
		// HAS it. It often won't: the reason an Admin is here is that the disk and
		// the provider disagree, and the record they want may live in a different
		// series altogether (a re-numbered continuation like The New Batman
		// Adventures, which has no season 3 for a file filed as S03). Opening on an
		// empty list in that case would look like "this series has no episodes".
		out.Season = defaultSeason(seasons, t.SeasonNumber)
	}
	eps, err := s.SeasonEpisodes(ctx, showExternalID, out.Season)
	if err != nil {
		return EpisodePicker{}, err
	}
	out.Episodes = eps
	return out, nil
}

// defaultSeason picks the season the chooser opens on: the file's own when the
// series has it, otherwise the first real season (preferring a numbered one over
// Specials, which is rarely what someone is looking for). Falls back to `want`
// when the series lists no seasons at all, so the caller still asks for something
// and surfaces the provider's own answer.
func defaultSeason(seasons []SeasonSummary, want int) int {
	first := -1
	for _, s := range seasons {
		if s.Season == want {
			return want
		}
		if s.Season > 0 && (first < 0 || s.Season < first) {
			first = s.Season
		}
	}
	if first >= 0 {
		return first
	}
	if len(seasons) > 0 {
		return seasons[0].Season
	}
	return want
}

// ApplyEpisodeOverride pins BOTH the series and the exact provider episode that
// decorates a leaf Episode, then re-enriches just it. The season/episode pinned
// here replace the parsed numbers for the LOOKUP ONLY: identity_key, the Title's
// own season/episode, its place in the library, and every User's watch state are
// untouched (ADR-0002/0014) — the file keeps its history and simply gains the right
// details. store.ErrNotFound for an unknown Title flows to the handler as a 404.
//
// Admin-facing: an Episode pin is the Slot's OWN choice (store.OriginChosen via
// MatchTitle), so it outranks its Show's Cascade (ADR-0046).
func (s *Service) ApplyEpisodeOverride(ctx context.Context, titleID, showExternalID string, season, episode int) error {
	if episode <= 0 {
		return fmt.Errorf("%w: an episode number is required", ErrExternalRefInvalid)
	}
	return s.MatchTitle(ctx, titleID, store.ExternalMatch{
		TMDBID:        showExternalID,
		EpisodeSeason: season,
		EpisodeNumber: episode,
	})
}

// ExternalMatchForKind maps a picked candidate's authoritative external id onto
// the right id column for the entity kind: music leaves (track) pin a MusicBrainz
// id, video leaves (movie/episode) a TMDB id. It is the small adapter the apply-
// Enrichment-override endpoint uses to reuse MatchTitle (the durable-pin +
// single-entity re-enrich primitive) from a candidate rather than a raw id form.
func ExternalMatchForKind(kind, externalID string) store.ExternalMatch {
	switch kind {
	case "artist", "album", "track":
		return store.ExternalMatch{MusicbrainzID: externalID}
	default:
		return store.ExternalMatch{TMDBID: externalID}
	}
}

// SearchTitleCandidates searches the authoritative provider for a single Title,
// deriving the searched kind from the Title itself. The service owns the lean
// existence+kind read (store.ErrNotFound for an unknown Title flows to the handler
// as a 404), so the HTTP layer needs no join-heavy detail fetch just to learn the
// kind.
func (s *Service) SearchTitleCandidates(ctx context.Context, titleID, query string, opts SearchOptions) ([]Candidate, error) {
	t, err := s.store.TitleForEnrichmentByID(titleID)
	if err != nil {
		return nil, err // ErrNotFound flows through
	}
	return s.SearchCandidates(ctx, t.Kind, query, opts)
}

// PreviewTitleExternal resolves a pasted MusicBrainz/TMDB id-or-URL to a single
// candidate for a leaf Title WITHOUT searching — the "paste an id when search isn't
// enough" escape hatch (item-editing/search-improvements). It parses the ref, rejects
// one whose entity kind doesn't match the Title's kind (ErrExternalRefKindMismatch)
// or that it can't read (ErrExternalRefInvalid), then looks the record up BY id so the
// Admin sees its title/artist/year before applying (a typo'd id previews as ErrNoMatch
// / a 404 rather than being pinned blind). Reads only; the apply reuses the existing
// enrichmentOverride endpoint. Unknown Title → store.ErrNotFound.
func (s *Service) PreviewTitleExternal(ctx context.Context, titleID, pastedRef string) (Candidate, error) {
	t, err := s.store.TitleForEnrichmentByID(titleID)
	if err != nil {
		return Candidate{}, err // ErrNotFound flows through
	}
	return s.previewExternal(ctx, t.Kind, pastedRef)
}

// ApplyOverride applies a picked candidate's authoritative external id as a durable
// Enrichment override on a leaf Title and re-enriches just it. It derives the id
// column from the Title's own kind (so the caller passes only the picked id), then
// reuses MatchTitle. Like SearchTitleCandidates it owns the lean kind read, so the
// HTTP layer needs no separate detail fetch to map the id. store.ErrNotFound for an
// unknown Title flows to the handler as a 404. Identity/watch state are untouched.
//
// Admin-facing, like MatchTitle: the record is the Title's OWN choice. The Cascade
// uses applyOverride with store.OriginCascaded (ADR-0046).
func (s *Service) ApplyOverride(ctx context.Context, titleID, externalID string) error {
	return s.applyOverride(ctx, titleID, externalID, store.OriginChosen)
}

// applyOverride is ApplyOverride with the record's origin spelled out.
func (s *Service) applyOverride(ctx context.Context, titleID, externalID string, origin store.RecordOrigin) error {
	t, err := s.store.TitleForEnrichmentByID(titleID)
	if err != nil {
		return err // ErrNotFound flows through
	}
	return s.matchTitle(ctx, titleID, ExternalMatchForKind(t.Kind, externalID), origin)
}

// entityKind maps a browse-parent entity type onto the fine search/lookup kind:
// a Show is searched as "show" (TMDB tv), an Artist/Album as "artist"/"album"
// (MusicBrainz). A Season is never Fix-info'd (edited at Show/Episode grain).
func entityKind(entityType string) string {
	switch entityType {
	case store.EntityArtist:
		return "artist"
	case store.EntityAlbum:
		return "album"
	default:
		return "show"
	}
}

// SearchEntityCandidates searches the authoritative provider for a browse-parent
// entity (Show/Artist/Album), deriving the searched kind from the entity type —
// the parent analogue of SearchTitleCandidates (ADR-0019). It reuses SearchCandidates
// (enablement-gated, capped); a disabled/unreachable provider surfaces
// ErrSearchUnavailable so the Edit-item box reports why. Reads only.
func (s *Service) SearchEntityCandidates(ctx context.Context, entityType, entityID, query string, opts SearchOptions) ([]Candidate, error) {
	return s.SearchCandidates(ctx, entityKind(entityType), query, opts)
}

// PreviewEntityExternal is the browse-parent analogue of PreviewTitleExternal: it
// resolves a pasted id-or-URL to a single candidate for a Show/Artist/Album, deriving
// the lookup kind from the entity type and validating the pasted ref's kind against it
// (item-editing/search-improvements). Reads only.
func (s *Service) PreviewEntityExternal(ctx context.Context, entityType, entityID, pastedRef string) (Candidate, error) {
	return s.previewExternal(ctx, entityKind(entityType), pastedRef)
}

// PreviewExternalForKind resolves a pasted MusicBrainz/TMDB id-or-URL for a bare
// entity KIND rather than an existing item — the Unmatched-file case, where no
// Title exists yet to derive the kind from, so the caller (which knows the
// Library's media kind) supplies it. Same parse/lookup/error contract as
// PreviewTitleExternal; reads only.
func (s *Service) PreviewExternalForKind(ctx context.Context, kind, pastedRef string) (Candidate, error) {
	return s.previewExternal(ctx, kind, pastedRef)
}

// previewExternal is the shared core of the paste-an-id escape hatch: parse + kind-
// validate the pasted ref for the item kind, then Lookup BY the id (enablement-gated,
// like SearchCandidates) and shape the record into a preview Candidate. A blank/
// unreadable paste is ErrExternalRefInvalid, a wrong-kind URL ErrExternalRefKindMismatch,
// a disabled/unconfigured provider ErrSearchUnavailable, and an unknown id ErrNoMatch
// (so a stale id previews as "not found" instead of hanging or 500ing).
func (s *Service) previewExternal(ctx context.Context, kind, pastedRef string) (Candidate, error) {
	externalID, err := externalIDForKind(kind, pastedRef)
	// A MusicBrainz /release/ URL isn't itself an album pin, but it names an edition of
	// a release-group — resolve it to that release-group (the album) rather than
	// rejecting it as an unsupported entity kind.
	var releaseMBID string
	if err != nil {
		if kind == "album" {
			if relID, ok := parseMusicBrainzReleaseRef(pastedRef); ok {
				releaseMBID, err = relID, nil
			}
		}
		if err != nil {
			return Candidate{}, err
		}
	}
	snap := s.snapshot()
	if !snap.enablement.enabledFor(kind) {
		return Candidate{}, ErrSearchUnavailable
	}
	// A video leaf/parent is corrected by re-pointing at its SHOW/MOVIE record, so an
	// episode/season previews the show its pasted tv id names (the by-id Lookup for
	// those kinds needs season/episode numbers a bare paste lacks; the show record is
	// the meaningful preview and matches how the override applies for TV).
	lookupKind := kind
	switch kind {
	case "season", "episode":
		lookupKind = "show"
	}
	ref := refWithPinnedEntityID(TitleRef{Kind: lookupKind}, externalID)
	if releaseMBID != "" {
		ref.ReleaseMBID = releaseMBID // resolve release → parent release-group in Lookup
	}
	meta, err := snap.provider.Lookup(ctx, ref)
	switch {
	case errors.Is(err, ErrNoMatch), err == nil && !meta.Matched:
		return Candidate{}, ErrNoMatch
	case err != nil:
		return Candidate{}, err
	}
	c := Candidate{ExternalID: externalID, Title: meta.Name, Year: meta.Year, Kind: kind}
	if meta.ExternalID != "" {
		c.ExternalID = meta.ExternalID
	}
	// The pasted EDITION rides back with the release-group it resolved to (ADR-0052).
	// This is the one moment a human names a release, and the preview→apply round trip
	// is where it used to be dropped: the preview answered with the parent group, the
	// client applied that, and the release existed nowhere. A pasted /release-group/
	// URL leaves this empty, which is what CLEARS a previously chosen edition.
	c.ReleaseID = releaseMBID
	if c.Year == 0 && len(meta.ReleaseDate) >= 4 {
		if y, err := strconv.Atoi(meta.ReleaseDate[:4]); err == nil && y > 0 {
			c.Year = y
		}
	}
	for _, a := range meta.Artwork {
		if a.Role == "cover" || a.Role == "poster" {
			c.ThumbnailURL = a.URL
			break
		}
	}
	return c, nil
}

// externalIDForKind parses a pasted id-or-URL into the authoritative external id for
// the item kind, validating that a TYPED URL names an entity of the right kind. Music
// kinds (artist/album/track) accept a MusicBrainz UUID/URL; video kinds a TMDB numeric
// id/URL (a movie url on a movie, a tv url on a show/season/episode). A bare id carries
// no kind, so it is trusted for the item's kind. Unreadable → ErrExternalRefInvalid;
// wrong entity kind → ErrExternalRefKindMismatch.
func externalIDForKind(kind, pasted string) (string, error) {
	switch kind {
	case "artist", "album", "track":
		refKind, id, ok := ParseMusicBrainzRef(pasted)
		if !ok {
			// A recognized MusicBrainz link of the wrong entity type (a /work/ or
			// /release/ URL) gets a distinct error so the Admin is told what to paste
			// instead, rather than the misleading "that's not a URL".
			if MusicBrainzRefUnsupported(pasted) {
				return "", ErrExternalRefUnsupportedKind
			}
			return "", ErrExternalRefInvalid
		}
		if refKind != "" && refKind != kind {
			return "", &ExternalRefKindMismatchError{Got: refKind, Want: kind}
		}
		return id, nil
	default: // movie/show/season/episode → TMDB
		urlKind, id, ok := parseTMDBRef(pasted)
		if !ok {
			return "", ErrExternalRefInvalid
		}
		if urlKind != "" {
			wantTV := kind != "movie"
			if (urlKind == "tv") != wantTV {
				got := "movie"
				if urlKind == "tv" {
					got = "show"
				}
				return "", &ExternalRefKindMismatchError{Got: got, Want: kind}
			}
		}
		return id, nil
	}
}

// EntityPin is the correction an Admin applied to a browse parent: the record they
// picked, and — for an Album — the exact EDITION of it they named (ADR-0052).
//
// A struct rather than a second string parameter because the two are same-typed
// MusicBrainz ids one level apart, and because it makes the clearing rule
// unmissable at every call site: ReleaseID is ALWAYS decided, and deciding it is
// empty is what a picked search candidate, a pasted /release-group/ URL, and a
// Cascade all mean.
type EntityPin struct {
	ExternalID string
	// ReleaseID is the release (one edition) the Admin named, when they named one —
	// a pasted /release/ URL, which resolves to its parent release-group for
	// ExternalID and keeps the release here. Empty CLEARS any edition the parent had.
	ReleaseID string
}

// ApplyEntityOverride pins a picked candidate's authoritative external id on a
// browse-parent entity as a durable Enrichment override and re-enriches just that
// parent (Fix-info on a Show/Artist/Album, ADR-0019). It persists the pin
// (SetEntityExternalMatch — external_id + external_id_origin + external_release_id,
// so future passes look up BY it and decorate from the edition the Admin named)
// then runs the single-parent enrich path honoring the parent's Locked fields.
// Identity and watch state are untouched — an edition is a decoration refinement and
// never enters a key (ADR-0038/0052). store.ErrNotFound for an unknown parent flows
// to the handler as a 404.
//
// Admin-facing: the record is this parent's OWN choice. The Artist→Albums
// recursion writes its Albums through applyEntityOverride with
// store.OriginCascaded instead, so a second Artist Cascade re-applies to them
// rather than reading its own last pin as the Album's correction (ADR-0046).
func (s *Service) ApplyEntityOverride(ctx context.Context, entityType, entityID string, pin EntityPin) error {
	return s.applyEntityOverride(ctx, entityType, entityID, pin, store.OriginChosen)
}

// applyEntityOverride is ApplyEntityOverride with the record's origin spelled out.
func (s *Service) applyEntityOverride(ctx context.Context, entityType, entityID string,
	pin EntityPin, origin store.RecordOrigin) error {

	libraryID, err := s.store.LibraryOfEntity(entityType, entityID)
	if err != nil {
		return err // ErrNotFound flows through
	}
	if err := s.store.SetEntityExternalMatch(entityType, entityID, store.EntityRecordPin{
		ExternalID: pin.ExternalID, ReleaseID: pin.ReleaseID, Origin: origin,
	}); err != nil {
		return err
	}
	// Serialize against a concurrent full pass over the same Library so the single-
	// parent re-enrich never races it (per-Library lock, as MatchTitle does).
	lock := s.libLock(libraryID)
	lock.Lock()
	defer lock.Unlock()

	snap, err := s.snapshotFor(ctx, libraryID)
	if err != nil {
		return err
	}
	ref := refWithPinnedEntityID(TitleRef{Kind: entityKind(entityType)}, pin.ExternalID)
	_, err = s.enrichParent(ctx, snap, ModeFull, entityType, entityID, ref)
	return err
}

// ArtworkCandidateLimit caps a provider image-candidate list so the Edit-item
// image picker stays usable when a popular Movie/Show has dozens of posters.
const ArtworkCandidateLimit = 24

// ArtworkCandidates lists the provider images offered for a role on the record
// ref points at, capped and enablement-gated — the shared core of the leaf +
// parent image pickers (Fix label, ADR-0019). It short-circuits with
// ErrSearchUnavailable when the kind's enrichment is off (an unconfigured provider
// — the box reports why instead of hanging). A record with no images for the role
// is (nil, nil); an unreachable provider surfaces its error to the handler. Reads
// only — picking an image is a separate, explicit write.
func (s *Service) ArtworkCandidates(ctx context.Context, ref TitleRef, role string) ([]ArtworkCandidate, error) {
	snap := s.snapshot()
	if !snap.enablement.enabledFor(ref.Kind) {
		return nil, ErrSearchUnavailable
	}
	cands, err := snap.provider.ArtworkCandidates(ctx, ref, role)
	if err != nil {
		return nil, err
	}
	if len(cands) > ArtworkCandidateLimit {
		cands = cands[:ArtworkCandidateLimit]
	}
	return cands, nil
}

// ListTitleArtworkCandidates lists the provider images offered for a leaf Title's
// role, deriving the lookup ref (kind + pinned external id) from the Title itself.
// store.ErrNotFound for an unknown Title flows to the handler as a 404. Reads only.
//
// It takes refFor plain — WITHOUT ADR-0050's album tier that the re-enrich path
// gets (singleLeafWork). That is deliberate and it is not a gap: an image set is
// release-group keyed at the Cover Art Archive, so a Track's ref lists no images at
// any id it could be given. Resolving the Track's Album to fill in a recording MBID
// would spend a store read and a provider call to hand the picker the same empty
// list. A Track's cover is its Album's, and the Album's own picker is what lists it.
func (s *Service) ListTitleArtworkCandidates(ctx context.Context, titleID, role string) ([]ArtworkCandidate, error) {
	t, err := s.store.TitleForEnrichmentByID(titleID)
	if err != nil {
		return nil, err // ErrNotFound flows through
	}
	key := titleCandidateKey(titleID, role)
	if cached, ok := s.candidates.get(key); ok {
		return cached, nil
	}
	cands, err := s.ArtworkCandidates(ctx, refFor(t), role)
	if err != nil {
		return nil, err // never cache an error/unavailable outcome
	}
	s.candidates.put(key, cands)
	return cands, nil
}

// ListEntityArtworkCandidates lists the provider images offered for a browse
// parent's role, deriving the lookup ref from the parent's pinned/resolved external
// id (a role has no candidates until the parent has an authoritative record). Reads
// only.
func (s *Service) ListEntityArtworkCandidates(ctx context.Context, entityType, entityID, role string) ([]ArtworkCandidate, error) {
	cur, err := s.store.EntityEnrichmentByID(entityType, entityID)
	if err != nil {
		return nil, err
	}
	key := entityCandidateKey(entityType, entityID, role)
	if cached, ok := s.candidates.get(key); ok {
		return cached, nil
	}
	ref := refWithPinnedEntityID(TitleRef{Kind: entityKind(entityType)}, cur.ExternalID)
	cands, err := s.ArtworkCandidates(ctx, ref, role)
	if err != nil {
		return nil, err // never cache an error/unavailable outcome
	}
	s.candidates.put(key, cands)
	return cands, nil
}

// PickTitleArtwork downloads a chosen provider image into the artwork cache and
// applies it to a leaf Title's role, Locking that role (Fix label image picker,
// ADR-0019). A later enrich pass then keeps the hand-picked image (the role is
// Locked); a LOCAL image for the role still wins at serve time. A failed download
// is an error the handler surfaces. Identity/watch state are untouched.
func (s *Service) PickTitleArtwork(ctx context.Context, titleID, role, imageURL string) error {
	if _, err := s.store.TitleForEnrichmentByID(titleID); err != nil {
		return err // ErrNotFound flows through
	}
	path, ok := s.cacheArtwork(ctx, titleID, ArtworkRef{Role: role, URL: imageURL})
	if !ok {
		return fmt.Errorf("enrich: downloading picked artwork for %q", role)
	}
	if err := s.store.PickTitleArtwork(titleID, role, path, uuid.NewString()); err != nil {
		return err
	}
	s.candidates.invalidate(titleCandidateKey(titleID, role))
	return nil
}

// PickEntityArtwork is PickTitleArtwork for a browse parent (Show/Artist/Album):
// download the chosen image, apply it to the parent's role, and Lock the role. A
// local parent image still wins at serve time. Identity/watch state untouched.
func (s *Service) PickEntityArtwork(ctx context.Context, entityType, entityID, role, imageURL string) error {
	path, ok := s.cacheArtwork(ctx, entityType+"-"+entityID, ArtworkRef{Role: role, URL: imageURL})
	if !ok {
		return fmt.Errorf("enrich: downloading picked artwork for %q", role)
	}
	if err := s.store.PickEntityArtwork(entityType, entityID, role, path, uuid.NewString()); err != nil {
		return err
	}
	s.candidates.invalidate(entityCandidateKey(entityType, entityID, role))
	return nil
}

// UploadTitleArtwork writes Admin-supplied image bytes into the artwork cache and
// applies them to a leaf Title's role, Locking that role (ADR-0026, upload-is-
// select). Unlike PickTitleArtwork the ArtworkFetcher is bypassed — the bytes are
// already in hand — so this works offline. The file is named source-qualified
// (…-uploaded.ext) so it never collides with the fetched cache file for the role;
// a stale prior upload with a different extension is removed. contentType is the
// caller-validated image type (JPEG/PNG/WebP), used to pick the extension.
func (s *Service) UploadTitleArtwork(titleID, role string, data []byte, contentType string) error {
	if _, err := s.store.TitleForEnrichmentByID(titleID); err != nil {
		return err // ErrNotFound flows through
	}
	path, err := s.writeUploadedArtwork(titleID, role, data, contentType)
	if err != nil {
		return err
	}
	replaced, err := s.store.UploadTitleArtwork(titleID, role, path, uuid.NewString())
	if err != nil {
		return err
	}
	s.removeReplacedUpload(replaced, path)
	s.candidates.invalidate(titleCandidateKey(titleID, role))
	return nil
}

// UploadEntityArtwork is UploadTitleArtwork for a browse parent (Show/Artist/
// Album): write the supplied bytes and apply them to the parent's role, Locking
// it. The handler has already confirmed the parent exists.
func (s *Service) UploadEntityArtwork(entityType, entityID, role string, data []byte, contentType string) error {
	path, err := s.writeUploadedArtwork(entityType+"-"+entityID, role, data, contentType)
	if err != nil {
		return err
	}
	replaced, err := s.store.UploadEntityArtwork(entityType, entityID, role, path, uuid.NewString())
	if err != nil {
		return err
	}
	s.removeReplacedUpload(replaced, path)
	s.candidates.invalidate(entityCandidateKey(entityType, entityID, role))
	return nil
}

// writeUploadedArtwork writes uploaded bytes to the artwork cache under a
// source-qualified name (key-role-uploaded.ext), returning the cache-relative name
// (the file lives directly under cacheDir) so the stored DB path survives a
// data-dir move. The "-uploaded" qualifier keeps it distinct from cacheArtwork's
// fetched file (key-role.ext) for the same role, so an upload and a fetch coexist
// on disk.
func (s *Service) writeUploadedArtwork(key, role string, data []byte, contentType string) (string, error) {
	name := key + "-" + role + "-uploaded" + extensionFor(contentType)
	if err := os.WriteFile(filepath.Join(s.cacheDir, name), data, 0o644); err != nil {
		return "", fmt.Errorf("enrich: writing uploaded artwork %q: %w", role, err)
	}
	return name, nil
}

// removeReplacedUpload deletes the file of a prior upload that a re-upload
// replaced, but only when the new upload landed at a different path (a changed
// extension) — a same-path re-upload overwrote it in place. The stored paths are
// cache-relative, so re-root each onto cacheDir before touching disk (a legacy
// absolute path is left as-is by cacheAbs). Best-effort: a dangling cache file is
// harmless (the DB row points at the new one).
func (s *Service) removeReplacedUpload(replacedPath, newPath string) {
	if replacedPath == "" || replacedPath == newPath {
		return
	}
	if err := os.Remove(s.cacheAbs(replacedPath)); err != nil && !os.IsNotExist(err) {
		log.Printf("obelo: enrich artwork: removing replaced upload %q: %v", replacedPath, err)
	}
}

// cacheAbs re-roots a cache-relative artwork path onto cacheDir for a filesystem
// op. An already-absolute path (a legacy pre-relativization row) is returned
// unchanged. Mirrors catalog.Service.ResolveArtworkPath on the serve side.
func (s *Service) cacheAbs(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(s.cacheDir, p)
}

// leafWork is one playable Title to enrich plus its provider lookup ref. For a
// Track sparseTitle is true: the canonical title is applied only when the parsed
// (tag-derived) title is empty, since tags are the Music identity authority
// (ADR-0002). For a Movie/Episode the parsed/canonical title flows through.
type leafWork struct {
	title       store.Title
	ref         TitleRef
	sparseTitle bool
	// tracklist is what the Track's ALBUM was able to say about its own contents
	// (ADR-0050). It is stamped here, at collection time, because that is the only
	// moment it is known: an Album that never matched and an Album whose tracklist
	// declined this Track both leave ref.MusicbrainzID empty, so by the time the
	// lookup comes back the two are indistinguishable — and they want opposite
	// actions from the Admin. Zero (tracklistUnavailable) for every non-Music leaf,
	// which is correct: no Album had anything to say about a Movie.
	tracklist tracklistOutcome
}

// unmatchedReason is ADR-0050's reason table, evaluated for a leaf the provider
// declined to match. err is the lookup's error (nil when the provider answered
// "no record" without one, which is an empty answer and not a rejection).
//
// The order is the classification, and each step is checked before the next
// because a later one would otherwise absorb it:
//
//  1. A NON-EMPTY ref id means an exact id was available and the provider had no
//     recording behind it. It never reached the search, so nothing further down
//     can be true of it. This is the file's tag id (the common case, ADR-0049), a
//     stored record id that has since gone away, and a tracklist-supplied id the
//     provider then declined — one bucket because the action is one action: the id
//     is the broken part.
//
//  2. The ALBUM tier outranks the search outcome, and that is a deliberate choice
//     rather than an accident of ordering. A Track under an unmatched Album goes
//     to the search too, and that search fails or gets rejected like any other —
//     but "fix the Album" is the action that clears it, and in a real library this
//     is 365 of 730 rows. Letting `search-rejected` win here would rename the
//     largest actionable bucket in the queue after the least useful thing that
//     happened to it last.
//
//  3. Only then the search's own two answers, rejection FIRST: ErrMatchRejected
//     wraps ErrNoMatch one-directionally (issue 05), so testing the plain
//     ErrNoMatch first would silently collapse the two into one reason.
//
// A non-Music leaf gets EnrichmentReasonNone and the generic sentence. The five
// values are Music-shaped by construction — four of them name an Album, a
// tracklist or a recording id — and stamping `search-no-match` on an unmatched
// Movie would put the word "recording" in front of an Admin looking at a film. A
// Movie's own failure taxonomy is a different question with different answers, and
// the empty reason leaves today's copy exactly where it is until someone asks it.
func (lw leafWork) unmatchedReason(err error) string {
	if lw.title.Kind != "track" {
		return store.EnrichmentReasonNone
	}
	if strings.TrimSpace(lw.ref.MusicbrainzID) != "" {
		return store.EnrichmentReasonTagIDUnresolved
	}
	switch lw.tracklist {
	case tracklistNoAlbumRecord:
		return store.EnrichmentReasonAlbumUnmatched
	case tracklistRead:
		return store.EnrichmentReasonNotInTracklist
	}
	if errors.Is(err, ErrMatchRejected) {
		return store.EnrichmentReasonSearchRejected
	}
	return store.EnrichmentReasonSearchNoMatch
}

// processLeaf enriches one leaf Title into res using the caller-resolved snapshot
// (the Library's effective provider + enablement), honoring Locked fields and the
// graceful-degradation rules (disabled → no call; no-match → unmatched; provider
// error → failed, pass continues). Identity is never touched (ADR-0002).
func (s *Service) processLeaf(ctx context.Context, snap providerSnapshot, lw leafWork, res *Result) error {
	t := lw.title
	if !snap.enablement.enabledFor(t.Kind) {
		// 'disabled' is a settled outcome with nothing to diagnose — nobody asked a
		// provider anything — so it writes the EMPTY reason, which also wipes whatever
		// a previous failure said. Leaving a stale sentence on a row the operator has
		// since switched enrichment off for is the exact stale-reason failure ADR-0050
		// calls worse than none.
		if err := s.store.SetTitleEnrichmentStatus(t.ID, "disabled", store.EnrichmentReasonNone); err != nil {
			return err
		}
		res.Disabled++
		return nil
	}

	locks, err := s.store.LockedFields(t.ID)
	if err != nil {
		return err
	}

	// Per-item Enrichment-override precedence (issue 06, ADR-0027): a Title pinned to
	// a specific provider's record keeps resolving via THAT provider — most-specific-
	// wins — even when the Library's Authoritative provider changed underneath it, as
	// long as the provider stays reachable. If a policy change made that provider
	// unreachable, the override is ORPHANED: file the Title to the attention list
	// (status 'unmatched') rather than silently resolving it against the wrong leader
	// or dropping the pin. Only engages when the pin's provider differs from the
	// current leader; when they match (the common case), the chain already resolves by
	// the pinned id, so the pass is unchanged. The pinned id column is never touched —
	// the override survives to be re-applied once its provider is reachable again.
	provider := snap.provider
	if pinSlug, pinned := pinnedProviderFor(t); pinned {
		if leader := snap.config.authoritativeSlugFor(t.Kind); pinSlug != leader {
			if !snap.config.providerReachable(pinSlug) {
				res.Unmatched++
				// An ORPHANED override is a policy problem, not one of ADR-0050's five
				// diagnoses: nothing was asked and nothing was learned about the item. The
				// empty reason renders the generic sentence and, just as importantly,
				// clears any diagnosis from before the provider went unreachable.
				return s.store.SetTitleEnrichmentStatus(t.ID, "unmatched", store.EnrichmentReasonNone)
			}
			// Reachable but no longer the leader: resolve via the pinned provider alone
			// so the override still wins (the chain leads a different source that can't
			// answer this record's id).
			if p := snap.config.newVideoProvider(pinSlug); p != nil {
				provider = p
			}
		}
	}

	meta, err := provider.Lookup(ctx, lw.ref)
	switch {
	case errors.Is(err, ErrNoMatch), err == nil && !meta.Matched:
		res.Unmatched++
		return s.store.SetTitleEnrichmentStatus(t.ID, "unmatched", lw.unmatchedReason(err))
	case err != nil:
		// Non-fatal: log + record the failure, keep going (story 36). Whether that
		// failure parks the Title or schedules a retry is the classification in
		// recordLeafFailure (ADR-0048).
		return s.recordLeafFailure(t, err, res)
	}

	// A canonical display title applies to an Episode always; to a Track only when
	// the tag title was sparse (tags win, ADR-0002). A Movie's display title is its
	// identity (parsed/fixed) title, never the provider's — meta.Name exists only
	// for by-id identity resolution, so drop it here. The "title" lock is honored
	// inside WriteTitleEnrichment.
	name := meta.Name
	if t.Kind == "movie" || (lw.sparseTitle && strings.TrimSpace(t.Title) != "") {
		name = ""
	}

	var fetched []store.Artwork
	for _, ar := range meta.Artwork {
		if locks[ar.Role] {
			continue // a hand-chosen image for this role is locked
		}
		path, ok := s.cacheArtwork(ctx, t.ID, ar)
		if !ok {
			continue
		}
		fetched = append(fetched, store.Artwork{
			ID: uuid.NewString(), Role: ar.Role, Path: path, Source: "fetched",
		})
	}

	// Cast headshots (cast-photos/01): download each cast member's photo into the
	// artwork cache and record it as a `person` row, keyed by the person ref so one
	// actor's photo is cached once across every Title (dedupe). A locked cast is not
	// refetched (its credits + their absent photos are preserved by the store), so
	// this is skipped entirely — mirroring how a locked artwork role is skipped above.
	if !locks["cast"] {
		s.fetchCastHeadshots(ctx, meta.Cast)
	}

	if err := s.store.WriteTitleEnrichment(t.ID, store.TitleEnrichment{
		Overview:       meta.Overview,
		Tagline:        meta.Tagline,
		ContentRating:  meta.ContentRating,
		ReleaseDate:    meta.ReleaseDate,
		RuntimeMinutes: meta.RuntimeMinutes,
		Studio:         meta.Studio,
		Source:         meta.Source,
		Name:           name,
		Genres:         meta.Genres,
		Cast:           toStoreCredits(meta.Cast),
		Artwork:        fetched,
		// Persist the resolved record id so the live artwork-candidate lookup has
		// an anchor (refFor) — a search-matched Movie/Track otherwise offers no
		// images in the Edit-item tabs. Fill-only in the store: an id already
		// pinned by a {tmdb-…} token or a Fix-info override is never rewritten by
		// a provider response. An Episode lookup reports no ExternalID (its
		// anchor is the Show id, pinned separately in collectTVLeaves).
		ExternalIDs: ExternalMatchForKind(t.Kind, meta.ExternalID),
	}, locks); err != nil {
		return err
	}
	res.Matched++
	return nil
}

// collectTVLeaves walks a TV Library's Shows → Seasons → Episodes: it enriches
// the Show and Season parents (the generic entity tables) and returns the Episode
// leaves to enrich in phase B. The resolved Show external id is threaded down to
// the Season/Episode refs so they resolve under the right show. In ModeNew only
// pending parents/episodes are touched; ModeRecheck adds the settled non-answers
// (ADR-0051); ModeFull re-does all.
func (s *Service) collectTVLeaves(ctx context.Context, snap providerSnapshot, libraryID string, mode Mode) ([]leafWork, error) {
	shows, err := s.store.ListAllShows(libraryID)
	if err != nil {
		return nil, err
	}
	var leaves []leafWork
	for _, sh := range shows {
		showExtID := sh.TMDBID // embedded {tmdb-…} fallback
		extID, err := s.enrichParent(ctx, snap, mode, store.EntityShow, sh.ID,
			TitleRef{Kind: "show", Title: sh.Title, Year: sh.Year, TMDBID: sh.TMDBID})
		if err != nil {
			return nil, err
		}
		if extID != "" {
			showExtID = extID
		}

		seasons, err := s.store.SeasonsForShow(sh.ID)
		if err != nil {
			return nil, err
		}
		for _, se := range seasons {
			if _, err := s.enrichParent(ctx, snap, mode, store.EntitySeason, se.ID,
				TitleRef{Kind: "season", TMDBID: showExtID, SeasonNumber: se.SeasonNumber}); err != nil {
				return nil, err
			}
			eps, err := s.store.EpisodesForSeason(se.ID)
			if err != nil {
				return nil, err
			}
			for _, ep := range eps {
				if !s.shouldProcessLeaf(snap, mode, ep) {
					continue
				}
				// Episode durability (ADR-0019, closing the gap deferred from slice 01):
				// an Episode's OWN record wins over the one derived from its Show, so a
				// pinned episode survives a full pass instead of being re-derived. ep.TMDBID
				// is the record (enrichment_tmdb_id first, ADR-0045), which is the right
				// question here — WHICH record decorates this leaf — and deliberately not
				// "did the Admin choose it": a co-File sibling inheriting a split's series
				// and a cleared pin's series-write are both legitimate anchors that carry no
				// lock. (The skip rule in cascade.go asks the other question and reads
				// EnrichmentIDOrigin; do not confuse the two.) An Episode with no record of
				// its own resolves under the Show's resolved id.
				epShowID := showExtID
				if ep.TMDBID != "" {
					epShowID = ep.TMDBID
				}
				ref := TitleRef{
					Kind: "episode", Title: ep.Title, TMDBID: epShowID,
					SeasonNumber: ep.SeasonNumber, EpisodeNumber: ep.EpisodeNumber, EpisodeLabel: ep.EpisodeLabel,
				}
				// The Episode pin, honored here as well as in refFor. A library pass
				// collects its own leaves, so without this the pass would quietly look a
				// repointed Slot up by the numbers it was pinned AWAY from — and a pass is
				// exactly what runs after a matcher Apply, which is where the pin is now set.
				leaves = append(leaves, leafWork{title: ep, ref: withEpisodePin(ref, ep)})
			}
		}
	}
	return leaves, nil
}

// collectMusicLeaves walks a Music Library's Artists → Albums → Tracks: it
// enriches the Artist and Album parents and returns the Track leaves. Tracks
// carry sparseTitle so a canonical title only fills a missing tag title.
//
// It is also where ADR-0050's tracklist tier lives, because that tier is
// ALBUM-grained: "the one local Track and the one tracklist position both still
// unclaimed are each other" cannot be decided one Track at a time. The Album's
// resolved record id — which enrichParent has always returned and this call site
// used to discard — is the anchor the whole tier hangs from.
func (s *Service) collectMusicLeaves(ctx context.Context, snap providerSnapshot, libraryID string, mode Mode) ([]leafWork, error) {
	artists, err := s.store.ListAllArtists(libraryID)
	if err != nil {
		return nil, err
	}
	var leaves []leafWork
	for _, ar := range artists {
		// The Albums are read BEFORE the Artist is enriched, not after, so a few of
		// them can ride the Artist's ref as corroboration (ADR-0053). The order used to
		// be the other way round, which is why the Artist could only ever be resolved
		// by its name — and a name is exactly what cannot tell the American "Eagles"
		// from the 1958 British group literally named "The Eagles".
		albums, err := s.store.AlbumsForArtist(ar.ID)
		if err != nil {
			return nil, err
		}
		if _, err := s.enrichParent(ctx, snap, mode, store.EntityArtist, ar.ID,
			TitleRef{Kind: "artist", Title: ar.Name, Artist: ar.Name,
				MusicbrainzID: ar.MusicbrainzID,
				AlbumHints:    musicAlbumHints(albums)}); err != nil {
			return nil, err
		}
		for _, al := range albums {
			albumRecordID, err := s.enrichParent(ctx, snap, mode, store.EntityAlbum, al.ID,
				TitleRef{Kind: "album", Title: al.Title, Album: al.Title, Year: al.Year, Artist: ar.Name,
					MusicbrainzID: al.MusicbrainzID})
			if err != nil {
				return nil, err
			}
			tracks, err := s.store.TracksForAlbum(al.ID)
			if err != nil {
				return nil, err
			}
			// Computed once for the whole Album, over its WHOLE local track list, and
			// empty whenever no call was worth making (or the read failed).
			fromTracklist, outcome := s.albumTracklistIDs(ctx, snap, mode, al, albumRecordID, tracks)
			for _, tr := range tracks {
				if !s.shouldProcessLeaf(snap, mode, tr) {
					continue
				}
				leaves = append(leaves, leafWork{title: tr, sparseTitle: true, tracklist: outcome, ref: TitleRef{
					Kind: "track", Title: tr.Title, Track: tr.Title,
					Artist: ar.Name, Album: al.Title,
					MusicbrainzID: trackAnchorID(tr, fromTracklist[tr.ID]),
				}})
			}
		}
	}
	return leaves, nil
}

// maxAlbumHints caps how many Albums travel on an Artist's ref as corroboration.
// Three is plenty: the provider makes exactly ONE corroborating call, and the
// hints exist so it can pick the best evidence available — an album the files
// identify by id, else an album with a title worth searching for — not so it can
// try them one after another. Carrying the whole discography would put an
// unbounded slice on a ref for no gain.
const maxAlbumHints = 3

// musicAlbumHints selects the Albums an Artist is corroborated by (ADR-0053).
//
// Two rules, and both are about asking the SAME question twice rather than about
// which album is nicest. Albums that carry a release-group MBID from their tags
// come first, because the provider can resolve one of those with a lookup and no
// search at all. Within each group the store's order is preserved — AlbumsForArtist
// orders by year, then sort title, then id — so a second pass hints the same albums
// in the same order, asks MusicBrainz the same question, and gets the answer from a
// cache rather than from the search cluster.
//
// An album with neither an id nor a title cannot corroborate anything and is
// dropped; an artist whose albums are all like that hints nothing, and its Lookup
// is exactly what it was before this existed.
func musicAlbumHints(albums []store.Album) []AlbumHint {
	hints := make([]AlbumHint, 0, maxAlbumHints)
	for _, al := range albums {
		if id := strings.TrimSpace(al.MusicbrainzID); id != "" {
			hints = append(hints, AlbumHint{Title: al.Title, ReleaseGroupMBID: id})
			if len(hints) == maxAlbumHints {
				return hints
			}
		}
	}
	for _, al := range albums {
		if strings.TrimSpace(al.MusicbrainzID) != "" || strings.TrimSpace(al.Title) == "" {
			continue
		}
		hints = append(hints, AlbumHint{Title: al.Title})
		if len(hints) == maxAlbumHints {
			return hints
		}
	}
	if len(hints) == 0 {
		return nil
	}
	return hints
}

// trackRecordID answers "which MusicBrainz recording should decorate this Track?"
// in precedence order (ADR-0049):
//
//  1. MusicbrainzID — the enrichment RECORD. Either the Admin's Fix-info choice or
//     an id a previous pass resolved and stored. A human's correction outranks
//     anything a file claims, and a resolved id spares a second search.
//  2. MusicbrainzRecordingID — what the FILE's tags assert. Exact, free, and
//     re-derived from disk each scan.
//  3. "" — neither; the provider falls back to a name+artist search.
//
// The distinction matters more than it looks. Tier 3 is `/ws/2/recording?query=`,
// MusicBrainz's SEARCH service — a separate cluster that sheds load globally under
// pressure and answers 503 while the lookup endpoints are perfectly healthy. For a
// tagged library, tiers 1 and 2 remove that dependency almost entirely.
func trackRecordID(t store.Title) string {
	if id := strings.TrimSpace(t.MusicbrainzID); id != "" {
		return id
	}
	return strings.TrimSpace(t.MusicbrainzRecordingID)
}

// trackAnchorID is trackRecordID with ADR-0050's tier spliced in between the tag
// and the search: record → tag → ALBUM TRACKLIST → search. fromTracklist is what
// the Album's own tracklist says this Track is, or "" when the tracklist declined
// it, was not fetched, or claimed it with no recording id behind it.
//
// The order is the whole point and it is the same order ADR-0049 gave: a human's
// correction outranks what the file asserts, which outranks what the Album asserts
// about its contents, which outranks a text search. The tracklist's product is an
// ID and nothing more — the leaf still resolves through the ordinary
// /recording/<mbid> lookup, so exactly one thing turns an id into a record.
func trackAnchorID(t store.Title, fromTracklist string) string {
	if id := trackRecordID(t); id != "" {
		return id
	}
	return strings.TrimSpace(fromTracklist)
}

// albumTracklistIDs is ADR-0050's tier AS A LIBRARY PASS ASKS FOR IT: the shared
// tier below (albumTrackAnchors), behind the one question only a pass can answer —
// whether any of this Album's IN-SCOPE Tracks still needs an anchor.
//
// That gate is the pass's alone, which is why it is here and not in the shared
// function. A pass walks whole albums under a Mode, so it can be looking at an
// album none of whose Tracks it will even process; the single-Title path is looking
// at one Track it already knows needs an anchor.
func (s *Service) albumTracklistIDs(ctx context.Context, snap providerSnapshot, mode Mode,
	al store.Album, albumRecordID string, tracks []store.Title) (map[string]string, tracklistOutcome) {

	if !s.albumNeedsTracklist(snap, mode, tracks) {
		// Every in-scope Track already has an id, so no Track here can be diagnosed by
		// the album tier: whatever settles them will have been an id that failed. An
		// album of fully tagged Tracks — the population ADR-0049 already fixed — must
		// not start paying a call per album to learn that.
		return nil, tracklistUnavailable
	}
	return s.albumTrackAnchors(ctx, snap, al, albumRecordID, tracks)
}

// albumTrackAnchors is ADR-0050's tier itself, computed once for one Album: the
// recording MBID the Album's own tracklist names for each of its local Tracks,
// keyed by Track (Title) id. An empty map means "this Album has nothing to say",
// and is the answer for every case in which no provider call was made at all.
//
// TWO CALLERS SHARE IT, and that is the point (issue 14): a library pass through
// albumTracklistIDs, and a single-Track re-enrich through trackAlbumAnchor. They
// must not be able to disagree about what a Track's anchor is — the last time this
// path carried a COMMENT asserting that parity instead of a caller, the parity
// quietly stopped being true when this tier was added and refFor was not.
//
// It makes a call only when an anchor exists: the Album's enrichment record id (an
// Admin's Fix-info choice included), else the release-group id the FILES assert. An
// Album that is neither matched nor tagged knows nothing about its contents and is
// not asked.
//
// WHICH EDITION that anchor resolves to is ADR-0052's precedence, applied by
// albumTracklistFor: the release an ADMIN chose (chosenAlbumEdition), then the
// release the FILES name (al.MusicbrainzReleaseID), then best fit by track count.
// The result carries FromChosenEdition — the licence — because the tracklist alone
// cannot say which of the three answered.
//
// tracks must be the Album's WHOLE local list — mapTracks is album-grained, and a
// Track the caller intends to skip (or, on the single-Title path, every Track but
// the one being re-enriched) still occupies its position on the release. The RESULT
// is filtered, never the input.
//
// A failed read is not a leaf failure. ErrNoTracklist is the settled "this album has
// no tracklist"; anything else is a transient failure (a 503 shed, a transport
// error) and must NOT settle the Album's Tracks either. Both return the empty map,
// and the Tracks fall through to the search path exactly as they did before this
// tier existed — where, if the search also fails transiently, recordLeafFailure's
// ADR-0048 classification schedules the retry that brings them back. The tracklist
// itself gets no retry machinery of its own: the next pass re-reads it for free.
// It returns its OUTCOME alongside the map, because that outcome is the only
// place the difference between "the Album never matched" and "the Album matched
// and its tracklist has no room for this Track" survives (ADR-0050). Both leave a
// Track with no anchor and an empty ref id, so nothing downstream can recover it:
// processLeaf would see two identical leaves wanting two different actions from
// the Admin — fix the ALBUM versus fix the Album's RELEASE.
func (s *Service) albumTrackAnchors(ctx context.Context, snap providerSnapshot,
	al store.Album, albumRecordID string, tracks []store.Title) (map[string]string, tracklistOutcome) {

	anchor := strings.TrimSpace(albumRecordID)
	if anchor == "" {
		anchor = strings.TrimSpace(al.MusicbrainzID)
	}
	if anchor == "" {
		return nil, tracklistNoAlbumRecord
	}
	res, err := s.albumTracklistFor(ctx, snap, TracklistRequest{
		ReleaseGroupID:  anchor,
		ReleaseID:       al.MusicbrainzReleaseID,
		LocalTrackCount: len(tracks),
	}, s.chosenAlbumEdition(store.EntityAlbum, al.ID, anchor))
	tl := res.Tracks
	switch {
	case errors.Is(err, ErrNoTracklist):
		// Settled: the provider answered, and the Album has no tracklist to give. It
		// could name none of its contents, which is the Admin's problem with the ALBUM.
		return nil, tracklistNoAlbumRecord
	case err != nil:
		// Transient: say so, and leave the Tracks unsettled. Emphatically NOT
		// tracklistNoAlbumRecord — the album is not unmatched, MusicBrainz was busy,
		// and a reason recorded from an outage is the "confidently explains a problem
		// that no longer exists" failure ADR-0050 exists to prevent.
		log.Printf("obelo: enrich album %q (%s): tracklist unavailable: %v — its tracks fall "+
			"back to search this pass", al.Title, al.ID, err)
		return nil, tracklistUnavailable
	}
	ids := make(map[string]string, len(tracks))
	// res.FromChosenEdition is the licence ADR-0052 grants position-alone mapping,
	// live at exactly the point mapTracks is called and nowhere else. It is the fact
	// that the ADMIN's edition produced THESE entries, not the stored intention: a pin
	// that fell back to tag/fit arrives here false, and rule 4 stays off.
	for titleID, tc := range mapTracks(tracks, tl, res.FromChosenEdition) {
		// A matched entry carrying no recording id is still a match (issue 03) — it
		// holds its position so a neighbour cannot claim it — but it is not an ANCHOR.
		// Writing it into ref.MusicbrainzID would pin the empty string and send the
		// leaf to a lookup of nothing.
		if ext := strings.TrimSpace(tc.ExternalID); ext != "" {
			ids[titleID] = ext
		}
	}
	return ids, tracklistRead
}

// singleLeafWork builds the leafWork for a re-enrich of ONE Title — the
// single-Title path (MatchTitle, PUT /titles/{id}/enrichmentMatch, a Cascade's
// per-child applyOverride) — which is refFor's three tiers plus ADR-0050's fourth,
// the one refFor cannot supply because a Track's Album is not on its row.
//
// For every non-Music leaf, and for every Track that already has a record or a tag
// id, this is exactly refFor and nothing else: trackAlbumAnchor returns before it
// reads anything, and trackAnchorID's first tier answers. A Movie or Episode
// re-enrich is byte-identical on the wire to what it was before this existed.
func (s *Service) singleLeafWork(ctx context.Context, snap providerSnapshot, t store.Title) leafWork {
	fromTracklist, outcome := s.trackAlbumAnchor(ctx, snap, t)
	ref := refFor(t)
	ref.MusicbrainzID = trackAnchorID(t, fromTracklist)
	return leafWork{title: t, ref: ref, sparseTitle: t.Kind == "track", tracklist: outcome}
}

// trackAlbumAnchor is ADR-0050's album tier for ONE Track, re-enriched on its own:
// the recording MBID this Track's Album names for it, and the tier's outcome so the
// reason processLeaf records is the ALBUM-scoped one (`album-unmatched` /
// `not-in-tracklist`) rather than a search reason for a search that never happened.
//
// It goes through the same albumTrackAnchors the pass does, over the Album's WHOLE
// local track list — issue 03's contract, and not negotiable here: the match rule is
// album-grained, so handing it the one Track being re-enriched would free every
// other Track's position and let the leftover rule fire on a hole that is not one.
// The RESULT is filtered to this Track. That is the whole reason this is an
// extraction and not a second implementation: a Track resolved by a pass and the
// same Track resolved alone must reach the same record.
//
// IT RETURNS BEFORE READING ANYTHING in the common case — a Track whose match
// carries an id (an Admin picking a recording, and every child of a Cascade, which
// passes one per child). Making that case fetch a tracklist would multiply one
// album's cascade by its track count, for an id it already holds and would use
// first anyway (trackAnchorID). A non-Track never gets here at all.
//
// Its three reads are the pass's three facts, minus the one a single Track may not
// have: the Album's own enrichment is READ, never re-resolved. A pass may re-look-up
// an Album parent (ModeFull) because a pass is what re-resolves parents; a re-enrich
// of one Track is not licensed to spend a provider call on its Album, still less to
// write the Album's record. So the anchor is the record the Album already has, else
// the release-group id the FILES assert — which is exactly what the pass's own
// anchor is in every mode but Full.
//
// Every read failure yields no anchor and tracklistUnavailable: a bookkeeping read
// must not be able to fail a re-enrich, and an outcome nothing was learned from must
// not diagnose the Track.
func (s *Service) trackAlbumAnchor(ctx context.Context, snap providerSnapshot,
	t store.Title) (string, tracklistOutcome) {

	if t.Kind != "track" || trackRecordID(t) != "" {
		return "", tracklistUnavailable
	}
	if !snap.enablement.enabledFor(t.Kind) {
		// processLeaf is about to write 'disabled' with no diagnosis and no call. Asking
		// the Album anything would be three store reads spent on an answer nobody reads.
		return "", tracklistUnavailable
	}
	tc, err := s.store.TrackContextForTitle(t.ID)
	if err != nil {
		return "", tracklistUnavailable // no album linkage (or an unreadable one)
	}
	al, err := s.store.AlbumByID(tc.AlbumID)
	if err != nil {
		return "", tracklistUnavailable
	}
	tracks, err := s.store.TracksForAlbum(al.ID)
	if err != nil {
		return "", tracklistUnavailable
	}
	ids, outcome := s.albumTrackAnchors(ctx, snap, al, s.albumRecordID(al.ID), tracks)
	return ids[t.ID], outcome
}

// albumRecordID is the enrichment record an Album already holds — the id
// enrichParent would return for a settled parent, read rather than re-resolved.
// "" when the Album has no record or the read fails; the tier then falls back to
// the release-group id the files assert, exactly as the pass does.
func (s *Service) albumRecordID(albumID string) string {
	e, err := s.store.EntityEnrichmentByID(store.EntityAlbum, albumID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(e.ExternalID)
}

// tracklistOutcome is what the Album tier learned about an Album as a whole, kept
// because it is NOT recoverable from a leaf after the fact: an Album that never
// matched and an Album whose tracklist declined a Track both leave that Track with
// an empty ref id (ADR-0050, issue 04). It is carried on leafWork and read only by
// the reason classification.
type tracklistOutcome int

const (
	// tracklistUnavailable — the tier has nothing to say about this Album, and no
	// leaf under it may be diagnosed from it. Three cases share this value because
	// they share that consequence: no in-scope Track needed an anchor, the read
	// failed TRANSIENTLY, or the Library's Music enrichment is off. The transient one
	// is why this value must exist separately from tracklistNoAlbumRecord — a 503 is
	// an outage, not a diagnosis, and its leaves fall through to search where a real
	// diagnosis is still available.
	tracklistUnavailable tracklistOutcome = iota
	// tracklistNoAlbumRecord — the Album could name none of its contents: it has no
	// record and no usable tag id, or the provider answered that it has no tracklist.
	// A Track left unanchored by this is EnrichmentReasonAlbumUnmatched, and the
	// action is on the Album.
	tracklistNoAlbumRecord
	// tracklistRead — a tracklist was fetched and mapped. A Track it left unanchored
	// was DECLINED by the ADR-0050 match rule, which is
	// EnrichmentReasonNotInTracklist: the Album is probably pinned to the wrong
	// release, and that is where the action is.
	tracklistRead
)

// chosenAlbumEdition returns the exact RELEASE an Admin pinned on this parent
// (ADR-0052), or "" when nobody pinned one — and, crucially, "" whenever the pin
// does not belong to the release-group the tracklist is about to be read under.
//
// That guard is what keeps the pin honest without a provider call. An edition is
// only ever stored beside the release-group it was resolved under, so anchor ==
// ExternalID is the normal case; anchor differs exactly when the Album's own record
// was NOT available and the tier fell back to the release-group the FILES assert
// (a transient parent-lookup failure, ADR-0048). Reading the Admin's edition under
// somebody else's release-group is the stranger's-tracklist decoration ADR-0052
// forbids, and it is cheaper to refuse here than to discover it a call later.
//
// A read failure yields "" — the album resolves by tag/fit exactly as it does with
// no pin at all. A bookkeeping read must not be able to fail a pass.
func (s *Service) chosenAlbumEdition(entityType, entityID, anchor string) string {
	e, err := s.store.EntityEnrichmentByID(entityType, entityID)
	if err != nil {
		return ""
	}
	if !strings.EqualFold(strings.TrimSpace(e.ExternalID), strings.TrimSpace(anchor)) {
		return ""
	}
	return e.ChosenReleaseID()
}

// albumNeedsTracklist reports whether any of the Album's in-scope Tracks still
// needs an id — the question that decides whether a tracklist is fetched at all.
//
// It reads each Track's STATUS, not merely the presence of an id, and that is the
// subtle part. SetTitleEnrichmentStatus writes the status and leaves musicbrainz_id
// alone, so a Track that matched once and later failed carries a record AND reads
// 'unmatched' (five such rows exist in the developer's library today, all on one
// album). trackRecordID still returns that id and the next pass still looks it up
// first, which is right — it either resolves and clears the row, or 404s and is
// diagnosable. What must not happen is this tier reading a stale record as a live
// one and deciding the Album has nothing left to resolve.
//
// A Track with an id and any other status is treated as anchored: 'pending' with a
// tag id is the fully-tagged library that already resolves by lookup, and 'failed'
// is a transient outage whose retry will use the same id regardless.
func (s *Service) albumNeedsTracklist(snap providerSnapshot, mode Mode, tracks []store.Title) bool {
	for _, tr := range tracks {
		if !s.shouldProcessLeaf(snap, mode, tr) {
			continue // out of scope for this pass; its state is not this pass's problem
		}
		if trackRecordID(tr) == "" || tr.EnrichmentStatus == "unmatched" {
			return true
		}
	}
	return false
}

// shouldProcessLeaf reports whether a leaf Title is in scope for this pass: every
// leaf in ModeFull (or when its kind is disabled in the resolved snapshot, so it
// still gets marked 'disabled'); in ModeNew, the never-enriched ('pending') leaves
// plus any whose transient failure has come due for another try (ADR-0048); in
// ModeRecheck, those PLUS the settled non-answers (ADR-0051).
// Enablement is read from the pass's resolved snapshot (the Library's effective
// policy), not the global one.
//
// This is the TV/Music twin of the SQL in store.TitlesForEnrichment — the Movie
// path selects its leaves with a query, these two walk their parent trees — so the
// two must agree on what "due" means, in every mode. Both defer to the same clock,
// the same retryDue rule and the same settledNonAnswer rule; a third mode is a
// third chance for them to drift, which is why they are asserted against each
// other in one test (recheck_mode_test.go).
func (s *Service) shouldProcessLeaf(snap providerSnapshot, mode Mode, t store.Title) bool {
	if mode == ModeFull || !snap.enablement.enabledFor(t.Kind) {
		return true
	}
	if t.EnrichmentStatus == "pending" || s.retryDue(t.EnrichmentStatus, t.EnrichmentRetryAt) {
		return true
	}
	return mode == ModeRecheck && settledNonAnswer(t.EnrichmentStatus, t.EnrichmentRetryAt)
}

// retryDue reports whether a 'failed' item's scheduled retry has arrived. An empty
// retryAt means no retry was scheduled — a permanent failure, parked — so it is
// never due.
//
// An unparseable timestamp counts as DUE. It can only be a corrupted write, and
// the two ways to be wrong are not symmetric: retrying an item early costs one
// provider call, while never retrying it strands the item in the exact state this
// whole mechanism exists to prevent — and invisibly, since a row with a retry
// scheduled is kept off the attention list until it escalates.
func (s *Service) retryDue(status, retryAt string) bool {
	if status != "failed" || retryAt == "" {
		return false
	}
	due, err := time.Parse(time.RFC3339, retryAt)
	if err != nil {
		return true
	}
	return !s.clock().Before(due)
}

// recordLeafFailure files a provider error against a leaf Title, choosing between
// the two outcomes a failed lookup can have (ADR-0048):
//
//   - transient — the provider could not be reached or could not answer. The Title
//     keeps whatever metadata it has, and a retry is scheduled on the backoff
//     schedule. It does NOT go on the attention list until the streak escalates:
//     there is nothing for an Admin to do about a 503.
//   - anything else — parked as 'failed' and surfaced for hand-matching, which is
//     what every failure did before the distinction existed.
//
// Either way the pass continues; one bad lookup never starves the rest.
//
// NEITHER branch records an ADR-0050 reason, for the same reason in two shapes. A
// transient failure writes nothing at all: it is in-flight work, and a diagnosis
// from an outage would outlive it. A parked one settles, so it must overwrite the
// column — but with the EMPTY reason, because "the provider refused the request"
// is not one of the five and is already the sentence the queue prints for a
// 'failed' row.
func (s *Service) recordLeafFailure(t store.Title, lookupErr error, res *Result) error {
	if !IsTransient(lookupErr) {
		log.Printf("obelo: enrich %q (%s): provider error: %v", t.Title, t.ID, lookupErr)
		res.Failed++
		return s.store.SetTitleEnrichmentStatus(t.ID, "failed", store.EnrichmentReasonNone)
	}
	attempts := t.EnrichmentAttempts + 1
	delay := retryDelay(attempts)
	log.Printf("obelo: enrich %q (%s): %v — attempt %d, retrying in %s",
		t.Title, t.ID, lookupErr, attempts, delay)
	res.Retrying++
	return s.store.SetTitleEnrichmentRetry(t.ID, attempts, s.clock().Add(delay))
}

// recordParentFailure is recordLeafFailure for a browse parent (Show / Season /
// Artist / Album). Parent outcomes are not counted in the pass Result — a parent
// is a decoration, not a leaf — so it takes no *Result.
//
// The stakes are higher than for a leaf. enrichParent returns early for any parent
// that is not 'pending', so before this a single 503 on a Show parked it, and with
// it every Season and Episode underneath, none of which appeared anywhere as a
// problem. A transient parent failure now expires.
func (s *Service) recordParentFailure(entityType, entityID string, cur store.EntityEnrichment, lookupErr error) error {
	if !IsTransient(lookupErr) {
		log.Printf("obelo: enrich %s %q: provider error: %v", entityType, entityID, lookupErr)
		return s.store.SetEntityEnrichmentStatus(entityType, entityID, "failed")
	}
	attempts := cur.Attempts + 1
	delay := retryDelay(attempts)
	log.Printf("obelo: enrich %s %q: %v — attempt %d, retrying in %s",
		entityType, entityID, lookupErr, attempts, delay)
	return s.store.SetEntityEnrichmentRetry(entityType, entityID, attempts, s.clock().Add(delay))
}

// enrichParent enriches one browse-parent entity (Show/Season/Artist/Album) into
// the generic entity tables, returning its resolved provider external id (so a
// child can resolve under it). It honors the same disabled / no-match / failed
// degradation as a leaf, and skips an already-matched parent in ModeNew (reusing
// its stored external id). Parent enrichment is not counted in the pass Result.
func (s *Service) enrichParent(ctx context.Context, snap providerSnapshot, mode Mode, entityType, entityID string, ref TitleRef) (string, error) {
	if !snap.enablement.enabledFor(ref.Kind) {
		return "", s.store.SetEntityEnrichmentStatus(entityType, entityID, "disabled")
	}
	cur, err := s.store.EntityEnrichmentByID(entityType, entityID)
	if err != nil {
		return "", err
	}
	// ModeRecheck re-asks PARENTS as well as leaves (ADR-0051). It has to: on the
	// motivating library 365 of 730 flagged Tracks hang under an Album that is
	// itself unmatched, and no amount of re-asking a Track fixes an Album that can
	// name none of its contents. A 'matched' parent is NOT a settled non-answer, so
	// it still short-circuits here to its stored id — which is what makes a recheck
	// free for the albums that are already fine, and what still hands ADR-0050's
	// tracklist tier its anchor.
	if mode != ModeFull && cur.Status != "pending" && !s.retryDue(cur.Status, cur.RetryAt) &&
		!(mode == ModeRecheck && settledNonAnswer(cur.Status, cur.RetryAt)) {
		return cur.ExternalID, nil // already settled; reuse its resolved id
	}
	// A durable Fix-info override (ADR-0019): resolve the parent BY the pinned id
	// every pass (New or Full) rather than re-searching by name, so the correction
	// survives later passes and rescans exactly like a leaf's pinned id.
	if cur.ExternalIDOrigin.Locked() && cur.ExternalID != "" {
		ref = refWithPinnedEntityID(ref, cur.ExternalID)
	}

	locks, err := s.store.EntityLockedFields(entityType, entityID)
	if err != nil {
		return "", err
	}

	meta, err := snap.provider.Lookup(ctx, ref)
	switch {
	case errors.Is(err, ErrNoMatch), err == nil && !meta.Matched:
		return "", s.store.SetEntityEnrichmentStatus(entityType, entityID, "unmatched")
	case err != nil:
		return "", s.recordParentFailure(entityType, entityID, cur, err)
	}

	var fetched []store.EntityArtworkRow
	for _, ar := range meta.Artwork {
		if locks[ar.Role] {
			continue // a hand-chosen image for this role is Locked
		}
		path, ok := s.cacheArtwork(ctx, entityType+"-"+entityID, ar)
		if !ok {
			continue
		}
		fetched = append(fetched, store.EntityArtworkRow{Role: ar.Role, Path: path})
	}
	// Cast headshots (cast-photos/02): download each parent cast member's photo into
	// the artwork cache as a `person` row, keyed by the person ref so an actor in
	// both a movie and a show shares one cached file (cross-kind dedupe). Reuses the
	// same non-fatal helper the leaf path does. A locked cast is not refetched (its
	// credits + absent photos are preserved by the store below), so this is skipped.
	if !locks["cast"] {
		s.fetchCastHeadshots(ctx, meta.Cast)
	}
	// A pinned override keeps its id even if the provider echoes a different one;
	// otherwise the resolved id is stored (and threaded to children).
	externalID := meta.ExternalID
	if cur.ExternalIDOrigin.Locked() && cur.ExternalID != "" {
		externalID = cur.ExternalID
	}
	if err := s.store.WriteEntityEnrichment(entityType, entityID, store.EntityEnrichmentWrite{
		Overview:      meta.Overview,
		ContentRating: meta.ContentRating,
		Network:       meta.Studio, // Studio carries the show network / album label
		Source:        meta.Source,
		ExternalID:    externalID,
		Genres:        meta.Genres,
		Artwork:       fetched,
		Cast:          toStoreCredits(meta.Cast),
	}, locks); err != nil {
		return "", err
	}
	return externalID, nil
}

// refWithPinnedEntityID rebuilds a parent lookup ref to resolve BY a pinned
// authoritative id: a Show pins a TMDB id, an Artist/Album a MusicBrainz id. The
// kind is preserved so the provider dispatches to the right by-id path.
func refWithPinnedEntityID(ref TitleRef, externalID string) TitleRef {
	switch ref.Kind {
	case "artist", "album", "track":
		ref.MusicbrainzID = externalID
	default:
		ref.TMDBID = externalID
	}
	return ref
}

// fetchCastHeadshots downloads the headshots for a Title's cast into the artwork
// cache and records each as a `person` entity_artwork row keyed by the person ref
// (cast-photos/01). It is best-effort and NON-FATAL: a cast member with no ref or
// no ImageURL is skipped (its name/character still persist via WriteTitleEnrichment),
// a person already cached is skipped (cross-title dedupe), and a download failure
// (fetcher error / oversized / benign 404) is logged inside cacheArtwork and drops
// only that one photo — never the cast member, never the Title's enrichment. A
// person's headshot is keyed under the `person` entity + `profile` role, so a
// re-fetch overwrites the same cached file in place.
func (s *Service) fetchCastHeadshots(ctx context.Context, cast []Credit) {
	for _, c := range cast {
		if c.PersonRef == "" || c.ImageURL == "" {
			continue // no ref or no headshot → the cast member persists without a photo
		}
		// Dedupe: a person already carrying a cached headshot is not re-downloaded,
		// so the same actor across many Titles costs one fetch + one file + one row.
		if _, err := s.store.PersonArtworkByRef(c.PersonRef, personProfileRole); err == nil {
			continue
		}
		path, ok := s.cacheArtwork(ctx, personCacheKey(c.PersonRef), ArtworkRef{Role: personProfileRole, URL: c.ImageURL})
		if !ok {
			continue // logged in cacheArtwork; non-fatal (the cast member keeps its name/character)
		}
		if err := s.store.UpsertPersonArtwork(c.PersonRef, personProfileRole, path); err != nil {
			log.Printf("obelo: enrich person headshot %q: store failed: %v", c.PersonRef, err)
		}
	}
}

// personCacheKey turns a provider-namespaced person ref ("tmdb:12345") into a
// filesystem-safe cache-file key. The ref's colon is not portable in a filename,
// so it is replaced; the "person-" prefix keeps person headshots visibly distinct
// from Title/entity artwork in the flat cache dir. Deterministic, so a re-fetch
// overwrites the same file in place (idempotent, like cacheArtwork's other keys).
func personCacheKey(personRef string) string {
	return "person-" + strings.ReplaceAll(personRef, ":", "-")
}

// cacheArtwork downloads one artwork reference and writes it to the cache under a
// deterministic name (key-role.ext, key being a Title id or an entityType-id), so
// re-enrichment overwrites in place (idempotent — no duplicate files). Returns the
// cache-relative name (just the filename; the file lives directly under cacheDir)
// so the stored DB path survives a data-dir move, and ok=false on any error
// (logged, non-fatal). The serve layer re-roots it via catalog.ResolveArtworkPath.
func (s *Service) cacheArtwork(ctx context.Context, key string, ar ArtworkRef) (string, bool) {
	data, contentType, err := s.fetcher.Fetch(ctx, ar.URL)
	if err != nil {
		// A missing image (404) is the normal "no art for this entity" outcome —
		// skip it quietly. Only real failures are worth a log (ADR-0001).
		if !errors.Is(err, ErrArtworkNotFound) {
			log.Printf("obelo: enrich artwork %q (%s): fetch failed: %v", ar.Role, key, err)
		}
		return "", false
	}
	// Raster only (ADR-0026, same rule as uploads): an SVG cached here would fall
	// through extensionFor to a raster extension and then be served with a wrong
	// content-type no browser will decode. Candidate listing already filters SVG
	// paths; this guards the mislabeled/other-provider cases.
	if strings.HasPrefix(contentType, "image/svg") {
		log.Printf("obelo: enrich artwork %q (%s): skipping SVG %s (raster images only)", ar.Role, key, ar.URL)
		return "", false
	}
	name := key + "-" + ar.Role + extensionFor(contentType)
	if err := os.WriteFile(filepath.Join(s.cacheDir, name), data, 0o644); err != nil {
		log.Printf("obelo: enrich artwork %q (%s): write failed: %v", ar.Role, key, err)
		return "", false
	}
	return name, true
}

// pinnedProviderFor reports the registry slug of the provider a Title's RECORD
// lives with — an Enrichment override's, or the embedded id token's when nothing
// overrode it (ADR-0045) — and whether there is such a record at all: a video Title with a TMDB id resolves against TMDB's record, a
// music Track with a MusicBrainz id against MusicBrainz's. It is how the pass
// recognizes an override whose record provider may differ from the Library's current
// Authoritative provider (issue 06). A Title with no external id is not pinned (its
// resolution simply follows the Library leader).
func pinnedProviderFor(t store.Title) (string, bool) {
	switch t.Kind {
	case "artist", "album", "track":
		if t.MusicbrainzID != "" {
			return SlugMusicBrainz, true
		}
	default:
		if t.TMDBID != "" {
			return SlugTMDB, true
		}
	}
	return "", false
}

// withEpisodePin redirects a lookup reference onto the provider episode an Admin
// pinned — the one and only place the pin takes effect. The Title's own
// SeasonNumber/EpisodeNumber (and so its place in the library, its identity_key
// and every User's watch state) are untouched (ADR-0002/0014).
//
// It is a free function rather than a step inside refFor because the two ways a
// leaf reaches a lookup — refFor for a single-Title re-enrich, collectTVLeaves for
// a library pass — must not be able to disagree about it.
func withEpisodePin(ref TitleRef, t store.Title) TitleRef {
	if season, episode, ok := t.EpisodePin(); ok {
		ref.SeasonNumber, ref.EpisodeNumber = season, episode
	}
	return ref
}

// refFor builds the provider lookup reference from a stored Title. External ids
// drive a by-id lookup; they are the RECORD ids, so an Admin's Enrichment override
// is what resolves when there is one and the id a folder name asserts otherwise
// (ADR-0045, resolved in store.recordExternalIDs).
//
// It is PURE, and it reaches only the tiers that are on the Title's own row. The
// music precedence is four tiers — record → tag → the ALBUM's tracklist → search
// (ADR-0049 as ADR-0050 narrowed it) — and the third of those is not on the row: it
// takes the Track's Album, that Album's record or tag release-group, its chosen
// edition, its whole local track list and a provider read. So refFor supplies tiers
// one, two and four, and singleLeafWork splices the album tier in on top of it
// through the same albumTrackAnchors a library pass uses.
//
// This comment used to claim the precedence was the pass's, in full. It was true
// when it was written and it stopped being true the day the tier was added — which
// is what a comment asserting parity between two code paths is worth without a test
// holding them to it. There is one now (issue 14).
func refFor(t store.Title) TitleRef {
	ref := TitleRef{
		Kind:   t.Kind,
		Title:  t.Title,
		Year:   t.Year,
		TMDBID: t.TMDBID,
		IMDBID: t.IMDBID,
		// Tiers one and two of the music precedence: the record wins, the file's tag id
		// is the fallback (ADR-0049). Tier three (the Album's tracklist, ADR-0050) is
		// added by singleLeafWork; a search is still the last resort.
		MusicbrainzID: trackRecordID(t),
		SeasonNumber:  t.SeasonNumber,
		EpisodeNumber: t.EpisodeNumber,
		EpisodeLabel:  t.EpisodeLabel,
	}
	// An Admin-pinned provider episode overrides the parsed numbers FOR THE LOOKUP
	// ONLY. It is what makes a file fixable when the provider numbers the series
	// differently from the disk.
	return withEpisodePin(ref, t)
}

func toStoreCredits(in []Credit) []store.Credit {
	out := make([]store.Credit, 0, len(in))
	for _, c := range in {
		out = append(out, store.Credit{
			Person:    c.Person,
			Role:      c.Role,
			Character: c.Character,
			Kind:      c.Kind,
			PersonRef: c.PersonRef,
		})
	}
	return out
}

// extensionFor maps a content-type to a file extension, defaulting to .jpg (the
// overwhelmingly common poster format).
func extensionFor(contentType string) string {
	switch contentType {
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".jpg"
	}
}

// EnsureCacheDir creates the artwork cache directory if absent. Called by app.New
// at boot; the dir is durable (not cleared), unlike transcode scratch.
func EnsureCacheDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("enrich: creating artwork cache %q: %w", dir, err)
	}
	return nil
}
