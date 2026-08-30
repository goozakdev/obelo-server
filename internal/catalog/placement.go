package catalog

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/google/uuid"
	"github.com/goozakdev/obelo-server/internal/scanner"
	"github.com/goozakdev/obelo-server/internal/store"
)

// placement.go is Apply: the Admin has rearranged which File fills which Slot
// inside an already-identified Show, and this commits it (ADR-0044).
//
// It is the closest sibling of identity_correction.go — the same shape of
// operation, one level finer — with one deliberate reversal that a reader must
// not miss. A Wrong-item correction RESETS watch state because the Admin said the
// file is a different work (ADR-0019). A Placement KEEPS it, because they said
// the same work was filed in the wrong place. Every re-key here is an UPDATE for
// that reason: the title_id never moves, so no User loses their history, and the
// next scan replays the same decisions, computes the same key and finds the same
// row.
//
// Apply takes effect immediately — the season list is expected to be correct the
// moment the screen closes, and ADR-0044 rejected applying through a Targeted
// scan as too slow. That makes Apply the SECOND writer of Title structure beside
// the Scanner, and ADR-0044 names their disagreement as the feature's central
// risk: if the two ever derive a different arrangement from the same decisions, a
// scheduled scan silently rearranges a Show the Admin sorted by hand, overnight.
// So the derivation is not repeated here. scanner.ResolveEpisodes decides the
// whole shape — the Slots, the identity keys, the part order, the joint Edition,
// the names — and this file only turns that answer into row moves.

var (
	// ErrScanRunning means a scan holds the Library's lock, so Apply refused
	// rather than write catalog rows underneath it (ADR-0031's one-scan-per-Library
	// rule, and the same idempotent posture a rejected scan already has). Nothing
	// was written; the Admin can simply try again.
	ErrScanRunning = errors.New("catalog: a scan is running for this library")
	// ErrSlotCollision means the arrangement would resolve two distinct Titles onto
	// one Slot. writeTitleRow resolves a Title by (library_id, identity_key), so the
	// second of two Episodes sharing a key does not error — it silently overwrites
	// the first's whole subtree. Apply refuses to commit such a set rather than let
	// a later scan collapse them unobserved.
	//
	// It is now a BACKSTOP rather than a case an Admin can reach. It was written
	// when scanner.ResolveEpisodes emitted one Episode per FILE on the parsed path,
	// so two filenames claiming one Slot really did produce two Titles with one key
	// and the destructive overwrite above. The resolver groups a Slot's Files before
	// building it now (tv-episode-editions/01), so a parse-vs-parse collision
	// resolves to ONE Episode holding both Files and flagged ambiguous — the
	// convention's answer — and the only remaining way to contest a key is a
	// Placement against a parse, which resolveWithoutCollisions settles by
	// displacing the parsed File. The sentinel and its 409 stay because the
	// invariant they assert (no two Titles share a key) is still the thing that
	// makes a write safe, and asserting it costs a map lookup.
	ErrSlotCollision = errors.New("catalog: two files would resolve to the same slot")
	// ErrOutsideShow means a decision named a File that does not live under the
	// Show being rearranged. The matcher is Show-scoped, and a decision is stored
	// per (library, path) with no Show on it — so accepting a foreign path would
	// let an Apply on one Show quietly claim another's File, and the next scan
	// would replay it there.
	ErrOutsideShow = errors.New("catalog: the file is not part of this show")
	// ErrEmptySlot means a record was pinned onto a Slot that no File fills. A
	// record decorates something: an empty Slot has nothing to decorate and, more
	// concretely, no Title to carry the pin — so accepting it would mean storing
	// the Admin's decision nowhere and reporting success (ADR-0044, "a Slot with no
	// File is real but invisible").
	ErrEmptySlot = errors.New("catalog: a record can only be pinned on a slot that has a file")
)

// SlotCollisionError is ErrSlotCollision with the facts an Admin needs to fix it:
// WHICH Slot is contested and WHICH Files claim it.
//
// A bare 409 is useless here. The collision Apply refuses is never one the matcher
// created — a Placement's own collision is settled by displacing the parsed File —
// so it is always two FILENAMES already claiming one Slot, which the Admin has not
// seen and cannot guess. Naming the Slot and both paths is what lets the screen
// offer the three real fixes: merge them onto that Slot as parts, move one to a
// free Slot, or take one off its Slot entirely.
type SlotCollisionError struct {
	// GroupNumber / SlotNumber are the contested Slot's position (season/episode
	// for TV, disc/track for Music), in the local library's own numbering.
	GroupNumber int
	SlotNumber  int
	// IdentityKey is the key both Episodes would resolve to — the mechanism of the
	// collision, kept for logs rather than for the Admin.
	IdentityKey string
	// Paths are the Files claiming the Slot, in the order the resolver saw them.
	Paths []string
}

func (e *SlotCollisionError) Error() string {
	return fmt.Sprintf("%s: S%02dE%02d (%q) is claimed by %v — merge them onto one slot, move one, or take one off its slot",
		ErrSlotCollision.Error(), e.GroupNumber, e.SlotNumber, e.IdentityKey, e.Paths)
}

// Unwrap keeps errors.Is(err, ErrSlotCollision) true, so every caller written
// against the sentinel still works and only the ones that want the detail reach
// for the type.
func (e *SlotCollisionError) Unwrap() error { return ErrSlotCollision }

// LibraryLock is the per-Library scan lock (ADR-0031). *scanner.Service satisfies
// it. One scan per Library at a time was chosen over finer-grained locking, and
// Apply is a catalog writer just as a scan is, so it takes the SAME lock rather
// than inventing a second scheme: a rearrangement half-written under a running
// scan is exactly the row race the rule exists to prevent.
//
// A nil lock (a Service built without a scanner, as in a unit test) means no
// lock is available and Apply proceeds.
type LibraryLock interface {
	// LockLibrary claims the Library for a non-scan catalog writer, returning false
	// when a scan (or another Apply) already holds it.
	LockLibrary(libraryID string) bool
	UnlockLibrary(libraryID string)
}

// Reenricher queues the re-enrichment Apply asks for once its transaction has
// committed. It must not block: enrichment is the optional decorator step, a
// provider timeout must never fail an Apply that has already succeeded, and the
// new titles and stills reach clients over the existing SSE channel (ADR-0016)
// whenever they arrive.
type Reenricher interface {
	ReenrichLibrary(libraryID string)
}

// SetLibraryLock installs the per-Library scan lock. The app wires the scanner
// service; leaving it unset disables the guard (tests, one-shot tools).
func (s *Service) SetLibraryLock(l LibraryLock) { s.libraryLock = l }

// SetReenricher installs the post-commit re-enrichment queue. Leaving it unset
// means an Apply simply leaves the moved Titles' Enrichment marked 'pending' for
// the next scheduled pass to pick up.
func (s *Service) SetReenricher(r Reenricher) { s.reenricher = r }

// PlacementInput is one Show's whole arrangement as the Admin left it: the
// COMPLETE sparse decision set for the Show, not a delta. A File returned to
// "follow the parse" is expressed by the absence of its rows, which is only
// meaningful against the whole set (see store.FileDecisionSet).
type PlacementInput struct {
	ShowID    string
	Decisions []store.FileDecision
	// Pins repoint what DECORATES a Slot, sparsely: a Slot the Admin did not touch
	// is absent and keeps whatever record it had. Unlike Decisions this is NOT the
	// whole set, and it cannot be — absence of a pin has no second meaning to
	// spend, since no pin is ever derived from a filename. Clearing one is said by
	// sending its Slot with Clear.
	Pins []SlotPin
}

// SlotPin is one Slot's record as the Admin left it, addressed by the Slot's own
// LOCAL position. The record it names is a position in ANOTHER (or the same)
// provider series, and stays there: repointing changes what decorates a Slot and
// never where it sits (ADR-0044).
type SlotPin struct {
	// Position is the local Slot being repointed.
	Position SlotPosition
	// Series is the provider series to borrow from. Empty means the series the
	// Slot's Title already resolves against — the same-series pin.
	Series string
	// Record is the borrowed record's position in that series.
	Record SlotPosition
	// Clear returns the Slot to its default record: this series, this position.
	Clear bool
}

// PlacementResult reports what Apply made of the arrangement, so the caller can
// tell the Admin rather than let them guess.
type PlacementResult struct {
	LibraryID string
	// Rearranged counts the Titles whose Slot changed (and whose Enrichment was
	// therefore reset to 'pending').
	Rearranged int
	// Displaced are Files Apply wrote a decision for on the Admin's behalf: a File
	// that still PARSED onto a Slot another File was placed on. Displacing a parsed
	// file is itself a decision and has to be written as one, or the next scan
	// re-places it from the very filename it was displaced from and both resolve to
	// the same Slot again.
	Displaced []string
	// Deferred are placed Files the catalog has never probed (an Unmatched file the
	// Admin placed for the first time). Their decision is stored, but no Title can
	// be built from them without ffprobe — which Apply deliberately does not run —
	// so those Episodes appear on the next scan instead of immediately.
	Deferred []string
}

// ApplyPlacement commits one Show's arrangement: the sparse decisions the Scanner
// will replay, and the live Title rows that make them true right now.
//
// It fails cleanly and writes nothing when a scan holds the Library lock
// (ErrScanRunning) or when the arrangement would put two distinct Titles on one
// Slot (ErrSlotCollision), and the whole write is one transaction, so a failure
// part-way leaves the Show exactly as it was.
func (s *Service) ApplyPlacement(in PlacementInput) (PlacementResult, error) {
	sh, err := s.store.ShowByID(in.ShowID)
	if err != nil {
		return PlacementResult{}, err // ErrNotFound flows through
	}
	if err := validateDecisions(in.Decisions); err != nil {
		return PlacementResult{}, err
	}

	if s.libraryLock != nil {
		if !s.libraryLock.LockLibrary(sh.LibraryID) {
			return PlacementResult{}, ErrScanRunning
		}
		defer s.libraryLock.UnlockLibrary(sh.LibraryID)
	}

	arrangement, result, err := s.planPlacement(sh, in.Decisions, in.Pins)
	if err != nil {
		return PlacementResult{}, err
	}
	if err := s.store.ApplyShowArrangement(arrangement); err != nil {
		return PlacementResult{}, err
	}

	// Outside the transaction, and best-effort by construction: the moved Titles
	// are already marked 'pending', so even a re-enrich that never runs is only a
	// delay until the next scheduled pass (ADR-0016).
	if s.reenricher != nil && result.Rearranged > 0 {
		s.reenricher.ReenrichLibrary(sh.LibraryID)
	}
	return result, nil
}

// planPlacement derives everything ApplyShowArrangement will write. It is pure
// apart from its reads, so the whole decision — which row moves where, what folds
// onto what — is settled before a single write happens.
func (s *Service) planPlacement(
	sh store.Show, submitted []store.FileDecision, pins []SlotPin,
) (store.ShowArrangement, PlacementResult, error) {
	result := PlacementResult{LibraryID: sh.LibraryID}

	showFiles, err := s.store.ShowFiles(sh.ID)
	if err != nil {
		return store.ShowArrangement{}, result, err
	}
	titleByKey, err := s.store.EpisodeTitleIDs(sh.ID)
	if err != nil {
		return store.ShowArrangement{}, result, err
	}
	// Which provider series each existing Episode Title resolves against, carried
	// into the tree so a row Apply INSERTS inherits it — the co-File siblings of a
	// split, which have no prior row to keep an override on.
	//
	// It used to be load-bearing for every Episode, not just the new ones: the tree
	// write blanked tmdb_id from whatever it was handed, so an Apply that did not
	// carry the series forward stripped the Enrichment override off the whole Show,
	// leaving each pin's season/episode pointing into the Show's OWN series — the
	// collision ADR-0044 exists to prevent. The record now lives in its own column,
	// which no tree write touches on an existing row (ADR-0045), so an existing row
	// keeps its own record whether or not this map has it.
	slots, err := s.store.ShowEpisodeSlots(sh.ID)
	if err != nil {
		return store.ShowArrangement{}, result, err
	}
	seriesOf := map[string]string{}
	for _, es := range slots {
		seriesOf[es.TitleID] = es.RecordSeries
	}

	// The replace scope: every File the Admin was looking at, whether or not it
	// ended up with a row. A path that keeps no decision must still be in scope, or
	// its OLD rows survive the Apply and the next scan replays a correction the
	// Admin just took back.
	scope := map[string]bool{}
	present := map[string]bool{}
	folders := map[string]bool{}
	for _, f := range showFiles {
		scope[f.Path] = true
		present[f.Path] = present[f.Path] || f.Present
		folders[showFolderOf(f.Path)] = true
	}
	for _, d := range submitted {
		// A decision carries no Show of its own, only (library, path), so a foreign
		// path would be replayed against whichever Show its folder resolves to —
		// silently rearranging a Show the Admin was not even looking at.
		if len(folders) > 0 && !folders[showFolderOf(d.Path)] {
			return store.ShowArrangement{}, result,
				fmt.Errorf("%w: %q", ErrOutsideShow, d.Path)
		}
		scope[d.Path] = true
	}

	decisions := groupDecisions(submitted)
	arrangement, displaced, err := s.resolveWithoutCollisions(sh, sortedPaths(scope), present, decisions)
	if err != nil {
		return store.ShowArrangement{}, result, err
	}
	result.Displaced = displaced

	// Stored File rows for everything the arrangement wants to build. Apply reuses
	// them verbatim — it never probes — which is the one genuine difference between
	// it and the Scanner.
	stored := map[string]store.File{}
	for _, ep := range arrangement.Episodes {
		for _, rf := range ep.Files {
			if _, ok := stored[rf.Path]; ok {
				continue
			}
			f, err := s.store.LoadStoredFile(rf.Path)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return store.ShowArrangement{}, result, err
			}
			stored[rf.Path] = f
		}
	}

	// Who owns each path today, and where each Title sits, so a re-key can move the
	// row that carries the watch state rather than mint a new one beside it.
	type titleInfo struct {
		key           string
		seasonNumber  int
		episodeNumber int
	}
	owners := map[string][]string{}
	info := map[string]titleInfo{}
	for _, f := range showFiles {
		owners[f.Path] = appendUnique(owners[f.Path], f.TitleID)
		info[f.TitleID] = titleInfo{f.IdentityKey, f.SeasonNumber, f.EpisodeNumber}
	}

	type planned struct {
		ep        scanner.ResolvedEpisode
		editions  []store.Edition
		ambiguous bool
		survivor  string
		sources   []store.WatchFoldPart
	}
	var plan []planned
	claimed := map[string]bool{}
	deferredTitles := map[string]bool{}
	inTree := map[string]bool{}
	// The Slots whose Episode waits for the next scan. A pin cannot land on one —
	// there is no Title yet to carry it — but it is not an empty Slot either, so it
	// is refused differently: the file is already reported as Deferred.
	deferredSlots := map[SlotPosition]bool{}

	for _, ep := range arrangement.Episodes {
		editions, ambiguous, ok := scanner.BuildEpisodeEditions(ep.Files, stored)
		if !ok {
			deferredSlots[SlotPosition{Group: ep.SeasonNumber, Slot: ep.EpisodeNumber}] = true
			// A Placement onto a File the catalog has never probed. The decision is
			// stored either way; the Episode waits for the next scan rather than being
			// invented from attributes Apply does not have.
			for _, rf := range ep.Files {
				result.Deferred = append(result.Deferred, rf.Path)
				inTree[rf.Path] = true
				for _, id := range owners[rf.Path] {
					deferredTitles[id] = true
				}
			}
			continue
		}
		for _, rf := range ep.Files {
			inTree[rf.Path] = true
		}
		plan = append(plan, planned{ep: ep, editions: editions, ambiguous: ambiguous})
	}

	// Survivor selection, in the Scanner's own order of preference. First the Title
	// that ALREADY holds this Slot's key and owns one of its Files: that is the row
	// writeTitleRow would resolve to on a scan, so preferring it keeps a split's
	// original Slot on its original row. Then, for a Slot whose key nothing holds,
	// the Title of its first File — which is what makes a MOVE carry its watch
	// state with it instead of leaving it behind on the old key.
	for i := range plan {
		id, ok := titleByKey[plan[i].ep.IdentityKey]
		if ok && !claimed[id] && ownsAny(owners, plan[i].ep.Files, id) {
			plan[i].survivor = id
			claimed[id] = true
		}
	}
	for i := range plan {
		if plan[i].survivor != "" {
			continue
		}
		for _, rf := range plan[i].ep.Files {
			for _, id := range owners[rf.Path] {
				if !claimed[id] {
					plan[i].survivor = id
					claimed[id] = true
					break
				}
			}
			if plan[i].survivor != "" {
				break
			}
		}
	}

	// Where each Title's watch state has to end up. The sources of a Slot are the
	// Titles that own its Files, in play order, each carrying the duration it
	// contributes to the joint timeline.
	for i := range plan {
		durations := map[string]int64{}
		var order []string
		for _, rf := range plan[i].ep.Files {
			f, ok := stored[rf.Path]
			if !ok {
				continue
			}
			for _, id := range owners[rf.Path] {
				if _, seen := durations[id]; !seen {
					order = append(order, id)
				}
				durations[id] += f.DurationMs
				break // one owner per file on the joint timeline
			}
		}
		for _, id := range order {
			plan[i].sources = append(plan[i].sources, store.WatchFoldPart{TitleID: id, DurationMs: durations[id]})
		}
	}

	out := store.ShowArrangement{
		ShowID:    sh.ID,
		LibraryID: sh.LibraryID,
		Decisions: store.FileDecisionSet{
			LibraryID: sh.LibraryID,
			Paths:     sortedPaths(scope),
			Decisions: flattenDecisions(decisions),
		},
	}

	seasons := map[int][]store.EpisodeTree{}
	var seasonOrder []int
	for _, p := range plan {
		if p.survivor != "" {
			out.Rekeys = append(out.Rekeys, store.TitleRekey{
				TitleID: p.survivor, IdentityKey: p.ep.IdentityKey,
			})
		}
		// A fold is needed exactly when the Slot's state does not already sit on the
		// row that will serve it: a MERGE (several source Titles collapsing onto one)
		// or a SPLIT (a second Slot on a brand-new co-File sibling row, inheriting the
		// original's state). A plain move needs none — the re-key carried the row, and
		// with it every User's history.
		if len(p.sources) > 0 && !(len(p.sources) == 1 && p.sources[0].TitleID == p.survivor) {
			out.Folds = append(out.Folds, store.WatchFold{
				TargetKey: p.ep.IdentityKey, Parts: p.sources,
			})
		}
		prior, known := info[p.survivor]
		if p.survivor == "" || !known || prior.key != p.ep.IdentityKey ||
			prior.seasonNumber != p.ep.SeasonNumber || prior.episodeNumber != p.ep.EpisodeNumber {
			out.PendingKeys = append(out.PendingKeys, p.ep.IdentityKey)
		}

		if _, ok := seasons[p.ep.SeasonNumber]; !ok {
			seasonOrder = append(seasonOrder, p.ep.SeasonNumber)
		}
		seasons[p.ep.SeasonNumber] = append(seasons[p.ep.SeasonNumber], store.EpisodeTree{
			TitleTree: store.TitleTree{
				Title: store.Title{
					ID:          uuid.NewString(),
					LibraryID:   sh.LibraryID,
					Kind:        "episode",
					Title:       p.ep.DisplayName,
					IdentityKey: p.ep.IdentityKey,
					SortTitle:   p.ep.SortTitle,
					NeedsReview: p.ep.NeedsReview,
					Ambiguous:   p.ambiguous,
				},
				// The surviving row's own Enrichment override, inherited by a row this
				// Apply INSERTS (a split's co-File siblings, which have no prior row to
				// keep it on). It is the ENRICHMENT record, not an identity id, so it
				// goes in RecordTMDBID (ADR-0045). Empty for a Slot nobody ever
				// repointed, which is the ordinary case.
				RecordTMDBID: seriesOf[p.survivor],
				Editions:     p.editions,
			},
			SeasonNumber:  p.ep.SeasonNumber,
			EpisodeNumber: p.ep.EpisodeNumber,
			EpisodeLabel:  p.ep.EpisodeLabel,
		})
	}
	sort.Ints(seasonOrder)
	for _, n := range seasonOrder {
		out.Seasons = append(out.Seasons, store.SeasonTree{
			SeasonNumber: n,
			IdentityKey:  scanner.SeasonIdentityKey(sh.IdentityKey, n),
			Episodes:     seasons[n],
		})
	}

	// Every other Episode Title of the Show serves no Slot any more. It is parked
	// alongside the movers (a mover may need the key it still holds) and then either
	// keeps its key as an empty, hidden, revivable row — which is exactly what a
	// scan leaves behind — or is dropped when another Title has taken that key.
	for _, id := range sortedValues(titleByKey) {
		if !claimed[id] && !deferredTitles[id] {
			out.Emptied = append(out.Emptied, id)
		}
	}

	// Files that belong to no Slot any more: the Admin's Unassigned and Ignored
	// decisions, and anything the arrangement simply did not place. They are
	// soft-deleted so they leave browse, exactly as a scan's Missing pass does;
	// nothing on disk is touched.
	for _, p := range sortedPaths(scope) {
		if !inTree[p] {
			out.AbsentPaths = append(out.AbsentPaths, p)
		}
	}

	// The pins, last: a Slot's record is a separate decision from where its File
	// sits (ADR-0044), so it is resolved against the arrangement that was just
	// planned rather than against the one the screen was opened on.
	keyAt := map[SlotPosition]string{}
	for _, p := range plan {
		keyAt[SlotPosition{Group: p.ep.SeasonNumber, Slot: p.ep.EpisodeNumber}] = p.ep.IdentityKey
	}
	// The Show's own record, resolved the ONE way every other read resolves it: an
	// Enrichment override outranks the id the folder name asserts (showSeries,
	// ADR-0045). A Clear hands this back to the Slot, and `shows.tmdb_id` is the
	// wrong half of that answer — empty for the ordinary searched-and-matched Show,
	// and the folder's embedded token for a Show whose record an Admin corrected
	// away from it.
	series, err := s.showSeries(sh)
	if err != nil {
		return store.ShowArrangement{}, result, err
	}
	out.Pins, err = resolvePins(series, pins, keyAt, deferredSlots)
	if err != nil {
		return store.ShowArrangement{}, result, err
	}

	result.Rearranged = len(out.PendingKeys)
	return out, result, nil
}

// resolvePins turns the Admin's per-position records into the per-identity_key
// writes the storage layer applies, refusing the one shape that cannot mean
// anything: a record on a Slot no File fills.
//
// A Slot whose Episode is DEFERRED is skipped rather than refused. Its File is
// already reported in `deferred` — "stored, but not playable until the next scan"
// — and failing the whole Apply because a record could not be attached to a Title
// that does not exist yet would throw away the rest of the Admin's work.
func resolvePins(
	series string, pins []SlotPin,
	keyAt map[SlotPosition]string, deferred map[SlotPosition]bool,
) ([]store.SlotPin, error) {
	var out []store.SlotPin
	seen := map[SlotPosition]bool{}
	for _, pin := range pins {
		if seen[pin.Position] {
			return nil, fmt.Errorf("catalog: slot %d/%d carries two records",
				pin.Position.Group, pin.Position.Slot)
		}
		seen[pin.Position] = true
		if !pin.Clear && pin.Record.Slot <= 0 {
			return nil, fmt.Errorf("catalog: the record for slot %d/%d names no slot of its own",
				pin.Position.Group, pin.Position.Slot)
		}
		key, ok := keyAt[pin.Position]
		if !ok {
			if deferred[pin.Position] {
				continue
			}
			return nil, fmt.Errorf("%w: %d/%d", ErrEmptySlot, pin.Position.Group, pin.Position.Slot)
		}
		if pin.Clear {
			// Back to the Show's own series at this Slot's own position. The series is
			// written explicitly because the Title may still be carrying a borrowed one,
			// and it is the RESOLVED series (showSeries) rather than shows.tmdb_id: the
			// Slot goes back to the record that actually decorates its Show, which for
			// an ordinarily-matched Show is not in that column at all and for a
			// {tmdb-N} folder an Admin has corrected is not the value in it.
			//
			// Writing a real id here used to have a side effect that made it unsafe —
			// enrich.childHasOwnOverride read any non-empty record id as "the Admin
			// chose this" and would have excluded every cleared Slot from later
			// Cascades (file-matcher/10 left it alone for exactly that reason). It
			// reads enrichment_id_origin now, and applyPinsTx RELEASES it on a
			// Clear, so the Slot keeps an anchor of its own — which is what a
			// single-Title re-enrich and the Edit-item image tab look a leaf up by —
			// while staying eligible for its Show's next "apply to children".
			out = append(out, store.SlotPin{IdentityKey: key, SeriesID: series, Clear: true})
			continue
		}
		out = append(out, store.SlotPin{
			IdentityKey: key,
			SeriesID:    pin.Series,
			Season:      pin.Record.Group,
			Episode:     pin.Record.Slot,
		})
	}
	return out, nil
}

// resolveWithoutCollisions runs the shared derivation and settles the one
// collision Apply is able to create.
//
// Two Episodes carrying the same identity_key do not error downstream — the
// second silently overwrites the first's whole subtree — and a Placement makes
// that easy to reach: place File A on S03E05 while File B still PARSES to S03E05
// and has no decision of its own, and both resolve to the same Slot. This is
// exactly what the Unassigned state exists for. Displacing a parsed File is itself
// a decision and is written as one here, on the Admin's behalf, because leaving it
// unrecorded means the next scan re-places it from the very filename it was
// displaced from.
//
// A displaced File keeps whatever OTHER Slots its filename claimed (the surviving
// half of a range file) as explicit Placements, and becomes Unassigned only when
// nothing is left — the per-File record cannot say "keep half" any other way.
//
// The loop then re-resolves, because a displacement can uncover another. The
// refusal at the end is a backstop only: a collision no Placement caused — two
// Files whose FILENAMES claim one Slot — is no longer a collision at all, because
// the resolver puts both Files on that one Slot and flags the Episode ambiguous
// (tv-episode-editions/01). See ErrSlotCollision.
func (s *Service) resolveWithoutCollisions(
	sh store.Show, paths []string, present map[string]bool,
	decisions map[string]store.FileDecisions,
) (scanner.ResolvedShow, []string, error) {
	var displaced []string
	show := scanner.Identity{Key: sh.IdentityKey}

	// Bounded by construction: each pass converts at least one parse-derived File
	// into an explicit decision, and an explicit decision is never converted back.
	for pass := 0; pass <= len(paths)+1; pass++ {
		arrangement := scanner.ResolveEpisodes(show, placementInputs(paths, present, decisions), decisions)

		byKey := map[string][]scanner.ResolvedEpisode{}
		var keys []string
		for _, ep := range arrangement.Episodes {
			if _, seen := byKey[ep.IdentityKey]; !seen {
				keys = append(keys, ep.IdentityKey)
			}
			byKey[ep.IdentityKey] = append(byKey[ep.IdentityKey], ep)
		}

		// Which keys are contested, and which of those a Placement is responsible for.
		contested := map[string]bool{}
		placedContested := map[string]bool{}
		for _, k := range keys {
			if len(byKey[k]) < 2 {
				continue
			}
			contested[k] = true
			for _, ep := range byKey[k] {
				if ep.Placed {
					placedContested[k] = true
				}
			}
		}
		if len(contested) == 0 {
			return arrangement, displaced, nil
		}

		// Displace the parsed Files a Placement pushed off their Slot. A parsed
		// Episode can hold SEVERAL Files (parts, or two quality-tagged rips of one
		// Episode — scanner.ResolveEpisodes groups them), so every one of them is
		// displaced, not just the first.
		moved := false
		for _, k := range keys {
			if !placedContested[k] {
				continue
			}
			for _, ep := range byKey[k] {
				if ep.Placed {
					continue
				}
				for _, rf := range ep.Files {
					path := rf.Path
					if decisions[path].State() != "" {
						continue // already decided; nothing to displace
					}
					var keep []store.FileDecision
					for _, other := range arrangement.Episodes {
						if other.Placed || !claimsPath(other, path) {
							continue
						}
						if placedContested[other.IdentityKey] {
							continue // this half is the one being displaced
						}
						keep = append(keep, store.FileDecision{
							Path: path, State: store.DecisionPlaced,
							GroupNumber: other.SeasonNumber, SlotNumber: other.EpisodeNumber, Ordinal: 1,
						})
					}
					if len(keep) == 0 {
						keep = []store.FileDecision{{Path: path, State: store.DecisionUnassigned}}
					}
					decisions[path] = keep
					displaced = appendUnique(displaced, path)
					moved = true
				}
			}
		}
		if !moved {
			var worst string
			for _, k := range keys {
				if contested[k] {
					worst = k
					break
				}
			}
			var claimants []string
			for _, ep := range byKey[worst] {
				for _, rf := range ep.Files {
					claimants = appendUnique(claimants, rf.Path)
				}
			}
			return scanner.ResolvedShow{}, nil, &SlotCollisionError{
				GroupNumber: byKey[worst][0].SeasonNumber,
				SlotNumber:  byKey[worst][0].EpisodeNumber,
				IdentityKey: worst,
				Paths:       claimants,
			}
		}
	}
	return scanner.ResolvedShow{}, nil, fmt.Errorf(
		"%w: the arrangement could not be settled", ErrSlotCollision)
}

// claimsPath reports whether a resolved Episode is built from the given File.
// It looks at every File rather than the first: an Episode can legitimately hold
// several (the parts of a multi-part Edition, or two quality-distinguished rips
// grouped onto one Slot), and asking only about Files[0] would lose the surviving
// half of a range file when its other half is displaced.
func claimsPath(ep scanner.ResolvedEpisode, path string) bool {
	for _, rf := range ep.Files {
		if rf.Path == path {
			return true
		}
	}
	return false
}

// placementInputs is Apply's half of the probe seam: the Scanner hands
// ResolveEpisodes what it walked, Apply hands it what the catalog already holds.
//
// A File is offered when it is on disk as of the last scan, or when the Admin has
// just said something about it — which covers re-placing a File they had
// previously taken off its Slot (soft-deleted, so `present` is 0 while the file
// is very much still there). A Missing File nobody mentioned is left out, exactly
// as the Scanner's walk leaves out a file that is no longer on disk.
//
// Junk is always false: a stored File row is one a scan already accepted, and a
// path the Admin placed by hand outranks a guess from its size anyway.
func placementInputs(paths []string, present map[string]bool, decisions map[string]store.FileDecisions) []scanner.EpisodeInput {
	var out []scanner.EpisodeInput
	for _, p := range paths {
		if !present[p] && decisions[p].State() == "" {
			continue
		}
		out = append(out, scanner.EpisodeInput{Path: p, SeasonHint: scanner.SeasonHintForPath(p)})
	}
	return out
}

// validateDecisions rejects a set the storage layer would refuse anyway, with an
// error that names the offending path rather than a constraint.
func validateDecisions(decisions []store.FileDecision) error {
	states := map[string]string{}
	for _, d := range decisions {
		if d.Path == "" {
			return fmt.Errorf("catalog: a file decision needs a path")
		}
		switch d.State {
		case store.DecisionPlaced, store.DecisionUnassigned, store.DecisionIgnored:
		default:
			return fmt.Errorf("catalog: unknown file decision state %q for %q", d.State, d.Path)
		}
		// A File is either placed or settled, never both: resolve would have to
		// decide whether to build a Title from a File it was also told to skip.
		if prior, ok := states[d.Path]; ok {
			if (prior == store.DecisionPlaced) != (d.State == store.DecisionPlaced) {
				return fmt.Errorf("catalog: %q is both placed and settled (%s / %s)", d.Path, prior, d.State)
			}
			if prior != store.DecisionPlaced {
				return fmt.Errorf("catalog: %q carries two settled decisions", d.Path)
			}
		}
		states[d.Path] = d.State
	}
	return nil
}

// groupDecisions collects a submitted set into the per-path shape the resolver
// reads, ordered by (group, slot, ordinal) exactly as FileDecisionsByLibrary
// returns it — so a File spanning several Slots comes back in Slot order and the
// Files sharing one Slot come back in part order.
func groupDecisions(decisions []store.FileDecision) map[string]store.FileDecisions {
	out := map[string]store.FileDecisions{}
	for _, d := range decisions {
		if d.Ordinal < 1 {
			d.Ordinal = 1
		}
		out[d.Path] = append(out[d.Path], d)
	}
	for path := range out {
		rows := out[path]
		sort.Slice(rows, func(i, j int) bool {
			switch {
			case rows[i].GroupNumber != rows[j].GroupNumber:
				return rows[i].GroupNumber < rows[j].GroupNumber
			case rows[i].SlotNumber != rows[j].SlotNumber:
				return rows[i].SlotNumber < rows[j].SlotNumber
			default:
				return rows[i].Ordinal < rows[j].Ordinal
			}
		})
	}
	return out
}

// flattenDecisions returns the grouped set as rows to store, path-ordered so an
// Apply writes the same rows in the same order every time.
func flattenDecisions(decisions map[string]store.FileDecisions) []store.FileDecision {
	var out []store.FileDecision
	for _, path := range sortedMapKeys(decisions) {
		out = append(out, decisions[path]...)
	}
	return out
}

// showFolderOf is the Show folder a File lives under, derived the way the
// Scanner's walk lays a Show out: an Episode sits either directly in the Show
// folder or one level down in a `Season NN` / `Specials` folder.
func showFolderOf(path string) string {
	dir := filepath.Dir(path)
	if _, ok := scanner.ParseSeasonFolder(filepath.Base(dir)); ok {
		return filepath.Dir(dir)
	}
	return dir
}

func ownsAny(owners map[string][]string, files []scanner.ResolvedFile, titleID string) bool {
	for _, rf := range files {
		for _, id := range owners[rf.Path] {
			if id == titleID {
				return true
			}
		}
	}
	return false
}

func appendUnique(list []string, v string) []string {
	for _, s := range list {
		if s == v {
			return list
		}
	}
	return append(list, v)
}

func sortedPaths(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// sortedMapKeys orders a map's keys, so every list Apply derives from a map — and
// therefore the order of its writes — is deterministic.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedValues returns a map's values in key order, so Apply's Emptied list (and
// therefore its writes) is deterministic.
func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, k := range sortedMapKeys(m) {
		out = append(out, m[k])
	}
	return out
}
