package musicbrainz

import (
	"strings"
	"unicode"
)

// Title normalization for MATCHING (not for identity), copied from the host's
// internal/enrich/track_match.go when this source became a plugin.
//
// IT IS A COPY, AND THE DUPLICATION IS THE MODULE SPLIT'S PRICE. The host applies
// this rule as ADR-0050's acceptance test — is the candidate it got back really
// this track — and keeps its own implementation, which is right: the judgement is
// the host's (ADR-0057) and it has to hold for sources this server does not ship.
// This plugin needs the same folding for ONE thing only, and it is not that
// judgement: artistIDFromAlbumSearch asks whether the top release-group hit is the
// album the library is holding, so that an artist credit hanging off it counts as
// EVIDENCE. See that function for why it is not an acceptance test.
//
// A plugin cannot import the server, so the alternatives were to copy this or to
// invent a second, weaker comparison for the corroboration step. A second rule
// would be the worse answer: the two would drift, and the drift would show up as
// an artist quietly resolving to the wrong band.
//
// It is DELIBERATELY NOT the scanner's identity normalizer, and must never become
// it. That one serves identity KEYS, where wrongly merging two distinct works is
// the failure that matters, so it is aggressive in the opposite direction and its
// output format can never move without orphaning every row in the catalog. Here
// the opposite failure matters — two spellings of one work failing to meet — and
// this function's output is a comparison value that lives for one call.
//
// The rules, in order:
//
//   - A TRAILING parenthesized or bracketed qualifier is dropped, repeatedly:
//     "Song (Remastered 2011)", "Song [Bonus Track]", "Song (Live)", "Song (Single
//     Version)" all reduce to "song". A LEADING parenthetical is KEPT, because
//     "(I Could Only) Whisper Your Name" IS the name.
//   - A TRAILING "feat. …" / "ft. …" / "featuring …" credit is dropped; taggers
//     and MusicBrainz disagree about it constantly.
//   - Case is folded, and diacritics with it ("Motörhead" = "motorhead").
//   - An apostrophe is DELETED rather than separated, so "Ain't" meets "Aint".
//     Every other punctuation mark and every whitespace run becomes one space,
//     which is also what makes "( I Could Only )", "(I Could Only )" and
//     "(I Could Only)" the same string.
//   - "&" and "and" are unified.
//   - A leading article is KEPT. This is a title match, not a sort key: "The
//     Bends" and "Bends" are two different names.
func normalizeMatchTitle(title string) string {
	s := strings.TrimSpace(title)
	for {
		next := stripTrailingCredit(stripTrailingQualifier(s))
		if next == s {
			break
		}
		s = next
	}
	return foldMatchText(s)
}

// creditWords are the trailing featured-artist credits this normalizer drops
// ("Song feat. X", "Song ft X", "Song featuring X"), longest first so that
// "featuring" is not read as "feat" followed by nonsense.
var creditWords = []string{"featuring", "feat", "ft"}

// findTrailingCredit returns the byte index where a trailing feat./ft. credit
// begins, or -1. It is the hand-written form of the regexp this used in the
// server — `(?i)\s+(?:feat|ft|featuring)\.?\s+\S.*$` — and it is hand-written for
// ONE reason: importing `regexp` into a WebAssembly guest costs about 190 KB of
// compressed module for a single pattern, which is a fifth of this plugin's whole
// size (ADR-0059 caps what the seven Bundled plugins may add to the binary).
//
// It is anchored on preceding whitespace, so a title whose own words merely start
// with those letters ("Feather", "Ftown") is untouched, and it requires a
// non-space AFTER the credit word, so a title ENDING in "feat." keeps it. The one
// difference from the regexp is that `.` there did not match a newline; a track
// title containing one would have kept its credit and now loses it, which is a
// case no tagger produces.
func findTrailingCredit(s string) int {
	for i := 0; i < len(s); i++ {
		if !isSpaceByte(s[i]) {
			continue
		}
		start := i
		for i < len(s) && isSpaceByte(s[i]) {
			i++
		}
		for _, word := range creditWords {
			rest := s[i:]
			if len(rest) < len(word) || !strings.EqualFold(rest[:len(word)], word) {
				continue
			}
			j := i + len(word)
			if j < len(s) && s[j] == '.' {
				j++
			}
			gap := j
			for gap < len(s) && isSpaceByte(s[gap]) {
				gap++
			}
			if gap == j || gap >= len(s) {
				continue // the credit needs whitespace and then something after it
			}
			return start
		}
		i-- // the loop's own i++ re-reads the first non-space byte
	}
	return -1
}

// isSpaceByte is Go's regexp \s: the five ASCII whitespace bytes.
func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\f' || b == '\r'
}

// stripTrailingCredit drops a trailing feat./ft. credit, unless doing so would
// leave nothing — a title that is ONLY a credit keeps it rather than vanishing.
func stripTrailingCredit(s string) string {
	i := findTrailingCredit(s)
	if i < 0 {
		return s
	}
	if head := strings.TrimSpace(s[:i]); head != "" {
		return head
	}
	return s
}

// stripTrailingQualifier drops ONE trailing (…) or […] group — the decoration
// taggers and MusicBrainz disagree about. It scans back from the closing bracket
// honoring nesting, and refuses to strip when nothing would remain, which is what
// keeps a title that IS a parenthetical (and, with the leading-only rule, keeps
// "(I Could Only) Whisper Your Name") intact.
//
// JUDGEMENT CALL (ADR-0050 names examples, not a vocabulary): EVERY trailing group
// is dropped, not only a recognized list of qualifier words. A keyword list is
// always one edition short, and being short is exactly the under-collapsing
// failure this normalizer exists to prevent.
func stripTrailingQualifier(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	var opener, closer rune
	switch s[len(s)-1] {
	case ')':
		opener, closer = '(', ')'
	case ']':
		opener, closer = '[', ']'
	default:
		return s
	}
	runes := []rune(s)
	depth := 0
	for i := len(runes) - 1; i >= 0; i-- {
		switch runes[i] {
		case closer:
			depth++
		case opener:
			depth--
			if depth == 0 {
				if head := strings.TrimSpace(string(runes[:i])); head != "" {
					return head
				}
				return s
			}
		}
	}
	return s
}

// foldMatchText case-folds, strips diacritics, deletes apostrophes, unifies "&"
// with "and", and collapses every other punctuation/whitespace run to one space.
func foldMatchText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastWasSep := true
	sep := func() {
		if b.Len() > 0 && !lastWasSep {
			b.WriteByte(' ')
			lastWasSep = true
		}
	}
	emit := func(str string) {
		b.WriteString(str)
		lastWasSep = false
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case isApostrophe(r):
			// Deleted, not separated: "Ain't" must equal "Aint".
		case r == '&':
			sep()
			emit("and")
			sep()
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			emit(foldRune(r))
		default:
			sep()
		}
	}
	return strings.TrimSpace(b.String())
}

// isApostrophe reports whether r is one of the marks a tagger, a shell and a
// metadata service each spell differently in the same word.
func isApostrophe(r rune) bool {
	switch r {
	case '\'', '‘', '’', 'ʼ', '´', '`':
		return true
	}
	return false
}

// latin1Folds maps U+00C0–U+00FF to an unaccented base letter, '-' where the code
// point is not a letter or needs a multi-rune expansion (see multiFolds).
const latin1Folds = "aaaaaa-ceeeeiiiidnooooo-ouuuuy--" + "aaaaaa-ceeeeiiiidnooooo-ouuuuy-y"

// latinExtAFolds maps U+0100–U+017F to an unaccented base letter, '-' where a
// multi-rune expansion is needed (see multiFolds).
const latinExtAFolds = "aaaaaa" + "cccccccc" + "dddd" + "eeeeeeeeee" + "gggggggg" +
	"hhhh" + "iiiiiiiiii" + "--" + "jj" + "kkk" + "llllllllll" + "nnnnnnnnn" +
	"oooooo" + "--" + "rrrrrr" + "ssssssss" + "tttttt" + "uuuuuuuuuuuu" + "ww" +
	"yyy" + "zzzzzz" + "s"

// multiFolds are the letters whose unaccented form is more than one letter. Only
// the lower-cased forms appear, because foldRune runs after strings.ToLower.
var multiFolds = map[rune]string{
	'æ': "ae", 'œ': "oe", 'ß': "ss", 'þ': "th", 'ĳ': "ij",
}

// foldRune returns r's unaccented base form. A rune outside the Latin blocks —
// Cyrillic, Greek, CJK, an emoji in a track title — is returned unchanged, so a
// non-Latin catalog still compares exactly rather than being mangled.
func foldRune(r rune) string {
	if r < 0x80 {
		return string(r)
	}
	if s, ok := multiFolds[r]; ok {
		return s
	}
	switch {
	case r >= 0x00c0 && r <= 0x00ff:
		if c := latin1Folds[r-0x00c0]; c != '-' {
			return string(rune(c))
		}
	case r >= 0x0100 && r <= 0x017f:
		if c := latinExtAFolds[r-0x0100]; c != '-' {
			return string(rune(c))
		}
	}
	return string(r)
}
