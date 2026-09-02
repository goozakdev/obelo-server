package enrich

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Track matching — ONE rule that maps an Album's provider tracklist onto the
// Album's local Tracks, shared by the Cascade (mapAlbumTracks) and by the
// enrichment pass (ADR-0050, .scratch/album-resolves-its-tracks/issues/03).
//
// The rule is ALBUM-GRAINED and PURE: it takes the album's local Tracks and the
// tracklist, and returns the pairing. It touches no store and no provider, so
// the hardest correctness surface in this feature is testable as a table.
//
// Five rules, applied over the WHOLE album, in order:
//
//  1. POSITION AND TITLE — the tracklist entry at the Track's own (disc, track),
//     disc defaulting to 1 on both sides, whose normalized title matches the
//     Track's. Pin it.
//  2. TITLE ANYWHERE — a normalized title with exactly ONE unclaimed entry and
//     exactly ONE unmatched local Track. Pin it. This is what rescues an album
//     whose local numbering drifted (a hand-numbered rip, a hidden track, a
//     pressing that splits a medley).
//  3. THE LEFTOVER PAIR — if after 1 and 2 exactly one local Track and exactly
//     one tracklist entry are still unclaimed, they are each other.
//  4. POSITION ALONE, regardless of title — and ONLY when a human chose this
//     album's edition (ADR-0052, chosenEdition).
//  5. DECLINE — absent from the returned map; the caller falls through to its
//     own next step (the Cascade routes to attention; the pass falls through to
//     search).
//
// NEVER POSITION ALONE — unless a human named the edition. That is what
// mapAlbumTracks did unconditionally before ADR-0050, and on a hand-numbered rip
// it silently pinned the wrong recording to every track after the drift — the
// confident-wrong-answer ADR-0049 called worse than no id at all. A position that
// agrees with a title is evidence; a position by itself is an assumption about a
// STRANGER's numbering.
//
// ADR-0052 narrows that reasoning rather than reversing it: once an Admin has said
// "this album is this release", the ordering is no longer our assumption, it is
// their assertion, and a disagreeing title stops being a veto and becomes
// information — the classical rip whose tags carry composer prefixes and backticks
// where apostrophes belong, whose numbering was right on all 54 albums measured.
// The licence is the CHOICE and nothing else: not the counts matching, not a
// proportion of the titles agreeing, not a similarity threshold. Those are this
// file's own heuristics wearing a new hat, and the one fact they never have is that
// a human asserted this edition.
//
// Rule 4 runs LAST, after the three title rules, so a chosen edition can only ever
// map MORE tracks than the same album maps without one — never differently.
//
// Deliberately NOT here: whether a matched entry is USABLE. An entry with no
// recording id still claims its position (issue 02) and is still returned — it is
// a real position on the release, and letting a neighbour claim it would be a lie.
// Turning "matched but id-less" into an outcome is the caller's job, because the
// two callers do different things with it.

// mapTracks pairs an album's local Tracks with its provider tracklist by the
// ADR-0050/0052 rule above, keyed by store.Title.ID. A Track that does not resolve
// is simply absent (rule 5). Order-independent: no rule's outcome depends on the
// order the locals or the entries arrive in.
//
// local must be the album's WHOLE track list, including Tracks the caller intends
// to skip — a skipped Track still occupies its position on the release, and
// leaving it out would let a neighbour claim its entry or make the leftover rule
// fire on a hole that is not one.
//
// chosenEdition is ADR-0052's LICENCE, and it enables rule 4 alone. It must be the
// fact that these ENTRIES came from the release a human named — the pass's
// albumTracklistResult.FromChosenEdition, the cascade's twin of it — and never the
// stored intention (store.EntityEnrichment.ChosenReleaseID() != ""). A pin that did
// not produce this tracklist (a stranger release, an anchor that is not the group
// the pin was stored under, a cascade that fell back to the candidate's preview)
// leaves a tracklist no human asserted, and licensing position there is decorating
// from a stranger with the Admin's name on it.
func mapTracks(local []store.Title, tracklist []TrackCandidate, chosenEdition bool) map[string]TrackCandidate {
	matched := make(map[string]TrackCandidate, len(local))
	if len(local) == 0 || len(tracklist) == 0 {
		return matched
	}

	entryNorm := make([]string, len(tracklist))
	claimed := make([]bool, len(tracklist))
	atPos := make(map[[2]int][]int, len(tracklist))
	for i, tc := range tracklist {
		entryNorm[i] = normalizeMatchTitle(tc.Title)
		key := [2]int{discOrDefault(tc.Disc), tc.Position}
		atPos[key] = append(atPos[key], i)
	}

	localNorm := make([]string, len(local))
	done := make([]bool, len(local))
	for i, tr := range local {
		localNorm[i] = normalizeMatchTitle(tr.Title)
	}

	pin := func(li, ei int) {
		matched[local[li].ID] = tracklist[ei]
		done[li] = true
		claimed[ei] = true
	}

	// Rule 1 — position AND title. A duplicated (disc, position) in the tracklist
	// is only evidence when exactly one of its entries also agrees on the title.
	for i, tr := range local {
		if localNorm[i] == "" {
			continue
		}
		hit, n := -1, 0
		for _, j := range atPos[[2]int{discOrDefault(tr.DiscNumber), tr.TrackNumber}] {
			if claimed[j] || entryNorm[j] != localNorm[i] {
				continue
			}
			hit, n = j, n+1
		}
		if n == 1 {
			pin(i, hit)
		}
	}

	// Rule 2 — title anywhere, but only when the title names exactly one home on
	// EACH side. Two local Tracks spelled alike competing for one entry is the same
	// coin flip as one Track matching two entries, and neither is worth a wrong pin.
	// Both sides are snapshotted before any rule-2 pin, so the outcome does not
	// depend on map iteration order.
	entriesByTitle := make(map[string][]int, len(tracklist))
	for j, n := range entryNorm {
		if n == "" || claimed[j] {
			continue
		}
		entriesByTitle[n] = append(entriesByTitle[n], j)
	}
	localsByTitle := make(map[string][]int, len(local))
	for i, n := range localNorm {
		if n == "" || done[i] {
			continue
		}
		localsByTitle[n] = append(localsByTitle[n], i)
	}
	for n, lis := range localsByTitle {
		if js := entriesByTitle[n]; len(lis) == 1 && len(js) == 1 {
			pin(lis[0], js[0])
		}
	}

	// Rule 3 — the leftover pair. EXACTLY one of each: two strays and two holes is
	// a coin flip wearing a rule's clothing, and both stay unmatched (ADR-0050).
	onlyLocal, nLocal := -1, 0
	for i := range local {
		if !done[i] {
			onlyLocal, nLocal = i, nLocal+1
		}
	}
	onlyEntry, nEntry := -1, 0
	for j := range tracklist {
		if !claimed[j] {
			onlyEntry, nEntry = j, nEntry+1
		}
	}
	if nLocal == 1 && nEntry == 1 {
		pin(onlyLocal, onlyEntry)
	}

	// Rule 4 — POSITION ALONE, regardless of title, and only under the licence
	// (ADR-0052). Every rule above has already run, so this only ever reaches the
	// Tracks the title rules declined: a chosen edition cannot make matching worse.
	//
	// "Exactly one on each side" is the same ambiguity guard rules 1 and 2 apply, NOT
	// a confidence test: two local Tracks both numbered 5 competing for one entry at
	// 5 would otherwise be resolved by the order the rows arrived in, and mapTracks is
	// documented as order-independent so it is safe to memoize per album. Each
	// position's entries are disjoint from every other position's, so a pin here can
	// never change another position's answer.
	if chosenEdition {
		freeAt := make(map[[2]int][]int, len(local))
		for i, tr := range local {
			if done[i] {
				continue
			}
			key := [2]int{discOrDefault(tr.DiscNumber), tr.TrackNumber}
			freeAt[key] = append(freeAt[key], i)
		}
		for key, lis := range freeAt {
			if len(lis) != 1 {
				continue
			}
			hit, n := -1, 0
			for _, j := range atPos[key] {
				if claimed[j] {
					continue
				}
				hit, n = j, n+1
			}
			// No entry at this position is a real disagreement about the album — the
			// pinned release genuinely has no track there — and it still declines, which
			// is what keeps `not-in-tracklist` meaningful under a pin (ADR-0052).
			if n == 1 {
				pin(lis[0], hit)
			}
		}
	}

	return matched
}

// --- Title normalization for MATCHING (not for identity) --------------------

// normalizeMatchTitle folds two spellings of one recording onto one string, so a
// local tag and a MusicBrainz track title can be compared (ADR-0050).
//
// It is DELIBERATELY NOT scanner.normalizeTitle, and must never become it. That
// one serves identity KEYS, where wrongly merging two distinct works is the
// failure that matters, so it is aggressive in the opposite direction and its
// output format can never move without orphaning every row in the catalog. Here
// the opposite failure matters — two spellings of one work failing to meet — and
// this function's output is a comparison value that lives for the length of one
// match. Sharing them would couple a heuristic to a storage format.
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
//     "(I Could Only)" the same string — the single space inside the parentheses
//     that started this whole feature.
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

// creditRe matches a trailing featured-artist credit ("Song feat. X", "Song ft X",
// "Song featuring X"). Anchored on preceding whitespace so a title whose own words
// merely start with those letters is untouched.
var creditRe = regexp.MustCompile(`(?i)\s+(?:feat|ft|featuring)\.?\s+\S.*$`)

// stripTrailingCredit drops a trailing feat./ft. credit, unless doing so would
// leave nothing — a title that is ONLY a credit keeps it rather than vanishing.
func stripTrailingCredit(s string) string {
	loc := creditRe.FindStringIndex(s)
	if loc == nil {
		return s
	}
	if head := strings.TrimSpace(s[:loc[0]]); head != "" {
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
// failure this normalizer exists to prevent. Over-collapsing is cheap here: two
// entries that fold together stop being "exactly one" and the rule DECLINES
// (rules 2 and 3), so the cost of being too eager is a fall-through, never a wrong
// pin.
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
