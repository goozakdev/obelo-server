package link

import (
	"fmt"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The home side of the mirror (ADR-0056 §3): what a pulled Export means once it
// has arrived.
//
// This file owns the WIRE — the shapes issue 05 publishes, read back — and the
// one decision the feed's shape forces on this side: the entities arrive ordered
// by change time, and they have to be applied parent-before-child. Where the rows
// actually land is store/mirror.go, which is the inverse of store/export.go and
// is meant to be read beside it.

// MirrorStore is everything the mirror writes through. *store.DB satisfies it.
// It is separate from Store (the Link rows) because a Server can hold Links
// without ever having pulled one, and a narrow unit test of the link flow should
// not have to implement a catalog.
type MirrorStore interface {
	UpsertLinkedLibrary(id, name, kind, linkID, remoteLibraryID string) (store.Library, error)
	LibrariesForLink(linkID string) ([]store.Library, error)
	DeleteLibrariesForLink(linkID string) (int, error)
	SetLibraryCheckpoint(id, checkpoint string) error
	ApplyMirror(libraryID string, entities []store.MirrorEntity, full bool) error
}

// ExportEntity is one row of a sharer's feed as it arrives. It is issue 05's
// exportEntityJSON read from the other direction, and the field tags are the
// contract — a rename here is a protocol break, not a refactor.
type ExportEntity struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	ParentID  string         `json:"parentId"`
	UpdatedAt string         `json:"updatedAt"`
	DeletedAt string         `json:"deletedAt"`
	Data      map[string]any `json:"data"`
}

// ExportLibrary is the `library` block every page of the feed repeats: which
// Library this is, over there.
type ExportLibrary struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// ExportPage is one page of the feed.
type ExportPage struct {
	LinkProtocolVersion int            `json:"linkProtocolVersion"`
	Library             ExportLibrary  `json:"library"`
	Entities            []ExportEntity `json:"entities"`
	NextCursor          string         `json:"nextCursor"`
	Checkpoint          string         `json:"checkpoint"`
}

// Apply writes one pull into a linked Library and records where the feed had
// reached (ADR-0056 §4). `full` says the entities are the whole Library rather
// than a change set — see store.DB.ApplyMirror, which is where that distinction
// does its work.
//
// linkID is taken, and checked, so a caller cannot apply one friend's feed into
// another friend's shelf: the remote ids are only meaningful under the Link they
// came from, and a mix-up there would be silent and permanent.
func (s *Service) Apply(linkID, libraryID string, entities []ExportEntity, checkpoint string, full bool) error {
	if s.mirror == nil {
		return ErrNoMirror
	}
	libs, err := s.mirror.LibrariesForLink(linkID)
	if err != nil {
		return err
	}
	found := false
	for _, l := range libs {
		if l.ID == libraryID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("link: library %q did not come over link %q", libraryID, linkID)
	}

	rows := make([]store.MirrorEntity, 0, len(entities))
	for _, e := range entities {
		rows = append(rows, store.MirrorEntity{
			Type:     e.Type,
			RemoteID: e.ID,
			ParentID: e.ParentID,
			Deleted:  e.DeletedAt != "",
			Data:     e.Data,
		})
	}
	if err := s.mirror.ApplyMirror(libraryID, rows, full); err != nil {
		return err
	}
	if checkpoint == "" {
		return nil
	}
	return s.mirror.SetLibraryCheckpoint(libraryID, checkpoint)
}
