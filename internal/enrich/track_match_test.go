package enrich

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// ADR-0050 / album-resolves-its-tracks issue 03: the album→track match rule and
// its title normalizer, exercised as pure functions — no provider, no store, no
// HTTP. This is the file that says what "matched" MEANS; the Cascade and the pass
// only decide what to do with the answer.

// --- helpers ----------------------------------------------------------------

// The two states of ADR-0052's licence, spelled at the call site so a bare true/false
// never has to be decoded. licensed means a HUMAN named this album's edition and
// these entries came from it, which is the only thing that turns rule 4 on.
const (
	licensed   = true
	unlicensed = false
)

// lt builds a local Track row: id, disc, track number, tagged title.
func lt(id string, disc, num int, title string) store.Title {
	return store.Title{ID: id, Kind: "track", Title: title, DiscNumber: disc, TrackNumber: num}
}

// tc builds a tracklist entry: disc, position, title, recording id.
func tc(disc, pos int, title, externalID string) TrackCandidate {
	return TrackCandidate{Disc: disc, Position: pos, Title: title, ExternalID: externalID}
}

// wantPairs asserts the exact pairing: every id in want maps to that recording id,
// and no local track outside want is mapped at all.
func wantPairs(t *testing.T, got map[string]TrackCandidate, want map[string]string) {
	t.Helper()
	for id, rec := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("track %s unmatched, want recording %q", id, rec)
			continue
		}
		if g.ExternalID != rec {
			t.Errorf("track %s matched recording %q, want %q", id, g.ExternalID, rec)
		}
	}
	for id, g := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("track %s matched recording %q (title %q), want NO match",
				id, g.ExternalID, g.Title)
		}
	}
}

// --- rule 1: position AND title ---------------------------------------------

// TestMapTracksAlignedAlbumMapsEveryTrack: the ordinary case. Every local number
// agrees with the release and every title agrees with it too, so rule 1 answers
// the whole album.
func TestMapTracksAlignedAlbumMapsEveryTrack(t *testing.T) {
	local := []store.Title{
		lt("t1", 1, 1, "Airbag"),
		lt("t2", 1, 2, "Paranoid Android"),
		lt("t3", 1, 3, "Subterranean Homesick Alien"),
	}
	list := []TrackCandidate{
		tc(1, 1, "Airbag", "rec-1"),
		tc(1, 2, "Paranoid Android", "rec-2"),
		tc(1, 3, "Subterranean Homesick Alien", "rec-3"),
	}
	wantPairs(t, mapTracks(local, list, unlicensed), map[string]string{"t1": "rec-1", "t2": "rec-2", "t3": "rec-3"})
}

// TestMapTracksDefaultsDiscOnBothSides: a single-disc rip that reports disc 0 and a
// tracklist that reports disc 1 are the same disc (discOrDefault, both sides).
func TestMapTracksDefaultsDiscOnBothSides(t *testing.T) {
	local := []store.Title{lt("t1", 0, 1, "Airbag"), lt("t2", 0, 2, "Paranoid Android")}
	list := []TrackCandidate{tc(1, 1, "Airbag", "rec-1"), tc(0, 2, "Paranoid Android", "rec-2")}
	wantPairs(t, mapTracks(local, list, unlicensed), map[string]string{"t1": "rec-1", "t2": "rec-2"})
}

// TestMapTracksSeparatesDiscs: the same position on two discs is two different
// recordings, and rule 1 must not confuse them.
func TestMapTracksSeparatesDiscs(t *testing.T) {
	local := []store.Title{lt("d1t1", 1, 1, "Overture"), lt("d2t1", 2, 1, "Reprise")}
	list := []TrackCandidate{tc(1, 1, "Overture", "rec-a"), tc(2, 1, "Reprise", "rec-b")}
	wantPairs(t, mapTracks(local, list, unlicensed), map[string]string{"d1t1": "rec-a", "d2t1": "rec-b"})
}

// --- the regression this issue exists for -----------------------------------

// TestMapTracksNeverPinsByPositionAlone is the defect, asserted directly: a local
// track whose NUMBER lands on an entry with a completely different title is not
// pinned to it (ADR-0050, "never position alone"). Before the rule, position 2
// silently pinned "Karma Police" to the recording of "Paranoid Android".
//
// The fixture leaves TWO holes on purpose. With one hole the leftover rule would
// fire and pair them, which is correct and deliberate — so a one-hole album cannot
// demonstrate anything about rule 1.
func TestMapTracksNeverPinsByPositionAlone(t *testing.T) {
	local := []store.Title{
		lt("t1", 1, 1, "Airbag"),
		lt("t2", 1, 2, "Karma Police"),
		lt("t3", 1, 3, "Lucky"),
	}
	list := []TrackCandidate{
		tc(1, 1, "Airbag", "rec-airbag"),
		tc(1, 2, "Paranoid Android", "rec-para"),
		tc(1, 3, "Subterranean Homesick Alien", "rec-sub"),
	}
	got := mapTracks(local, list, unlicensed)
	wantPairs(t, got, map[string]string{"t1": "rec-airbag"})
	if g, ok := got["t2"]; ok {
		t.Errorf("position-only pin: %q took the recording of %q (%s)", "Karma Police", g.Title, g.ExternalID)
	}
}

// --- rule 2: title anywhere -------------------------------------------------

// TestMapTracksNumberedFromZeroMapsByTitle: the hand-numbered rip. Every local
// number is one behind the release, so rule 1 never fires and rule 2 answers the
// whole album — including the two tracks whose OFF-BY-ONE number lands on a real
// entry with the wrong title.
func TestMapTracksNumberedFromZeroMapsByTitle(t *testing.T) {
	local := []store.Title{
		lt("t1", 1, 0, "Airbag"),
		lt("t2", 1, 1, "Paranoid Android"),
		lt("t3", 1, 2, "Subterranean Homesick Alien"),
	}
	list := []TrackCandidate{
		tc(1, 1, "Airbag", "rec-1"),
		tc(1, 2, "Paranoid Android", "rec-2"),
		tc(1, 3, "Subterranean Homesick Alien", "rec-3"),
	}
	wantPairs(t, mapTracks(local, list, unlicensed), map[string]string{"t1": "rec-1", "t2": "rec-2", "t3": "rec-3"})
}

// TestMapTracksTitleMatchingTwoEntriesDeclines: a title that names two positions on
// the release is not evidence for either, and (with two entries still unclaimed)
// the leftover rule does not rescue it. Neither rule 2 nor rule 3 fires.
func TestMapTracksTitleMatchingTwoEntriesDeclines(t *testing.T) {
	local := []store.Title{lt("t1", 1, 9, "Intro"), lt("t2", 1, 3, "Outro")}
	list := []TrackCandidate{
		tc(1, 1, "Intro", "rec-intro-a"),
		tc(1, 2, "Intro", "rec-intro-b"),
		tc(1, 3, "Outro", "rec-outro"),
	}
	wantPairs(t, mapTracks(local, list, unlicensed), map[string]string{"t2": "rec-outro"})
}

// TestMapTracksTwoLocalsOneEntryDeclines: the same ambiguity from the other side. A
// duplicated rip gives two local tracks one spelling; one unclaimed entry cannot be
// both of them, and picking the first in row order would be a coin flip dressed as
// a rule.
func TestMapTracksTwoLocalsOneEntryDeclines(t *testing.T) {
	local := []store.Title{lt("t1", 1, 7, "Intro"), lt("t2", 1, 8, "Intro"), lt("t3", 1, 3, "Outro")}
	list := []TrackCandidate{
		tc(1, 1, "Intro", "rec-intro"),
		tc(1, 3, "Outro", "rec-outro"),
	}
	wantPairs(t, mapTracks(local, list, unlicensed), map[string]string{"t3": "rec-outro"})
}

// --- rule 3: the leftover pair ----------------------------------------------

// TestMapTracksOneWrongNumberAndOneWrongTitle: the mixed album the spec names. The
// mis-numbered track is rescued by rule 2 (its title is unique), and the
// mis-titled one — a local spelling nothing on the release matches — is rescued by
// rule 3, because after 1 and 2 exactly one track and exactly one position remain.
func TestMapTracksOneWrongNumberAndOneWrongTitle(t *testing.T) {
	local := []store.Title{
		lt("t1", 1, 1, "Airbag"),
		lt("t2", 1, 2, "Paranoid Android"),
		lt("t3", 1, 9, "Exit Music"),          // wrong NUMBER, right title  → rule 2
		lt("t4", 1, 4, "Let Down [alt take]"), // right number, wrong title → rule 3
	}
	list := []TrackCandidate{
		tc(1, 1, "Airbag", "rec-1"),
		tc(1, 2, "Paranoid Android", "rec-2"),
		tc(1, 3, "Exit Music", "rec-3"),
		tc(1, 4, "Let Down", "rec-4"),
	}
	wantPairs(t, mapTracks(local, list, unlicensed), map[string]string{
		"t1": "rec-1", "t2": "rec-2", "t3": "rec-3", "t4": "rec-4",
	})
}

// TestMapTracksTwoStraysMapNeither is the "She" shape: several holes, not one. Two
// unmatched tracks and two free positions are a coin flip, so both stay unmatched
// and reach the Admin (ADR-0050 consequences).
func TestMapTracksTwoStraysMapNeither(t *testing.T) {
	local := []store.Title{
		lt("t1", 1, 1, "Airbag"),
		lt("t2", 1, 2, "Paranoid Android"),
		lt("t3", 1, 3, "A Hidden Track"),
		lt("t4", 1, 4, "Another Stray"),
	}
	list := []TrackCandidate{
		tc(1, 1, "Airbag", "rec-1"),
		tc(1, 2, "Paranoid Android", "rec-2"),
		tc(1, 3, "Exit Music", "rec-3"),
		tc(1, 4, "Let Down", "rec-4"),
	}
	wantPairs(t, mapTracks(local, list, unlicensed), map[string]string{"t1": "rec-1", "t2": "rec-2"})
}

// TestMapTracksLeftoverNeedsExactlyOneOfEach: one stray track but TWO free
// positions (the release has a track the library does not) is not a pair either —
// "exactly one of each" means both sides.
func TestMapTracksLeftoverNeedsExactlyOneOfEach(t *testing.T) {
	local := []store.Title{lt("t1", 1, 1, "Airbag"), lt("t2", 1, 2, "A Stray")}
	list := []TrackCandidate{
		tc(1, 1, "Airbag", "rec-1"),
		tc(1, 2, "Paranoid Android", "rec-2"),
		tc(1, 3, "Exit Music", "rec-3"),
	}
	wantPairs(t, mapTracks(local, list, unlicensed), map[string]string{"t1": "rec-1"})
}

// TestMapTracksIdlessEntryStillClaimsItsPosition: a tracklist entry with no
// recording id is a real position on the release (issue 02). It still claims its
// slot and is still returned — deciding that an id-less match is unusable is the
// CALLER's job, and hiding it here would free the position for a neighbour.
func TestMapTracksIdlessEntryStillClaimsItsPosition(t *testing.T) {
	local := []store.Title{lt("t1", 1, 1, "Airbag"), lt("t2", 1, 2, "Paranoid Android")}
	list := []TrackCandidate{
		tc(1, 1, "Airbag", ""),
		tc(1, 2, "Paranoid Android", "rec-2"),
	}
	got := mapTracks(local, list, unlicensed)
	wantPairs(t, got, map[string]string{"t1": "", "t2": "rec-2"})
	if got["t1"].Title != "Airbag" {
		t.Errorf("the id-less entry was not returned: %+v", got["t1"])
	}
}

// --- degenerate inputs ------------------------------------------------------

// TestMapTracksEmptyInputs: no tracklist and no tracks both answer "nothing
// matched" rather than panicking or inventing a pair. An album with one track and
// an EMPTY tracklist must not trip the leftover rule.
func TestMapTracksEmptyInputs(t *testing.T) {
	if got := mapTracks(nil, []TrackCandidate{tc(1, 1, "Airbag", "rec-1")}, unlicensed); len(got) != 0 {
		t.Errorf("no local tracks matched %d entries", len(got))
	}
	if got := mapTracks([]store.Title{lt("t1", 1, 1, "Airbag")}, nil, unlicensed); len(got) != 0 {
		t.Errorf("an empty tracklist matched %d tracks", len(got))
	}
}

// --- the case that started the feature ---------------------------------------

// TestMapTracksWhisperYourName is the real tag out of the developer's library,
// verbatim: "( I Could Only ) Whisper Your Name" against MusicBrainz's
// "(I Could Only) Whisper Your Name". The single space inside the parentheses was
// the entire failure. Note the leading parenthetical is NOT a qualifier — it is
// the name — so the two must meet on rule 1, at the position they agree on.
func TestMapTracksWhisperYourName(t *testing.T) {
	local := []store.Title{
		lt("t1", 1, 1, "Sonny Cried"),
		lt("t2", 1, 2, "( I Could Only ) Whisper Your Name"),
		lt("t3", 1, 3, "Follow the Music"),
	}
	list := []TrackCandidate{
		tc(1, 1, "Sonny Cried", "rec-1"),
		tc(1, 2, "(I Could Only) Whisper Your Name", "rec-2"),
		tc(1, 3, "Follow the Music", "rec-3"),
	}
	wantPairs(t, mapTracks(local, list, unlicensed), map[string]string{"t1": "rec-1", "t2": "rec-2", "t3": "rec-3"})
}

// --- normalization ------------------------------------------------------------

// TestNormalizeMatchTitleMeets carries the real disagreements two spellings of ONE
// recording show up with. Each pair must fold to the same string.
func TestNormalizeMatchTitleMeets(t *testing.T) {
	pairs := [][2]string{
		// The tag that started the feature, verbatim, against MusicBrainz's spelling.
		{"( I Could Only ) Whisper Your Name", "(I Could Only) Whisper Your Name"},
		// Padding inside brackets, in every arrangement.
		{"( X )", "(X)"},
		{"(X )", "(X)"},
		{"( X)", "(X)"},
		// Trailing qualifiers taggers and MusicBrainz disagree about.
		{"Song (Remastered 2011)", "Song"},
		{"Song [Bonus Track]", "Song"},
		{"Song (Live)", "Song"},
		{"Song (Single Version)", "Song"},
		{"Song (Live) (Remastered 2011)", "Song"},
		// Featured-artist credits, bare and parenthesized.
		{"Song feat. X", "Song"},
		{"Song ft. X", "Song"},
		{"Song ft X", "Song"},
		{"Song featuring X And Y", "Song"},
		{"Song (feat. X)", "Song"},
		{"Song (Live) feat. X", "Song"},
		// Apostrophes are deleted, not separated — every mark a tagger might use.
		{"Ain't", "Aint"},
		{"Rockin’ the Suburbs", "Rockin' the Suburbs"},
		// Case, diacritics, and "&"/"and".
		{"MOTÖRHEAD", "motorhead"},
		{"Björk", "Bjork"},
		{"Sigur Rós", "Sigur Ros"},
		{"Rock & Roll", "Rock and Roll"},
		{"Rock&Roll", "rock and roll"},
		// Other punctuation and whitespace collapse to one separator.
		{"Weird  Fishes / Arpeggi", "Weird Fishes - Arpeggi"},
	}
	for _, p := range pairs {
		if a, b := normalizeMatchTitle(p[0]), normalizeMatchTitle(p[1]); a != b {
			t.Errorf("%q → %q, %q → %q; want the same string", p[0], a, p[1], b)
		}
	}
}

// TestNormalizeMatchTitleStaysApart carries the pairs that must NOT fold together.
// Over-collapsing here costs a wrong recording on a track, which ADR-0049 calls
// worse than no id at all.
func TestNormalizeMatchTitleStaysApart(t *testing.T) {
	pairs := [][2]string{
		// A LEADING parenthetical is the name, not a qualifier.
		{"Whisper Your Name", "(I Could Only) Whisper Your Name"},
		// A leading article is kept: this is a title match, not a sort key.
		{"The Bends", "Bends"},
		// Two genuinely different recordings.
		{"Airbag", "Karma Police"},
		{"Exit Music (For a Film)", "Let Down"},
	}
	for _, p := range pairs {
		if a, b := normalizeMatchTitle(p[0]), normalizeMatchTitle(p[1]); a == b {
			t.Errorf("%q and %q both → %q; want different strings", p[0], p[1], a)
		}
	}
}

// TestNormalizeMatchTitleNeverEmptiesATitle: the strip rules refuse to consume a
// whole title. A track really named "(Untitled)" or "feat." keeps something to
// match on rather than folding onto every other degenerate title on the release.
func TestNormalizeMatchTitleNeverEmptiesATitle(t *testing.T) {
	for _, in := range []string{"(Untitled)", "[Untitled]", "(( Nested ))", "feat. X"} {
		if got := normalizeMatchTitle(in); got == "" {
			t.Errorf("normalizeMatchTitle(%q) = %q, want a non-empty comparison value", in, got)
		}
	}
	// A title with no letters at all has nothing to compare, and an empty value is
	// never treated as a title match by mapTracks.
	if got := normalizeMatchTitle("   "); got != "" {
		t.Errorf("normalizeMatchTitle(blank) = %q, want empty", got)
	}
}

// TestNormalizeMatchTitleIsNotTheIdentityNormalizer pins the one place the two
// normalizers deliberately disagree (ADR-0050): scanner.normalizeTitle turns an
// apostrophe into a separator, because merging two distinct works is the failure
// that matters for an identity KEY. Here the opposite failure matters, so the
// apostrophe is deleted and "Ain't" reaches "Aint". If this ever starts producing
// "ain t", the two have been quietly merged.
func TestNormalizeMatchTitleIsNotTheIdentityNormalizer(t *testing.T) {
	if got := normalizeMatchTitle("Ain't No Sunshine"); got != "aint no sunshine" {
		t.Errorf("normalizeMatchTitle(%q) = %q, want %q — the identity normalizer's "+
			"punctuation-as-separator rule must not leak in here",
			"Ain't No Sunshine", got, "aint no sunshine")
	}
}

// TestFoldTablesAreWellFormed guards the two hand-written fold tables: an index
// error in either is silent (it mangles one accented letter) and would only ever
// be noticed by a user with an accented title.
func TestFoldTablesAreWellFormed(t *testing.T) {
	if len(latin1Folds) != 64 {
		t.Errorf("latin1Folds covers %d code points, want 64 (U+00C0–U+00FF)", len(latin1Folds))
	}
	if len(latinExtAFolds) != 128 {
		t.Errorf("latinExtAFolds covers %d code points, want 128 (U+0100–U+017F)", len(latinExtAFolds))
	}
	// Spot-checks at both ends and across the multi-rune expansions.
	for in, want := range map[string]string{
		"àáâãäå": "aaaaaa", "ç": "c", "ñ": "n", "ø": "o", "ÿ": "y",
		"æ": "ae", "œ": "oe", "ß": "ss", "þ": "th", "ĳ": "ij",
		"ā": "a", "ž": "z", "ſ": "s", "ł": "l", "đ": "d",
		// Outside the Latin blocks nothing is folded — a non-Latin catalog
		// compares exactly rather than being mangled.
		"日本語": "日本語", "Привет": "привет",
	} {
		if got := normalizeMatchTitle(in); got != want {
			t.Errorf("normalizeMatchTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
