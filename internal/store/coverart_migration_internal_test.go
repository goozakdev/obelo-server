package store

import (
	"path/filepath"
	"testing"
)

// What migration 0071 does to a library that already exists
// (.scratch/bundled-plugins issue 06).
//
// `coverart` was never a source. It had no client, nothing read its enable switch,
// and its one live effect was the host MusicBrainz's album-cover URLs pointed at —
// which the server resolved out of this row and into that provider's second URL
// through a special case in three different files. MusicBrainz is now a Bundled
// plugin whose own manifest declares that host, so the row has nothing left to do.
//
// What must NOT be lost is an operator's MIRROR, and what must not be INVENTED is
// an override they never set. Those two are the whole of this migration and the
// whole of this file; it runs the REAL statements over REAL pre-0071 rows, which is
// the only way the SQL itself is covered.

// preCoverArtVersion is the last migration before the one under test.
const preCoverArtVersion = "0070_bundled_plugins"

// coverArtFixture builds a database at the pre-0071 schema, seeds it through seed,
// and then upgrades it exactly as a running install would.
func coverArtFixture(t *testing.T, seed func(exec func(string, ...any))) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migrateThrough(t, db, preCoverArtVersion)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}
	seed(exec)

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// providerRow reads one metadata_providers row's two hosts, and whether it exists
// at all.
func providerHosts(t *testing.T, db *DB, slug string) (base, image string, found bool) {
	t.Helper()
	rows, err := db.Query(
		`SELECT COALESCE(base_url, ''), COALESCE(image_base_url, '') FROM metadata_providers WHERE slug = ?`, slug)
	if err != nil {
		t.Fatalf("reading %s: %v", slug, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return "", "", false
	}
	if err := rows.Scan(&base, &image); err != nil {
		t.Fatalf("scanning %s: %v", slug, err)
	}
	return base, image, true
}

func overrideProviders(t *testing.T, db *DB, libraryID string) map[string]bool {
	t.Helper()
	got, err := db.libraryProviderOverrides(libraryID)
	if err != nil {
		t.Fatalf("reading overrides: %v", err)
	}
	return got
}

// THE UPGRADE PATH, as the issue states it: a DB with a `coverart` row whose
// base_url is a mirror ends with musicbrainz.url2 = that mirror and no `coverart`
// row — and a Library override that disabled `coverart` is scrubbed while the
// override's other keys survive.
func TestAMirroredCoverArtHostBecomesTheMusicLeadsSecondURL(t *testing.T) {
	db := coverArtFixture(t, func(exec func(string, ...any)) {
		exec(`INSERT INTO metadata_providers (slug, enabled, base_url) VALUES ('musicbrainz', 1, 'https://mb.mirror.example/ws/2')`)
		exec(`INSERT INTO metadata_providers (slug, enabled, base_url) VALUES ('coverart', 1, 'https://mirror.example/caa')`)
		exec(`INSERT INTO metadata_providers (slug, enabled, api_key) VALUES ('fanarttv', 1, 'fk')`)
		exec(`INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Music', 'music')`)
		exec(`INSERT INTO library_provider_override (library_id, provider, enabled) VALUES ('lib', 'coverart', 0)`)
		exec(`INSERT INTO library_provider_override (library_id, provider, enabled) VALUES ('lib', 'fanarttv', 1)`)
		exec(`INSERT INTO library_provider_override (library_id, provider, enabled) VALUES ('lib', 'theaudiodb', 0)`)
	})

	base, image, found := providerHosts(t, db, "musicbrainz")
	if !found {
		t.Fatal("the musicbrainz row is gone")
	}
	if image != "https://mirror.example/caa" {
		t.Errorf("musicbrainz image_base_url = %q, want the operator's mirror — a migration "+
			"that dropped it would silently send every album cover back to the public host", image)
	}
	if base != "https://mb.mirror.example/ws/2" {
		t.Errorf("musicbrainz base_url = %q, want it untouched", base)
	}
	if _, _, found := providerHosts(t, db, "coverart"); found {
		t.Error("the `coverart` row survived; this server has no code for it any more")
	}

	got := overrideProviders(t, db, "lib")
	if _, still := got["coverart"]; still {
		t.Error("the per-Library `coverart` override survived — a forced on/off for a source " +
			"nobody can call is not an opinion the next Plugin to claim that slug inherits")
	}
	// THE OTHER KEYS SURVIVE, which is the half a rewrite of the set would lose.
	if on, ok := got["fanarttv"]; !ok || !on {
		t.Errorf("the fanart.tv override = %v (ok %v), want the forced ON it was", on, ok)
	}
	if on, ok := got["theaudiodb"]; !ok || on {
		t.Errorf("the TheAudioDB override = %v (ok %v), want the forced OFF it was", on, ok)
	}
}

// A `coverart` row holding the PUBLIC default is carrying no decision — it is what
// the first-boot seed wrote — so it must not become a pinned override. The
// difference matters forever: "follow the shipped default" and "always use exactly
// this URL" are different instructions, and only one of them survives the day the
// Cover Art Archive changes hostname.
func TestAnUnchangedCoverArtRowPinsNothing(t *testing.T) {
	for _, seeded := range []string{"https://coverartarchive.org", ""} {
		t.Run("base_url "+seeded, func(t *testing.T) {
			db := coverArtFixture(t, func(exec func(string, ...any)) {
				exec(`INSERT INTO metadata_providers (slug, enabled, base_url) VALUES ('musicbrainz', 1, '')`)
				exec(`INSERT INTO metadata_providers (slug, enabled, base_url) VALUES ('coverart', 1, ?)`, seeded)
			})
			_, image, found := providerHosts(t, db, "musicbrainz")
			if !found {
				t.Fatal("the musicbrainz row is gone")
			}
			if image != "" {
				t.Errorf("musicbrainz image_base_url = %q, want empty — the row carried the "+
					"shipped default, which is the absence of an override, and pinning it "+
					"would outlive the default it copied", image)
			}
			if _, _, found := providerHosts(t, db, "coverart"); found {
				t.Error("the `coverart` row survived")
			}
		})
	}
}

// An operator who somehow already set MusicBrainz's own second host keeps it: the
// more specific setting wins over the one inherited from a row that is going away.
func TestAnExistingMusicBrainzImageHostIsNotOverwritten(t *testing.T) {
	db := coverArtFixture(t, func(exec func(string, ...any)) {
		exec(`INSERT INTO metadata_providers (slug, enabled, image_base_url) VALUES ('musicbrainz', 1, 'https://mine.example/caa')`)
		exec(`INSERT INTO metadata_providers (slug, enabled, base_url) VALUES ('coverart', 1, 'https://mirror.example/caa')`)
	})
	if _, image, _ := providerHosts(t, db, "musicbrainz"); image != "https://mine.example/caa" {
		t.Errorf("musicbrainz image_base_url = %q, want the one already set there", image)
	}
}

// A database with NO `coverart` row — a fresh-ish install that never configured
// music — migrates without inventing one, and without touching the music row.
func TestNoCoverArtRowIsNothingToMigrate(t *testing.T) {
	db := coverArtFixture(t, func(exec func(string, ...any)) {
		exec(`INSERT INTO metadata_providers (slug, enabled, base_url) VALUES ('musicbrainz', 1, 'https://mb.example/ws/2')`)
	})
	base, image, found := providerHosts(t, db, "musicbrainz")
	if !found || base != "https://mb.example/ws/2" || image != "" {
		t.Errorf("musicbrainz = (%q, %q, found %v), want its base URL and no image host",
			base, image, found)
	}
}
