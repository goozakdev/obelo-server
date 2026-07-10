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

	"github.com/marioquake/juicebox/internal/store"
	"github.com/google/uuid"
)

// Store is the persistence the enrich service needs. *store.DB satisfies it; the
// narrow interface keeps the seam explicit and the service testable.
type Store interface {
	LibraryByID(id string) (store.Library, error)
	TitlesForEnrichment(libraryID string, sel store.EnrichSelect) ([]store.Title, error)
	LockedFields(titleID string) (map[string]bool, error)
	WriteTitleEnrichment(titleID string, e store.TitleEnrichment, locks map[string]bool) error
	SetTitleEnrichmentStatus(titleID, status string) error

	// Single-Title match correction (issue 05): an Admin re-points a Title's
	// external metadata id, then it re-enriches just that Title. SetTitleExternalMatch
	// writes the external id WITHOUT touching identity_key; TitleForEnrichmentByID
	// reads the one Title back to re-resolve it.
	SetTitleExternalMatch(titleID string, m store.ExternalMatch) error
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
	WriteEntityEnrichment(entityType, entityID string, e store.EntityEnrichmentWrite, locks map[string]bool) error
	SetEntityEnrichmentStatus(entityType, entityID, status string) error
	EntityEnrichmentByID(entityType, entityID string) (store.EntityEnrichment, error)

	// Parent-entity Fix-info + Locked fields (issue item-editing/02): an Admin pins
	// a durable Enrichment override on a Show/Artist/Album (SetEntityExternalMatch)
	// and the parent enrich path honors its hand-set Locked fields (EntityLockedFields).
	// LibraryOfEntity gives the single-parent re-enrich the Library to serialize on.
	SetEntityExternalMatch(entityType, entityID, externalID string) error
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
)

// providerSnapshot bundles the MetadataProvider with its derived per-kind
// Enablement so the two are always swapped as a unit — an in-flight pass that
// captures the current snapshot never observes a provider paired with a stale
// enablement, or vice versa.
type providerSnapshot struct {
	provider   MetadataProvider
	enablement Enablement
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

	// current holds the live provider + enablement snapshot, swapped atomically by
	// SetProvider and read (never mutated in place) at pass time. Every provider
	// lookup and enablement check reads the CURRENT snapshot, so a runtime swap
	// takes effect on the next read — and, because provider + enablement travel
	// together in one pointer, never half-applied.
	current atomic.Pointer[providerSnapshot]

	cacheDir string

	// candidates is the short-lived, bounded per-session cache of provider
	// candidate-list results keyed by (entity, role), so artwork tabs that
	// auto-search on open don't re-hit the metadata providers on every toggle
	// (PRD artwork-management, slice 04). A pure optimization: a miss falls through
	// to the live query, applying/uploading an image invalidates the entry, and a
	// zero TTL disables it with no behavior change.
	candidates *candidateCache

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
		candidates: newCandidateCache(candidateTTL),
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
}

// snapshot returns the current provider + enablement snapshot. Callers read it
// once per use so a concurrent SetProvider swap is picked up on the next read.
func (s *Service) snapshot() providerSnapshot { return *s.current.Load() }

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

// enabled reports whether the given media kind is on in the CURRENT snapshot.
// Music kinds (artist/album/track) gate on Music; the video kinds (movie/show/
// season/episode, and any default) gate on Video. A disabled kind is a no-op
// recorded 'disabled' (ADR-0001).
func (s *Service) enabled(kind string) bool {
	return s.snapshot().enablement.enabledFor(kind)
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
}

// Result summarizes a completed pass.
type Result struct {
	Total     int
	Matched   int
	Unmatched int
	Failed    int
	Disabled  int
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
		leaves, err = s.collectTVLeaves(ctx, libraryID, mode)
	case "music":
		leaves, err = s.collectMusicLeaves(ctx, libraryID, mode)
	default:
		sel := store.EnrichPending
		if mode == ModeFull {
			sel = store.EnrichAll
		}
		var titles []store.Title
		titles, err = s.store.TitlesForEnrichment(libraryID, sel)
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
				Failed: res.Failed, Disabled: res.Disabled,
			})
		}
	}
	emit(0)
	for i, lw := range leaves {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := s.processLeaf(ctx, lw, &res); err != nil {
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
func (s *Service) MatchTitle(ctx context.Context, titleID string, m store.ExternalMatch) error {
	if err := s.store.SetTitleExternalMatch(titleID, m); err != nil {
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

	// A Music leaf (Track) carries sparseTitle so a provider's canonical recording
	// name only fills a MISSING tag title — embedded tags are the Music display/
	// identity authority (ADR-0002), exactly as the album full-pass treats tracks.
	// Without this, applying a Track override would overwrite the tag title with
	// MusicBrainz's canonical name. A Movie/Episode display title is unaffected.
	var res Result
	return s.processLeaf(ctx, leafWork{title: t, ref: refFor(t), sparseTitle: t.Kind == "track"}, &res)
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
func (s *Service) ApplyOverride(ctx context.Context, titleID, externalID string) error {
	t, err := s.store.TitleForEnrichmentByID(titleID)
	if err != nil {
		return err // ErrNotFound flows through
	}
	return s.MatchTitle(ctx, titleID, ExternalMatchForKind(t.Kind, externalID))
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

// ApplyEntityOverride pins a picked candidate's authoritative external id on a
// browse-parent entity as a durable Enrichment override and re-enriches just that
// parent (Fix-info on a Show/Artist/Album, ADR-0019). It persists the pin
// (SetEntityExternalMatch — external_id + external_id_locked, so future passes look
// up BY it) then runs the single-parent enrich path honoring the parent's Locked
// fields. Identity and watch state are untouched. store.ErrNotFound for an unknown
// parent flows to the handler as a 404.
func (s *Service) ApplyEntityOverride(ctx context.Context, entityType, entityID, externalID string) error {
	libraryID, err := s.store.LibraryOfEntity(entityType, entityID)
	if err != nil {
		return err // ErrNotFound flows through
	}
	if err := s.store.SetEntityExternalMatch(entityType, entityID, externalID); err != nil {
		return err
	}
	// Serialize against a concurrent full pass over the same Library so the single-
	// parent re-enrich never races it (per-Library lock, as MatchTitle does).
	lock := s.libLock(libraryID)
	lock.Lock()
	defer lock.Unlock()

	ref := refWithPinnedEntityID(TitleRef{Kind: entityKind(entityType)}, externalID)
	_, err = s.enrichParent(ctx, ModeFull, entityType, entityID, ref)
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
		log.Printf("juicebox: enrich artwork: removing replaced upload %q: %v", replacedPath, err)
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
}

// processLeaf enriches one leaf Title into res, honoring Locked fields and the
// graceful-degradation rules (disabled → no call; no-match → unmatched; provider
// error → failed, pass continues). Identity is never touched (ADR-0002).
func (s *Service) processLeaf(ctx context.Context, lw leafWork, res *Result) error {
	t := lw.title
	snap := s.snapshot()
	if !snap.enablement.enabledFor(t.Kind) {
		if err := s.store.SetTitleEnrichmentStatus(t.ID, "disabled"); err != nil {
			return err
		}
		res.Disabled++
		return nil
	}

	locks, err := s.store.LockedFields(t.ID)
	if err != nil {
		return err
	}

	meta, err := snap.provider.Lookup(ctx, lw.ref)
	switch {
	case errors.Is(err, ErrNoMatch), err == nil && !meta.Matched:
		res.Unmatched++
		return s.store.SetTitleEnrichmentStatus(t.ID, "unmatched")
	case err != nil:
		// Non-fatal: log + record failed, keep going (story 36).
		log.Printf("juicebox: enrich %q (%s): provider error: %v", t.Title, t.ID, err)
		res.Failed++
		return s.store.SetTitleEnrichmentStatus(t.ID, "failed")
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
// pending parents/episodes are touched; ModeFull re-does all.
func (s *Service) collectTVLeaves(ctx context.Context, libraryID string, mode Mode) ([]leafWork, error) {
	shows, err := s.store.ListAllShows(libraryID)
	if err != nil {
		return nil, err
	}
	var leaves []leafWork
	for _, sh := range shows {
		showExtID := sh.TMDBID // embedded {tmdb-…} fallback
		extID, err := s.enrichParent(ctx, mode, store.EntityShow, sh.ID,
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
			if _, err := s.enrichParent(ctx, mode, store.EntitySeason, se.ID,
				TitleRef{Kind: "season", TMDBID: showExtID, SeasonNumber: se.SeasonNumber}); err != nil {
				return nil, err
			}
			eps, err := s.store.EpisodesForSeason(se.ID)
			if err != nil {
				return nil, err
			}
			for _, ep := range eps {
				if !s.shouldProcessLeaf(mode, ep.Kind, ep.EnrichmentStatus) {
					continue
				}
				// Episode durability (ADR-0019, closing the gap deferred from slice 01):
				// an Episode Enrichment override pins the CORRECTED show id on the episode's
				// own tmdb_id, so honor that per-episode anchor over the show-derived id — a
				// pinned episode survives a full pass instead of being re-derived from its
				// Show. An un-pinned episode still resolves under the Show's resolved id.
				epShowID := showExtID
				if ep.TMDBID != "" {
					epShowID = ep.TMDBID
				}
				leaves = append(leaves, leafWork{title: ep, ref: TitleRef{
					Kind: "episode", Title: ep.Title, TMDBID: epShowID,
					SeasonNumber: ep.SeasonNumber, EpisodeNumber: ep.EpisodeNumber, EpisodeLabel: ep.EpisodeLabel,
				}})
			}
		}
	}
	return leaves, nil
}

// collectMusicLeaves walks a Music Library's Artists → Albums → Tracks: it
// enriches the Artist and Album parents and returns the Track leaves. Tracks
// carry sparseTitle so a canonical title only fills a missing tag title.
func (s *Service) collectMusicLeaves(ctx context.Context, libraryID string, mode Mode) ([]leafWork, error) {
	artists, err := s.store.ListAllArtists(libraryID)
	if err != nil {
		return nil, err
	}
	var leaves []leafWork
	for _, ar := range artists {
		if _, err := s.enrichParent(ctx, mode, store.EntityArtist, ar.ID,
			TitleRef{Kind: "artist", Title: ar.Name, Artist: ar.Name}); err != nil {
			return nil, err
		}
		albums, err := s.store.AlbumsForArtist(ar.ID)
		if err != nil {
			return nil, err
		}
		for _, al := range albums {
			if _, err := s.enrichParent(ctx, mode, store.EntityAlbum, al.ID,
				TitleRef{Kind: "album", Title: al.Title, Album: al.Title, Year: al.Year, Artist: ar.Name}); err != nil {
				return nil, err
			}
			tracks, err := s.store.TracksForAlbum(al.ID)
			if err != nil {
				return nil, err
			}
			for _, tr := range tracks {
				if !s.shouldProcessLeaf(mode, tr.Kind, tr.EnrichmentStatus) {
					continue
				}
				leaves = append(leaves, leafWork{title: tr, sparseTitle: true, ref: TitleRef{
					Kind: "track", Title: tr.Title, Track: tr.Title,
					Artist: ar.Name, Album: al.Title, MusicbrainzID: tr.MusicbrainzID,
				}})
			}
		}
	}
	return leaves, nil
}

// shouldProcessLeaf reports whether a leaf Title of the given kind + enrichment_
// status is in scope for this pass: every leaf in ModeFull (or when its kind is
// disabled, so it still gets marked 'disabled'); only never-enriched ('pending')
// leaves in ModeNew.
func (s *Service) shouldProcessLeaf(mode Mode, kind, status string) bool {
	if mode == ModeFull || !s.enabled(kind) {
		return true
	}
	return status == "pending"
}

// enrichParent enriches one browse-parent entity (Show/Season/Artist/Album) into
// the generic entity tables, returning its resolved provider external id (so a
// child can resolve under it). It honors the same disabled / no-match / failed
// degradation as a leaf, and skips an already-matched parent in ModeNew (reusing
// its stored external id). Parent enrichment is not counted in the pass Result.
func (s *Service) enrichParent(ctx context.Context, mode Mode, entityType, entityID string, ref TitleRef) (string, error) {
	snap := s.snapshot()
	if !snap.enablement.enabledFor(ref.Kind) {
		return "", s.store.SetEntityEnrichmentStatus(entityType, entityID, "disabled")
	}
	cur, err := s.store.EntityEnrichmentByID(entityType, entityID)
	if err != nil {
		return "", err
	}
	if mode != ModeFull && cur.Status != "pending" {
		return cur.ExternalID, nil // already settled; reuse its resolved id
	}
	// A durable Fix-info override (ADR-0019): resolve the parent BY the pinned id
	// every pass (New or Full) rather than re-searching by name, so the correction
	// survives later passes and rescans exactly like a leaf's pinned id.
	if cur.ExternalIDLocked && cur.ExternalID != "" {
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
		log.Printf("juicebox: enrich %s %q: provider error: %v", entityType, entityID, err)
		return "", s.store.SetEntityEnrichmentStatus(entityType, entityID, "failed")
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
	if cur.ExternalIDLocked && cur.ExternalID != "" {
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
			log.Printf("juicebox: enrich person headshot %q: store failed: %v", c.PersonRef, err)
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
			log.Printf("juicebox: enrich artwork %q (%s): fetch failed: %v", ar.Role, key, err)
		}
		return "", false
	}
	name := key + "-" + ar.Role + extensionFor(contentType)
	if err := os.WriteFile(filepath.Join(s.cacheDir, name), data, 0o644); err != nil {
		log.Printf("juicebox: enrich artwork %q (%s): write failed: %v", ar.Role, key, err)
		return "", false
	}
	return name, true
}

// refFor builds the provider lookup reference from a stored Title. External ids
// (parsed from a {tmdb-…} token or assigned by fix-match) drive a by-id lookup.
func refFor(t store.Title) TitleRef {
	return TitleRef{
		Kind:          t.Kind,
		Title:         t.Title,
		Year:          t.Year,
		TMDBID:        t.TMDBID,
		IMDBID:        t.IMDBID,
		MusicbrainzID: t.MusicbrainzID,
		SeasonNumber:  t.SeasonNumber,
		EpisodeNumber: t.EpisodeNumber,
		EpisodeLabel:  t.EpisodeLabel,
	}
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
