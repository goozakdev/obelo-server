package catalog

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/goozakdev/obelo-server/internal/scanner"
	"github.com/goozakdev/obelo-server/internal/store"
)

// matcher.go is the READ behind the file matcher — one Show's whole working set:
// every Slot it has, every File under it, and what the Admin has said about each
// (ADR-0044). Its write half is placement.go.
//
// Two properties are load-bearing and easy to lose.
//
// THE FILE LIST MUST BE COMPLETE. The screen's promise is "here is everything on
// disk under this Show"; a File it omits is a File the Admin cannot place, and
// they have no way to tell an omission from an absence. The Files are therefore
// gathered from every place the ingestion path can leave one — the on-disk Files
// of the Show's Episode Titles, the Library's Unmatched rows under the Show's
// folders, and the paths carrying an explicit decision, which by design produce
// NEITHER a Title NOR an Unmatched row (see scanner/arrangement.go: a recorded
// decision is not a parse failure).
//
// COMPLETE, not longer than the disk. The mirror-image failure is a File the
// Admin cannot ACT on: a soft-deleted row whose path no longer exists (a rename,
// most often) is unplaceable by construction, so listing it manufactures work that
// no amount of sorting can finish. Only a decision brings such a path back.
//
// THE DERIVATION IS NOT REPEATED HERE. What the Files add up to — which Slots
// exist, which File fills which — is decided by scanner.ResolveEpisodes, the same
// function the Scanner and Apply run. If this file worked it out for itself, the
// matcher could show an arrangement neither writer would produce, which is the
// worst kind of wrong: it would look right until Apply disagreed with it.

// Why a Show's Slots cannot be listed from a provider. Every one of these is a
// FIRST-CLASS state, never an error: the screen still works for pure renumbering
// with bare numbered Slots, so the response carries the reason and the UI explains
// it (ADR-0044, "A Slot may have no record").
const (
	// SlotsNoSeries: the Show never matched a provider record, so there is no
	// series to list. Nothing is misconfigured; there is simply nothing to ask.
	SlotsNoSeries = "no-series-match"
	// SlotsEnrichmentDisabled: Enrichment is off for this server (switched off,
	// unconfigured, or consent not granted).
	SlotsEnrichmentDisabled = "enrichment-disabled"
	// SlotsProviderCannotList: the Authoritative provider does not implement the
	// optional episode-listing capability. Only TMDB does.
	SlotsProviderCannotList = "provider-cannot-list"
	// SlotsProviderUnreachable: the provider was asked and failed — offline server,
	// dead API, bad key. The one reason that may fix itself.
	SlotsProviderUnreachable = "provider-unreachable"
)

// Where a group's Slots came from, which is what tells the UI whether an empty
// Slot list means "the provider says this season has none" or "nobody has
// asked yet".
const (
	// SlotSourceLocal: the group's Slots are the positions local Files claim. Bare
	// numbers, no records.
	SlotSourceLocal = "local"
	// SlotSourceProvider: the provider knows this group and reported how many Slots
	// it has (their records arrive when the group is expanded).
	SlotSourceProvider = "provider"
)

// SlotGroupSummary is one group the provider knows about: its number and how many
// Slots it holds. It is what a COLLAPSED group needs — the count, not the records
// — and is why opening the matcher costs one provider call rather than one per
// group.
type SlotGroupSummary struct {
	Number    int
	SlotCount int
}

// SlotRecord is one Slot's provider record: what decorates the position, never
// the position itself (ADR-0044 keeps those two halves apart).
type SlotRecord struct {
	Group    int
	Slot     int
	Name     string
	Overview string
	AirDate  string
	// StillURL is the provider's own URL. The transport layer rewrites it to the
	// same-origin image proxy before it reaches a browser (ADR-0001).
	StillURL string
}

// SlotLister lists a series' groups and their Slots from the Library's
// Authoritative provider. The app wires it to the enrich service; nil (a Service
// built without one, as in a unit test) is the degraded path, not a failure —
// every Show then reports SlotsEnrichmentDisabled and serves bare Slots.
//
// It is deliberately kind-neutral and defined here rather than imported from
// enrich, so the Album matcher can hang a MusicBrainz release off the same seam
// without the browse domain learning about either provider.
type SlotLister interface {
	// Unavailable reports why Slots cannot be listed at all — one of the Slots*
	// constants — or "" when they can.
	Unavailable() string
	// ListGroups lists the series' groups. One call, whatever the group count.
	ListGroups(ctx context.Context, seriesID string) ([]SlotGroupSummary, error)
	// ListSlots lists one group's Slots with their records. One call per group,
	// made only when a group is expanded.
	ListSlots(ctx context.Context, seriesID string, group int) ([]SlotRecord, error)
}

// SetSlotLister installs the provider seam the matcher fills Slot records from.
// Leaving it unset is a supported state: the matcher degrades to bare numbered
// Slots, which is all pure renumbering needs.
func (s *Service) SetSlotLister(l SlotLister) { s.slotLister = l }

// SlotPosition is one Slot's position — always the local library's own numbering
// (season+episode for TV, disc+track for Music), never a provider's.
type SlotPosition struct {
	Group int
	Slot  int
}

// MatcherSlot is one Slot as the matcher shows it: its position, the Title
// serving it if any, and its record where one could be fetched.
type MatcherSlot struct {
	SlotPosition
	// TitleID is the Episode Title occupying this Slot, empty for an empty Slot.
	TitleID string
	// Name / Overview / AirDate / StillURL are the provider record, empty on the
	// degraded path and on a Slot the provider does not list.
	Name     string
	Overview string
	AirDate  string
	StillURL string
	// Record describes an Episode pin: the Admin repointed what decorates this
	// Slot, either at another position in the Show's own series (the common case —
	// the provider counts a run of episodes in the next season) or in another
	// series entirely (the Batman → New Batman Adventures shape). Nil where no pin
	// exists, because a Slot decorated from its own position has nothing to say.
	Record *SlotRecordRef
}

// SlotRecordRef is a repointed Slot's record: WHERE it was borrowed from, and
// what it says.
//
// The decoration rides alongside the reference rather than replacing the Slot's
// own Name/Overview/AirDate/StillURL, and that separation is the whole discipline
// of ADR-0044: the borrowed record supplies the words, the Slot keeps its own
// POSITION and its own default record, so clearing the pin shows what the Slot
// would say unpinned (for a Slot the Show's series does not list, nothing at all)
// and the borrowed NUMBERING never reaches the code the screen prints.
type SlotRecordRef struct {
	// Series is the provider series the record was borrowed from — the Show's own
	// when the pin only moved position within it.
	Series string
	// Position is the record's position IN THAT SERIES. It is provenance, never a
	// position this Slot takes on.
	Position SlotPosition
	// Name / Overview / AirDate / StillURL are the borrowed record itself, filled
	// only for the ONE group a caller expanded (and left empty when the provider
	// could not be asked).
	Name     string
	Overview string
	AirDate  string
	StillURL string
}

// MatcherGroup is one group (a season) with its Slots and its counts.
type MatcherGroup struct {
	Number int
	// Source is SlotSourceProvider or SlotSourceLocal — where this group's Slots
	// came from.
	Source string
	// SlotCount is the provider's own count for the group, 0 when unknown. It lets a
	// collapsed group say "24 slots, 19 filled" without fetching the records.
	SlotCount int
	// Loaded reports whether this group's provider records have been fetched. False
	// on every group of the first response; true for the one group a caller
	// expanded.
	Loaded bool
	// Unavailable is set when THIS group's records were asked for and could not be
	// had, leaving the rest of the response intact.
	Unavailable string
	// FileCount / PlacedCount / UnassignedCount / IgnoredCount count the Files whose
	// current arrangement puts them in this group — the collapsed row's summary.
	// An unassigned or ignored File counts in the group its FILENAME points at,
	// since that is the only group it can be said to be near.
	FileCount       int
	PlacedCount     int
	UnassignedCount int
	IgnoredCount    int
	Slots           []MatcherSlot
}

// MatcherPlacement is one Slot a File currently fills, with its order among the
// Files sharing that Slot.
type MatcherPlacement struct {
	SlotPosition
	Ordinal int
}

// MatcherFile is one File under the Show, in whatever state the Admin left it.
type MatcherFile struct {
	Path string
	// State is store.DecisionPlaced / DecisionUnassigned / DecisionIgnored — the
	// File's CURRENT state, whether decided or derived. A File nobody has said
	// anything about is placed when its filename numbers it and unassigned when it
	// does not (CONTEXT.md "Unassigned" reaches that state two ways).
	State string
	// TitleID is the Episode Title that owns this File today, empty for a File that
	// is part of none.
	TitleID string
	// Parsed are the Slots this File's FILENAME claims, ignoring every decision —
	// what the arrangement would be if the Admin had said nothing. The screen
	// compares it against Placements to highlight a disagreement, which is the
	// decision being made (PRD file-matcher).
	Parsed []SlotPosition
	// Placements are the Slots the File fills now. Empty for an unassigned or
	// ignored File.
	Placements []MatcherPlacement
	// Decided is true when a stored decision — not the filename — put the File
	// where it is. It is what Revert needs, and what tells "unassigned because the
	// Admin said so" from "unassigned because nothing could number it".
	Decided bool
	// Orphaned is true when a Placement's anchor file is no longer on disk. The
	// correction is broken rather than done and is surfaced, never dropped
	// (CONTEXT.md "Orphaned correction").
	Orphaned bool
	// Reason is why an unnumbered File could not be placed, from the Unmatched row
	// or from the parse itself. Empty when the File parses.
	Reason string
}

// Matcher is one container's whole working set — the response to "let me sort
// this Show out". Nothing in it is named after a season or an episode: the same
// document describes an Album's discs and tracks when the Music adapter lands.
type Matcher struct {
	ContainerID   string
	ContainerType string
	LibraryID     string
	Title         string
	Year          int
	// SeriesExternalID is the provider record the container matched, and the series
	// its Slots are listed from. Empty when it never matched.
	SeriesExternalID string
	// Unavailable is why no group has provider records at all — one of the Slots*
	// constants, empty when they could be fetched.
	Unavailable string
	Groups      []MatcherGroup
	Files       []MatcherFile
	// Applied is set only on the response to an Apply, reporting what the server
	// made of the submitted arrangement.
	Applied *PlacementResult
}

// ShowMatcher assembles one Show's working set.
//
// group selects the ONE group whose provider records are fetched (nil fetches
// none). That is the whole per-group loading rule: opening a ten-season Show costs
// a single ListGroups call and expanding a season costs one ListSlots, rather than
// ten on open. Everything LOCAL — every File, every decision, every group with its
// counts and the Slots local Files claim — is complete in every response and needs
// no provider at all.
func (s *Service) ShowMatcher(ctx context.Context, showID string, group *int) (Matcher, error) {
	sh, err := s.store.ShowByID(showID)
	if errors.Is(err, store.ErrNotFound) {
		return Matcher{}, ErrNotFound
	}
	if err != nil {
		return Matcher{}, err
	}

	series, err := s.showSeries(sh)
	if err != nil {
		return Matcher{}, err
	}

	out := Matcher{
		ContainerID:      sh.ID,
		ContainerType:    "show",
		LibraryID:        sh.LibraryID,
		Title:            sh.Title,
		Year:             sh.Year,
		SeriesExternalID: series,
	}

	local, err := s.localArrangement(sh)
	if err != nil {
		return Matcher{}, err
	}
	out.Files = local.files

	groups := map[int]*MatcherGroup{}
	groupOf := func(n int) *MatcherGroup {
		g, ok := groups[n]
		if !ok {
			g = &MatcherGroup{Number: n, Source: SlotSourceLocal}
			groups[n] = g
		}
		return g
	}
	// The local half of the Slot union: every position a File claims or already
	// holds. These exist whatever the provider says, which is precisely why the
	// screen works offline.
	slots := map[SlotPosition]*MatcherSlot{}
	slotOf := func(p SlotPosition) *MatcherSlot {
		sl, ok := slots[p]
		if !ok {
			sl = &MatcherSlot{SlotPosition: p}
			slots[p] = sl
			groupOf(p.Group)
		}
		return sl
	}
	for _, p := range local.positions {
		slotOf(p)
	}
	for _, es := range local.episodes {
		sl := slotOf(SlotPosition{Group: es.SeasonNumber, Slot: es.EpisodeNumber})
		sl.TitleID = es.TitleID
		// A pin is worth reporting only when it points somewhere else — either
		// another series, or another position within the Show's own.
		if es.RecordEpisode > 0 {
			sl.Record = &SlotRecordRef{
				Series:   es.RecordSeries,
				Position: SlotPosition{Group: es.RecordSeason, Slot: es.RecordEpisode},
			}
		}
	}
	for _, f := range local.files {
		g := groupOf(groupNearest(f))
		g.FileCount++
		switch f.State {
		case store.DecisionIgnored:
			g.IgnoredCount++
		case store.DecisionUnassigned:
			g.UnassignedCount++
		default:
			g.PlacedCount++
		}
	}

	// The provider half. Its failure modes are states, not errors: every one of
	// them leaves the local half above exactly as it is and only costs the records.
	out.Unavailable = s.fillProviderSlots(ctx, series, group, groupOf, slotOf, slots)

	for p, sl := range slots {
		g := groupOf(p.Group)
		g.Slots = append(g.Slots, *sl)
	}
	numbers := make([]int, 0, len(groups))
	for n := range groups {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	for _, n := range numbers {
		g := groups[n]
		sort.Slice(g.Slots, func(i, j int) bool { return g.Slots[i].Slot < g.Slots[j].Slot })
		out.Groups = append(out.Groups, *g)
	}
	return out, nil
}

// showSeries is the ONE place a Show's provider series is resolved, and the only
// answer to "which series are this Show's Slots listed from".
//
// A Show has two sources for that id, in two different tables, and which one holds
// it is decided by HOW the Show was matched:
//
//  1. entity_enrichment.external_id — what Enrichment matched, which for the
//     overwhelming majority of Shows means an ordinary provider title search. It
//     also holds an Admin's Fix-info correction (which sets external_id_origin).
//  2. shows.tmdb_id — written by only two narrow paths: an embedded {tmdb-12345}
//     in the folder name, and a Wrong-item re-key (RekeyShowIdentity). Nothing in
//     the enrichment path ever writes it.
//
// Enrichment first, and deliberately so. The two can only disagree when an Admin
// has used Fix info to point the Show at a different record; that is a later,
// explicit statement about WHICH RECORD DECORATES THIS SHOW, which is precisely
// what a Slot's titles are. An embedded id normally produces the same answer
// anyway — Enrichment anchors on it — so the fallback is for the never-enriched
// and enrichment-disabled cases, not to overrule a correction. It is also the
// order collectTVLeaves already threads down to the Season and Episode refs, so
// the matcher shows the records a pass would actually write (file-matcher/10).
//
// Keep it in one function. ADR-0045 gave a leaf Title the same split this
// precedence already assumed for a Show — an Enrichment override outranks the id a
// folder name asserts — so a Show's answer is unchanged and this stays the single
// place it could change.
func (s *Service) showSeries(sh store.Show) (string, error) {
	enr, err := s.store.EntityEnrichmentByID(store.EntityShow, sh.ID)
	if err != nil {
		return "", err
	}
	if id := strings.TrimSpace(enr.ExternalID); id != "" {
		return id, nil
	}
	return strings.TrimSpace(sh.TMDBID), nil
}

// fillProviderSlots layers the provider's groups (and, for the one expanded
// group, its records) over the local Slots, returning why it could not — never an
// error, because a matcher without records still renumbers (ADR-0044).
//
// series is the Show's resolved provider series (showSeries), never a raw column.
func (s *Service) fillProviderSlots(
	ctx context.Context, series string, group *int,
	groupOf func(int) *MatcherGroup, slotOf func(SlotPosition) *MatcherSlot,
	slots map[SlotPosition]*MatcherSlot,
) string {
	if s.slotLister == nil {
		return SlotsEnrichmentDisabled
	}
	if reason := s.slotLister.Unavailable(); reason != "" {
		return reason
	}
	// A Show that never matched has no series to list — from EITHER source, which
	// is what makes the sentence the screen prints ("this Show never matched a
	// series") true. This is not a provider failure and must not read as one: the
	// fix is to match the Show, not to reconfigure enrichment.
	if series == "" {
		return SlotsNoSeries
	}

	summaries, err := s.slotLister.ListGroups(ctx, series)
	if err != nil {
		return SlotsProviderUnreachable
	}
	for _, sum := range summaries {
		g := groupOf(sum.Number)
		g.Source = SlotSourceProvider
		g.SlotCount = sum.SlotCount
	}
	if group == nil {
		return ""
	}

	g := groupOf(*group)
	records, err := s.slotLister.ListSlots(ctx, series, *group)
	if err != nil {
		// One group's records failing leaves the rest of the response whole, so the
		// Admin can carry on in the seasons that did load.
		g.Unavailable = SlotsProviderUnreachable
	} else {
		g.Loaded = true
		for _, rec := range records {
			// The Slot's POSITION stays local. A provider record is laid onto the
			// position the caller expanded, never onto the one the record names, or a
			// borrowed series' numbering would silently renumber the Show (ADR-0044).
			sl := slotOf(SlotPosition{Group: *group, Slot: rec.Slot})
			sl.Name, sl.Overview, sl.AirDate, sl.StillURL = rec.Name, rec.Overview, rec.AirDate, rec.StillURL
		}
	}
	// Deliberately outside that branch. The borrowed records come from a DIFFERENT
	// series, so the Show's own list failing says nothing about them — and here that
	// failure is the norm rather than the exception: the Batman case borrows
	// precisely because the Show's own series has no such group, which a provider
	// answers with a 404. Skipping the borrowed fetch on that would leave the five
	// Slots bare in exactly the case the feature exists for.
	s.fillPinnedRecords(ctx, *group, slots)
	return ""
}

// fillPinnedRecords fetches the records the expanded group's REPOINTED Slots
// borrowed, and lays each one on its own Slot's `Record` — never on the Slot's own
// Name, and never on the position the record names.
//
// Without it the Batman case ends where it started: five files placed into a
// Season 4 the Show's own series does not have are decorated by nothing, so the
// screen would show the pin as a bare reference to a record it cannot display, and
// the Admin could not see whether they had borrowed the right one.
//
// It costs one call per distinct (series, group) among the pins of the ONE group
// being expanded — one call for a whole borrowed run, since a run borrows from one
// group — and the lister caches, so re-expanding is free. A failure is silent by
// design: the reference is still reported, only its words are missing, exactly as
// the rest of this file treats a provider that cannot answer.
func (s *Service) fillPinnedRecords(ctx context.Context, group int, slots map[SlotPosition]*MatcherSlot) {
	type source struct {
		series string
		group  int
	}
	borrowed := map[source][]*MatcherSlot{}
	for pos, sl := range slots {
		if pos.Group != group || sl.Record == nil || sl.Record.Series == "" {
			continue
		}
		src := source{series: sl.Record.Series, group: sl.Record.Position.Group}
		borrowed[src] = append(borrowed[src], sl)
	}
	for src, targets := range borrowed {
		records, err := s.slotLister.ListSlots(ctx, src.series, src.group)
		if err != nil {
			continue
		}
		byNumber := map[int]SlotRecord{}
		for _, rec := range records {
			byNumber[rec.Slot] = rec
		}
		for _, sl := range targets {
			rec, ok := byNumber[sl.Record.Position.Slot]
			if !ok {
				continue
			}
			sl.Record.Name = rec.Name
			sl.Record.Overview = rec.Overview
			sl.Record.AirDate = rec.AirDate
			sl.Record.StillURL = rec.StillURL
		}
	}
}

// localArrangement is everything the matcher knows without a provider: the Files,
// their states, and the Slots they claim.
type localArrangement struct {
	files []MatcherFile
	// positions are the Slots local Files claim or already hold.
	positions []SlotPosition
	// episodes are the Show's existing Episode Titles, which supply each occupied
	// Slot's Title id and its Episode pin.
	episodes []store.EpisodeSlot
	// folders are the Show's on-disk folders — the scope that decides whether a
	// Library-wide decision or Unmatched row is this Show's business. Callers that
	// fold a Library list onto Shows (the Needs-Fixing queue) must attribute paths
	// the same way, so the set is handed back rather than re-derived.
	folders map[string]bool
}

// libraryFileState is the Library-wide half of a Show's arrangement: the two
// reads that are the same for every Show in the Library.
//
// It is separated only so a caller looping over every Show of a Library — the
// Needs-Fixing queue's per-Show counts — pays for them once instead of once per
// Show. Nothing about the arrangement itself changes.
type libraryFileState struct {
	decisions map[string]store.FileDecisions
	unmatched []store.UnmatchedFile
}

func (s *Service) libraryFileState(libraryID string) (libraryFileState, error) {
	var out libraryFileState
	var err error
	out.decisions, err = s.store.FileDecisionsByLibrary(libraryID)
	if err != nil {
		return out, err
	}
	out.unmatched, err = s.store.ListUnmatched(libraryID)
	if err != nil {
		return out, err
	}
	return out, nil
}

func (s *Service) localArrangement(sh store.Show) (localArrangement, error) {
	lib, err := s.libraryFileState(sh.LibraryID)
	if err != nil {
		return localArrangement{}, err
	}
	return s.showArrangement(sh, lib)
}

func (s *Service) showArrangement(sh store.Show, lib libraryFileState) (localArrangement, error) {
	var out localArrangement

	showFiles, err := s.store.ShowFiles(sh.ID)
	if err != nil {
		return out, err
	}
	decisions, unmatched := lib.decisions, lib.unmatched
	out.episodes, err = s.store.ShowEpisodeSlots(sh.ID)
	if err != nil {
		return out, err
	}

	// The Show's folders, derived exactly as Apply derives them, so the read and the
	// write agree on what "under this Show" means. Everything else is scoped by
	// them: a decision or an Unmatched row is this Show's business only if its file
	// lives in one.
	folders := map[string]bool{}
	out.folders = folders
	present := map[string]bool{}
	titleOf := map[string]string{}
	candidates := map[string]bool{}
	for _, f := range showFiles {
		folders[showFolderOf(f.Path)] = true
		present[f.Path] = present[f.Path] || f.Present
		if _, ok := titleOf[f.Path]; !ok {
			titleOf[f.Path] = f.TitleID
		}
	}
	// Source one: the Files of the Show's Episode Titles that are still ON DISK.
	//
	// A Missing row is NOT one of them, and that exclusion is the whole difference
	// between a file the Admin can act on and a ghost they cannot. ShowFiles returns
	// Missing rows on purpose (a File taken off its Slot is soft-deleted, not gone —
	// store/placement.go), but the same soft-delete catches a file that was RENAMED
	// on disk: its old path keeps its row, present=0, forever. Offered here it would
	// be unplaceable — resolve refuses to build a Slot from a file that is not there
	// (placementInputs) — so it would sit in the matcher as an unassigned File that
	// no amount of sorting can settle, and keep its Show in the Needs-Fixing queue
	// for good. A soft-deleted File the Admin actually decided about still arrives,
	// through source three, which is where that row's second life belongs.
	for path, isPresent := range present {
		if isPresent {
			candidates[path] = true
		}
	}

	// Source two: the Library's Unmatched rows under those folders. A file the
	// Scanner could not number is not a Title and has no decision, so it exists
	// nowhere else — and it is the file in the worst shape, which is exactly what
	// the matcher is for (PRD user story 7).
	reasons := map[string]string{}
	for _, u := range unmatched {
		if !folders[showFolderOf(u.Path)] {
			continue
		}
		candidates[u.Path] = true
		present[u.Path] = true
		reasons[u.Path] = u.Reason
	}

	// Source three: paths carrying an explicit decision. An Unassigned or Ignored
	// File deliberately produces NO Title and NO Unmatched row (a recorded decision
	// is not outstanding work — scanner/arrangement.go), so omitting this source
	// would make a File disappear the moment the Admin took it off its Slot. It
	// covers placed decisions too, which is what surfaces an ORPHANED Placement and
	// a Deferred one — neither has a Title yet.
	for path := range decisions {
		if !folders[showFolderOf(path)] {
			continue
		}
		candidates[path] = true
	}

	paths := sortedPaths(candidates)
	inputs := placementInputs(paths, present, decisions)
	show := scanner.Identity{Key: sh.IdentityKey}
	// Twice, deliberately. The first run is the arrangement as it stands — decisions
	// applied — and is what the Slots and Placements are read off. The second
	// ignores every decision, which is the only way to recover what each FILENAME
	// says: the screen highlights the disagreement between the two, and that
	// disagreement is the correction being made.
	effective := scanner.ResolveEpisodes(show, inputs, decisions)
	parsed := scanner.ResolveEpisodes(show, inputs, nil)

	placements := map[string][]MatcherPlacement{}
	for _, ep := range effective.Episodes {
		pos := SlotPosition{Group: ep.SeasonNumber, Slot: ep.EpisodeNumber}
		out.positions = append(out.positions, pos)
		for i, rf := range ep.Files {
			ordinal := rf.PartOrdinal
			if ordinal < 1 {
				ordinal = i + 1
			}
			placements[rf.Path] = append(placements[rf.Path],
				MatcherPlacement{SlotPosition: pos, Ordinal: ordinal})
		}
	}
	for _, es := range out.episodes {
		out.positions = append(out.positions, SlotPosition{Group: es.SeasonNumber, Slot: es.EpisodeNumber})
	}

	parsedOf := map[string][]SlotPosition{}
	for _, ep := range parsed.Episodes {
		for _, rf := range ep.Files {
			parsedOf[rf.Path] = append(parsedOf[rf.Path],
				SlotPosition{Group: ep.SeasonNumber, Slot: ep.EpisodeNumber})
		}
	}
	for _, u := range parsed.Unresolved {
		if _, ok := reasons[u.Path]; !ok {
			reasons[u.Path] = u.Reason
		}
	}

	for _, path := range paths {
		f := MatcherFile{
			Path:       path,
			TitleID:    titleOf[path],
			Parsed:     parsedOf[path],
			Placements: placements[path],
			Reason:     reasons[path],
		}
		decision := decisions[path]
		f.Decided = len(decision) > 0
		for _, d := range decision {
			if d.Orphaned {
				f.Orphaned = true
			}
		}
		switch decision.State() {
		case store.DecisionIgnored:
			f.State = store.DecisionIgnored
		case store.DecisionUnassigned:
			f.State = store.DecisionUnassigned
		default:
			// No decision, or a placed one: the arrangement itself says whether the
			// File landed anywhere. A placed decision that landed nowhere is a Deferred
			// or orphaned Placement, and reads as unassigned because that is what the
			// catalog currently shows.
			if len(f.Placements) > 0 {
				f.State = store.DecisionPlaced
			} else {
				f.State = store.DecisionUnassigned
			}
		}
		if f.State == store.DecisionPlaced {
			f.Reason = ""
		}
		out.files = append(out.files, f)
	}
	return out, nil
}

// groupNearest is the group a File is counted under. A placed File counts where
// it sits; an unplaced one counts where its FILENAME points, so an unnumbered
// file lands in the group whose folder holds it rather than vanishing from every
// count.
func groupNearest(f MatcherFile) int {
	if len(f.Placements) > 0 {
		return f.Placements[0].Group
	}
	if len(f.Parsed) > 0 {
		return f.Parsed[0].Group
	}
	return scanner.SeasonHintForPath(f.Path)
}

// ApplyShowMatcher commits an arrangement and returns the working set as it now
// stands, so the client never has to guess what the server made of its payload —
// which files were displaced on its behalf, which placements are Deferred to the
// next scan, and where every File ended up.
//
// The re-read deliberately loads no provider records (the caller re-expands the
// group it cares about), so an Apply costs at most the one ListGroups call.
func (s *Service) ApplyShowMatcher(ctx context.Context, in PlacementInput) (Matcher, error) {
	result, err := s.ApplyPlacement(in)
	if err != nil {
		return Matcher{}, err
	}
	out, err := s.ShowMatcher(ctx, in.ShowID, nil)
	if err != nil {
		return Matcher{}, err
	}
	out.Applied = &result
	return out, nil
}
