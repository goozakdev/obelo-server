package scanner

import (
	"context"
	"os"
	"path/filepath"
	"sort"

	"github.com/google/uuid"
	"github.com/marioquake/juicebox/internal/store"
)

// TV folder resolution (issue tv-music/01). The scanner branches on the owning
// Library's kind: a TV Library's top-level folders are Show folders
// (`Show (Year)/`), each containing Season folders (`Season NN`/`Specials`) whose
// recognized media files are Episodes. An Episode is a Title that owns the same
// Edition→File→Stream chain a Movie does (assembleTitle, reused unchanged), plus
// the Season→Show linkage and episode ordering.
//
// Determinism + the attention surface generalize from Movies: a Show folder whose
// name yields no identity, or a media file with no recognized episode token, goes
// to Unmatched (never auto-guessed). A yearless Show is filed + needs-review.

// resolveShowFolder resolves one on-disk Show folder into a store.ShowTree plus
// any Unmatched files. ok=false (with the unmatched files) when the folder has no
// parseable Show identity or contains no resolvable Episodes.
func (s *Service) resolveShowFolder(ctx context.Context, sc *scanCtx, lib store.Library, folder string) (store.ShowTree, []store.UnmatchedFile, bool, error) {
	id, idOK := ParseIdentity(filepath.Base(folder))

	// A folder-anchored Match override overrules the parsed Show identity and
	// rescues an unparseable folder (same mechanism as Movies).
	if ov, ok := sc.overrides[folder]; ok {
		id = Identity{Title: ov.Title, Year: ov.Year, Key: ov.IdentityKey, TMDBID: ov.TMDBID, IMDBID: ov.IMDBID}
		idOK = true
	}

	// A Show folder that can't be read after retries is skipped (recorded in
	// sc.unresolved so the prune spares it) rather than aborting the whole scan.
	entries := sc.readDirTolerant(folder)

	var unmatched []store.UnmatchedFile
	unmatchedSeen := map[string]bool{}
	// addUnmatched records a file as Unmatched at most once. A range file
	// (S01E05-E06) expands into multiple Episode Titles that all share the same
	// physical path; when that file can't be resolved, every episode of the range
	// would otherwise append the identical path, and the duplicate trips the
	// global UNIQUE(path) constraint on unmatched_files (aborting the whole scan).
	addUnmatched := func(path, reason string) {
		if unmatchedSeen[path] {
			return
		}
		unmatchedSeen[path] = true
		unmatched = append(unmatched, unmatchedFile(path, reason))
	}

	// Group resolved Episodes by season number.
	seasonEpisodes := map[int][]store.EpisodeTree{}
	var seasonOrder []int
	seenSeason := map[int]bool{}

	addSeason := func(n int) {
		if !seenSeason[n] {
			seenSeason[n] = true
			seasonOrder = append(seasonOrder, n)
		}
	}

	// Resolve one media file under a known season into one or two Episode trees.
	resolveEpisodeFile := func(path string, seasonHint int) {
		base := stripKnownExt(filepath.Base(path))
		if isJunk(filepath.Base(path), fileSize(path)) {
			return // sample/junk ignored entirely
		}
		tok, ok := ParseEpisodeToken(base, seasonHint)
		if !ok {
			addUnmatched(path, "no recognized episode token (SxxExx / date / absolute)")
			return
		}
		season := tok.Season
		addSeason(season)

		// Build the Edition→File→Stream subtree for this single file, reusing the
		// EXACT Movie edition logic (quality/part/{edition-…}); an Episode carries
		// its own Editions/Files/Streams just like a Movie.
		cf := classifiedFile{path: path, name: filepath.Base(path), part: partNumber(filepath.Base(path))}

		// One File → two Episode Titles for a range (S01E05-E06): both get the same
		// physical File subtree (plays once); watch state is per-Title so marking
		// one watched is propagated to the other by the playback layer.
		episodes := []int{tok.Episode}
		if tok.IsRange() {
			episodes = nil
			for e := tok.Episode; e <= tok.EpisodeEnd; e++ {
				episodes = append(episodes, e)
			}
		}

		for _, epNum := range episodes {
			epTok := tok
			epTok.Episode = epNum
			epTok.EpisodeEnd = epNum
			displayName := episodeTitleName(base, tok)
			if tok.IsRange() {
				// Disambiguate the two Titles of a range so each is browsable.
				displayName = displayName + " (" + episodeCode(season, epNum, epNum) + ")"
			}

			identityKey := episodeIdentityKey(id, season, epTok)
			tree, err := s.assembleTitle(ctx, sc, lib, Identity{
				Title: displayName, Year: 0, Key: identityKey,
			}, []classifiedFile{cf}, nil, nil, nil)
			if err != nil {
				addUnmatched(path, "could not probe episode file: "+err.Error())
				continue
			}
			// assembleTitle stamps kind = lib.Kind ("tv"); an Episode leaf is "episode".
			tree.Title.Kind = "episode"
			tree.Title.IdentityKey = identityKey
			tree.Title.SortTitle = sortTitle(displayName)
			tree.Title.NeedsReview = tok.Kind != "sxxexx" // date/absolute need Enrichment to map canonically
			et := store.EpisodeTree{
				TitleTree:     tree,
				SeasonNumber:  season,
				EpisodeNumber: epTok.Episode,
				EpisodeLabel:  episodeLabelFor(tok),
			}
			seasonEpisodes[season] = append(seasonEpisodes[season], et)
		}
	}

	// Walk: subfolders that are Season/Specials folders hold episodes; recognized
	// media directly in the Show folder (no Season subfolder) is filed under a
	// season inferred from its own SxxExx token (seasonHint = -1).
	var subdirs []os.DirEntry
	var topFiles []os.DirEntry
	for _, e := range entries {
		if e.IsDir() {
			subdirs = append(subdirs, e)
		} else {
			topFiles = append(topFiles, e)
		}
	}

	// Episodes living directly in the Show folder (loose layout).
	sort.Slice(topFiles, func(i, j int) bool { return topFiles[i].Name() < topFiles[j].Name() })
	for _, e := range topFiles {
		if !isMedia(e.Name()) {
			continue
		}
		resolveEpisodeFile(filepath.Join(folder, e.Name()), -1)
	}

	// Season subfolders.
	sort.Slice(subdirs, func(i, j int) bool { return subdirs[i].Name() < subdirs[j].Name() })
	for _, e := range subdirs {
		season, isSeason := ParseSeasonFolder(e.Name())
		if !isSeason {
			continue // a non-season subfolder (extras etc.) is ignored this slice
		}
		sub := filepath.Join(folder, e.Name())
		// An unreadable season folder is skipped (recorded, spared from prune); the
		// Show's other seasons still resolve.
		subEntries := sc.readDirTolerant(sub)
		sort.Slice(subEntries, func(i, j int) bool { return subEntries[i].Name() < subEntries[j].Name() })
		for _, se := range subEntries {
			if se.IsDir() || !isMedia(se.Name()) {
				continue
			}
			resolveEpisodeFile(filepath.Join(sub, se.Name()), season)
		}
	}

	if !idOK {
		// A Show folder with no parseable identity routes its episodes to Unmatched.
		for _, eps := range seasonEpisodes {
			for _, et := range eps {
				for _, ed := range et.Editions {
					for _, f := range ed.Files {
						addUnmatched(f.Path, "no parseable Show identity from folder name")
					}
				}
			}
		}
		return store.ShowTree{}, unmatched, false, nil
	}

	if len(seasonOrder) == 0 {
		return store.ShowTree{}, unmatched, false, nil
	}

	sort.Ints(seasonOrder)
	show := store.Show{
		ID:          uuid.NewString(),
		LibraryID:   lib.ID,
		Title:       id.Title,
		Year:        id.Year,
		IdentityKey: id.Key,
		SortTitle:   sortTitle(id.Title),
		TMDBID:      id.TMDBID,
		IMDBID:      id.IMDBID,
		NeedsReview: !id.HasYear(),
	}
	tree := store.ShowTree{Show: show}
	for _, n := range seasonOrder {
		eps := seasonEpisodes[n]
		// Stable episode order within a season.
		sort.Slice(eps, func(i, j int) bool {
			if eps[i].EpisodeNumber != eps[j].EpisodeNumber {
				return eps[i].EpisodeNumber < eps[j].EpisodeNumber
			}
			return eps[i].Title.SortTitle < eps[j].Title.SortTitle
		})
		tree.Seasons = append(tree.Seasons, store.SeasonTree{
			SeasonNumber: n,
			IdentityKey:  id.Key + "|s" + pad2(n),
			Episodes:     eps,
		})
	}
	return tree, unmatched, true, nil
}

// episodeIdentityKey derives the stable identity key for an Episode Title within
// a Show. For a canonical SxxExx it is "<show>|s<NN>e<MM>"; for a degraded
// date/absolute token it incorporates the raw token so the same file re-resolves
// to the same Episode offline (identity stability, ADR-0014). A range member is
// keyed by its own episode number so the two Titles are distinct.
func episodeIdentityKey(show Identity, season int, tok EpisodeToken) string {
	switch tok.Kind {
	case "date":
		return show.Key + "|date:" + tok.Raw
	case "absolute":
		return show.Key + "|abs:" + tok.Raw
	default:
		return show.Key + "|s" + pad2(season) + "e" + pad2(tok.Episode)
	}
}

// episodeLabelFor returns the degraded-offline label (date / absolute number) for
// an Episode, empty for a canonical SxxExx (which labels by its numbers).
func episodeLabelFor(tok EpisodeToken) string {
	if tok.Kind == "sxxexx" {
		return ""
	}
	return tok.Label
}

// unmatchedFile mirrors scanner.unmatched (the Movie helper) for TV call sites.
func unmatchedFile(path, reason string) store.UnmatchedFile {
	return store.UnmatchedFile{ID: uuid.NewString(), Path: path, Reason: reason}
}
