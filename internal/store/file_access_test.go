package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// LibraryAndRatingOfFile is the access guard behind the sessionless direct-file
// download: that route addresses a File by its own id and never loads a Title,
// so this join is the only thing that can tell the caller's Scope which Library
// the bytes live in and what they are rated.

// TestLibraryAndRatingOfFileResolvesOwningTitle: the happy path — a File resolves
// to the Library and Content rating of the Title that owns it, through
// files → editions → titles, and an unknown File id is ErrNotFound.
func TestLibraryAndRatingOfFileResolvesOwningTitle(t *testing.T) {
	db := openTemp(t)

	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmov','Movies','movie')`)
	mustExec(t, db,
		`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, content_rating)
		 VALUES ('mv1','libmov','movie','Movie','mv|1','movie','R')`)
	mustExec(t, db, `INSERT INTO editions (id, title_id) VALUES ('ed1','mv1')`)
	mustExec(t, db, `INSERT INTO files (id, edition_id, path) VALUES ('f1','ed1','/movies/a.mkv')`)

	lib, rating, err := db.LibraryAndRatingOfFile("f1")
	if err != nil {
		t.Fatalf("LibraryAndRatingOfFile: %v", err)
	}
	if lib != "libmov" || rating != "R" {
		t.Errorf("LibraryAndRatingOfFile = (%q, %q), want (\"libmov\", \"R\")", lib, rating)
	}

	if _, _, err := db.LibraryAndRatingOfFile("no-such-file"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown file id err = %v, want ErrNotFound", err)
	}
}

// TestLibraryAndRatingOfFileFailsClosedOnOrphan: a File whose Edition/Title
// chain is broken — a torn write, a half-finished prune, a hand-edited DB — is
// ErrNotFound, EVEN THOUGH the file row itself still loads fine. That gap is the
// whole risk: the download route asks this query "may the caller have these
// bytes?", and an orphan has no owning Title to answer with, so the only safe
// answer is "no such file". The INNER JOINs are what produce it; a LEFT JOIN
// would answer with an empty library_id and an empty (== unrated) content_rating,
// which any all-access Scope happily allows.
//
// The orphan is manufactured with foreign_keys OFF on a dedicated connection,
// because the schema's ON DELETE CASCADE makes this state unreachable through
// normal writes — which is exactly why it needs a test rather than trust.
func TestLibraryAndRatingOfFileFailsClosedOnOrphan(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	// The pragma is per-CONNECTION, so the inserts must ride the same one — and the
	// pool is capped at a single connection (ADR-0007's single-writer model), so it
	// has to be handed back before any read below, or the read waits forever.
	func() {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("dedicated conn: %v", err)
		}
		defer conn.Close()
		for _, q := range []string{
			`PRAGMA foreign_keys=OFF`,
			// An Edition pointing at a Title row that does not exist, and a File under it.
			`INSERT INTO editions (id, title_id) VALUES ('ed9','ghost-title')`,
			`INSERT INTO files (id, edition_id, path) VALUES ('f9','ed9','/movies/orphan.mkv')`,
			// A File whose Edition does not exist either — the other broken link.
			`INSERT INTO files (id, edition_id, path) VALUES ('f8','ghost-edition','/movies/orphan2.mkv')`,
			// Leave enforcement as we found it: this connection goes back to the pool.
			`PRAGMA foreign_keys=ON`,
		} {
			if _, err := conn.ExecContext(ctx, q); err != nil {
				t.Fatalf("exec %q: %v", q, err)
			}
		}
	}()

	for _, id := range []string{"f9", "f8"} {
		// The File row really is there — the refusal below is the guard, not a
		// missing row.
		if _, err := db.FileByID(id); err != nil {
			t.Fatalf("FileByID(%s) = %v, want the orphaned row to load", id, err)
		}
		if _, _, err := db.LibraryAndRatingOfFile(id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("LibraryAndRatingOfFile(%s) err = %v, want ErrNotFound (fail closed)", id, err)
		}
	}
}
