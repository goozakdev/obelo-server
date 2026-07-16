package organize

import (
	"errors"
	"strings"

	"github.com/marioquake/juicebox/internal/access"
	"github.com/marioquake/juicebox/internal/store"
	"github.com/google/uuid"
)

// The User-owned, ordered, single-media-kind Playlist (collections-playlists 03) —
// the second half of the organize domain, mirroring the Collection half but with
// two policy knobs flipped: OWNERSHIP (a Playlist is private to one User; a
// non-owner — INCLUDING an Admin, no override — is hidden a 404, the same
// existence-hiding as another User's playback session) and ORDERING + SINGLE-KIND
// (an explicit position, and one media kind fixed by the first item).

// ErrKindMismatch means an append named a Title whose kind does not map to the
// Playlist's already-fixed kind (a Movie into a music Playlist, etc.). The append
// is rejected and the Playlist's kind is left unchanged (→ 422 KIND_MISMATCH).
var ErrKindMismatch = errors.New("organize: title kind does not match playlist kind")

// watchlistSystemSlug is the stable `system` slug of the per-User Watchlist (the
// one system Playlist today); watchlistName is the display name a freshly-seeded
// Watchlist gets.
const (
	watchlistSystemSlug = "watchlist"
	watchlistName       = "Watchlist"
)

// PlaylistMember pairs a resolved, visible member Title with the playlist_items id
// of the entry that points at it. The item id is what makes DUPLICATES
// distinguishable (the same Title added twice yields two members with the same
// Title summary but distinct ItemIDs) and is what issue 04's remove-by-item-id
// will target.
type PlaylistMember struct {
	ItemID string
	Title  store.Title
}

// playlistKindForTitle maps a raw Title kind (movie|episode|track) onto the
// Playlist media kind (movie|tv|music). It returns "" for an unrecognized kind so
// the caller can reject defensively (titles.kind is always one of the three today).
func playlistKindForTitle(titleKind string) string {
	switch titleKind {
	case "movie":
		return "movie"
	case "episode":
		return "tv"
	case "track":
		return "music"
	default:
		return ""
	}
}

// CreatePlaylist makes a new empty, untyped (kind "") Playlist owned by
// ownerUserID. The name is required (trimmed); ErrValidation for a blank name.
func (s *Service) CreatePlaylist(ownerUserID, name string) (store.Playlist, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.Playlist{}, ErrValidation
	}
	return s.store.CreatePlaylist(uuid.NewString(), ownerUserID, name)
}

// ListPlaylists returns ONLY ownerUserID's own Playlists (newest-created first),
// each carrying its raw ItemCount. Another User's Playlists are never returned —
// ownership privacy starts here.
func (s *Service) ListPlaylists(ownerUserID string) ([]store.Playlist, error) {
	return s.store.ListPlaylistsByOwner(ownerUserID)
}

// getOwned fetches a Playlist and enforces ownership: an unknown id OR a Playlist
// owned by someone else BOTH return ErrNotFound, so a non-owner can never tell the
// difference between "does not exist" and "not yours" (404 hide-existence, no Admin
// override). Every owner-scoped read/write funnels through this one gate.
func (s *Service) getOwned(ownerUserID, id string) (store.Playlist, error) {
	p, err := s.store.PlaylistByID(id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Playlist{}, ErrNotFound
	}
	if err != nil {
		return store.Playlist{}, err
	}
	if p.OwnerUserID != ownerUserID {
		return store.Playlist{}, ErrNotFound
	}
	return p, nil
}

// GetPlaylist returns one of ownerUserID's Playlists, or ErrNotFound (unknown id OR
// not owned by the caller).
func (s *Service) GetPlaylist(ownerUserID, id string) (store.Playlist, error) {
	return s.getOwned(ownerUserID, id)
}

// RenamePlaylist renames one of ownerUserID's Playlists. ErrValidation for a blank
// name; ErrNotFound for an unknown/foreign id.
func (s *Service) RenamePlaylist(ownerUserID, id, name string) (store.Playlist, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.Playlist{}, ErrValidation
	}
	p, err := s.getOwned(ownerUserID, id)
	if err != nil {
		return store.Playlist{}, err // ErrNotFound flows through (incl. non-owner)
	}
	if p.System != "" {
		return store.Playlist{}, ErrSystemPlaylist // the Watchlist et al. can't be renamed
	}
	return s.store.UpdatePlaylistName(id, name)
}

// DeletePlaylist deletes one of ownerUserID's Playlists (its item rows cascade
// away). ErrNotFound for an unknown/foreign id.
func (s *Service) DeletePlaylist(ownerUserID, id string) error {
	p, err := s.getOwned(ownerUserID, id)
	if err != nil {
		return err // ErrNotFound flows through (incl. non-owner)
	}
	if p.System != "" {
		return ErrSystemPlaylist // the Watchlist et al. can't be deleted
	}
	return s.store.DeletePlaylist(id)
}

// Watchlist returns ownerUserID's Watchlist (the per-User system Playlist),
// creating it on first access if it does not exist yet. It always exists after
// this returns — the guarantee the "add to watchlist" affordance and future
// watchlist features rely on.
func (s *Service) Watchlist(ownerUserID string) (store.Playlist, error) {
	return s.store.EnsureSystemPlaylist(ownerUserID, watchlistSystemSlug, watchlistName)
}

// AppendToWatchlist adds a Title to the END of ownerUserID's Watchlist, ensuring
// the Watchlist exists first, and returns the new item id. It reuses the ordinary
// append (so the single-kind rule applies: the first movie fixes the Watchlist to
// the "movie" kind, and a later cross-kind add is ErrKindMismatch). ErrUnknownTitle
// for an unknown Title. Duplicates are allowed (a Title may be on the Watchlist more
// than once, each its own item).
func (s *Service) AppendToWatchlist(ownerUserID, titleID string) (string, error) {
	wl, err := s.Watchlist(ownerUserID)
	if err != nil {
		return "", err
	}
	return s.AppendPlaylistItem(ownerUserID, wl.ID, titleID)
}

// AppendPlaylistItem appends a Title to the END of one of ownerUserID's Playlists
// and returns the new item id. It enforces the single-kind rule: the first item
// fixes the Playlist kind (Movie→movie, Episode→tv, Track→music); a subsequent add
// whose Title maps to a different kind is rejected with ErrKindMismatch and the
// Playlist kind is left unchanged. ErrNotFound for an unknown/foreign Playlist;
// ErrUnknownTitle for an unknown Title. Duplicates are allowed (a Title may be
// appended more than once, each its own item).
func (s *Service) AppendPlaylistItem(ownerUserID, id, titleID string) (string, error) {
	p, err := s.getOwned(ownerUserID, id)
	if err != nil {
		return "", err // ErrNotFound flows through (incl. non-owner)
	}
	titleKind, err := s.store.TitleKind(titleID)
	if errors.Is(err, store.ErrUnknownTitle) {
		return "", ErrUnknownTitle
	}
	if err != nil {
		return "", err
	}
	mapped := playlistKindForTitle(titleKind)
	if mapped == "" {
		// Defensive: a Title kind we don't know how to map can't join a Playlist.
		return "", ErrKindMismatch
	}
	// Single-kind: once typed, every further item must map to the SAME kind.
	if p.Kind != "" && p.Kind != mapped {
		return "", ErrKindMismatch
	}
	return s.store.AppendPlaylistItem(id, titleID, mapped)
}

// ReorderPlaylistItems rewrites the order of one of ownerUserID's Playlists to the
// given item-id sequence (collections-playlists 04). It funnels through getOwned,
// so a non-owner (Member OR Admin, no override) gets ErrNotFound, exactly like every
// other owner-scoped op. itemIDs must be the FULL desired order of the Playlist's
// currently VISIBLE item ids under `scope` — the same set PlaylistMembers returned —
// with no missing/foreign/duplicate id, or the reorder is rejected as a no-op with
// ErrItemSetMismatch, leaving the order intact. The rewrite is transactional and
// atomic. Idempotent (same input → same order).
//
// The scope is what keeps reorder reachable: members hidden from PlaylistMembers
// (Missing, or out of scope) keep their place in the sequence rather than counting
// against a payload the caller had no way to name them in. See the store method.
func (s *Service) ReorderPlaylistItems(scope access.Scope, ownerUserID, id string, itemIDs []string) error {
	if _, err := s.getOwned(ownerUserID, id); err != nil {
		return err // ErrNotFound flows through (incl. non-owner)
	}
	switch err := s.store.ReorderPlaylistItems(id, itemIDs, scope.StoreFilter()); {
	case errors.Is(err, store.ErrItemSetMismatch):
		return ErrItemSetMismatch
	case errors.Is(err, store.ErrNotFound):
		// Should not happen after getOwned, but translate to the domain error.
		return ErrNotFound
	default:
		return err
	}
}

// RemovePlaylistItem removes one entry of one of ownerUserID's Playlists by its
// item id (collections-playlists 04). It funnels through getOwned (a non-owner —
// Member OR Admin — gets ErrNotFound). An itemID that is not a row of THIS Playlist
// is ErrNotFound too (hide-existence). Removal is by item id, not Title id, so a
// duplicate Title's other entry is untouched, and the surviving entries keep their
// relative order (positions are not renumbered). The Playlist's fixed kind PERSISTS
// even when the last item is removed.
func (s *Service) RemovePlaylistItem(ownerUserID, id, itemID string) error {
	if _, err := s.getOwned(ownerUserID, id); err != nil {
		return err // ErrNotFound flows through (incl. non-owner)
	}
	if err := s.store.RemovePlaylistItem(id, itemID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound // unknown item / foreign Playlist → 404
		}
		return err
	}
	return nil
}

// PlaylistMembers resolves one of ownerUserID's Playlists into its VISIBLE member
// Titles in position order, each paired with its item id. It reuses the shared
// ResolveVisibleTitles read: items are read in position order, the Titles resolved
// through the owner's access Scope (Missing/hidden and out-of-scope members omitted,
// ADR-0008), and each ORDERED item paired with its resolved Title — preserving order
// AND duplicates (the map is keyed by Title id, but iteration is over the ordered
// item list, so a twice-added Title yields two members with the same summary and
// DISTINCT item ids). The item rows for omitted members persist (their survival is
// issue 04's explicit test). ErrNotFound for an unknown/foreign Playlist.
func (s *Service) PlaylistMembers(scope access.Scope, ownerUserID, id string) ([]PlaylistMember, error) {
	if _, err := s.getOwned(ownerUserID, id); err != nil {
		return nil, err // ErrNotFound flows through (incl. non-owner)
	}
	items, err := s.store.PlaylistItemsInOrder(id)
	if err != nil {
		return nil, err
	}
	titleIDs := make([]string, 0, len(items))
	for _, it := range items {
		titleIDs = append(titleIDs, it.TitleID)
	}
	visible, err := s.store.ResolveVisibleTitles(titleIDs, scope.StoreFilter())
	if err != nil {
		return nil, err
	}
	out := make([]PlaylistMember, 0, len(items))
	for _, it := range items {
		if t, ok := visible[it.TitleID]; ok {
			out = append(out, PlaylistMember{ItemID: it.ID, Title: t})
		}
	}
	return out, nil
}
