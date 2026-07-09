package scanner

import (
	"regexp"
	"strconv"
	"strings"
)

// Identity is the deterministic, offline-derived identity of a Movie (ADR-0002,
// docs/naming-convention.md): a display title, a year (0 when none parsed), and
// a normalized key used to dedup/re-resolve across rescans.
//
// When the folder name carries an embedded external id ({tmdb-N}/{imdb-ttN}),
// that id IS the identity: it overrides title+year for the key (so a same-title
// same-year collision is broken) and is recorded on the Title. The display
// title/year are still parsed for presentation.
type Identity struct {
	Title string
	Year  int
	Key   string
	// TMDBID / IMDBID are the embedded external ids parsed from the folder name,
	// empty when absent. At most one is normally present.
	TMDBID string
	IMDBID string
}

// HasYear reports whether a year was parsed. A yearless movie is filed but
// flagged needs-review (docs/naming-convention.md "Filed + needs-review").
func (i Identity) HasYear() bool { return i.Year != 0 }

// yearRe matches a trailing 4-digit year in parentheses, e.g. "Dune (2021)".
// The year is the LAST parenthesized 19xx/20xx group so a title that itself
// contains parentheses still resolves. Anchored to end-of-string (after the
// trailing tags are stripped) so it reads the identity year, not an incidental
// number.
var yearRe = regexp.MustCompile(`^(.*)\((19\d{2}|20\d{2})\)\s*$`)

// tmdbRe / imdbRe match an embedded external id tag anywhere in the name:
// "{tmdb-438631}" or "{imdb-tt1160419}" (case-insensitive on the prefix).
var (
	tmdbRe = regexp.MustCompile(`(?i)\{tmdb-(\d+)\}`)
	imdbRe = regexp.MustCompile(`(?i)\{imdb-(tt\d+)\}`)
)

// editionTagRe matches an explicit named-cut tag "{edition-Director's Cut}".
var editionTagRe = regexp.MustCompile(`(?i)\{edition-([^}]+)\}`)

// ParseIdentity derives a Movie Identity from a folder or bare-file base name
// (extension already stripped). It expects the canonical `Title (Year)` form,
// tolerating embedded `{tmdb/imdb-id}` and `{edition-…}` tags, which are
// stripped before title/year parsing. When no year is present it still yields a
// title-only identity (year 0) so the scanner can file it and flag it
// needs-review. Returns ok = false only when no usable title remains.
func ParseIdentity(name string) (Identity, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Identity{}, false
	}

	var id Identity
	if m := tmdbRe.FindStringSubmatch(name); m != nil {
		id.TMDBID = m[1]
	}
	if m := imdbRe.FindStringSubmatch(name); m != nil {
		id.IMDBID = m[1]
	}

	// Strip the recognized brace tags so they don't pollute title/year parsing.
	stripped := tmdbRe.ReplaceAllString(name, "")
	stripped = imdbRe.ReplaceAllString(stripped, "")
	stripped = editionTagRe.ReplaceAllString(stripped, "")
	stripped = strings.TrimSpace(stripped)

	title := stripped
	year := 0
	if m := yearRe.FindStringSubmatch(stripped); m != nil {
		title = strings.TrimSpace(m[1])
		title = strings.TrimRight(title, " -._")
		year, _ = strconv.Atoi(m[2])
	}
	title = strings.TrimSpace(strings.TrimRight(title, " -._"))
	if title == "" || !hasTitleContent(title) {
		// A folder named only by its embedded id (rare) still has an identity.
		if id.TMDBID != "" || id.IMDBID != "" {
			title = name
		} else {
			// No minimal identity could be extracted (empty, or only a junk /
			// quality token like "1080p" or "sample") → caller routes to
			// Unmatched, never auto-guessed (naming-convention.md).
			return Identity{}, false
		}
	}

	id.Year = year
	id.Title = title
	// Key on the parsed title BEFORE un-inverting: identity_key is the rescan
	// dedup key (identity.go identityKey → normalizeTitle), so changing it would
	// orphan the existing row and insert a duplicate. Only the display title and
	// the article-stripped sort key take the natural form.
	id.Key = identityKey(id)
	// Present a library-inverted title ("Island, The") in natural order ("The
	// Island") so every title reads consistently with its article at the front.
	id.Title = uninvertTitle(title)
	return id, true
}

// invertedTitleRe matches a display title that trails its leading article behind
// a comma — the library-catalog convention some sources use so a title files
// under its real first word ("Island, The", "American Tail, An", "Beautiful
// Mind, A"). The article is captured case-insensitively.
var invertedTitleRe = regexp.MustCompile(`(?i)^(.+),\s+(the|an|a)$`)

// uninvertTitle rewrites a trailing ", The"/", An"/", A" back to the natural
// leading-article form ("Island, The" → "The Island"), normalizing the article's
// casing. A title without that suffix is returned unchanged.
func uninvertTitle(title string) string {
	m := invertedTitleRe.FindStringSubmatch(strings.TrimSpace(title))
	if m == nil {
		return title
	}
	article := strings.ToLower(m[2])
	article = strings.ToUpper(article[:1]) + article[1:]
	return article + " " + strings.TrimSpace(m[1])
}

// IdentityKeyFor computes the catalog dedup key for a corrected identity (a
// fix-match Match override), using the same rule the scanner applies to a parse
// (ADR-0002): an embedded external id wins, else normalized-title|year. Exposed
// so the match domain keys an override the same way a scan would, guaranteeing
// the override re-resolves to the intended Title.
func IdentityKeyFor(title string, year int, tmdbID, imdbID string) string {
	return identityKey(Identity{Title: title, Year: year, TMDBID: tmdbID, IMDBID: imdbID})
}

// identityKey is the normalized dedup key. When an embedded external id is
// present it IS the key (overriding title+year, so same-title-same-year folders
// disambiguate). Otherwise the key is the case-folded, punctuation-collapsed
// title joined with the year (naming-convention.md "normalized title + year").
func identityKey(id Identity) string {
	if id.TMDBID != "" {
		return "tmdb:" + id.TMDBID
	}
	if id.IMDBID != "" {
		return "imdb:" + strings.ToLower(id.IMDBID)
	}
	return normalizeTitle(id.Title) + "|" + strconv.Itoa(id.Year)
}

// hasTitleContent reports whether a parsed title carries real identity rather
// than only a stray quality/junk token. A name that reduces to just "1080p",
// "sample", "remux" etc. has no minimal identity (→ Unmatched). The check is
// deterministic and offline: it inspects the normalized residue after the known
// quality token (if any) is removed.
func hasTitleContent(title string) bool {
	norm := normalizeTitle(title)
	if norm == "" {
		return false
	}
	// Strip a leading/sole quality token; if nothing meaningful remains, reject.
	residue := strings.TrimSpace(qualityTokenRe.ReplaceAllString(norm, ""))
	residue = strings.TrimSpace(junkRe.ReplaceAllString(residue, ""))
	return residue != ""
}

// normalizeTitle case-folds and collapses any run of punctuation/whitespace to a
// single space, so two on-disk spellings of the same title collapse.
func normalizeTitle(title string) string {
	var b strings.Builder
	lastWasSep := false
	for _, r := range strings.ToLower(title) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastWasSep = false
		default:
			if !lastWasSep && b.Len() > 0 {
				b.WriteByte(' ')
				lastWasSep = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// sortTitle is the case-insensitive ordering key for a title. A leading English
// article ("The ", "A ", "An ") is stripped so, e.g., "The Matrix" sorts under M,
// next to "Matrix Reloaded", rather than bunching every "The …" title under T.
func sortTitle(title string) string {
	return stripLeadingArticle(strings.ToLower(strings.TrimSpace(title)))
}

// sortArticles are leading words dropped from a sort key, longest first so a more
// specific prefix wins. Each includes its trailing space, so a bare article and
// words that merely begin with those letters (e.g. "theater") are untouched.
var sortArticles = []string{"the ", "an ", "a "}

// stripLeadingArticle drops a leading English article from an already-lower-cased,
// trimmed sort key so article-prefixed titles order by the following word.
func stripLeadingArticle(s string) string {
	for _, article := range sortArticles {
		if strings.HasPrefix(s, article) {
			return strings.TrimSpace(s[len(article):])
		}
	}
	return s
}
