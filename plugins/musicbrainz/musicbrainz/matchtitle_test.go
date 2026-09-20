package musicbrainz

import "testing"

// The title normalizer, held to the SERVER's (internal/enrich/track_match.go).
//
// This package carries a copy — a plugin cannot import the server — and the copy's
// one deliberate divergence is findTrailingCredit, which is the hand-written form
// of a regexp because `regexp` costs a fifth of this module's compressed size. The
// table below is what keeps the two honest about the cases that matter; the server
// has the same rows in its own suite.
func TestNormalizeMatchTitle(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"a trailing qualifier goes", "Song (Remastered 2011)", "song"},
		{"a bracketed one too", "Song [Bonus Track]", "song"},
		{"repeatedly", "Song (Live) (Remastered)", "song"},
		{"a LEADING parenthetical is the name", "(I Could Only) Whisper Your Name", "i could only whisper your name"},
		{"padding inside the brackets folds away", "( I Could Only ) Whisper Your Name", "i could only whisper your name"},
		{"a title that is ONLY a parenthetical keeps it", "(Reprise)", "reprise"},
		{"apostrophes are deleted, not separated", "Ain't Misbehavin'", "aint misbehavin"},
		{"case and diacritics fold", "Café BLEU", "cafe bleu"},
		{"& and and are one word", "Salt & Pepper", "salt and pepper"},
		{"a leading article is KEPT", "The Bends", "the bends"},
		{"punctuation collapses to one space", "Hello,   World!!", "hello world"},
		{"nothing but punctuation normalizes to nothing", "!!!", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeMatchTitle(c.in); got != c.want {
				t.Errorf("normalizeMatchTitle(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The trailing credit, which is the hand-written half. Every row here is a case the
// regexp answered, stated directly so a future edit to the scanner has something to
// fail against.
func TestTrailingCredit(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"feat. with a dot", "Song feat. Someone", "song"},
		{"feat without one", "Song feat Someone", "song"},
		{"ft.", "Song ft. Someone", "song"},
		{"ft", "Song ft Someone", "song"},
		{"featuring is not read as feat", "Song featuring Someone", "song"},
		{"case does not matter", "Song FEAT. Someone", "song"},
		{"a tab is whitespace too", "Song\tfeat.\tSomeone", "song"},
		{"a word that merely BEGINS with the letters is untouched", "Song Feather Duster", "song feather duster"},
		{"so is one in the middle of a word", "Software Song", "software song"},
		{"a title ENDING in feat. keeps it", "Song feat.", "song feat"},
		{"a credit with nothing after it keeps it", "Song feat ", "song feat"},
		{"a title that is ONLY a credit keeps it", "feat. Someone", "feat someone"},
		{"the credit goes before the qualifier does", "Song (Live) feat. Someone", "song"},
		{"and after it", "Song feat. Someone (Live)", "song"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeMatchTitle(c.in); got != c.want {
				t.Errorf("normalizeMatchTitle(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// And the scanner's own answer, so a failure above can be read: the INDEX where the
// credit starts, which is what stripTrailingCredit cuts at.
func TestFindTrailingCredit(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"Song feat. Someone", 4},
		{"Song  ft  Someone", 4},
		{"Song featuring Someone", 4},
		{"Song Feather Duster", -1},
		{"Song", -1},
		{"", -1},
		{"Song feat.", -1},
		{"   feat. Someone", 0},
	} {
		if got := findTrailingCredit(c.in); got != c.want {
			t.Errorf("findTrailingCredit(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
