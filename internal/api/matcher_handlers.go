package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/enrich"
	"github.com/goozakdev/obelo-server/internal/store"
)

// matcher_handlers.go is the transport for the file matcher (ADR-0044): the whole
// working set of one already-identified container, and the Apply that commits a
// rearrangement of it.
//
// It replaces nothing and adds one thing: an ADDRESS. `GET
// /titles/{id}/episodeCandidates` answers "which provider episode is THIS file?",
// one Title and one season at a time, which is the wrong shape for a screen whose
// whole point is that five files are one problem. These routes are anchored on the
// CONTAINER, so the Admin sees every Slot and every File at once — including the
// Files that are not Titles, which the per-Title route can by definition never
// reach.
//
// Every field here is kind-neutral — groups, slots, files, addressed by container
// id — because ADR-0044 requires the Album matcher to be the SAME contract at
// /albums/{id}/matcher rather than a second one that looks like it. Nothing on the
// wire is named after a season or an episode.

// --- Wire shapes ------------------------------------------------------------

// slotPositionJSON is one Slot's position, always in the local library's own
// numbering (season+episode for TV, disc+track for Music) — never a provider's.
type slotPositionJSON struct {
	Group int `json:"group"`
	Slot  int `json:"slot"`
}

// slotRecordRefJSON names the provider record decorating a Slot when it is NOT
// the container's own record at that position — the Episode pin. Present only
// where one was written, which is the minority case it exists for (ADR-0044).
//
// It carries the record's own words as well as its address, and that split is the
// point: the Slot's `name`/`overview`/`stillUrl` stay whatever the container's OWN
// record says at that position (nothing at all, for the Season 4 the Batman case
// invents), while the borrowed words hang off `record`. So the screen can show the
// borrowed title, say where it came from, and still print the Slot's own local
// code — and clearing the pin falls straight back to the default record with no
// second fetch.
type slotRecordRefJSON struct {
	ExternalID string `json:"externalId,omitempty"`
	Group      int    `json:"group"`
	Slot       int    `json:"slot"`
	Name       string `json:"name,omitempty"`
	Overview   string `json:"overview,omitempty"`
	AirDate    string `json:"airDate,omitempty"`
	StillURL   string `json:"stillUrl,omitempty"`
}

// matcherSlotJSON is one Slot: a position, the Title serving it if any, and its
// record where one could be fetched. Everything but the position is absent on the
// degraded path, which is the point of the degraded path.
type matcherSlotJSON struct {
	Group    int    `json:"group"`
	Slot     int    `json:"slot"`
	TitleID  string `json:"titleId,omitempty"`
	Name     string `json:"name,omitempty"`
	Overview string `json:"overview,omitempty"`
	AirDate  string `json:"airDate,omitempty"`
	// StillURL is a same-origin /providerImage reference, never the provider's own
	// host (ADR-0001) — the same rule every other candidate thumbnail follows.
	StillURL string             `json:"stillUrl,omitempty"`
	Record   *slotRecordRefJSON `json:"record,omitempty"`
}

// matcherGroupJSON is one group (a season) with its Slots and its counts. The
// counts are what a COLLAPSED group renders, which is why they are complete in
// every response while the Slot records are not.
type matcherGroupJSON struct {
	Number    int    `json:"number"`
	Source    string `json:"source"`
	SlotCount int    `json:"slotCount,omitempty"`
	// SlotsLoaded says whether this group's provider records have been fetched.
	// False everywhere until the group is expanded with ?group=.
	SlotsLoaded      bool              `json:"slotsLoaded"`
	SlotsUnavailable string            `json:"slotsUnavailable,omitempty"`
	FileCount        int               `json:"fileCount"`
	PlacedCount      int               `json:"placedCount"`
	UnassignedCount  int               `json:"unassignedCount"`
	IgnoredCount     int               `json:"ignoredCount"`
	Slots            []matcherSlotJSON `json:"slots"`
}

// matcherPlacementJSON is one Slot a File fills, with its order among the Files
// sharing that Slot (1-based; several Files on one Slot is a multi-part Edition).
type matcherPlacementJSON struct {
	Group   int `json:"group"`
	Slot    int `json:"slot"`
	Ordinal int `json:"ordinal,omitempty"`
}

// matcherFileJSON is one File under the container. `parsed` and `placements` are
// both present because the screen compares them: their disagreement is the
// correction the Admin is making.
type matcherFileJSON struct {
	Path       string                 `json:"path"`
	State      string                 `json:"state"`
	TitleID    string                 `json:"titleId,omitempty"`
	Parsed     []slotPositionJSON     `json:"parsed,omitempty"`
	Placements []matcherPlacementJSON `json:"placements,omitempty"`
	Decided    bool                   `json:"decided"`
	Orphaned   bool                   `json:"orphaned,omitempty"`
	Reason     string                 `json:"reason,omitempty"`
}

// matcherAppliedJSON reports what the server made of a submitted arrangement.
//
// `deferred` is not a detail. A Placement onto a File the catalog has never
// probed is STORED but cannot become an Episode until the next scan builds it, so
// without this the screen would appear to have silently dropped the Admin's most
// deliberate correction — the one they made on the file in the worst shape.
type matcherAppliedJSON struct {
	Rearranged int      `json:"rearranged"`
	Displaced  []string `json:"displaced"`
	Deferred   []string `json:"deferred"`
}

// matcherJSON is one container's whole working set.
type matcherJSON struct {
	ContainerID      string              `json:"containerId"`
	ContainerType    string              `json:"containerType"`
	LibraryID        string              `json:"libraryId"`
	Title            string              `json:"title"`
	Year             int                 `json:"year,omitempty"`
	SeriesExternalID string              `json:"seriesExternalId,omitempty"`
	SlotsUnavailable string              `json:"slotsUnavailable,omitempty"`
	Groups           []matcherGroupJSON  `json:"groups"`
	Files            []matcherFileJSON   `json:"files"`
	Applied          *matcherAppliedJSON `json:"applied,omitempty"`
}

// matcherApplyRequest is one container's WHOLE arrangement, not a delta.
//
// A File absent from `files` carries no decision at all, which is the meaningful
// third answer: "follow the filename". That is why the payload has to be complete
// — a sparse store spends "no row" on the parse, so taking a File off its Slot can
// only be said by sending it with state `unassigned`, and taking a correction BACK
// can only be said by omitting it (ADR-0027's sparse-override precedent).
type matcherApplyRequest struct {
	Files []matcherApplyFile `json:"files"`
	// Slots repoints what DECORATES a Slot — the Episode pin (ADR-0044), reached
	// from the Slot itself now rather than one queue row at a time.
	//
	// It rides here rather than in a write of its own because it has to: the pin is
	// stored on a Title, and the Slots this exists to repoint are ones the Admin has
	// just placed files onto, whose Titles do not exist until this call commits.
	// There is nothing to pin at the moment of the gesture — and a pin that applied
	// immediately would be the one change Revert could not undo.
	//
	// Unlike `files` this is a SPARSE list, not the whole set: an absent Slot keeps
	// the record it has. Absence carries no second meaning here (no record is ever
	// derived from a filename), so there is nothing to spend it on, and a client that
	// never sends the field cannot disturb a pin.
	Slots []matcherApplySlot `json:"slots"`
}

type matcherApplyFile struct {
	Path       string                 `json:"path"`
	State      string                 `json:"state"`
	Placements []matcherPlacementJSON `json:"placements"`
}

// matcherApplySlot is one Slot's record. A null (or absent) `record` CLEARS the
// pin, returning the Slot to its default record — this series, this position —
// which for a Slot the container's series does not list means bare again.
type matcherApplySlot struct {
	Group  int                `json:"group"`
	Slot   int                `json:"slot"`
	Record *slotRecordRefJSON `json:"record"`
}

// --- Handlers ---------------------------------------------------------------

// handleShowMatcher serves GET /shows/{id}/matcher[?group=N] (Admin-only): every
// Slot of one Show and every File under it.
//
// The LOCAL half is complete in every response — every File, every decision, every
// group with its counts, and the Slots the local Files already claim — and needs
// no provider at all. The provider half loads PER GROUP: opening a ten-season Show
// costs one round-trip (the group list), and expanding a season costs one more,
// cached thereafter. Ten seasons would otherwise be ten calls on open, against a
// rate-limited API, for records nine collapsed sections do not show.
//
// It never answers 5xx for a missing provider. A Library whose Authoritative
// provider cannot list episodes, enrichment switched off, an offline server, a
// Show that never matched — each returns bare numbered Slots and a
// `slotsUnavailable` reason the UI can explain, because pure renumbering works
// offline and is most of what this screen is for (ADR-0044).
func handleShowMatcher(deps Deps, showID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		group, ok := matcherGroupParam(w, r)
		if !ok {
			return
		}
		m, err := deps.Catalog.ShowMatcher(r.Context(), showID, group)
		if !writeMatcherError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, toMatcherJSON(deps, m))
	}
}

// handleApplyShowMatcher serves PUT /shows/{id}/matcher (Admin-only): commit the
// arrangement and hand back the re-read working set, so the client never has to
// guess what the server made of its payload.
func handleApplyShowMatcher(deps Deps, showID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req matcherApplyRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		decisions, ok := toFileDecisions(w, req)
		if !ok {
			return
		}
		pins, ok := toSlotPins(w, req)
		if !ok {
			return
		}
		m, err := deps.Catalog.ApplyShowMatcher(r.Context(),
			catalog.PlacementInput{ShowID: showID, Decisions: decisions, Pins: pins})
		if !writeMatcherError(w, err) {
			return
		}
		// A rearrangement changes what browse shows, exactly as every other identity
		// mutation does, so clients refetch off the same event.
		if deps.Events != nil {
			deps.Events.PublishLibraryUpdated(m.LibraryID)
		}
		writeJSON(w, http.StatusOK, toMatcherJSON(deps, m))
	}
}

// seriesSlotsJSON is another series' groups and (for the one expanded group) its
// Slots — the record side only. No position here is the library's own.
type seriesSlotsJSON struct {
	ExternalID string             `json:"externalId"`
	Groups     []seriesGroupJSON  `json:"groups"`
	Slots      []matcherSlotJSON  `json:"slots,omitempty"`
	Group      *slotGroupSelector `json:"group,omitempty"`
}

type seriesGroupJSON struct {
	Number    int `json:"number"`
	SlotCount int `json:"slotCount"`
}

// slotGroupSelector echoes which group's Slots were listed, so a client that
// pipelined two expands can tell the answers apart.
type slotGroupSelector struct {
	Number int `json:"number"`
}

// handleShowSeriesSeasons serves GET /shows/{id}/seriesSeasons?externalId=[&group=]
// (Admin-only): another series' groups, and one group's Slots on request.
//
// It exists for the case ADR-0044 built the Episode pin for — a run of episodes
// the provider counts in a re-numbered continuation series (Batman: The Animated
// Series' last five season-3 files are season 1 of The New Batman Adventures). The
// Slot's POSITION stays local; only its RECORD is repointed, through the existing
// pin. Without this the matcher could only offer records from the Show's own
// series, which is exactly the series that does not have them.
//
// Unlike the matcher itself, a provider that cannot list is an ERROR here: this
// route has nothing else to return, so 503 SEARCH_UNAVAILABLE says why rather than
// answering an empty list that reads as "that series has no episodes".
func handleShowSeriesSeasons(deps Deps, showID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The Show is resolved even though nothing about it is used: it is what makes
		// the route addressable and 404s an unknown id, rather than letting any
		// authenticated Admin probe provider ids through a bare path. The cheap
		// existence read is deliberate — assembling the whole working set to answer
		// "does this Show exist" would cost four table reads for nothing.
		if _, err := deps.Catalog.LibraryOfEntity(store.EntityShow, showID); !writeMatcherError(w, err) {
			return
		}
		seriesID := strings.TrimSpace(r.URL.Query().Get("externalId"))
		if seriesID == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"externalId (the series to list slots from) is required", nil)
			return
		}
		group, ok := matcherGroupParam(w, r)
		if !ok {
			return
		}
		if deps.Enrich == nil {
			writeEpisodeCandidatesError(w, enrich.ErrSearchUnavailable)
			return
		}

		seasons, err := deps.Enrich.SeriesSeasons(r.Context(), seriesID)
		if err != nil {
			writeEpisodeCandidatesError(w, err)
			return
		}
		out := seriesSlotsJSON{ExternalID: seriesID, Groups: []seriesGroupJSON{}}
		for _, s := range seasons {
			out.Groups = append(out.Groups, seriesGroupJSON{Number: s.Season, SlotCount: s.EpisodeCount})
		}
		if group != nil {
			eps, epErr := deps.Enrich.SeasonEpisodes(r.Context(), seriesID, *group)
			if epErr != nil {
				writeEpisodeCandidatesError(w, epErr)
				return
			}
			out.Group = &slotGroupSelector{Number: *group}
			out.Slots = []matcherSlotJSON{}
			for _, e := range eps {
				out.Slots = append(out.Slots, matcherSlotJSON{
					Group: e.Season, Slot: e.Episode, Name: e.Name,
					Overview: e.Overview, AirDate: e.AirDate,
					StillURL: deps.providerImages.proxyURL(e.StillURL),
				})
			}
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// --- Translation ------------------------------------------------------------

// matcherGroupParam reads the group to expand. `group` is the kind-neutral name
// the payload uses; `season` is accepted as its alias so a TV client reads the
// same word it sees everywhere else in this API. Absent means "expand nothing",
// which is the cheap first load.
func matcherGroupParam(w http.ResponseWriter, r *http.Request) (*int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("group"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("season"))
	}
	if raw == "" {
		return nil, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "group must be a number", nil)
		return nil, false
	}
	return &n, true
}

// toFileDecisions flattens the request into the sparse per-Slot rows the domain
// stores, rejecting the two shapes that cannot mean anything: an unknown state,
// and a placement with no Slot to put the File on.
func toFileDecisions(w http.ResponseWriter, req matcherApplyRequest) ([]store.FileDecision, bool) {
	var out []store.FileDecision
	for _, f := range req.Files {
		path := strings.TrimSpace(f.Path)
		if path == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest, "every file needs a path", nil)
			return nil, false
		}
		switch f.State {
		case store.DecisionUnassigned, store.DecisionIgnored:
			if len(f.Placements) > 0 {
				writeError(w, http.StatusBadRequest, codeBadRequest,
					"a "+f.State+" file has no placements: "+path, nil)
				return nil, false
			}
			out = append(out, store.FileDecision{Path: path, State: f.State})
		case store.DecisionPlaced:
			if len(f.Placements) == 0 {
				writeError(w, http.StatusBadRequest, codeBadRequest,
					"a placed file needs at least one placement: "+path, nil)
				return nil, false
			}
			for _, p := range f.Placements {
				out = append(out, store.FileDecision{
					Path: path, State: store.DecisionPlaced,
					GroupNumber: p.Group, SlotNumber: p.Slot, Ordinal: p.Ordinal,
				})
			}
		default:
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"state must be placed, unassigned or ignored: "+path, nil)
			return nil, false
		}
	}
	return out, true
}

// toSlotPins reads the per-Slot records, rejecting the one shape that cannot mean
// anything: a record that names no slot of its own. A null record is not that — it
// is how a pin is CLEARED, which is a decision in its own right.
func toSlotPins(w http.ResponseWriter, req matcherApplyRequest) ([]catalog.SlotPin, bool) {
	var out []catalog.SlotPin
	seen := map[catalog.SlotPosition]bool{}
	for _, sl := range req.Slots {
		pin := catalog.SlotPin{Position: catalog.SlotPosition{Group: sl.Group, Slot: sl.Slot}}
		if seen[pin.Position] {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"a slot carries two records; send one record per slot", nil)
			return nil, false
		}
		seen[pin.Position] = true
		if sl.Record == nil {
			pin.Clear = true
			out = append(out, pin)
			continue
		}
		if sl.Record.Slot <= 0 {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"a slot record needs the slot it points at; send a null record to clear the pin", nil)
			return nil, false
		}
		pin.Series = strings.TrimSpace(sl.Record.ExternalID)
		pin.Record = catalog.SlotPosition{Group: sl.Record.Group, Slot: sl.Record.Slot}
		out = append(out, pin)
	}
	return out, true
}

// writeMatcherError maps the domain's refusals onto the envelope, returning false
// once it has written one. Each of the three 4xx cases is ACTIONABLE by design —
// a bare "conflict" leaves the Admin with a screen full of work and no move.
func writeMatcherError(w http.ResponseWriter, err error) bool {
	var collision *catalog.SlotCollisionError
	switch {
	case err == nil:
		return true
	case errors.Is(err, catalog.ErrNotFound), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "show not found", nil)
	case errors.Is(err, catalog.ErrScanRunning):
		// Nothing was written. A scan holds the Library's lock (ADR-0031) and Apply is
		// a catalog writer just as a scan is, so it refuses rather than races — and
		// the Admin can simply press Apply again when the scan finishes.
		writeError(w, http.StatusConflict, codeScanRunning,
			"a scan is running for this library — nothing was written; try again when it finishes", nil)
	case errors.As(err, &collision):
		// The Slot and BOTH paths, because this collision is never one the matcher
		// created: it is two filenames already claiming one Slot, which the Admin has
		// not seen. Naming them is what lets the screen offer the three real fixes.
		writeError(w, http.StatusConflict, codeSlotCollision,
			collision.Error(), map[string]any{
				"slot":  slotPositionJSON{Group: collision.GroupNumber, Slot: collision.SlotNumber},
				"paths": collision.Paths,
			})
	case errors.Is(err, catalog.ErrSlotCollision):
		writeError(w, http.StatusConflict, codeSlotCollision, err.Error(), nil)
	case errors.Is(err, catalog.ErrOutsideShow):
		writeError(w, http.StatusUnprocessableEntity, codeOutsideShow, err.Error(), nil)
	case errors.Is(err, catalog.ErrEmptySlot):
		// A record decorates something. Refusing beats accepting a decision there is
		// nowhere to keep — the Admin would be told it worked and see nothing change.
		writeError(w, http.StatusUnprocessableEntity, codeEmptySlot, err.Error(), nil)
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to read the matcher", nil)
	}
	return false
}

func toMatcherJSON(deps Deps, m catalog.Matcher) matcherJSON {
	out := matcherJSON{
		ContainerID:      m.ContainerID,
		ContainerType:    m.ContainerType,
		LibraryID:        m.LibraryID,
		Title:            m.Title,
		Year:             m.Year,
		SeriesExternalID: m.SeriesExternalID,
		SlotsUnavailable: m.Unavailable,
		Groups:           make([]matcherGroupJSON, 0, len(m.Groups)),
		Files:            make([]matcherFileJSON, 0, len(m.Files)),
	}
	for _, g := range m.Groups {
		gj := matcherGroupJSON{
			Number: g.Number, Source: g.Source, SlotCount: g.SlotCount,
			SlotsLoaded: g.Loaded, SlotsUnavailable: g.Unavailable,
			FileCount: g.FileCount, PlacedCount: g.PlacedCount,
			UnassignedCount: g.UnassignedCount, IgnoredCount: g.IgnoredCount,
			Slots: make([]matcherSlotJSON, 0, len(g.Slots)),
		}
		for _, s := range g.Slots {
			sj := matcherSlotJSON{
				Group: s.Group, Slot: s.Slot, TitleID: s.TitleID,
				Name: s.Name, Overview: s.Overview, AirDate: s.AirDate,
				StillURL: deps.providerImages.proxyURL(s.StillURL),
			}
			if s.Record != nil {
				sj.Record = &slotRecordRefJSON{
					ExternalID: s.Record.Series,
					Group:      s.Record.Position.Group,
					Slot:       s.Record.Position.Slot,
					Name:       s.Record.Name,
					Overview:   s.Record.Overview,
					AirDate:    s.Record.AirDate,
					StillURL:   deps.providerImages.proxyURL(s.Record.StillURL),
				}
			}
			gj.Slots = append(gj.Slots, sj)
		}
		out.Groups = append(out.Groups, gj)
	}
	for _, f := range m.Files {
		fj := matcherFileJSON{
			Path: f.Path, State: f.State, TitleID: f.TitleID,
			Decided: f.Decided, Orphaned: f.Orphaned, Reason: f.Reason,
		}
		for _, p := range f.Parsed {
			fj.Parsed = append(fj.Parsed, slotPositionJSON{Group: p.Group, Slot: p.Slot})
		}
		for _, p := range f.Placements {
			fj.Placements = append(fj.Placements,
				matcherPlacementJSON{Group: p.Group, Slot: p.Slot, Ordinal: p.Ordinal})
		}
		out.Files = append(out.Files, fj)
	}
	if m.Applied != nil {
		out.Applied = &matcherAppliedJSON{
			Rearranged: m.Applied.Rearranged,
			Displaced:  append([]string{}, m.Applied.Displaced...),
			Deferred:   append([]string{}, m.Applied.Deferred...),
		}
	}
	return out
}
