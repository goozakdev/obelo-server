package api

import (
	"errors"
	"net/http"

	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/enrich"
	"github.com/goozakdev/obelo-server/internal/store"
)

// The edition picker's data (ADR-0052, album-resolves-its-tracks/12).
//
// An album IS a release-group (ADR-0038), and a release-group holds editions — the
// original, the remaster, the deluxe with three bonus tracks, the Japanese pressing
// with four. Which one an album's files are decides which track list decorates them,
// and until this route existed the only way to say so was to leave the product,
// find the release on musicbrainz.org and paste its URL back in. The operator's
// words: *"It shows the best guess, but I cant choose a specific edition out of that
// release-group."*
//
// This route lists them. It does NOT apply one: an edition is a refinement of the
// album's record, so it is applied through the album's existing
// PUT /albums/{id}/enrichmentOverride carrying the release id — one apply path, one
// cascade, one summary (see handleEntityEnrichmentOverride).

// albumEditionJSON is one edition an Admin can choose: what a listener would use to
// tell two pressings apart, plus the number that actually decides it.
type albumEditionJSON struct {
	ReleaseID string `json:"releaseId"`
	Date      string `json:"date,omitempty"`
	Country   string `json:"country,omitempty"`
	Format    string `json:"format,omitempty"`
	// TrackCount is never omitted, even at zero: "0 tracks" is a real and useful
	// statement about an edition MusicBrainz lists but has not filled in, and an
	// absent field would render as a blank the client would have to guess about.
	TrackCount     int    `json:"trackCount"`
	Disambiguation string `json:"disambiguation,omitempty"`
}

// albumEditionsJSON is the edition picker's whole payload.
//
// localTrackCount is on the RESPONSE rather than left to the client to count,
// because the client's count is the wrong one: the queue row knows how many tracks
// are FLAGGED, not how many the album holds, and comparing sixteen editions against
// "the twelve that are broken" would point at the wrong pressing.
type albumEditionsJSON struct {
	AlbumID        string `json:"albumId"`
	ReleaseGroupID string `json:"releaseGroupId,omitempty"`
	// ChosenReleaseID is the edition an Admin has pinned (ADR-0052), "" when nobody
	// has. InUseReleaseID is the one that actually decorates the tracks today, which
	// is the same thing when a pin applies and something else when it does not;
	// InUseSource says which tier answered — "chosen", "tagged" or "fit".
	ChosenReleaseID string             `json:"chosenReleaseId,omitempty"`
	InUseReleaseID  string             `json:"inUseReleaseId,omitempty"`
	InUseSource     string             `json:"inUseSource,omitempty"`
	LocalTrackCount int                `json:"localTrackCount"`
	Editions        []albumEditionJSON `json:"editions"`
}

// handleAlbumEditions lists the editions of an Album's matched release-group (GET
// /albums/{id}/editions, Admin-only), so an Admin can choose the exact one their
// files are without leaving Obelo (ADR-0052). Reads only — choosing is the existing
// enrichmentOverride apply, carrying the picked release id.
//
// An unknown album is 404. An album with no matched release-group is a 200 with an
// EMPTY list and no release-group id — not an error: "this album has no editions to
// choose from because it isn't matched yet" is an answer, and the fix for it is the
// record picker sitting above this section. The same SEARCH_UNAVAILABLE (503)
// semantics as every other provider-backed list covers the rest, and the client
// degrades to the pasted-URL escape hatch on it rather than showing an error page.
func handleAlbumEditions(enrichSvc *enrich.Service, cat *catalog.Service, albumID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, err := cat.EntityExists(store.EntityAlbum, albumID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list editions", nil)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		if enrichSvc == nil {
			writeEditionsUnavailable(w, enrich.ErrSearchUnavailable)
			return
		}
		eds, err := enrichSvc.AlbumEditions(r.Context(), albumID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		case err != nil:
			writeEditionsUnavailable(w, err)
			return
		}
		out := albumEditionsJSON{
			AlbumID:         albumID,
			ReleaseGroupID:  eds.ReleaseGroupID,
			ChosenReleaseID: eds.ChosenReleaseID,
			InUseReleaseID:  eds.InUseReleaseID,
			InUseSource:     eds.InUseSource,
			LocalTrackCount: eds.LocalTrackCount,
			Editions:        make([]albumEditionJSON, 0, len(eds.Editions)),
		}
		for _, e := range eds.Editions {
			out.Editions = append(out.Editions, albumEditionJSON{
				ReleaseID: e.ReleaseID, Date: e.Date, Country: e.Country,
				Format: e.Format, TrackCount: e.TrackCount, Disambiguation: e.Disambiguation,
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// writeEditionsUnavailable reports a provider that cannot answer, in the shape every
// other provider-backed list uses — 503 SEARCH_UNAVAILABLE, with the two reasons
// worded apart so an Admin knows whether to change a setting or wait.
func writeEditionsUnavailable(w http.ResponseWriter, err error) {
	if errors.Is(err, enrich.ErrSearchUnavailable) {
		writeError(w, http.StatusServiceUnavailable, codeSearchUnavailable,
			"the metadata provider cannot list this album's editions — it is unconfigured or disabled", nil)
		return
	}
	writeError(w, http.StatusServiceUnavailable, codeSearchUnavailable,
		"listing this album's editions failed — the metadata source may be unreachable", nil)
}
