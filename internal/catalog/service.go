// Package catalog is the browse/read domain over the scanned Movie catalog
// (ADR-0006 seam): it lists a Library's Titles with stable cursor pagination and
// returns one Title with its nested Editions → Files → Streams. It is
// transport-agnostic — it speaks Titles and cursors, not HTTP; the api package
// wraps it in thin handlers, and the scanner package writes what it reads.
package catalog

import (
	"errors"
	"os"

	"github.com/marioquake/juicebox/internal/access"
	"github.com/marioquake/juicebox/internal/store"
)

// Domain errors mapped to HTTP by the api layer.
var (
	// ErrNotFound means the requested Library or Title does not exist. The api
	// layer maps it to 404 (never 403 — hide existence, api-contract.md).
	ErrNotFound = errors.New("catalog: not found")
	// ErrBadCursor means the cursor query param could not be decoded.
	ErrBadCursor = errors.New("catalog: invalid cursor")
)

// Store is the persistence the catalog service reads. *store.DB satisfies it.
type Store interface {
	LibraryExists(id string) (bool, error)
	LibraryByID(id string) (store.Library, error)
	ListTitles(libraryID string, sort store.TitleSort, cursor *store.TitleCursor, limit int, genre string, filter store.AccessFilter) (store.TitlePage, error)
	TitleByID(id string) (store.TitleDetail, error)
	// FileByID loads one File (with Streams) by its stable id, or ErrNotFound —
	// the lookup behind the sessionless direct-file download route.
	FileByID(id string) (store.File, error)
	// GenresForTitles bulk-reads enriched genres for a page of Titles, keyed by
	// id, so a browse list attaches genres without an N+1 query.
	GenresForTitles(titleIDs []string) (map[string][]string, error)
	// ArtworkVersionsForTitles bulk-reads a per-Title artwork cache-bust version
	// (newest artwork added_at), keyed by id, so a browse grid reloads only the
	// posters whose artwork actually changed.
	ArtworkVersionsForTitles(titleIDs []string) (map[string]string, error)
	// TitleEnrichmentForMany bulk-reads enrichment display fields (overview /
	// enriched_title / status) for a page of Titles, so the Home + search readers
	// (which use the lean Title projection) can surface enrichment.
	TitleEnrichmentForMany(titleIDs []string) (map[string]store.Title, error)
	ListUnmatched(libraryID string) ([]store.UnmatchedFile, error)
	// TitlesNeedingMatch lists the Titles whose Enrichment could not settle on a
	// record (status unmatched/failed) — the Admin attention surface for correcting
	// a wrong/missing metadata match (issue 05), distinct from the identity
	// Unmatched files and needs-review Titles.
	TitlesNeedingMatch(libraryID string) ([]store.Title, error)
	// Needs-review attention surface (identity, not enrichment): the scanner-set
	// needs_review flag is now Admin-resolvable. The two reads collect every still-
	// flagged Title / Show of a Library; the two writes dismiss a flag the parse got
	// right (sticky across rescans, migration 0012).
	TitlesNeedingReview(libraryID string) ([]store.NeedsReviewItem, error)
	ShowsNeedingReview(libraryID string) ([]store.NeedsReviewItem, error)
	MarkTitleReviewed(id string) error
	MarkShowReviewed(id string) error
	ArtworkByTitleRole(titleID, role string) (store.Artwork, error)
	// Library resolution for the access guard on artwork: which Library a bare
	// Title / browse-parent id belongs to, so an out-of-scope entity's artwork is
	// hidden as 404 (ErrNotFound for an unknown id).
	LibraryOfTitle(id string) (string, error)
	LibraryOfEntity(entityType, id string) (string, error)
	// Hand-editing + Locked fields (external-metadata-enrichment issue 04). An
	// Admin's PUT /metadata writes each supplied field and Locks it; DELETE
	// /metadata/locks/{field} releases a lock back to auto. Identity is untouched.
	WriteTitleMetadata(titleID string, edit store.MetadataEdit) error
	ReleaseFieldLock(titleID, field string) error
	// ReleaseTitleArtworkLock / ReleaseEntityArtworkLock release an artwork role's
	// Lock and, when the role is backed by an Admin upload, delete the 'uploaded'
	// row — the lock-coupled "undo my upload" (ADR-0026). They return the deleted
	// upload's on-disk path (empty if none) so the caller removes the cached bytes.
	ReleaseTitleArtworkLock(titleID, role string) (removedPath string, err error)
	// Parent-entity hand-edit + Locked fields (item-editing/02, ADR-0019): the
	// Show/Artist/Album analogue of the Title lock surface. WriteEntityMetadata
	// writes-and-Locks a parent's descriptive fields; ReleaseEntityFieldLock releases
	// one; EntityLockedFieldsSorted lists them for the parent detail. Identity untouched.
	WriteEntityMetadata(entityType, entityID string, edit store.EntityMetadataEdit) error
	ReleaseEntityFieldLock(entityType, entityID, field string) error
	ReleaseEntityArtworkLock(entityType, entityID, role string) (removedPath string, err error)
	EntityLockedFieldsSorted(entityType, entityID string) ([]string, error)
	// Wrong-item identity correction (item-editing/04, ADR-0019/0014): the
	// destructive Match-override apply re-keys the live Movie/Show row to the picked
	// work and — because identity actually changed — resets its watch state and
	// clears its Locked fields (a clean slate). AnyFilePathForShow derives the Show's
	// folder anchor. The folder-keyed override itself is written by the match domain.
	RekeyTitleIdentity(titleID, title string, year int, tmdbID, identityKey string) error
	RekeyShowIdentity(showID, title string, year int, tmdbID, identityKey string) error
	ResetWatchStateForTitle(titleID string) error
	ResetWatchStateForShow(showID string) error
	ClearTitleFieldLocks(titleID string) error
	ClearEntityFieldLocks(entityType, entityID string) error
	AnyFilePathForShow(showID string) (string, error)
	// TV browse (issue tv-music/01): the explicit Show → Season → Episode
	// hierarchy. ListShows is the top-level grid for a TV Library; the rest drill
	// down. EpisodeContextForTitle attaches parent context to an Episode detail.
	ListShows(libraryID string, cursor *store.TitleCursor, limit int, genre string, filter store.AccessFilter) (store.ShowPage, error)
	ShowByID(id string) (store.Show, error)
	SeasonsForShow(showID string) ([]store.Season, error)
	SeasonByID(id string) (store.Season, error)
	EpisodesForSeason(seasonID string) ([]store.Title, error)
	EpisodeContextForTitle(titleID string) (store.EpisodeContext, error)
	// Music browse (issue tv-music/03): the explicit Artist → Album → Track
	// hierarchy. ListArtists is the top-level list for a Music Library; the rest
	// drill down. TrackContextForTitle attaches parent context to a Track detail.
	ListArtists(libraryID string, cursor *store.TitleCursor, limit int, genre string) (store.ArtistPage, error)
	ArtistByID(id string) (store.Artist, error)
	AlbumsForArtist(artistID string) ([]store.Album, error)
	AlbumByID(id string) (store.Album, error)
	TracksForAlbum(albumID string) ([]store.Title, error)
	TrackDurationsForAlbum(albumID string) (map[string]int64, error)
	TrackContextForTitle(titleID string) (store.TrackContext, error)
	AlbumArtworkByID(albumID string) (store.Artwork, error)
	// Watch state + Home rows (issue 08), all per-User and hidden-excluded.
	WatchStateFor(userID, titleID string) (store.WatchState, error)
	WatchStatesForTitles(userID string, titleIDs []string) (map[string]store.WatchState, error)
	// The Home rows + Search are cross-Library aggregates, so they take the access
	// filter and apply it in SQL (so a restricted row never leaves the store and
	// cursor/limit stay correct). An all-access filter adds no predicate.
	ContinueWatching(userID string, limit int, filter store.AccessFilter) ([]store.ContinueWatchingRow, error)
	RecentlyAdded(limit int, filter store.AccessFilter) ([]store.Title, error)
	// UpNext is the TV-only computed Home row: the next unwatched Episode in Show
	// order for each Show the User has started (issue tv-music/02).
	UpNext(userID string, limit int, filter store.AccessFilter) ([]store.UpNextRow, error)
	// Search matches across all browse kinds by display name (issue tv-music/04),
	// hidden-excluded. UnwatchedEpisodeCounts powers the Show-poster watched
	// affordance (per-User unwatched count for a page of Shows).
	Search(query string, limit int, filter store.AccessFilter) (store.SearchResults, error)
	UnwatchedEpisodeCounts(userID string, showIDs []string) (map[string]int, error)

	// Parent-entity Enrichment (issue 03): the Show/Season/Artist/Album browse
	// parents gain descriptive metadata + fetched artwork via the generic entity
	// tables. The bulk readers decorate a grid without an N+1; EntityArtworkByRole
	// serves a fetched parent image.
	EntityEnrichmentByID(entityType, entityID string) (store.EntityEnrichment, error)
	EntityEnrichmentForMany(entityType string, ids []string) (map[string]store.EntityEnrichment, error)
	// EntityCredits reads a browse-parent's enriched cast (a Show's series main cast)
	// in billing order, each row carrying its person_ref + the person's headshot
	// cache-bust version (cast-photos/02) — the parent analogue of a Title's cast.
	EntityCredits(entityType, entityID string) ([]store.Credit, error)
	EntityArtworkByRole(entityType, entityID, role string) (store.Artwork, error)
	EntityArtworkRolesForMany(entityType string, ids []string) (map[string]map[string]bool, error)
	// Cast headshots (cast-photos/01): a person's fetched headshot is served by ref
	// (PersonArtworkByRef). A person has no Library of its own, so its access follows
	// the Titles that credit it — LibrariesCreditingPerson gives the Library set the
	// serve guard checks against the caller's Scope.
	PersonArtworkByRef(personRef, role string) (store.Artwork, error)
	LibrariesCreditingPerson(personRef string) ([]string, error)
	// EntityArtworkVersionsForMany bulk-reads a per-entity artwork cache-bust
	// version (newest entity_artwork added_at), keyed by id, so a browse client
	// reloads a parent poster only when its fetched image actually changed.
	EntityArtworkVersionsForMany(entityType string, ids []string) (map[string]string, error)
}

// Service implements the browse operations.
type Service struct {
	store Store
}

// NewService builds the catalog service over the given store.
func NewService(s Store) *Service {
	return &Service{store: s}
}

// Sort is the requested ordering for a title listing, parsed from the API
// sort= param.
type Sort int

const (
	// SortTitle orders case-insensitively by title (default).
	SortTitle Sort = iota
	// SortDateAdded orders newest-added first.
	SortDateAdded
)

// ListInput is a validated title-listing request.
type ListInput struct {
	LibraryID string
	Sort      Sort
	// Cursor is the opaque cursor from the previous page, or "" for the first.
	Cursor string
	// Limit is the page size; the service clamps it to a sane range.
	Limit int
	// Genre, when non-empty, filters the listing to Titles carrying that enriched
	// genre (the filter[genre] browse param). An un-enriched Title has no genres.
	Genre string
}

// Page is one page of Title summaries plus the opaque cursor for the next page
// ("" when there are no more).
type Page struct {
	Titles     []store.Title
	NextCursor string
}

const (
	defaultLimit = 20
	maxLimit     = 100
)

// ListTitles returns one page of a Library's Titles. It returns ErrNotFound for
// an unknown Library (so the caller answers 404, not an empty 200), and
// ErrBadCursor for an undecodable cursor.
func (s *Service) ListTitles(scope access.Scope, in ListInput) (Page, error) {
	// A Library the caller may not access is hidden as 404 (not an empty 200) —
	// the library is fixed by the path, so this is the access check for the whole
	// listing. No-op under an all-access scope.
	if !scope.AllowsLibrary(in.LibraryID) {
		return Page{}, ErrNotFound
	}
	exists, err := s.store.LibraryExists(in.LibraryID)
	if err != nil {
		return Page{}, err
	}
	if !exists {
		return Page{}, ErrNotFound
	}

	limit := in.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	sortKind := store.SortByTitle
	if in.Sort == SortDateAdded {
		sortKind = store.SortByDateAdded
	}

	var cursor *store.TitleCursor
	if in.Cursor != "" {
		c, err := decodeCursor(in.Cursor)
		if err != nil {
			return Page{}, ErrBadCursor
		}
		cursor = &c
	}

	page, err := s.store.ListTitles(in.LibraryID, sortKind, cursor, limit, in.Genre, scope.StoreFilter())
	if err != nil {
		return Page{}, err
	}

	out := Page{Titles: page.Titles}
	if page.HasMore && len(page.Titles) > 0 {
		last := page.Titles[len(page.Titles)-1]
		out.NextCursor = encodeCursor(store.TitleCursor{
			SortKey: cursorSortKey(in.Sort, last),
			ID:      last.ID,
		})
	}
	return out, nil
}

// GetTitle returns one Title with its nested Editions/Files/Streams, or
// ErrNotFound.
func (s *Service) GetTitle(scope access.Scope, id string) (store.TitleDetail, error) {
	d, err := s.getTitleDetail(id)
	if err != nil {
		return store.TitleDetail{}, err
	}
	// A Title in a Library the caller may not access, or above their Rating
	// ceiling, is hidden as 404 (existence-hiding). No-op under an all-access scope.
	if !scope.AllowsLibrary(d.LibraryID) || !scope.AllowsRating(d.ContentRating) {
		return store.TitleDetail{}, ErrNotFound
	}
	return d, nil
}

// getTitleDetail reads a Title's full detail, mapping store.ErrNotFound to the
// catalog ErrNotFound. It applies NO access scope: the User-facing GetTitle adds
// the scope guard, while the Admin metadata ops (EditMetadata/ReleaseLock) read
// unscoped — an Admin is all-access, and identity edits are never gated on browse
// visibility.
func (s *Service) getTitleDetail(id string) (store.TitleDetail, error) {
	d, err := s.store.TitleByID(id)
	if errors.Is(err, store.ErrNotFound) {
		return store.TitleDetail{}, ErrNotFound
	}
	if err != nil {
		return store.TitleDetail{}, err
	}
	return d, nil
}

// guardTitle returns ErrNotFound when a Title is unknown or its Library is
// outside the Scope — the cheap visibility check the artwork read uses (no full
// detail load). No-op under an all-access scope.
func (s *Service) guardTitle(scope access.Scope, titleID string) error {
	lib, err := s.store.LibraryOfTitle(titleID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !scope.AllowsLibrary(lib) {
		return ErrNotFound
	}
	return nil
}

// guardEntity is guardTitle for a browse parent (Show/Season/Artist/Album):
// ErrNotFound when the entity is unknown or its Library is outside the Scope.
func (s *Service) guardEntity(scope access.Scope, entityType, entityID string) error {
	lib, err := s.store.LibraryOfEntity(entityType, entityID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !scope.AllowsLibrary(lib) {
		return ErrNotFound
	}
	return nil
}

// FileByID returns one File (with its Streams) by id, or ErrNotFound. It backs
// the sessionless direct-file download (an external player like VLC fetching the
// original bytes), addressed by the stable File id rather than a Playback session.
func (s *Service) FileByID(id string) (store.File, error) {
	f, err := s.store.FileByID(id)
	if errors.Is(err, store.ErrNotFound) {
		return store.File{}, ErrNotFound
	}
	if err != nil {
		return store.File{}, err
	}
	return f, nil
}

// EditMetadata applies an Admin's hand-edit to a Title's descriptive fields and
// Locks each edited field (CONTEXT.md "Locked field"), then returns the updated
// detail. ErrNotFound for an unknown Title so the api layer answers 404 (hide
// existence). Identity (identity_key / watch state) is never touched — only
// descriptive columns, child rows, and lock rows change (ADR-0002).
func (s *Service) EditMetadata(titleID string, edit store.MetadataEdit) (store.TitleDetail, error) {
	if _, err := s.getTitleDetail(titleID); err != nil {
		return store.TitleDetail{}, err // ErrNotFound flows through
	}
	if err := s.store.WriteTitleMetadata(titleID, edit); err != nil {
		return store.TitleDetail{}, err
	}
	return s.getTitleDetail(titleID)
}

// ReleaseLock releases a Title's Lock on one field so the next enrich pass
// refreshes it again, returning the updated detail. ErrNotFound for an unknown
// Title. Releasing an absent lock is a no-op (idempotent).
func (s *Service) ReleaseLock(titleID, field string) (store.TitleDetail, error) {
	if _, err := s.getTitleDetail(titleID); err != nil {
		return store.TitleDetail{}, err // ErrNotFound flows through
	}
	// An artwork role is lock-coupled to any Admin upload: releasing it deletes the
	// uploaded row + cached file and reverts to the auto image (ADR-0026). A role
	// with no upload releases exactly like a descriptive field (lock dropped only).
	if isArtworkRole(field) {
		removed, err := s.store.ReleaseTitleArtworkLock(titleID, field)
		if err != nil {
			return store.TitleDetail{}, err
		}
		removeArtworkFile(removed)
	} else if err := s.store.ReleaseFieldLock(titleID, field); err != nil {
		return store.TitleDetail{}, err
	}
	return s.getTitleDetail(titleID)
}

// isArtworkRole reports whether a Locked field is an artwork role (poster /
// background / cover) rather than a descriptive field, so release can take the
// upload-aware path (ADR-0026). The set matches the pickable artwork roles.
func isArtworkRole(field string) bool {
	switch field {
	case "poster", "background", "cover":
		return true
	default:
		return false
	}
}

// removeArtworkFile deletes a released upload's cached bytes (best-effort — a
// dangling file is harmless). Empty path (the role had no upload) is a no-op.
func removeArtworkFile(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
}

// EntityExists reports whether a browse-parent (Show/Artist/Album) exists, so an
// Admin parent-edit endpoint answers 404 for an unknown id (hide existence). A
// Season is never edited (v1), so it is treated as non-existent for editing.
func (s *Service) EntityExists(entityType, entityID string) (bool, error) {
	var err error
	switch entityType {
	case store.EntityShow:
		_, err = s.store.ShowByID(entityID)
	case store.EntityArtist:
		_, err = s.store.ArtistByID(entityID)
	case store.EntityAlbum:
		_, err = s.store.AlbumByID(entityID)
	default:
		return false, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// EditEntityMetadata applies an Admin's hand-edit to a browse-parent's descriptive
// fields and Locks each edited field (the parent analogue of EditMetadata,
// ADR-0019 Fix-label for parents). ErrNotFound for an unknown parent (hide
// existence). Identity is never touched.
func (s *Service) EditEntityMetadata(entityType, entityID string, edit store.EntityMetadataEdit) error {
	ok, err := s.EntityExists(entityType, entityID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return s.store.WriteEntityMetadata(entityType, entityID, edit)
}

// ReleaseEntityLock releases a browse-parent's Lock on one field so the next
// enrich pass refreshes it again. ErrNotFound for an unknown parent; releasing an
// absent lock is a no-op (idempotent).
func (s *Service) ReleaseEntityLock(entityType, entityID, field string) error {
	ok, err := s.EntityExists(entityType, entityID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if isArtworkRole(field) {
		removed, err := s.store.ReleaseEntityArtworkLock(entityType, entityID, field)
		if err != nil {
			return err
		}
		removeArtworkFile(removed)
		return nil
	}
	return s.store.ReleaseEntityFieldLock(entityType, entityID, field)
}

// EntityLockedFields returns a browse-parent's Locked field names (stable order),
// for the parent detail's lockedFields[].
func (s *Service) EntityLockedFields(entityType, entityID string) ([]string, error) {
	return s.store.EntityLockedFieldsSorted(entityType, entityID)
}

// LibraryOfEntity returns the Library id a browse parent belongs to (for the
// realtime libraryUpdated nudge after a parent correction). ErrNotFound flows
// through for an unknown parent.
func (s *Service) LibraryOfEntity(entityType, entityID string) (string, error) {
	return s.store.LibraryOfEntity(entityType, entityID)
}

// ListUnmatched returns a Library's Unmatched files (the Admin attention
// surface). ErrNotFound for an unknown Library so the caller answers 404.
func (s *Service) ListUnmatched(libraryID string) ([]store.UnmatchedFile, error) {
	exists, err := s.store.LibraryExists(libraryID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	return s.store.ListUnmatched(libraryID)
}

// TitlesNeedingMatch returns the Library's Titles whose Enrichment could not
// settle on a record (status unmatched/failed) — the Admin attention surface for
// hand-matching, kept distinct from the identity Unmatched bucket and the
// needs-review list. ErrNotFound for an unknown Library so the caller answers 404.
func (s *Service) TitlesNeedingMatch(libraryID string) ([]store.Title, error) {
	exists, err := s.store.LibraryExists(libraryID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	return s.store.TitlesNeedingMatch(libraryID)
}

// NeedsReview returns every still-flagged needs-review item of a Library — the
// Titles (Movies, Episodes, Tracks) followed by the Shows — for the Admin
// attention surface. This is the identity-parse attention list (uncertain parse),
// distinct from the enrichment TitlesNeedingMatch above. ErrNotFound for an
// unknown Library so the caller answers 404.
func (s *Service) NeedsReview(libraryID string) ([]store.NeedsReviewItem, error) {
	exists, err := s.store.LibraryExists(libraryID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	titles, err := s.store.TitlesNeedingReview(libraryID)
	if err != nil {
		return nil, err
	}
	shows, err := s.store.ShowsNeedingReview(libraryID)
	if err != nil {
		return nil, err
	}
	items := append(titles, shows...)

	// Derive each item's fix-match anchor from its file path + the Library roots,
	// per kind — a Movie's folder (or the file itself when loose), a Show's
	// top-level folder, a Track's album folder; an Episode has none (its
	// needs-review is a numbering problem Enrichment maps, not a folder override).
	// store.NeedsReviewAnchor mirrors how the scanner keys each kind's override.
	lib, err := s.store.LibraryByID(libraryID)
	if err != nil {
		return nil, err
	}
	roots := make([]string, 0, len(lib.Roots))
	for _, r := range lib.Roots {
		roots = append(roots, r.Path)
	}
	for i := range items {
		items[i].Anchor = store.NeedsReviewAnchor(items[i].Kind, items[i].Path, roots)
	}
	return items, nil
}

// MarkTitleReviewed dismisses a Title's (Movie / Episode / Track) needs_review
// flag — an Admin confirmed the uncertain parse is fine. ErrNotFound for an
// unknown Title. MarkShowReviewed is the same for a Show.
func (s *Service) MarkTitleReviewed(id string) error {
	if err := s.store.MarkTitleReviewed(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// MarkShowReviewed dismisses a Show's needs_review flag. ErrNotFound for an
// unknown Show.
func (s *Service) MarkShowReviewed(id string) error {
	if err := s.store.MarkShowReviewed(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// Artwork returns the local artwork file for a Title+role, or ErrNotFound when
// the Title has no artwork in that role (the API serves the bytes).
// Artwork returns a Title's local artwork for a role, or ErrNotFound — including
// when the Title is outside the caller's access Scope (hide existence: an
// out-of-scope poster URL 404s exactly like a missing one). No-op under an
// all-access scope.
func (s *Service) Artwork(scope access.Scope, titleID, role string) (store.Artwork, error) {
	if err := s.guardTitle(scope, titleID); err != nil {
		return store.Artwork{}, err
	}
	a, err := s.store.ArtworkByTitleRole(titleID, role)
	if errors.Is(err, store.ErrNotFound) {
		return store.Artwork{}, ErrNotFound
	}
	if err != nil {
		return store.Artwork{}, err
	}
	return a, nil
}

// --- Watch state on Titles + the Home surface (issue 08) --------------------

// WatchStateFor returns the calling User's watch state (resume + watched) for a
// Title — the value the api layer attaches to the Title detail/summary so a
// client knows where to resume. Absence is the zero value (not started); never
// an error.
func (s *Service) WatchStateFor(userID, titleID string) (store.WatchState, error) {
	return s.store.WatchStateFor(userID, titleID)
}

// WatchStatesForTitles bulk-reads the User's watch state for a page of Titles,
// keyed by Title id (absent = not started). Used to decorate a browse list
// without an N+1 query.
func (s *Service) WatchStatesForTitles(userID string, titleIDs []string) (map[string]store.WatchState, error) {
	return s.store.WatchStatesForTitles(userID, titleIDs)
}

// GenresForTitles bulk-reads enriched genres for a page of Titles, keyed by id,
// so a browse list can attach genres without an N+1 query.
func (s *Service) GenresForTitles(titleIDs []string) (map[string][]string, error) {
	return s.store.GenresForTitles(titleIDs)
}

// ArtworkVersionsForTitles bulk-reads a per-Title artwork cache-bust version for
// a page of Titles, keyed by id (absent = no artwork), so a browse grid can
// reload only the posters whose artwork actually changed (no N+1).
func (s *Service) ArtworkVersionsForTitles(titleIDs []string) (map[string]string, error) {
	return s.store.ArtworkVersionsForTitles(titleIDs)
}

// TitleEnrichmentForMany bulk-reads enrichment display fields (overview /
// enriched_title / status) for a page of Titles, keyed by id, so the Home +
// search readers can surface enrichment without per-Title queries.
func (s *Service) TitleEnrichmentForMany(titleIDs []string) (map[string]store.Title, error) {
	return s.store.TitleEnrichmentForMany(titleIDs)
}

// HomeRow is one computed Home row: a label and the Titles in it, each paired
// with the resume position a client would seek to (0 for Recently Added).
type HomeRow struct {
	Titles []HomeTitle
}

// HomeTitle is a Title in a Home row plus its resume position (ms). Recently
// Added and Up Next entries carry resume 0; Continue Watching entries carry the
// stored resume. Episode carries the Show/Season/episode parent context for a
// non-Movie leaf (an Episode) so the row reads as "The Bear · S01E03"; it is nil
// for a Movie (issue tv-music/02, additive — Movie entries are unchanged).
type HomeTitle struct {
	store.Title
	ResumePositionMs int64
	Episode          *store.EpisodeContext
	// Track is the Artist/Album/disc/track parent context for a non-Movie Track
	// leaf (kind "track"), so a Home card reads as "Radiohead · OK Computer"; nil
	// for a Movie/Episode (issue tv-music/03, additive).
	Track *store.TrackContext
}

// Home is the per-User computed Home surface (CONTEXT.md): Continue Watching
// (in-progress Titles between the 2%/90% band, most-recently-played first),
// Up Next (TV-only: the next unwatched Episode in Show order for started Shows),
// and Recently Added (Titles newest-added first). All three are computed views,
// never stored entities, and all exclude hidden (all-Files-Missing) Titles.
//
// The 2%/90% band is enforced where the resume is WRITTEN (playback's
// ReportProgress), so Continue Watching here is simply "has a resume, not
// watched" — the threshold is not re-derived at read time. Continue Watching and
// Recently Added Episode entries are decorated with their parent context so a
// client can label them without an extra round-trip (issue tv-music/02).
func (s *Service) Home(scope access.Scope, userID string, limit int) (continueWatching, upNext, recentlyAdded HomeRow, err error) {
	// The Home rows are cross-Library aggregates, so the access filter is pushed
	// into each query (filtered in SQL). No-op under an all-access scope.
	filter := scope.StoreFilter()
	cw, err := s.store.ContinueWatching(userID, limit, filter)
	if err != nil {
		return HomeRow{}, HomeRow{}, HomeRow{}, err
	}
	continueWatching.Titles = make([]HomeTitle, 0, len(cw))
	for _, r := range cw {
		ht := HomeTitle{Title: r.Title, ResumePositionMs: r.ResumePositionMs}
		s.attachEpisodeContext(&ht)
		continueWatching.Titles = append(continueWatching.Titles, ht)
	}

	un, err := s.store.UpNext(userID, limit, filter)
	if err != nil {
		return HomeRow{}, HomeRow{}, HomeRow{}, err
	}
	upNext.Titles = make([]HomeTitle, 0, len(un))
	for _, r := range un {
		ht := HomeTitle{Title: r.Title}
		// Up Next is always an Episode; attach its parent context to label it.
		s.attachEpisodeContext(&ht)
		upNext.Titles = append(upNext.Titles, ht)
	}

	ra, err := s.store.RecentlyAdded(limit, filter)
	if err != nil {
		return HomeRow{}, HomeRow{}, HomeRow{}, err
	}
	recentlyAdded.Titles = make([]HomeTitle, 0, len(ra))
	for _, t := range ra {
		ht := HomeTitle{Title: t}
		s.attachEpisodeContext(&ht)
		recentlyAdded.Titles = append(recentlyAdded.Titles, ht)
	}

	// Decorate every Home leaf with its enrichment (overview/genres/display title)
	// in two bulk reads — the Home queries use the lean Title projection, so this
	// is how enrichment reaches the Home surface (issue 03).
	rows := []*HomeRow{&continueWatching, &upNext, &recentlyAdded}
	var ids []string
	for _, row := range rows {
		for i := range row.Titles {
			ids = append(ids, row.Titles[i].ID)
		}
	}
	enr, _ := s.store.TitleEnrichmentForMany(ids)
	genres, _ := s.store.GenresForTitles(ids)
	for _, row := range rows {
		for i := range row.Titles {
			if e, ok := enr[row.Titles[i].ID]; ok {
				row.Titles[i].Overview = e.Overview
				row.Titles[i].EnrichedTitle = e.EnrichedTitle
				row.Titles[i].EnrichmentStatus = e.EnrichmentStatus
			}
			row.Titles[i].Genres = genres[row.Titles[i].ID]
		}
	}
	return continueWatching, upNext, recentlyAdded, nil
}

// attachEpisodeContext fills ht.Episode with the Show/Season/episode parent
// context when ht is an Episode (kind "episode"); a Movie is left untouched
// (Episode stays nil). Reuses EpisodeContextForTitle (issue tv-music/01). A
// lookup miss is treated as "no context" rather than a fatal error — the row
// still renders as a bare title rather than failing the whole Home payload.
func (s *Service) attachEpisodeContext(ht *HomeTitle) {
	switch ht.Kind {
	case "episode":
		c, err := s.store.EpisodeContextForTitle(ht.ID)
		if err != nil {
			return
		}
		ctx := c
		ht.Episode = &ctx
	case "track":
		c, err := s.store.TrackContextForTitle(ht.ID)
		if err != nil {
			return
		}
		ctx := c
		ht.Track = &ctx
	}
}

// --- TV browse (issue tv-music/01) -----------------------------------------

// LibraryKind returns a Library's media kind ("movie"|"tv"|"music"), or
// ErrNotFound. The browse handler reads it to decide whether GET
// /libraries/{id}/titles returns Movies (Titles) or Shows.
func (s *Service) LibraryKind(libraryID string) (string, error) {
	lib, err := s.store.LibraryByID(libraryID)
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return lib.Kind, nil
}

// ShowsPage is one page of Show summaries plus the next-page cursor.
type ShowsPage struct {
	Shows      []store.Show
	NextCursor string
}

// ListShows returns one cursor-paginated page of a TV Library's Shows — the
// top-level grid for a TV Library (the analogue of ListTitles for Movies). It
// returns ErrNotFound for an unknown Library and ErrBadCursor for a bad cursor.
func (s *Service) ListShows(scope access.Scope, in ListInput) (ShowsPage, error) {
	if !scope.AllowsLibrary(in.LibraryID) {
		return ShowsPage{}, ErrNotFound
	}
	exists, err := s.store.LibraryExists(in.LibraryID)
	if err != nil {
		return ShowsPage{}, err
	}
	if !exists {
		return ShowsPage{}, ErrNotFound
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	var cursor *store.TitleCursor
	if in.Cursor != "" {
		c, err := decodeCursor(in.Cursor)
		if err != nil {
			return ShowsPage{}, ErrBadCursor
		}
		cursor = &c
	}
	page, err := s.store.ListShows(in.LibraryID, cursor, limit, in.Genre, scope.StoreFilter())
	if err != nil {
		return ShowsPage{}, err
	}
	out := ShowsPage{Shows: page.Shows}
	if page.HasMore && len(page.Shows) > 0 {
		last := page.Shows[len(page.Shows)-1]
		out.NextCursor = encodeCursor(store.TitleCursor{SortKey: last.SortTitle, ID: last.ID})
	}
	return out, nil
}

// Seasons returns a Show's Seasons (ordered, hidden excluded), or ErrNotFound for
// an unknown Show (so the handler answers 404, hide-existence).
func (s *Service) Seasons(scope access.Scope, showID string) (store.Show, []store.Season, error) {
	show, err := s.store.ShowByID(showID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Show{}, nil, ErrNotFound
	}
	if err != nil {
		return store.Show{}, nil, err
	}
	// A Show in a Library the caller may not access, or whose enriched maturity
	// rating is above their ceiling, is hidden as 404. No-op under all-access.
	if !scope.AllowsLibrary(show.LibraryID) || !s.showRatingAllowed(scope, showID) {
		return store.Show{}, nil, ErrNotFound
	}
	seasons, err := s.store.SeasonsForShow(showID)
	if err != nil {
		return store.Show{}, nil, err
	}
	return show, seasons, nil
}

// Episodes returns a Season's Episodes (Titles), or ErrNotFound for an unknown
// Season.
func (s *Service) Episodes(scope access.Scope, seasonID string) (store.Season, []store.Title, error) {
	season, err := s.store.SeasonByID(seasonID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Season{}, nil, ErrNotFound
	}
	if err != nil {
		return store.Season{}, nil, err
	}
	eps, err := s.store.EpisodesForSeason(seasonID)
	if err != nil {
		return store.Season{}, nil, err
	}
	// A Season carries no library_id of its own; its Episodes (Titles) do, and a
	// Season's Episodes all share one Library — so a single check on the first
	// Episode hides a Season in an inaccessible Library as 404. The Season's parent
	// Show is also rating-gated, so a direct hit on a hidden Show's Season 404s too
	// (not just via the grid). No-op under an all-access scope.
	if len(eps) > 0 && !scope.AllowsLibrary(eps[0].LibraryID) {
		return store.Season{}, nil, ErrNotFound
	}
	if !s.showRatingAllowed(scope, season.ShowID) {
		return store.Season{}, nil, ErrNotFound
	}
	return season, eps, nil
}

// showRatingAllowed reports whether a Show's enriched maturity rating is at or
// below the caller's ceiling. A Show with no enriched rating (or an unknown
// label) is visible — the same "unrated is visible" policy the SQL filter
// applies. Always true under an uncapped scope, so it short-circuits cheaply.
func (s *Service) showRatingAllowed(scope access.Scope, showID string) bool {
	if scope.RatingCeiling == 0 {
		return true
	}
	enr, err := s.store.EntityEnrichmentByID(store.EntityShow, showID)
	if err != nil {
		return true // no enrichment row → no rating info → visible
	}
	return scope.AllowsRating(enr.ContentRating)
}

// EpisodeContext returns the Show/Season/episode parent context for an Episode
// Title, or ErrNotFound when the Title is a Movie (no TV linkage) — the caller
// simply omits the context in that case.
func (s *Service) EpisodeContext(titleID string) (store.EpisodeContext, error) {
	c, err := s.store.EpisodeContextForTitle(titleID)
	if errors.Is(err, store.ErrNotFound) {
		return store.EpisodeContext{}, ErrNotFound
	}
	return c, err
}

// --- Music browse (issue tv-music/03) --------------------------------------

// ArtistsPage is one page of Artist summaries plus the next-page cursor.
type ArtistsPage struct {
	Artists    []store.Artist
	NextCursor string
}

// ListArtists returns one cursor-paginated page of a Music Library's Artists —
// the top-level list for a Music Library (the analogue of ListShows for TV). It
// returns ErrNotFound for an unknown Library and ErrBadCursor for a bad cursor.
func (s *Service) ListArtists(scope access.Scope, in ListInput) (ArtistsPage, error) {
	if !scope.AllowsLibrary(in.LibraryID) {
		return ArtistsPage{}, ErrNotFound
	}
	exists, err := s.store.LibraryExists(in.LibraryID)
	if err != nil {
		return ArtistsPage{}, err
	}
	if !exists {
		return ArtistsPage{}, ErrNotFound
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	var cursor *store.TitleCursor
	if in.Cursor != "" {
		c, err := decodeCursor(in.Cursor)
		if err != nil {
			return ArtistsPage{}, ErrBadCursor
		}
		cursor = &c
	}
	page, err := s.store.ListArtists(in.LibraryID, cursor, limit, in.Genre)
	if err != nil {
		return ArtistsPage{}, err
	}
	out := ArtistsPage{Artists: page.Artists}
	if page.HasMore && len(page.Artists) > 0 {
		last := page.Artists[len(page.Artists)-1]
		out.NextCursor = encodeCursor(store.TitleCursor{SortKey: last.SortName, ID: last.ID})
	}
	return out, nil
}

// Albums returns an Artist's Albums (ordered, hidden excluded), or ErrNotFound
// for an unknown Artist (so the handler answers 404, hide-existence).
func (s *Service) Albums(scope access.Scope, artistID string) (store.Artist, []store.Album, error) {
	artist, err := s.store.ArtistByID(artistID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Artist{}, nil, ErrNotFound
	}
	if err != nil {
		return store.Artist{}, nil, err
	}
	// An Artist in a Library the caller may not access is hidden as 404. No-op
	// under an all-access scope.
	if !scope.AllowsLibrary(artist.LibraryID) {
		return store.Artist{}, nil, ErrNotFound
	}
	albums, err := s.store.AlbumsForArtist(artistID)
	if err != nil {
		return store.Artist{}, nil, err
	}
	return artist, albums, nil
}

// Tracks returns an Album's Tracks (Titles) in disc/track order, or ErrNotFound
// for an unknown Album.
func (s *Service) Tracks(scope access.Scope, albumID string) (store.Album, store.Artist, []store.Title, error) {
	album, err := s.store.AlbumByID(albumID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Album{}, store.Artist{}, nil, ErrNotFound
	}
	if err != nil {
		return store.Album{}, store.Artist{}, nil, err
	}
	tracks, err := s.store.TracksForAlbum(albumID)
	if err != nil {
		return store.Album{}, store.Artist{}, nil, err
	}
	// An Album carries no library_id of its own; its Tracks (Titles) do and share
	// one Library, so a single check on the first Track hides an Album in an
	// inaccessible Library as 404. No-op under an all-access scope.
	if len(tracks) > 0 && !scope.AllowsLibrary(tracks[0].LibraryID) {
		return store.Album{}, store.Artist{}, nil, ErrNotFound
	}
	// The parent Artist supplies the album header's artist name/link. A missing
	// Artist row (data integrity) degrades to an empty Artist rather than failing
	// the whole listing.
	artist, err := s.store.ArtistByID(album.ArtistID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Album{}, store.Artist{}, nil, err
	}
	return album, artist, tracks, nil
}

// TrackDurations returns each Track's playable duration in milliseconds for an
// Album, keyed by Track (Title) id — so the Album track list can show each song's
// length without a per-Track detail fetch. A Track with no file is absent from the
// map (the API then reports 0). See store.TrackDurationsForAlbum.
func (s *Service) TrackDurations(albumID string) (map[string]int64, error) {
	return s.store.TrackDurationsForAlbum(albumID)
}

// AlbumArtwork returns the cover image for an Album, or ErrNotFound, honoring the
// serve precedence uploaded > local > fetched (ADR-0026). An Admin-uploaded cover
// (an entity_artwork 'uploaded' row) outranks even a local folder cover
// (cover.jpg/folder.jpg in albums.artwork_path); absent an upload, the local
// cover still WINS over a fetched/picked one (CONTEXT.md "local wins", unchanged).
// The Album's local cover lives in a column (albums.artwork_path), not an
// entity_artwork 'local' row, so precedence is resolved here rather than by
// EntityArtworkByRole's ordering alone. ErrNotFound when the Album's Library is
// outside the caller's Scope. No-op under an all-access scope.
func (s *Service) AlbumArtwork(scope access.Scope, albumID string) (store.Artwork, error) {
	if err := s.guardEntity(scope, store.EntityAlbum, albumID); err != nil {
		return store.Artwork{}, err
	}
	// The entity_artwork winner (uploaded, else fetched — there is no local
	// entity_artwork row for an Album). An upload beats the folder cover below.
	ent, eerr := s.store.EntityArtworkByRole(store.EntityAlbum, albumID, "cover")
	if eerr != nil && !errors.Is(eerr, store.ErrNotFound) {
		return store.Artwork{}, eerr
	}
	if eerr == nil && ent.Source == "uploaded" {
		return ent, nil
	}
	// No upload: a local folder cover wins over a fetched cover (unchanged rule).
	local, lerr := s.store.AlbumArtworkByID(albumID)
	if lerr == nil {
		return local, nil
	}
	if !errors.Is(lerr, store.ErrNotFound) {
		return store.Artwork{}, lerr
	}
	// Neither uploaded nor local: fall back to a fetched cover if one exists.
	if errors.Is(eerr, store.ErrNotFound) {
		return store.Artwork{}, ErrNotFound
	}
	return ent, nil
}

// --- Parent-entity Enrichment reads (issue 03) ------------------------------

// EntityEnrichment returns a browse parent's enriched metadata (overview,
// genres, content rating, network), or the zero value (Status "pending") when it
// has never been enriched. entityType is one of store.Entity{Show,Season,Artist,
// Album}.
func (s *Service) EntityEnrichment(entityType, entityID string) (store.EntityEnrichment, error) {
	return s.store.EntityEnrichmentByID(entityType, entityID)
}

// EntityCredits returns a browse parent's enriched cast in billing order (a Show's
// series main cast), each credit carrying its person ref + headshot cache-bust
// version so the detail can render the same cast strip a Movie does (cast-photos/02).
// Empty for a parent with no captured cast. entityType is one of store.Entity{Show,
// Season,Artist,Album}.
func (s *Service) EntityCredits(entityType, entityID string) ([]store.Credit, error) {
	return s.store.EntityCredits(entityType, entityID)
}

// EntityEnrichmentForMany bulk-reads parent enrichment for a page of entity ids,
// keyed by id (absent = never enriched). Lets the Show grid attach overview/
// genres without an N+1 query.
func (s *Service) EntityEnrichmentForMany(entityType string, ids []string) (map[string]store.EntityEnrichment, error) {
	return s.store.EntityEnrichmentForMany(entityType, ids)
}

// EntityArtworkRoles returns, per entity id, which artwork roles it has fetched,
// so a grid can advertise a poster URL only when one exists.
func (s *Service) EntityArtworkRoles(entityType string, ids []string) (map[string]map[string]bool, error) {
	return s.store.EntityArtworkRolesForMany(entityType, ids)
}

// EntityArtworkVersions returns, per entity id, an opaque artwork cache-bust
// version (newest fetched-artwork timestamp), so a browse client busts a parent
// poster's URL only when its image changed (e.g. across a re-enrich).
func (s *Service) EntityArtworkVersions(entityType string, ids []string) (map[string]string, error) {
	return s.store.EntityArtworkVersionsForMany(entityType, ids)
}

// EntityArtwork returns the on-disk fetched artwork for a parent entity + role,
// or ErrNotFound (the API serves the bytes).
// EntityArtwork returns a browse parent's fetched artwork, or ErrNotFound —
// including when the entity's Library is outside the caller's Scope. No-op under
// an all-access scope.
func (s *Service) EntityArtwork(scope access.Scope, entityType, entityID, role string) (store.Artwork, error) {
	if err := s.guardEntity(scope, entityType, entityID); err != nil {
		return store.Artwork{}, err
	}
	a, err := s.store.EntityArtworkByRole(entityType, entityID, role)
	if errors.Is(err, store.ErrNotFound) {
		return store.Artwork{}, ErrNotFound
	}
	if err != nil {
		return store.Artwork{}, err
	}
	return a, nil
}

// PersonArtwork returns a cast member's fetched headshot for a role, or
// ErrNotFound — including when the person is not credited by any Title in the
// caller's access Scope (hide existence: an out-of-scope or photoless person's
// headshot URL 404s exactly like a missing one, so access control never leaks
// through the cast strip). Access follows the Titles that credit the person: the
// photo is served when the caller can reach at least one crediting Library. A
// person credited by no Title (unknown ref) is ErrNotFound. No-op access under an
// all-access scope, which still requires a cached photo to serve (cast-photos/01).
func (s *Service) PersonArtwork(scope access.Scope, personRef, role string) (store.Artwork, error) {
	libs, err := s.store.LibrariesCreditingPerson(personRef)
	if err != nil {
		return store.Artwork{}, err
	}
	allowed := false
	for _, lib := range libs {
		if scope.AllowsLibrary(lib) {
			allowed = true
			break
		}
	}
	if !allowed {
		return store.Artwork{}, ErrNotFound // unknown ref, or every crediting Library is out of scope
	}
	a, err := s.store.PersonArtworkByRef(personRef, role)
	if errors.Is(err, store.ErrNotFound) {
		return store.Artwork{}, ErrNotFound
	}
	if err != nil {
		return store.Artwork{}, err
	}
	return a, nil
}

// TrackContext returns the Artist/Album/disc/track parent context for a Track
// Title, or ErrNotFound when the Title is not a Track — the caller omits it.
func (s *Service) TrackContext(titleID string) (store.TrackContext, error) {
	c, err := s.store.TrackContextForTitle(titleID)
	if errors.Is(err, store.ErrNotFound) {
		return store.TrackContext{}, ErrNotFound
	}
	return c, err
}

// --- Cross-kind search + Show watched affordance (issue tv-music/04) --------

// Search returns catalog entities whose display name matches query, grouped by
// kind (Movies, Shows, Artists, Albums, Episodes, Tracks), hidden-excluded. The
// per-group cap is clamped to the same sane range as a browse page. An empty
// query returns empty results (the handler still answers 200 with empty groups).
func (s *Service) Search(scope access.Scope, query string, limit int) (store.SearchResults, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	// Search spans every Library, so the access filter is applied in SQL across
	// each kind's query (filtered before rows leave the store). No-op under an
	// all-access scope.
	return s.store.Search(query, limit, scope.StoreFilter())
}

// UnwatchedEpisodeCounts returns the per-User unwatched-Episode count for each of
// the given Shows (absent = 0), so the TV grid can show a watched affordance on a
// Show poster the way a Movie shows a resume bar.
func (s *Service) UnwatchedEpisodeCounts(userID string, showIDs []string) (map[string]int, error) {
	return s.store.UnwatchedEpisodeCounts(userID, showIDs)
}

// cursorSortKey extracts the sort-key value of a Title for the active sort, so
// the next page seeks strictly past it.
func cursorSortKey(sort Sort, t store.Title) string {
	if sort == SortDateAdded {
		return t.AddedAt
	}
	return t.SortTitle
}
