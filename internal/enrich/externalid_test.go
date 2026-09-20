package enrich

import (
	"errors"
	"testing"
)

// The HOST's own reading of a pasted id or URL.
//
// It is the host's because the COLUMNS are: `titles.tmdb_id` and
// `titles.musicbrainz_id` belong to this server, not to whichever Plugin happens
// to claim a slug (ADR-0045/0049). A Plugin that declares CapabilityExternalRef
// answers a paste for itself and its answer stands — the MusicBrainz plugin does,
// and its own vocabulary is asserted in plugins/musicbrainz/musicbrainz — but a
// host that gets "unavailable" reads the paste itself for the namespaces it keeps
// those columns for, and that fallback is what these tests hold.
//
// They were in search_improvements_test.go, beside the provider tests that have
// since become the MusicBrainz plugin's (.scratch/bundled-plugins: issue 06). The
// code they cover never moved: it is internal/enrich/externalid.go.

func TestParseMusicBrainzRef(t *testing.T) {
	cases := []struct {
		in       string
		wantKind string
		wantID   string
		wantOK   bool
	}{
		{"629a5133-a2b4-41ec-9db4-2b266d7a0e7a", "", "629a5133-a2b4-41ec-9db4-2b266d7a0e7a", true},
		{"  629A5133-A2B4-41EC-9DB4-2B266D7A0E7A  ", "", "629a5133-a2b4-41ec-9db4-2b266d7a0e7a", true},
		{"https://musicbrainz.org/release-group/629a5133-a2b4-41ec-9db4-2b266d7a0e7a", "album", "629a5133-a2b4-41ec-9db4-2b266d7a0e7a", true},
		{"http://beta.musicbrainz.org/artist/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/releases", "artist", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", true},
		{"https://musicbrainz.org/recording/11111111-2222-3333-4444-555555555555?tport=80", "track", "11111111-2222-3333-4444-555555555555", true},
		{"not a uuid or url", "", "", false},
		{"https://example.com/foo/bar", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		k, id, ok := ParseMusicBrainzRef(c.in)
		if k != c.wantKind || id != c.wantID || ok != c.wantOK {
			t.Errorf("ParseMusicBrainzRef(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, k, id, ok, c.wantKind, c.wantID, c.wantOK)
		}
	}
}

func TestParseTMDBRef(t *testing.T) {
	cases := []struct {
		in       string
		wantKind string
		wantID   string
		wantOK   bool
	}{
		{"438631", "", "438631", true},
		{"https://www.themoviedb.org/movie/438631-dune", "movie", "438631", true},
		{"https://www.themoviedb.org/tv/1399", "tv", "1399", true},
		{"themoviedb.org/tv/1399-game-of-thrones/seasons", "tv", "1399", true},
		{"not-a-ref", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		k, id, ok := parseTMDBRef(c.in)
		if k != c.wantKind || id != c.wantID || ok != c.wantOK {
			t.Errorf("parseTMDBRef(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, k, id, ok, c.wantKind, c.wantID, c.wantOK)
		}
	}
}

// TestExternalIDForKind: a bare id is trusted for the item's kind; a typed URL of the
// wrong kind is rejected; an unreadable paste is invalid.
func TestExternalIDForKind(t *testing.T) {
	mbID := "629a5133-a2b4-41ec-9db4-2b266d7a0e7a"
	// A release-group URL applied to an album — accepted.
	if id, err := externalIDForKind("album", "https://musicbrainz.org/release-group/"+mbID); err != nil || id != mbID {
		t.Errorf("album + release-group url = (%q,%v), want (%q,nil)", id, err, mbID)
	}
	// An artist URL applied to a track — kind mismatch that carries what was pasted
	// (artist) vs what's needed (track) so the handler can guide the Admin.
	if _, err := externalIDForKind("track", "https://musicbrainz.org/artist/"+mbID); !errors.Is(err, ErrExternalRefKindMismatch) {
		t.Errorf("track + artist url err = %v, want ErrExternalRefKindMismatch", err)
	} else {
		var m *ExternalRefKindMismatchError
		if !errors.As(err, &m) || m.Got != "artist" || m.Want != "track" {
			t.Errorf("track + artist url mismatch = %+v, want {Got:artist Want:track}", m)
		}
	}
	// A release-group (album) URL applied to an artist — the reported real-world case.
	if _, err := externalIDForKind("artist", "https://musicbrainz.org/release-group/"+mbID); !errors.Is(err, ErrExternalRefKindMismatch) {
		t.Errorf("artist + release-group url err = %v, want ErrExternalRefKindMismatch", err)
	} else {
		var m *ExternalRefKindMismatchError
		if !errors.As(err, &m) || m.Got != "album" || m.Want != "artist" {
			t.Errorf("artist + release-group url mismatch = %+v, want {Got:album Want:artist}", m)
		}
	}
	// A bare UUID is trusted for the item's kind.
	if id, err := externalIDForKind("track", mbID); err != nil || id != mbID {
		t.Errorf("track + bare uuid = (%q,%v), want (%q,nil)", id, err, mbID)
	}
	// A tv URL applied to a movie — kind mismatch (pasted show, item is a movie).
	if _, err := externalIDForKind("movie", "https://www.themoviedb.org/tv/1399"); !errors.Is(err, ErrExternalRefKindMismatch) {
		t.Errorf("movie + tv url err = %v, want ErrExternalRefKindMismatch", err)
	} else {
		var m *ExternalRefKindMismatchError
		if !errors.As(err, &m) || m.Got != "show" || m.Want != "movie" {
			t.Errorf("movie + tv url mismatch = %+v, want {Got:show Want:movie}", m)
		}
	}
	// Garbage — invalid.
	if _, err := externalIDForKind("album", "gibberish"); err != ErrExternalRefInvalid {
		t.Errorf("album + gibberish err = %v, want ErrExternalRefInvalid", err)
	}
	// A valid MusicBrainz link of an entity we can't pin (a /work/ URL — the common
	// "I grabbed the wrong id" case) is distinguished from unreadable garbage so the
	// Admin is told what to paste instead.
	if _, err := externalIDForKind("album", "https://musicbrainz.org/work/"+mbID); err != ErrExternalRefUnsupportedKind {
		t.Errorf("album + work url err = %v, want ErrExternalRefUnsupportedKind", err)
	}
	// A /release/ URL (one edition of a release-group) is likewise recognized-but-
	// unsupported, not gibberish.
	if _, err := externalIDForKind("album", "https://musicbrainz.org/release/"+mbID); err != ErrExternalRefUnsupportedKind {
		t.Errorf("album + release url err = %v, want ErrExternalRefUnsupportedKind", err)
	}
}
