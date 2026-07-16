package scanner

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// TV identity parsing (docs/naming-convention.md "TV: seasons and episodes",
// ADR-0002). Pure and offline: the folder/file name carries Episode identity.
// The canonical layout is `Show (Year)/Season NN/Show (Year) - S01E05 - Title.ext`.
//
// This file holds the kind-specific TV grammar; identity.go's ParseIdentity is
// reused unchanged to parse the Show folder (a Show identity is title+year, like
// a Movie). The episode-token grammar below is the load-bearing new code, kept as
// pure functions so the parser is unit-tested without touching the filesystem
// (mirrors identity_test.go).

// EpisodeToken is the parsed episode identity from a filename, one of three
// kinds (Kind):
//   - "sxxexx"  — canonical SxxExx (Season/Episode numbers); a range SxxExx-Eyy
//     sets EpisodeEnd > Episode, meaning one File maps to TWO Episode Titles.
//   - "date"    — a YYYY-MM-DD air-date token (daily/talk shows); filed by its raw
//     token, labeled by date (canonical mapping needs Enrichment, out of scope).
//   - "absolute"— a bare absolute episode number (anime); filed by its raw token,
//     labeled by number.
//
// Label is the human label for a degraded-offline episode ("date"/"absolute");
// empty for a canonical SxxExx. Raw is the verbatim token used as identity for
// the degraded kinds (so the same file re-resolves to the same Episode offline).
type EpisodeToken struct {
	Kind       string
	Season     int
	Episode    int
	EpisodeEnd int // > Episode only for a multi-episode range (SxxExx-Eyy)
	Label      string
	Raw        string
}

// IsRange reports whether the token spans two Episodes (S01E05-E06).
func (t EpisodeToken) IsRange() bool { return t.EpisodeEnd > t.Episode }

// sxxexxRe matches the canonical SxxExx token (case-insensitive), optionally a
// range SxxExx-Eyy / SxxExxE06 / SxxExx-06. The season/episode are 1+ digits.
var sxxexxRe = regexp.MustCompile(`(?i)\bs(\d{1,4})e(\d{1,4})(?:[-_ ]?e?(\d{1,4}))?\b`)

// dateRe matches a YYYY-MM-DD air-date token anywhere in the name.
var dateRe = regexp.MustCompile(`\b(\d{4})[.\-](\d{2})[.\-](\d{2})\b`)

// absoluteRe matches a bare absolute episode number in the canonical
// "Show - NNN - Title" / "Show - NNN.ext" position: a 1-4 digit run delimited by
// " - " separators (so a year or resolution embedded elsewhere is not mistaken
// for one). Anchored to the recognized separator so it is deterministic.
var absoluteRe = regexp.MustCompile(`(?:^|[-_ ])(?:e|ep|episode)?[ _]?(\d{1,4})(?:$|[-_ ])`)

// seasonFolderRe matches a "Season NN" folder name (case-insensitive), capturing
// the number. "Specials" is handled by the caller as season 0.
var seasonFolderRe = regexp.MustCompile(`(?i)^season[ _]?(\d{1,4})$`)

// ParseSeasonFolder parses a season folder name into its number, reporting ok.
// "Season 00" / "Season 0" and the "Specials" alias both resolve to season 0
// (Specials, naming-convention.md). A non-season folder name → ok=false.
func ParseSeasonFolder(name string) (season int, ok bool) {
	n := strings.TrimSpace(name)
	if strings.EqualFold(n, "specials") {
		return 0, true
	}
	if m := seasonFolderRe.FindStringSubmatch(n); m != nil {
		v, _ := strconv.Atoi(m[1])
		return v, true
	}
	return 0, false
}

// ParseSeasonPoster parses a Show-folder image named for a season — `Season 01.jpg`,
// and by the same grammar `Season 1.png` and `Specials.jpg` (season 0) — into the
// season it posters (naming-convention.md "Local artwork"). ok=false for any other
// name, including the Show's own `poster.jpg`/`fanart.jpg`, which artworkRole
// classifies instead.
//
// The name is parsed by ParseSeasonFolder, deliberately: a season poster is named
// for its season FOLDER, so the two grammars cannot drift apart and `Specials.jpg`
// works because `Specials/` does. This is a separate classifier from artworkRole
// rather than an extension of it — artworkRole answers "which role?" from a
// basename alone, while this must also answer "which season?", and it is only
// meaningful in a Show folder.
func ParseSeasonPoster(name string) (season int, ok bool) {
	if !imageExtensions[strings.ToLower(filepath.Ext(name))] {
		return 0, false
	}
	return ParseSeasonFolder(strings.TrimSuffix(name, filepath.Ext(name)))
}

// ParseEpisodeToken extracts the Episode identity from a file's base name
// (extension stripped). It tries, in order: the canonical SxxExx (incl. ranges),
// then a date token, then a bare absolute number. Returns ok=false when no
// recognized episode token is present (the caller routes the file to Unmatched —
// never auto-guessed). seasonHint is the season number confirmed by the folder
// (-1 when unknown); a date/absolute token files under it so daily/anime shows
// group sensibly offline.
func ParseEpisodeToken(name string, seasonHint int) (EpisodeToken, bool) {
	base := strings.TrimSpace(name)
	if base == "" {
		return EpisodeToken{}, false
	}

	// 1. Canonical SxxExx (authoritative Episode identity).
	if m := sxxexxRe.FindStringSubmatch(base); m != nil {
		season, _ := strconv.Atoi(m[1])
		ep, _ := strconv.Atoi(m[2])
		tok := EpisodeToken{Kind: "sxxexx", Season: season, Episode: ep, EpisodeEnd: ep}
		if m[3] != "" {
			if end, err := strconv.Atoi(m[3]); err == nil && end > ep {
				tok.EpisodeEnd = end
			}
		}
		return tok, true
	}

	// 2. Date-based (daily/talk shows): filed by its raw token, labeled by date.
	if m := dateRe.FindStringSubmatch(base); m != nil {
		raw := m[1] + "-" + m[2] + "-" + m[3]
		season := 0
		if seasonHint >= 0 {
			season = seasonHint
		}
		return EpisodeToken{Kind: "date", Season: season, Label: raw, Raw: raw}, true
	}

	// 3. Absolute numbering (anime): a bare episode number. Only accepted when the
	// name carries no SxxExx/date — already handled above. We take the LAST such
	// number so "Show (2011) - 135" reads 135, not 2011 (years are excluded by the
	// separator anchor, but prefer the trailing position regardless).
	if matches := absoluteRe.FindAllStringSubmatch(base, -1); len(matches) > 0 {
		last := matches[len(matches)-1]
		n, _ := strconv.Atoi(last[1])
		if n > 0 {
			season := 0
			if seasonHint >= 0 {
				season = seasonHint
			}
			return EpisodeToken{Kind: "absolute", Season: season, Episode: n,
				EpisodeEnd: n, Label: "Episode " + strconv.Itoa(n), Raw: strconv.Itoa(n)}, true
		}
	}

	return EpisodeToken{}, false
}

// episodeTitleName derives the human display title for an Episode from its
// filename, stripping the Show prefix and the episode token. The canonical
// `Show (Year) - S01E05 - The Title.ext` yields "The Title"; when no trailing
// title segment exists it falls back to a synthesized "S01E05" / the date /
// "Episode N" label so the Episode always has a name.
func episodeTitleName(base string, tok EpisodeToken) string {
	// Prefer the segment AFTER the episode token (the canonical " - Title" tail).
	if loc := sxxexxRe.FindStringIndex(base); loc != nil && tok.Kind == "sxxexx" {
		tail := strings.TrimSpace(base[loc[1]:])
		tail = strings.TrimLeft(tail, " -._")
		if t := strings.TrimSpace(tail); hasTitleContent(t) {
			return t
		}
	}
	// Date / absolute: take the tail after the raw token if present and meaningful.
	if tok.Raw != "" {
		if i := strings.LastIndex(base, tok.Raw); i >= 0 {
			tail := strings.TrimSpace(base[i+len(tok.Raw):])
			tail = strings.TrimLeft(tail, " -._")
			if t := strings.TrimSpace(tail); hasTitleContent(t) {
				return t
			}
		}
	}
	// Fallback: a synthesized canonical label so the Episode is never nameless.
	switch tok.Kind {
	case "date":
		return tok.Label
	case "absolute":
		return tok.Label
	default:
		return episodeCode(tok.Season, tok.Episode, tok.EpisodeEnd)
	}
}

// episodeCode formats the canonical SxxExx[-Eyy] code for a label/fallback name.
func episodeCode(season, episode, end int) string {
	code := "S" + pad2(season) + "E" + pad2(episode)
	if end > episode {
		code += "-E" + pad2(end)
	}
	return code
}

func pad2(n int) string {
	s := strconv.Itoa(n)
	if len(s) < 2 {
		return "0" + s
	}
	return s
}

// isAudioFolderName / isSeasonOrSpecials helps the resolver decide whether a
// subfolder under a Show is a Season folder. Kept here next to ParseSeasonFolder.
func isSeasonOrSpecials(name string) bool {
	_, ok := ParseSeasonFolder(name)
	return ok
}

// stripKnownExt is a thin wrapper for clarity in tv resolution (extension off).
func stripKnownExt(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}
