// Package organize is the authored-grouping domain: the Admin-curated, shared
// Collection (and, in a later slice, the User-owned ordered Playlist). It is the
// counterpart to catalog — where catalog is the read-only SCANNED catalog,
// organize owns the hand-AUTHORED groupings over it (CONTEXT.md "Organization").
//
// Like catalog, it is transport-agnostic: it speaks Collections and Title ids,
// not HTTP, over a narrow Store interface that *store.DB satisfies, and returns
// domain errors the api layer maps to HTTP envelopes. Its core operation —
// "resolve a grouping's member Title ids into visible, ordered, lean Title rows"
// (ResolveMembers) — is the shared seam Collections use now and Playlists
// (issue 03) reuse; the api layer decorates those rows with the existing catalog
// bulk readers + toTitleSummary, so a grouping grid is identical to a browse grid.
package organize

import (
	"errors"
	"strings"

	"github.com/marioquake/juicebox/internal/access"
	"github.com/marioquake/juicebox/internal/store"
	"github.com/google/uuid"
)

// Domain errors mapped to HTTP by the api layer.
var (
	// ErrNotFound means the requested Collection does not exist (→ 404, hide
	// existence per api-contract.md).
	ErrNotFound = errors.New("organize: not found")
	// ErrUnknownTitle means an item-add named a Title id that does not exist; the
	// whole add is rejected and the membership set is left unchanged (→ 422).
	ErrUnknownTitle = errors.New("organize: unknown title in item set")
	// ErrValidation means a Collection write was missing a required field (a blank
	// name) (→ 400).
	ErrValidation = errors.New("organize: collection name is required")
	// ErrItemSetMismatch means a Playlist reorder payload's item-id set did not
	// EXACTLY match the Playlist's current items (a missing, foreign/unknown, or
	// duplicated id); the reorder is rejected as a no-op (→ 422 ITEM_SET_MISMATCH).
	ErrItemSetMismatch = errors.New("organize: playlist reorder item set mismatch")
	// ErrSystemPlaylist means a write targeted a system Playlist (e.g. the
	// Watchlist) that the User owns but may not rename or delete — it belongs to the
	// system, not the User (→ 422 SYSTEM_PLAYLIST).
	ErrSystemPlaylist = errors.New("organize: system playlist cannot be renamed or deleted")
)

// Store is the persistence the organize service reads/writes. *store.DB
// satisfies it. It is a narrow interface so the service stays unit-testable and
// the seam explicit, mirroring catalog.Store.
type Store interface {
	CreateCollection(id, name, description string) (store.Collection, error)
	UpdateCollection(id, name, description string) (store.Collection, error)
	DeleteCollection(id string) error
	CollectionByID(id string) (store.Collection, error)
	ListCollections() ([]store.Collection, error)
	AddCollectionItems(collectionID string, titleIDs []string) error
	RemoveCollectionItem(collectionID, titleID string) error
	// CollectionMemberIDs returns a Collection's member Title ids in stable
	// sort_title order (Missing members included; ResolveVisibleTitles filters them).
	CollectionMemberIDs(collectionID string) ([]string, error)
	// ResolveVisibleTitles maps Title ids to their lean (enriched-summary) rows,
	// omitting Missing/hidden ones AND any the access filter excludes (ungranted
	// Library / above ceiling) — the shared grouping member-resolution read.
	ResolveVisibleTitles(titleIDs []string, filter store.AccessFilter) (map[string]store.Title, error)

	// --- Playlists (collections-playlists 03): the User-owned, ordered,
	// single-kind half of the domain. ---
	CreatePlaylist(id, ownerUserID, name string) (store.Playlist, error)
	PlaylistByID(id string) (store.Playlist, error)
	// EnsureSystemPlaylist returns the caller's system Playlist for the given slug,
	// lazily creating it if absent (the get-or-create behind the Watchlist).
	EnsureSystemPlaylist(ownerUserID, system, name string) (store.Playlist, error)
	ListPlaylistsByOwner(ownerUserID string) ([]store.Playlist, error)
	UpdatePlaylistName(id, name string) (store.Playlist, error)
	DeletePlaylist(id string) error
	// TitleKind returns a Title's raw kind (movie|episode|track) or ErrUnknownTitle.
	TitleKind(id string) (string, error)
	// AppendPlaylistItem appends a Title at the end and returns the new item id,
	// fixing the Playlist kind to mappedKind on the first item.
	AppendPlaylistItem(playlistID, titleID, mappedKind string) (string, error)
	// PlaylistItemsInOrder returns a Playlist's (itemId, titleId) pairs in position
	// order (Missing/duplicates included; the service resolves + filters them).
	PlaylistItemsInOrder(playlistID string) ([]store.PlaylistItem, error)
	// ReorderPlaylistItems rewrites a Playlist's order to exactly the itemIDs
	// sequence (ErrItemSetMismatch if the set doesn't match the current items).
	ReorderPlaylistItems(playlistID string, itemIDs []string) error
	// RemovePlaylistItem removes one entry by its item id (ErrNotFound if no such
	// row belongs to the Playlist).
	RemovePlaylistItem(playlistID, itemID string) error
}

// Service implements the Collection operations (and the shared member resolution
// Playlists will reuse). Constructed once and shared.
type Service struct {
	store Store
}

// NewService builds the organize service over the given store.
func NewService(s Store) *Service { return &Service{store: s} }

// Create makes a new Collection. The name is required (trimmed); description is
// optional. Returns ErrValidation for a blank name.
func (s *Service) Create(name, description string) (store.Collection, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.Collection{}, ErrValidation
	}
	return s.store.CreateCollection(uuid.NewString(), name, strings.TrimSpace(description))
}

// Update renames a Collection and replaces its description. ErrValidation for a
// blank name; ErrNotFound for an unknown id.
func (s *Service) Update(id, name, description string) (store.Collection, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.Collection{}, ErrValidation
	}
	c, err := s.store.UpdateCollection(id, name, strings.TrimSpace(description))
	if errors.Is(err, store.ErrNotFound) {
		return store.Collection{}, ErrNotFound
	}
	return c, err
}

// Delete removes a Collection (its membership rows cascade away). ErrNotFound for
// an unknown id.
func (s *Service) Delete(id string) error {
	if err := s.store.DeleteCollection(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// Get returns one Collection, or ErrNotFound.
func (s *Service) Get(id string) (store.Collection, error) {
	c, err := s.store.CollectionByID(id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Collection{}, ErrNotFound
	}
	return c, err
}

// List returns every Collection (newest-created first).
func (s *Service) List() ([]store.Collection, error) {
	return s.store.ListCollections()
}

// AddItems adds Titles to a Collection idempotently (re-adding a member is a
// no-op). ErrNotFound for an unknown Collection; ErrUnknownTitle if any id is not
// a real Title (the whole add is rejected, set unchanged).
func (s *Service) AddItems(id string, titleIDs []string) error {
	err := s.store.AddCollectionItems(id, titleIDs)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, store.ErrUnknownTitle):
		return ErrUnknownTitle
	}
	return err
}

// RemoveItem removes one Title from a Collection. ErrNotFound for an unknown
// Collection; removing a non-member Title is a harmless no-op.
func (s *Service) RemoveItem(id, titleID string) error {
	if _, err := s.Get(id); err != nil {
		return err // ErrNotFound flows through
	}
	return s.store.RemoveCollectionItem(id, titleID)
}

// Members resolves a Collection's VISIBLE member Titles in stable sort_title
// order, FILTERED to the viewer's access Scope — Missing (hidden) members are
// omitted from the view but their membership persists (ADR-0008), and a member in
// an ungranted Library or above the viewer's Rating ceiling is absent for that
// viewer (filtered in the store). An Admin's all-access Scope sees full membership.
// ErrNotFound for an unknown Collection. The returned rows are lean
// (enriched-summary, no nested Editions); the api layer decorates them with the
// catalog bulk readers.
//
// scope is the FIRST param (consistent with catalog.Service methods); it is the
// viewer's resolved access, projected to a store filter via scope.StoreFilter().
func (s *Service) Members(scope access.Scope, id string) ([]store.Title, error) {
	if _, err := s.Get(id); err != nil {
		return nil, err // ErrNotFound flows through
	}
	ids, err := s.store.CollectionMemberIDs(id)
	if err != nil {
		return nil, err
	}
	return s.ResolveMembers(scope, ids)
}

// ResolveMembers maps an ORDERED list of member Title ids to their visible lean
// Title rows in the SAME order, omitting any that are Missing/hidden (ADR-0008),
// no longer exist, OR are outside the viewer's access Scope (ungranted Library /
// above ceiling). It is the shared grouping-resolution step Playlists (issue 04)
// reuse: a Collection passes its ids in sort_title order, a Playlist will pass them
// in position order, and both get the identical "resolve → omit-Missing →
// access-filter → preserve order" behavior from one place. scope.StoreFilter()
// carries the access predicate into the store; an all-access (Admin) Scope adds
// no predicate, so the full membership is returned.
func (s *Service) ResolveMembers(scope access.Scope, orderedTitleIDs []string) ([]store.Title, error) {
	visible, err := s.store.ResolveVisibleTitles(orderedTitleIDs, scope.StoreFilter())
	if err != nil {
		return nil, err
	}
	out := make([]store.Title, 0, len(orderedTitleIDs))
	for _, tid := range orderedTitleIDs {
		if t, ok := visible[tid]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}
