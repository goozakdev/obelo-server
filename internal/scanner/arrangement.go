package scanner

import (
	"path/filepath"
	"sort"

	"github.com/goozakdev/obelo-server/internal/store"
)

// arrangement.go holds the SINGLE derivation of a Show's Episode structure from
// its Files plus the Admin's file-anchored decisions (ADR-0044). It exists
// because that structure has TWO writers:
//
//   - the Scanner, which rebuilds a Show from disk on every scan
//     (resolveShowFolder), and
//   - Apply (catalog/placement.go), which writes the same structure directly the
//     moment the matcher screen closes, because ADR-0044 rejected applying via a
//     Targeted scan as too slow.
//
// ADR-0044 names their disagreement as the feature's central risk: if the two
// ever derive a different arrangement from the same decisions, a scheduled scan
// silently rearranges a Show the Admin sorted by hand, overnight and unobserved.
// Every knob that could differ — how Files group onto a Slot, the identity_key
// (from the ASSIGNED season and slot, never the parse's), the part ordinals and
// their order, the joint Edition that keeps two differently-tagged Files as one
// Episode, the display name, the needs-review flag, the Episode label — is
// decided HERE, once, so the two callers cannot drift.
//
// The genuine difference between the callers is narrow and is exactly the seam:
// the Scanner probes Files from disk, Apply reuses already-stored File rows.
// Everything on this side of that seam is pure: no disk, no store, no ffprobe.

// EpisodeInput is one candidate media File offered to the resolver.
//
// The two disk facts the resolver cannot get for itself travel with it, so the
// derivation stays pure: SeasonHint is the season the File's FOLDER suggests
// (see SeasonHintForPath) and Junk is the sample/stray-rip verdict, which needs
// the file's size. Junk is deliberately consulted only for a File the Admin said
// nothing about — a File they placed by hand outranks a guess from its size.
type EpisodeInput struct {
	Path       string
	SeasonHint int
	Junk       bool
}

// ResolvedFile is one File of a resolved Episode, in play order.
type ResolvedFile struct {
	// Path is the absolute on-disk path.
	Path string
	// PartOrdinal orders this File within its Edition, 1-based (0 for a File that
	// is not part of a multi-part set). For a placed File it is the Admin's
	// ordinal, which no filename carries; for a parsed one it is partNumber().
	PartOrdinal int
	// JointEdition forces this File into the single unnamed Edition rather than
	// letting its filename's quality token label one of its own — set only where
	// the Admin placed several Files on ONE Slot, which makes them one multi-part
	// Edition by decision rather than by filename.
	JointEdition bool
}

// ResolvedEpisode is one Episode the resolve step decided to build: every field
// a Title row and its Edition membership need, with nothing probed yet.
type ResolvedEpisode struct {
	SeasonNumber  int
	EpisodeNumber int
	// IdentityKey is the Title's stable key. For a placed Episode it is derived
	// from the ASSIGNED Slot, never the parse — the only rule that is total, since
	// a split needs two distinct keys and the filename supplies one, and a merge
	// has two keys and needs one (ADR-0044).
	IdentityKey string
	DisplayName string
	SortTitle   string
	// EpisodeLabel is the degraded-offline label (a date, an absolute number),
	// empty for a canonical SxxExx and always empty for a placed Episode (a Slot
	// is canonical numbers, so it labels by them).
	EpisodeLabel string
	// NeedsReview flags a degraded parse the Admin still has to confirm. Always
	// false for a placed Episode: the numbering came from the Admin, so there is
	// nothing left to confirm.
	NeedsReview bool
	// Placed is true when this Episode came from a Placement rather than from a
	// filename parse. Apply reads it to tell a Slot the Admin assigned from one a
	// filename merely claims, which is what makes displacing a parsed File
	// decidable (see catalog/placement.go).
	Placed bool
	Files  []ResolvedFile
}

// UnresolvedFile is a File the resolver could place nowhere — a genuine parse
// failure the Admin still has to settle, which is why an Ignored or Unassigned
// File is never reported here (that would count a decision they already made as
// outstanding work).
type UnresolvedFile struct {
	Path   string
	Reason string
}

// ResolvedShow is one Show's whole derived arrangement.
type ResolvedShow struct {
	// Episodes are in (season, episode, sort title, identity key) order, so both
	// writers lay a Show out identically regardless of walk or map order.
	Episodes []ResolvedEpisode
	// Seasons are the season numbers the Episodes CLAIM, ascending — never the
	// folders on disk. That is what lets a Placement conjure a Season row with no
	// folder behind it and leave a Season emptied by reassignment uncreated.
	Seasons []int
	// Unresolved are the parse failures (Unmatched files).
	Unresolved []UnresolvedFile
}

// ResolveEpisodes derives one Show's whole Episode arrangement from its candidate
// Files and what the Admin said about each of them.
//
// show supplies only the identity KEY every Episode key hangs off; the rest of
// the Show identity is the caller's business.
//
// Decision precedence, in order: an Ignored or Unassigned File contributes
// nothing at all (no Episode, no Unresolved row — a recorded decision is not a
// parse failure); a placed File uses its Slots instead of its filename; a File
// with NO row follows the parse, which is the common case and the reason the
// stored set stays sparse.
func ResolveEpisodes(show Identity, inputs []EpisodeInput, decisions map[string]store.FileDecisions) ResolvedShow {
	var out ResolvedShow

	// Placements gathered first, keyed by the Slot they assign: a Slot is not a
	// File — several can share one — so the Episodes they produce cannot be built
	// until every File claiming a Slot has been seen.
	slots := map[slotKey][]placedFile{}
	// How many Slots each placed path was spread across, so a File split over two
	// Slots can have its two Titles named distinguishably.
	slotsPerPath := map[string]int{}

	// The parsed files are gathered by the Slot they resolve to, for exactly the
	// same reason the placed ones are: a Slot is not a File. `- part1`/`- part2`,
	// a 1080p rip beside a 720p one, and a range file overlapping a standalone all
	// put SEVERAL Files on ONE Slot, and every one of them has to be in hand
	// before the Episode can be assembled. Emitting one ResolvedEpisode per file
	// instead — which is what this branch used to do — gave two trees the same
	// identity_key, and writeTitleSubtree's `DELETE FROM editions WHERE title_id`
	// then made the second silently destroy the first's Edition and Files
	// (tv-episode-editions/01). Grouping here is what lets groupEditions apply the
	// convention's one policy to TV as it already does to a Movie folder: parts
	// join into one Edition, distinct quality tokens split into two, and a genuine
	// collision is flagged ambiguous rather than guessed.
	//
	// The Slot is keyed by the identity key rather than by (season, episode)
	// because that key IS what makes two trees one Title downstream, and it is the
	// only thing that stays right for the degraded kinds (a date token keys by its
	// raw date, and every absolute-numbered Episode keys by its number).
	parsed := map[string]*parsedEpisode{}
	var parsedOrder []string

	for _, in := range inputs {
		switch decision := decisions[in.Path]; decision.State() {
		case store.DecisionIgnored, store.DecisionUnassigned:
			// Neither contributes anything to the tree. In particular neither may
			// become an Unresolved row — an Unmatched File is a PARSE FAILURE the
			// Admin still has to resolve, and counting a decision they already made
			// as work would double-count it in the Needs-Fixing queue. (The two
			// differ only outside resolve: an Unassigned File is still listed in the
			// matcher and keeps its Show queued; an Ignored one is settled and
			// silent.)
			continue
		case store.DecisionPlaced:
			// One Slot per placement row: several rows sharing a path split the File
			// across Slots, several rows sharing a Slot make it multi-part. The junk
			// heuristic is skipped on purpose — the Admin looked at this file and
			// said where it goes, which outranks a guess from its size.
			for _, p := range decision.Placements() {
				k := slotKey{season: p.GroupNumber, episode: p.SlotNumber}
				slots[k] = append(slots[k], placedFile{path: p.Path, ordinal: p.Ordinal})
			}
			slotsPerPath[in.Path] = len(decision.Placements())
			continue
		}

		if in.Junk {
			continue // sample/junk ignored entirely
		}
		base := stripKnownExt(filepath.Base(in.Path))
		tok, ok := ParseEpisodeToken(base, in.SeasonHint)
		if !ok {
			out.Unresolved = append(out.Unresolved,
				UnresolvedFile{Path: in.Path, Reason: "no recognized episode token (SxxExx / date / absolute)"})
			continue
		}

		// One File → two Episode Titles for a range (S01E05-E06): both get the same
		// physical File (it plays once); watch state is per-Title so marking one
		// watched is propagated to the other by the playback layer.
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
				displayName += " (" + episodeCode(tok.Season, epNum, epNum) + ")"
			}
			key := episodeIdentityKey(show, tok.Season, epTok)
			pe, ok := parsed[key]
			if !ok {
				pe = &parsedEpisode{}
				parsed[key] = pe
				parsedOrder = append(parsedOrder, key)
			}
			pe.files = append(pe.files, parsedFile{
				path:        in.Path,
				ordinal:     partNumber(filepath.Base(in.Path)),
				season:      tok.Season,
				episode:     epNum,
				displayName: displayName,
				label:       episodeLabelFor(tok),
				// A date/absolute token needs Enrichment to map it canonically.
				needsReview: tok.Kind != "sxxexx",
			})
		}
	}

	// Every File claiming a parsed Slot has now been seen, so its Episode can be
	// built from all of them at once.
	for _, key := range parsedOrder {
		files := parsed[key].files
		// Part order first, path as the tiebreak — the same ordering the placed
		// branch uses, so parts[0] is part 1 and the choice is independent of walk
		// order. groupEditions re-sorts within each Edition by the same rule; this
		// sort is what fixes files[0], the File whose filename names the Episode.
		sort.Slice(files, func(i, j int) bool {
			if files[i].ordinal != files[j].ordinal {
				return files[i].ordinal < files[j].ordinal
			}
			return files[i].path < files[j].path
		})
		// Every File in the group shares the identity key, so all of them agree on
		// the token kind and therefore on the label and the needs-review flag; the
		// season agrees too for a canonical SxxExx (it is IN the key). Only a
		// date-keyed Episode can disagree about the season, since its key is the raw
		// date alone — files[0] settles it, deterministically.
		lead := files[0]
		rfs := make([]ResolvedFile, 0, len(files))
		for _, f := range files {
			// JointEdition is deliberately NOT set: unlike a Slot the Admin filled by
			// hand, these Files still get to say what Edition they are through their
			// filenames, which is the whole point of grouping them here.
			rfs = append(rfs, ResolvedFile{Path: f.path, PartOrdinal: f.ordinal})
		}
		out.Episodes = append(out.Episodes, ResolvedEpisode{
			SeasonNumber:  lead.season,
			EpisodeNumber: lead.episode,
			IdentityKey:   key,
			DisplayName:   lead.displayName,
			SortTitle:     sortTitle(lead.displayName),
			EpisodeLabel:  lead.label,
			NeedsReview:   lead.needsReview,
			Files:         rfs,
		})
	}

	// Every File claiming a Slot has now been seen, so the placed Episodes can be
	// built. This is the merge/split step: a Slot several Files claim becomes ONE
	// Episode with a multi-part Edition, and a File claiming several Slots becomes
	// one Episode per Slot, all sharing the path.
	for _, k := range sortedSlots(slots) {
		parts := slots[k]
		// Ordinal decides which half plays first and therefore the joint timeline
		// (Edition.PartAt / TotalDurationMs); path is only the tiebreak, because the
		// filenames are exactly what the Admin was overruling. This sort is what
		// fixes parts[0] — the part whose filename names the Episode —
		// deterministically.
		sort.Slice(parts, func(i, j int) bool {
			if parts[i].ordinal != parts[j].ordinal {
				return parts[i].ordinal < parts[j].ordinal
			}
			return parts[i].path < parts[j].path
		})

		joint := len(parts) > 1
		files := make([]ResolvedFile, 0, len(parts))
		for _, part := range parts {
			files = append(files, ResolvedFile{
				Path: part.path, PartOrdinal: part.ordinal, JointEdition: joint,
			})
		}

		// THE key line of the replay: episodeIdentityKey is called with the ASSIGNED
		// season and slot, never the parse's. It is what makes Apply's in-place
		// re-key and the Scanner's recomputation agree, so an afternoon's sorting
		// survives the next scheduled scan (ADR-0044, ADR-0014).
		slotTok := EpisodeToken{Kind: "sxxexx", Season: k.season, Episode: k.episode, EpisodeEnd: k.episode}
		displayName := placedEpisodeName(parts[0].path, k, slotsPerPath[parts[0].path] > 1)
		out.Episodes = append(out.Episodes, ResolvedEpisode{
			SeasonNumber:  k.season,
			EpisodeNumber: k.episode,
			IdentityKey:   episodeIdentityKey(show, k.season, slotTok),
			DisplayName:   displayName,
			SortTitle:     sortTitle(displayName),
			// The Slot is canonical numbers, so it labels by them (no date /
			// absolute-number fallback label) and never needs review.
			EpisodeLabel: "",
			NeedsReview:  false,
			Placed:       true,
			Files:        files,
		})
	}

	sort.Slice(out.Episodes, func(i, j int) bool {
		a, b := out.Episodes[i], out.Episodes[j]
		switch {
		case a.SeasonNumber != b.SeasonNumber:
			return a.SeasonNumber < b.SeasonNumber
		case a.EpisodeNumber != b.EpisodeNumber:
			return a.EpisodeNumber < b.EpisodeNumber
		case a.SortTitle != b.SortTitle:
			return a.SortTitle < b.SortTitle
		default:
			return a.IdentityKey < b.IdentityKey
		}
	})

	seen := map[int]bool{}
	for _, ep := range out.Episodes {
		if !seen[ep.SeasonNumber] {
			seen[ep.SeasonNumber] = true
			out.Seasons = append(out.Seasons, ep.SeasonNumber)
		}
	}
	sort.Ints(out.Seasons)
	return out
}

// SeasonHintForPath derives the season a File's FOLDER suggests, exactly as the
// Scanner's walk does: a `Season NN` / `Specials` parent folder supplies its
// number, and a File loose in the Show folder supplies -1 ("no hint — read the
// season off the filename").
//
// It is exported because Apply works from stored File rows rather than a walk and
// must arrive at the same hint the walk would have handed the resolver; the walk
// itself calls it too, so there is one rule rather than two.
func SeasonHintForPath(path string) int {
	if season, ok := ParseSeasonFolder(filepath.Base(filepath.Dir(path))); ok {
		return season
	}
	return -1
}

// SeasonIdentityKey is a Season's stable key within a Show. Shared for the same
// reason as everything else here: Apply creates Season rows for assigned seasons
// that have none, and a scan must then re-resolve those rows rather than
// duplicate them.
func SeasonIdentityKey(showKey string, season int) string {
	return showKey + "|s" + pad2(season)
}

// BuildEpisodeEditions groups a resolved Episode's ALREADY-STORED Files into the
// Editions a scan would build for it, returning the Editions, whether the Episode
// is Ambiguous, and ok=false when any of its Files has no stored row yet.
//
// This is Apply's half of the probe seam. The Scanner reaches the same grouping
// through assembleTitle, which probes what it must and reuses stored rows for
// unchanged files; Apply has only stored rows and never probes, so a Placement
// onto a File the catalog has never seen (an Unmatched file) cannot be built here
// — ok=false says so, and the caller defers that Episode to the next scan rather
// than inventing attributes for it.
func BuildEpisodeEditions(files []ResolvedFile, stored map[string]store.File) ([]store.Edition, bool, bool) {
	ps := make([]probedFile, 0, len(files))
	for _, rf := range files {
		f, ok := stored[rf.Path]
		if !ok {
			return nil, false, false
		}
		cf := classifiedFile{
			path: rf.Path, name: filepath.Base(rf.Path),
			part: rf.PartOrdinal, jointEdition: rf.JointEdition,
		}
		ps = append(ps, probedFile{
			cf: cf, reused: true, stored: f, mtime: f.Mtime, size: f.SizeBytes,
			ed: editionNameFor(cf, f.Height),
		})
	}
	editions, ambiguous := groupEditions(ps)
	return editions, ambiguous, true
}
