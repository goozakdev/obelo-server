package scanner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"github.com/google/uuid"
	"github.com/goozakdev/obelo-server/internal/store"
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
//
// The resolve step is also where the Admin's FILE-ANCHORED corrections are
// replayed (ADR-0044). It has to be here and nowhere else: the three arrangements
// an Admin can express — one File per Slot, several Files on one Slot (a
// multi-part Edition), one File across several Slots (co-File sibling Titles) —
// all decide HOW MANY Title rows exist, and only resolve creates, merges and
// splits Title rows. writeTitleRow rewrites season_id, season_number,
// episode_number and episode_label from whatever resolve produced on every single
// upsert, so a correction applied to live rows alone would be undone by the next
// scheduled scan.

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
	// addUnreadable is addUnmatched for the file ffprobe refused: same list, same
	// once-only rule, different kind.
	addUnreadable := func(path, reason string) {
		if unmatchedSeen[path] {
			return
		}
		unmatchedSeen[path] = true
		unmatched = append(unmatched, unreadableFile(path, reason))
	}

	// The walk's only job is to hand the resolver every candidate media File plus
	// the two facts only disk can supply — the season its folder suggests, and the
	// sample/junk verdict. Deciding what those Files ADD UP TO (which Slots exist,
	// how many Title rows, their keys and names) belongs to ResolveEpisodes, which
	// Apply runs too; see arrangement.go for why that must be one function.
	var inputs []EpisodeInput
	addInput := func(path string) {
		inputs = append(inputs, EpisodeInput{
			Path:       path,
			SeasonHint: SeasonHintForPath(path),
			Junk:       isJunk(filepath.Base(path), fileSize(path)),
		})
	}

	// Walk: subfolders that are Season/Specials folders hold episodes; recognized
	// media directly in the Show folder (no Season subfolder) is filed under a
	// season inferred from its own SxxExx token.
	var subdirs []os.DirEntry
	var topFiles []os.DirEntry
	for _, e := range entries {
		if e.IsDir() {
			subdirs = append(subdirs, e)
		} else {
			topFiles = append(topFiles, e)
		}
	}

	// Local artwork in the Show folder (naming-convention.md): `poster.jpg`/`cover.jpg`
	// and `fanart.jpg`/`backdrop.jpg` poster/background the SHOW; `Season NN.jpg`
	// posters that season. First wins per role, and topFiles is sorted, so a folder
	// with both `poster.jpg` and `cover.jpg` resolves the same way on every rescan.
	// This is the Movie path's per-folder discovery (resolveFolder) reaching TV at
	// last — without it a TV Library has artwork ONLY from Enrichment, so a server
	// with no metadata provider shows a grid of blank cards.
	showArt := map[string]string{}
	var showArtOrder []string
	seasonArt := map[int]string{}

	// Episodes living directly in the Show folder (loose layout), plus that folder's
	// artwork.
	sort.Slice(topFiles, func(i, j int) bool { return topFiles[i].Name() < topFiles[j].Name() })
	for _, e := range topFiles {
		if role := artworkRole(e.Name()); role != "" {
			if _, seen := showArt[role]; !seen {
				showArt[role] = filepath.Join(folder, e.Name())
				showArtOrder = append(showArtOrder, role)
			}
			continue
		}
		if season, ok := ParseSeasonPoster(e.Name()); ok {
			if _, seen := seasonArt[season]; !seen {
				seasonArt[season] = filepath.Join(folder, e.Name())
			}
			continue
		}
		if !isMedia(e.Name()) {
			continue
		}
		addInput(filepath.Join(folder, e.Name()))
	}

	// Season subfolders.
	sort.Slice(subdirs, func(i, j int) bool { return subdirs[i].Name() < subdirs[j].Name() })
	for _, e := range subdirs {
		if _, isSeason := ParseSeasonFolder(e.Name()); !isSeason {
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
			addInput(filepath.Join(sub, se.Name()))
		}
	}

	// The single derivation, shared with Apply (ADR-0044).
	arrangement := ResolveEpisodes(id, inputs, sc.decisions)
	for _, u := range arrangement.Unresolved {
		addUnmatched(u.Path, u.Reason)
	}

	// Group resolved Episodes by season number. Seasons come from the numbers the
	// resolved Episodes CLAIM, never from the folders on disk — which is what lets
	// a Placement conjure a Season row with no folder behind it and leave a Season
	// emptied by reassignment uncreated (ADR-0044).
	seasonEpisodes := map[int][]store.EpisodeTree{}
	seasonOrder := arrangement.Seasons

	for _, re := range arrangement.Episodes {
		cfs := make([]classifiedFile, 0, len(re.Files))
		for _, rf := range re.Files {
			cfs = append(cfs, classifiedFile{
				path: rf.Path, name: filepath.Base(rf.Path),
				part: rf.PartOrdinal, jointEdition: rf.JointEdition,
			})
		}
		tree, err := s.assembleTitle(ctx, sc, lib, Identity{
			Title: re.DisplayName, Year: 0, Key: re.IdentityKey,
		}, cfs, nil, nil, nil)
		if err != nil {
			// A probe failure is a real failure, not a decision, so it surfaces on the
			// attention list exactly as a parse failure does — but as its OWN kind.
			// Nothing about this file's identity is wrong (its name numbered it, which
			// is how it got this far), so the fix is to replace the file, and an
			// attention row that offered to re-name the work would be inert forever
			// (CONTEXT.md "Unreadable").
			var ue *UnreadableError
			if errors.As(err, &ue) {
				broken := map[string]bool{}
				for _, p := range ue.Paths {
					broken[p] = true
				}
				for _, rf := range re.Files {
					if broken[rf.Path] {
						addUnreadable(rf.Path, ue.Error())
					} else {
						// A readable file that lost the Episode its broken sibling was half
						// of. It is not corrupt, so it is not marked as though it were.
						addUnmatched(rf.Path, "part of an episode whose other file could not be read")
					}
				}
				continue
			}
			for _, rf := range re.Files {
				addUnmatched(rf.Path, "could not probe episode file: "+err.Error())
			}
			continue
		}
		// assembleTitle stamps kind = lib.Kind ("tv"); an Episode leaf is "episode".
		tree.Title.Kind = "episode"
		tree.Title.IdentityKey = re.IdentityKey
		tree.Title.SortTitle = re.SortTitle
		tree.Title.NeedsReview = re.NeedsReview
		seasonEpisodes[re.SeasonNumber] = append(seasonEpisodes[re.SeasonNumber], store.EpisodeTree{
			TitleTree:     tree,
			SeasonNumber:  re.SeasonNumber,
			EpisodeNumber: re.EpisodeNumber,
			EpisodeLabel:  re.EpisodeLabel,
		})
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
	for _, role := range showArtOrder {
		tree.Artwork = append(tree.Artwork, store.EntityArtworkRow{Role: role, Path: showArt[role]})
	}
	for _, n := range seasonOrder {
		// Episode order within a season is already fixed by ResolveEpisodes, so both
		// writers lay a Season out identically.
		st := store.SeasonTree{
			SeasonNumber: n,
			IdentityKey:  SeasonIdentityKey(id.Key, n),
			Episodes:     seasonEpisodes[n],
		}
		// A `Season NN.jpg` naming a season with no episodes is ignored: seasonOrder
		// holds only seasons that resolved episodes, and a poster must not conjure a
		// Season row that no media backs.
		if p, ok := seasonArt[n]; ok {
			st.Artwork = []store.EntityArtworkRow{{Role: "poster", Path: p}}
		}
		tree.Seasons = append(tree.Seasons, st)
	}
	return tree, unmatched, true, nil
}

// slotKey names one Slot within the Show being resolved: the assigned group
// (season) and slot (episode) numbers, always in the local library's OWN
// numbering — a borrowed provider record's numbering would collide with it,
// which is the Episode pin's separate job (ADR-0044).
type slotKey struct {
	season  int
	episode int
}

// placedFile is one File the Admin placed on one Slot, with the ordinal that
// orders it among the Files sharing that Slot (1-based; 1 for the ordinary
// one-File-per-Slot case).
type placedFile struct {
	path    string
	ordinal int
}

// parsedFile is one File whose FILENAME claims a Slot, carrying the per-file
// facts the Episode is named and labeled from. Several can share one Slot — two
// parts, two quality-distinguished rips, or a range file overlapping a standalone
// — which is why they are collected before any Episode is built.
type parsedFile struct {
	path    string
	ordinal int
	season  int
	episode int
	// displayName / label / needsReview are derived from THIS file's token. Every
	// File on a Slot agrees on the last two (they share an identity key, so they
	// share a token kind); the name is taken from the leading File.
	displayName string
	label       string
	needsReview bool
}

// parsedEpisode accumulates the Files whose filenames claim one Slot.
type parsedEpisode struct {
	files []parsedFile
}

// sortedSlots returns the assigned Slots in (season, episode) order so a scan is
// deterministic regardless of map iteration order.
func sortedSlots(slots map[slotKey][]placedFile) []slotKey {
	keys := make([]slotKey, 0, len(slots))
	for k := range slots {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].season != keys[j].season {
			return keys[i].season < keys[j].season
		}
		return keys[i].episode < keys[j].episode
	})
	return keys
}

// placedEpisodeName names an Episode built from a Placement. The Slot decides the
// numbering, but the filename still carries the only human title available with
// no provider — so the parsed " - Title" tail is preferred and the Slot's
// canonical code is the fallback, and an Episode is never nameless.
//
// disambiguate suffixes the Slot's code, for a File spread across several Slots:
// its Titles would otherwise be identically named and indistinguishable in
// browse. It is the same treatment a range file (S01E05-E06) already gets.
func placedEpisodeName(path string, k slotKey, disambiguate bool) string {
	code := episodeCode(k.season, k.episode, k.episode)
	name := code
	base := stripKnownExt(filepath.Base(path))
	if tok, ok := ParseEpisodeToken(base, k.season); ok {
		name = episodeTitleName(base, tok)
	}
	if disambiguate && name != code {
		name += " (" + code + ")"
	}
	return name
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
