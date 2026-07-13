package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/marioquake/juicebox/internal/access"
	"github.com/marioquake/juicebox/internal/audio"
	"github.com/marioquake/juicebox/internal/catalog"
	"github.com/marioquake/juicebox/internal/events"
	"github.com/marioquake/juicebox/internal/playback"
	"github.com/marioquake/juicebox/internal/scanner"
	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/subtitle"
)

// Wire shapes for the browse/scan surface (docs/api-contract.md): camelCase,
// the single source of truth for what crosses the HTTP boundary. A Title
// summary (list) is deliberately lighter than the detail (get-one) so the list
// stays cheap and the detail carries the full nested Editions → Files → Streams.

// --- Title summary (list) ---------------------------------------------------

type titleSummaryJSON struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Year  int    `json:"year,omitempty"`
	// NeedsReview/Ambiguous are the Admin attention flags (omitted when false so
	// a clean Title stays lean). A client can filter the list on either.
	NeedsReview bool   `json:"needsReview,omitempty"`
	Ambiguous   bool   `json:"ambiguous,omitempty"`
	TMDBID      string `json:"tmdbId,omitempty"`
	IMDBID      string `json:"imdbId,omitempty"`
	AddedAt     string `json:"addedAt,omitempty"`
	// Watch state (issue 08), per calling User: resumePositionMs is where to
	// resume (0 = none); watched is the threshold/manual outcome. Both omitempty
	// so an untouched Title stays lean.
	ResumePositionMs int64 `json:"resumePositionMs,omitempty"`
	Watched          bool  `json:"watched,omitempty"`
	// Enrichment (external-metadata-enrichment): descriptive fields the optional
	// Enrichment step decorates a Title with. All omitempty so an un-enriched Title
	// stays lean. enrichmentStatus is pending|matched|unmatched|failed|disabled.
	Overview         string   `json:"overview,omitempty"`
	ContentRating    string   `json:"contentRating,omitempty"`
	ReleaseDate      string   `json:"releaseDate,omitempty"`
	RuntimeMinutes   int      `json:"runtimeMinutes,omitempty"`
	Studio           string   `json:"studio,omitempty"`
	Genres           []string `json:"genres,omitempty"`
	EnrichmentStatus string   `json:"enrichmentStatus,omitempty"`
	// ArtworkVersion is an opaque per-Title cache-bust token for the poster: it
	// changes exactly when the Title's served artwork could have changed (a
	// re-enrich that re-fetches the image, or a rescan that rewrites a local one),
	// and is stable across reads otherwise. A browse client appends it to the
	// artwork URL so a re-fetched poster reloads in place while a text-only edit
	// leaves it untouched (no flicker). Omitted when the Title has no artwork.
	ArtworkVersion string `json:"artworkVersion,omitempty"`
}

type titlesResponse struct {
	Titles     []titleSummaryJSON `json:"titles"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

// mergeEnrichment overlays the bulk-read enrichment display fields (overview /
// status / enriched title) onto a lean Title from a reader that didn't load them
// (Home / search). A miss leaves the Title unchanged.
func mergeEnrichment(t store.Title, enr map[string]store.Title) store.Title {
	if e, ok := enr[t.ID]; ok {
		t.Overview = e.Overview
		t.EnrichmentStatus = e.EnrichmentStatus
		t.EnrichedTitle = e.EnrichedTitle
	}
	return t
}

func toTitleSummary(t store.Title, ws store.WatchState, genres []string) titleSummaryJSON {
	return titleSummaryJSON{
		ID:               t.ID,
		Kind:             t.Kind,
		Title:            t.Title,
		Year:             t.Year,
		NeedsReview:      t.NeedsReview,
		Ambiguous:        t.Ambiguous,
		TMDBID:           t.TMDBID,
		IMDBID:           t.IMDBID,
		AddedAt:          formatTimestamp(t.AddedAt),
		ResumePositionMs: ws.ResumePositionMs,
		Watched:          ws.Watched,
		Overview:         t.Overview,
		ContentRating:    t.ContentRating,
		ReleaseDate:      t.ReleaseDate,
		RuntimeMinutes:   t.RuntimeMinutes,
		Studio:           t.Studio,
		Genres:           genres,
		EnrichmentStatus: t.EnrichmentStatus,
	}
}

// --- Title detail (get one) -------------------------------------------------

type streamJSON struct {
	Index     int    `json:"index"`
	Kind      string `json:"kind"`
	Codec     string `json:"codec"`
	Language  string `json:"language,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Channels  int    `json:"channels,omitempty"`
	IsDefault bool   `json:"isDefault"`
}

// subtitleTrackJSON is one selectable Subtitle track on a Title detail — the
// union of every source (CONTEXT.md, ADR-0020). id selects the track (a Stream
// id for embedded, a subtitle-row id for sidecar/fetched). source is
// embedded|sidecar|fetched; kind is text|image; language is ISO-639-1 ("" =
// Unknown); label is the ready-made menu string. Delivery (URLs, HLS renditions,
// burn-in) arrives in later slices — this slice is the read path only.
type subtitleTrackJSON struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Kind     string `json:"kind"`
	Language string `json:"language,omitempty"`
	Forced   bool   `json:"forced"`
	Label    string `json:"label"`
}

// audioStreamJSON is one embedded audio Stream of a File, presented for the
// viewer's Audio menu (audio-streams/01, ADR-0022): id is the stable selector
// later slices pass back as audioStreamId; language is normalized to ISO-639-1
// ("" = Unknown); layout is the familiar surround label ("Stereo"/"5.1"/"7.1");
// label is the ready-made menu string (language + layout, or a title tag like
// "Director's Commentary"). Per CONTEXT.md this is the audio Stream itself, not a
// coined "Audio track". Delivery (renditions, direct-play escalation) arrives in
// later slices — this slice is the read path only.
type audioStreamJSON struct {
	ID         string `json:"id"`
	Index      int    `json:"index"`
	Codec      string `json:"codec"`
	Language   string `json:"language,omitempty"`
	Channels   int    `json:"channels,omitempty"`
	Layout     string `json:"layout,omitempty"`
	IsDefault  bool   `json:"isDefault"`
	Commentary bool   `json:"commentary,omitempty"`
	Label      string `json:"label"`
}

// videoStreamJSON is one selectable video Stream of a File, presented for the
// viewer's Video menu (selectable-video/01, ADR-0025): id is the stable selector
// (later slices pass it back as videoStreamId); label is the ready menu string (the
// embedded title tag like "Black & White"/"Colour", else a resolution token like
// "1080p"/"4K"); width/height carry the resolution. Per CONTEXT.md this is the video
// Stream itself, not a coined "Video track". Cover-art/thumbnail Streams are excluded.
// No per-Stream bitrate is surfaced: the streams table stores none, and the feature
// deliberately adds no schema change (a multi-bitrate set is distinguished by title
// tag or, failing that, resolution).
type videoStreamJSON struct {
	ID        string `json:"id"`
	Index     int    `json:"index"`
	Codec     string `json:"codec"`
	Language  string `json:"language,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	IsDefault bool   `json:"isDefault"`
	Label     string `json:"label"`
}

type fileJSON struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Container  string `json:"container"`
	VideoCodec string `json:"videoCodec,omitempty"`
	AudioCodec string `json:"audioCodec,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	Bitrate    int64  `json:"bitrate,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	SizeBytes  int64  `json:"sizeBytes,omitempty"`
	// Missing is true when this File is absent from disk (soft-delete, ADR-0008);
	// it remains in the catalog so it (and its Title) can return on a later scan.
	Missing bool         `json:"missing,omitempty"`
	Streams []streamJSON `json:"streams"`
	// AudioStreams is this File's embedded audio Streams, labeled for the Audio
	// menu (audio-streams/01). A separate projection from the FFmpeg-pure Streams
	// list above (which stays raw), mirroring how Subtitle tracks are projected.
	// Non-nil; empty for a File with no audio.
	AudioStreams []audioStreamJSON `json:"audioStreams"`
	// VideoStreams is this File's non-cover-art video Streams, labeled for the Video
	// menu (selectable-video/01). The browse path is capability-agnostic, so the
	// default flag here reflects the container is_default disposition; the playback
	// decision re-flags it with the capability-then-quality pick. Non-nil; a single-
	// video File carries a one-element list.
	VideoStreams []videoStreamJSON `json:"videoStreams"`
}

type editionJSON struct {
	ID    string     `json:"id"`
	Name  string     `json:"name"`
	Files []fileJSON `json:"files"`
}

// extraJSON is a clip attached to a Title (trailer/featurette/…). It is never a
// browsable Title; it appears only here under its parent's detail.
type extraJSON struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Path       string `json:"path"`
	Container  string `json:"container,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
}

// artworkJSON points at the artwork-serving endpoint for a role; clients fetch
// the bytes from url. path is included for the Admin (on-disk source). source is
// "local" or "fetched" — local wins over fetched at serve time (CONTEXT.md).
type artworkJSON struct {
	Role   string `json:"role"`
	URL    string `json:"url"`
	Path   string `json:"path"`
	Source string `json:"source,omitempty"`
}

// creditJSON is one enriched cast/crew member on a Title detail. PersonID is the
// provider-namespaced person ref ("tmdb:<id>", omitted when the provider supplied
// none) — the client builds the headshot URL from it (a person with no ref/photo
// simply 404s → placeholder). PhotoVersion is an opaque cache-bust token for the
// headshot (its person artwork added_at), omitted when there's no cached photo —
// analogous to the poster ArtworkVersion. Person/Character/Role/Kind stay
// populated even when there's no photo (cast-photos/01).
type creditJSON struct {
	Person       string `json:"person"`
	Role         string `json:"role,omitempty"`
	Character    string `json:"character,omitempty"`
	Kind         string `json:"kind,omitempty"`
	PersonID     string `json:"personId,omitempty"`
	PhotoVersion string `json:"photoVersion,omitempty"`
}

// toCreditsJSON maps a store cast list (a Title's leaf cast or a browse-parent's
// entity cast) onto the wire creditJSON shape, carrying each member's person ref +
// headshot version so a Movie and a Show detail render the same cast strip
// (cast-photos/01, /02). Returns a non-nil empty slice for an empty cast.
func toCreditsJSON(credits []store.Credit) []creditJSON {
	out := make([]creditJSON, 0, len(credits))
	for _, c := range credits {
		out = append(out, creditJSON{
			Person:       c.Person,
			Role:         c.Role,
			Character:    c.Character,
			Kind:         c.Kind,
			PersonID:     c.PersonRef,
			PhotoVersion: c.PhotoVersion,
		})
	}
	return out
}

type titleDetailJSON struct {
	ID string `json:"id"`
	// LibraryID is the Library this Title belongs to — the client's detail "Back"
	// link returns a Movie to its owning Library (an Episode returns to its Show
	// via the Episode context below).
	LibraryID   string `json:"libraryId,omitempty"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Year        int    `json:"year,omitempty"`
	NeedsReview bool   `json:"needsReview,omitempty"`
	Ambiguous   bool   `json:"ambiguous,omitempty"`
	// Hidden is true when every File is Missing — the Title is excluded from the
	// browse list but still fetchable here so its state is recoverable (ADR-0008).
	Hidden bool   `json:"hidden,omitempty"`
	TMDBID string `json:"tmdbId,omitempty"`
	IMDBID string `json:"imdbId,omitempty"`
	// Watch state (issue 08), per calling User. A client reads resumePositionMs to
	// pass as startPosition on the next playback negotiation (issue 07 unchanged).
	ResumePositionMs int64         `json:"resumePositionMs,omitempty"`
	Watched          bool          `json:"watched,omitempty"`
	AddedAt          string        `json:"addedAt,omitempty"`
	Editions         []editionJSON `json:"editions"`
	Extras           []extraJSON   `json:"extras"`
	Artwork          []artworkJSON `json:"artwork"`
	// ArtworkVersion is an opaque per-Title cache-bust token (newest artwork
	// added_at, exactly like the browse-list titleSummaryJSON), changing whenever
	// any of the Title's served artwork could have changed. The detail hero
	// appends it to its Logo/Background <img> URLs so a re-fetch/pick/upload
	// reloads them in place — the fetched-artwork cache filename is stable per
	// (Title, role), so the row `path` can't serve as the bust key. Omitted when
	// the Title has no artwork.
	ArtworkVersion string `json:"artworkVersion,omitempty"`
	// Subtitles is every selectable Subtitle track the Title offers, from all
	// sources in one list (ADR-0020). Non-nil; empty when the Title has none.
	Subtitles []subtitleTrackJSON `json:"subtitles"`
	// Enrichment (external-metadata-enrichment): the descriptive fields the
	// optional Enrichment step decorates a Title with. All omitempty so an
	// un-enriched Title's detail is unchanged. enrichmentStatus is
	// pending|matched|unmatched|failed|disabled.
	Overview         string       `json:"overview,omitempty"`
	Tagline          string       `json:"tagline,omitempty"`
	ContentRating    string       `json:"contentRating,omitempty"`
	ReleaseDate      string       `json:"releaseDate,omitempty"`
	RuntimeMinutes   int          `json:"runtimeMinutes,omitempty"`
	Studio           string       `json:"studio,omitempty"`
	Genres           []string     `json:"genres,omitempty"`
	Cast             []creditJSON `json:"cast,omitempty"`
	EnrichmentStatus string       `json:"enrichmentStatus,omitempty"`
	// LockedFields lists the descriptive fields an Admin hand-edited and Locked
	// (CONTEXT.md): re-enrichment skips them. Omitted when nothing is locked. A
	// client shows these as pinned and offers to release a lock back to auto.
	LockedFields []string `json:"lockedFields,omitempty"`
	// IdentityKey is the Title's stable identity key (ADR-0014). Surfaced so a
	// client — and the Wrong-item correction tests — can observe an identity change:
	// a Fix-info/Fix-label edit leaves it untouched, a Wrong-item correction re-keys
	// it to the picked work (item-editing/04).
	IdentityKey string `json:"identityKey,omitempty"`
	// DisplayTitle is the canonical enriched title for an Episode/Track (e.g. a
	// real episode name for a date-based episode) — display only; Title above stays
	// the parsed identity value. Omitted when there is no enriched title.
	DisplayTitle string `json:"displayTitle,omitempty"`
	// Episode is the Show/Season/episode parent context, present only when this
	// Title is an Episode (kind "episode"); nil/omitted for a Movie.
	Episode *episodeContextJSON `json:"episode,omitempty"`
	// Track is the Artist/Album/disc/track parent context, present only when this
	// Title is a Track (kind "track"); nil/omitted otherwise.
	Track *trackContextJSON `json:"track,omitempty"`
}

func toTitleDetail(d store.TitleDetail, ws store.WatchState) titleDetailJSON {
	editions := make([]editionJSON, 0, len(d.Editions))
	for _, e := range d.Editions {
		files := make([]fileJSON, 0, len(e.Files))
		for _, f := range e.Files {
			streams := make([]streamJSON, 0, len(f.Streams))
			for _, s := range f.Streams {
				streams = append(streams, streamJSON{
					Index:     s.Index,
					Kind:      s.Kind,
					Codec:     s.Codec,
					Language:  s.Language,
					Width:     s.Width,
					Height:    s.Height,
					Channels:  s.Channels,
					IsDefault: s.IsDefault,
				})
			}
			files = append(files, fileJSON{
				ID:           f.ID,
				Path:         f.Path,
				Container:    f.Container,
				VideoCodec:   f.VideoCodec,
				AudioCodec:   f.AudioCodec,
				Width:        f.Width,
				Height:       f.Height,
				Bitrate:      f.Bitrate,
				DurationMs:   f.DurationMs,
				SizeBytes:    f.SizeBytes,
				Missing:      !f.Present,
				Streams:      streams,
				AudioStreams: toAudioStreams(f.Streams),
				VideoStreams: toVideoStreams(f.Streams, ""),
			})
		}
		editions = append(editions, editionJSON{ID: e.ID, Name: e.Name, Files: files})
	}

	extras := make([]extraJSON, 0, len(d.Extras))
	for _, ex := range d.Extras {
		extras = append(extras, extraJSON{
			ID:         ex.ID,
			Type:       ex.Type,
			Path:       ex.Path,
			Container:  ex.Container,
			DurationMs: ex.DurationMs,
		})
	}

	artwork := make([]artworkJSON, 0, len(d.Artwork))
	// artworkVersion = newest added_at across the resolved rows (same token the
	// browse list computes via ArtworkVersionsForTitles), so the detail hero can
	// cache-bust its Logo/Background <img> on a value that advances on every
	// re-fetch/pick/upload. It travels on every detail response (GET + the
	// correction/pick/upload endpoints all return toTitleDetail), so a hero
	// reloads in place without a page refresh.
	var artworkVersion string
	for _, a := range d.Artwork {
		artwork = append(artwork, artworkJSON{
			Role:   a.Role,
			URL:    APIPrefix + "/titles/" + d.ID + "/artwork/" + a.Role,
			Path:   a.Path,
			Source: a.Source,
		})
		if a.AddedAt > artworkVersion {
			artworkVersion = a.AddedAt
		}
	}

	cast := toCreditsJSON(d.Cast)

	return titleDetailJSON{
		Subtitles:        toSubtitleTracks(d),
		ID:               d.ID,
		LibraryID:        d.LibraryID,
		Kind:             d.Kind,
		Title:            d.Title.Title,
		Year:             d.Year,
		NeedsReview:      d.NeedsReview,
		Ambiguous:        d.Ambiguous,
		Hidden:           d.Hidden,
		TMDBID:           d.TMDBID,
		IMDBID:           d.IMDBID,
		ResumePositionMs: ws.ResumePositionMs,
		Watched:          ws.Watched,
		AddedAt:          formatTimestamp(d.AddedAt),
		Editions:         editions,
		Extras:           extras,
		Artwork:          artwork,
		ArtworkVersion:   artworkVersion,
		Overview:         d.Overview,
		Tagline:          d.Tagline,
		ContentRating:    d.ContentRating,
		ReleaseDate:      d.ReleaseDate,
		RuntimeMinutes:   d.RuntimeMinutes,
		Studio:           d.Studio,
		Genres:           d.Genres,
		Cast:             cast,
		EnrichmentStatus: d.EnrichmentStatus,
		LockedFields:     d.LockedFields,
		IdentityKey:      d.IdentityKey,
		DisplayTitle:     d.EnrichedTitle,
	}
}

// toSubtitleTracks assembles the Title's full Subtitle-track list from all
// sources in one pass: the embedded subtitle Streams (projected from each File)
// followed by the persisted Sidecar/Fetched tracks. Every entry's language is
// normalized to ISO-639-1 and given a ready-made menu label, so the client
// treats the sources uniformly and doesn't care where a track came from
// (ADR-0020). Delivery details are added in later slices. Always non-nil.
func toSubtitleTracks(d store.TitleDetail) []subtitleTrackJSON {
	out := make([]subtitleTrackJSON, 0)
	// Embedded: one entry per distinct subtitle Stream, in edition/file/stream
	// order. The Stream id selects the track; kind and language are derived from
	// the codec and tag (the streams table stays FFmpeg-pure — no normalized copy
	// stored). Identical streams across a Title's Editions/parts (e.g. an English
	// sub in both a 1080p and a 4K Edition) collapse to one menu entry, keyed by
	// the observable (kind, language, forced) — the client sees one "English",
	// not one per file.
	seenEmbedded := map[string]bool{}
	for _, e := range d.Editions {
		for _, f := range e.Files {
			for _, s := range f.Streams {
				if s.Kind != "subtitle" {
					continue
				}
				kind := subtitle.KindForCodec(s.Codec)
				lang := subtitle.NormalizeLang(s.Language)
				key := kind + "|" + lang + "|" + strconv.FormatBool(s.Forced)
				if seenEmbedded[key] {
					continue
				}
				seenEmbedded[key] = true
				out = append(out, subtitleTrackJSON{
					ID:       s.ID,
					Source:   "embedded",
					Kind:     kind,
					Language: lang,
					Forced:   s.Forced,
					Label:    subtitle.Label(lang, s.Forced),
				})
			}
		}
	}
	// Sidecar + Fetched: already source/kind-tagged; normalize the language
	// defensively (idempotent) so a pre-normalization row still presents cleanly.
	for _, sub := range d.Subtitles {
		lang := subtitle.NormalizeLang(sub.Language)
		out = append(out, subtitleTrackJSON{
			ID:       sub.ID,
			Source:   sub.Source,
			Kind:     sub.Kind,
			Language: lang,
			Forced:   sub.Forced,
			Label:    subtitle.Label(lang, sub.Forced),
		})
	}
	return out
}

// toAudioStreams projects a File's embedded audio Streams onto the labeled
// wire shape the Audio menu consumes (audio-streams/01). It keeps the raw
// Streams list FFmpeg-pure and normalizes here at read time — reusing the ISO-639
// machinery the subtitle work built — exactly as toSubtitleTracks does: the
// language folds to ISO-639-1, the channel count becomes a familiar layout, and
// the label combines language + layout (or a title tag like "Director's
// Commentary"). Only "audio" Streams surface. Always non-nil.
func toAudioStreams(streams []store.Stream) []audioStreamJSON {
	out := make([]audioStreamJSON, 0)
	for _, s := range streams {
		if s.Kind != "audio" {
			continue
		}
		lang := audio.NormalizeLang(s.Language)
		out = append(out, audioStreamJSON{
			ID:         s.ID,
			Index:      s.Index,
			Codec:      s.Codec,
			Language:   lang,
			Channels:   s.Channels,
			Layout:     audio.ChannelLayout(s.Channels),
			IsDefault:  s.IsDefault,
			Commentary: s.Commentary,
			Label:      audio.Label(lang, s.Channels, s.Title, s.Commentary),
		})
	}
	return out
}

// toVideoStreams projects a File's non-cover-art video Streams onto the wire shape the
// Video menu consumes (selectable-video/01, ADR-0025), parallel to toAudioStreams. It
// reuses the negotiation's selectable set (playback.SelectableVideoStreams) so the
// browse list and the playback decision offer EXACTLY the same Streams — embedded
// cover art excluded by the same rule. The default flag marks the resolved default:
// when resolvedID is non-empty (the playback decision knows the capability-then-quality
// pick) the Stream with that id is flagged; otherwise (the capability-agnostic browse
// path) the container is_default disposition is used. Always non-nil; a single-video
// File yields a one-element list.
func toVideoStreams(streams []store.Stream, resolvedID string) []videoStreamJSON {
	selectable := playback.SelectableVideoStreams(store.File{Streams: streams})
	out := make([]videoStreamJSON, 0, len(selectable))
	for _, s := range selectable {
		isDefault := s.IsDefault
		if resolvedID != "" {
			isDefault = s.ID == resolvedID
		}
		out = append(out, videoStreamJSON{
			ID:        s.ID,
			Index:     s.Index,
			Codec:     s.Codec,
			Language:  s.Language,
			Width:     s.Width,
			Height:    s.Height,
			IsDefault: isDefault,
			Label:     videoStreamLabel(s),
		})
	}
	return out
}

// videoStreamLabel is the ready menu string for a video Stream (selectable-video/01):
// the embedded title tag when present (e.g. "Black & White", "Colour"), else a
// resolution token from the height ("4K" for UHD, else "1080p"/"720p"/…), else the
// codec as a last resort. So a titled cut reads by name and an untitled multi-res set
// reads by resolution — the label rule ADR-0025 specifies (title tag → resolution).
func videoStreamLabel(s store.Stream) string {
	if t := strings.TrimSpace(s.Title); t != "" {
		return t
	}
	if s.Height >= 2160 {
		return "4K"
	}
	if s.Height > 0 {
		return strconv.Itoa(s.Height) + "p"
	}
	if c := strings.TrimSpace(s.Codec); c != "" {
		return c
	}
	return "Video"
}

// --- Scan status ------------------------------------------------------------

type scanStatusJSON struct {
	LibraryID    string `json:"libraryId"`
	State        string `json:"state"`
	TitlesFound  int    `json:"titlesFound"`
	FilesFound   int    `json:"filesFound"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	StartedAt    string `json:"startedAt,omitempty"`
	FinishedAt   string `json:"finishedAt,omitempty"`
	// Scope is the entity label of a running Targeted scan (ADR-0030), "" for a full
	// scan; a client shows "Scanning <scope>…" only while State is running.
	Scope string `json:"scope,omitempty"`
}

func toScanStatus(s store.ScanStatus) scanStatusJSON {
	return scanStatusJSON{
		LibraryID:    s.LibraryID,
		State:        s.State,
		TitlesFound:  s.TitlesFound,
		FilesFound:   s.FilesFound,
		ErrorMessage: s.ErrorMessage,
		StartedAt:    formatTimestamp(s.StartedAt),
		FinishedAt:   formatTimestamp(s.FinishedAt),
		Scope:        s.Scope,
	}
}

// --- Handlers ---------------------------------------------------------------

// scanRequest is the optional JSON body of POST /libraries/{id}/scan. mode
// "full" forces a full re-derivation; absent/"incremental" is the default.
type scanRequest struct {
	Mode string `json:"mode"`
}

// handleScan triggers a scan of the Library's roots (Admin) and returns 202
// Accepted immediately — the scan runs in the BACKGROUND, not for the lifetime
// of this request. By default it is incremental (only new/changed/absent files,
// ADR-0008); pass {"mode":"full"} in the body or ?mode=full to force a full
// re-derivation. The 202 body is the scan status (state "running") — the same
// shape the pollable GET returns; the client tracks the scan to completion via
// that pollable GET /scan and the scanProgress SSE stream (ADR-0016), so it need
// not stay connected. A client that navigates away therefore cannot cancel the
// scan, and an unknown Library is still 404 (api-contract.md hide-existence
// posture) because existence is validated before the 202.
//
// The scan runs on a context detached from the request (the request is already
// done by the time the goroutine walks): a client disconnect can't cancel it,
// exactly like the scheduled safety-net scan (app.runScheduledScans).
//
// When the scan settles, the done callback fires the post-scan side-effects off
// the scanner's events-free core: on success it enqueues a background Enrichment
// pass (auto-after-scan; enrichTrigger may be nil) and publishes a library-scoped
// libraryUpdated event (ADR-0016) telling connected clients to refetch; on
// failure it emits a terminal scanProgress (Complete=true) so a client's
// "scanning…" indicator clears instead of hanging. broker may be nil (events
// simply aren't published).
func handleScan(scan *scanner.Service, status ScanStatusReader, enrichTrigger func(string), broker *events.Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := pathParam(r.URL.Path, "/libraries/", "/scan")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}

		// Mode from ?mode=full or a JSON body {"mode":"full"} (either is fine).
		mode := scanner.ModeIncremental
		if strings.EqualFold(r.URL.Query().Get("mode"), "full") {
			mode = scanner.ModeFull
		} else if r.ContentLength > 0 {
			var req scanRequest
			// Best-effort: a malformed body just leaves the default mode.
			if json.NewDecoder(r.Body).Decode(&req) == nil && strings.EqualFold(req.Mode, "full") {
				mode = scanner.ModeFull
			}
		}

		// While the scan walks the Library it publishes scanProgress events over the
		// SSE Broker (ADR-0016) so a connected client shows an advancing "scanning…"
		// indicator; the Scanner fires a terminal Complete event with the final
		// counts. broker may be nil (events simply aren't published).
		var onProgress func(scanner.Progress)
		if broker != nil {
			onProgress = func(p scanner.Progress) { broker.PublishScanProgress(toScanEvent(p)) }
		}
		// Post-scan side-effects, run on the background goroutine once the scan
		// settles (see StartScan). Kept here, not in the scanner, so the scanner
		// stays free of any events/enrich import (ADR-0006).
		done := func(scanErr error) {
			if scanErr != nil {
				// Terminal-on-error: a failed scan never reaches the Scanner's own
				// terminal event, so emit one here so the client's "scanning…"
				// indicator clears instead of hanging.
				if broker != nil {
					broker.PublishScanProgress(events.ScanProgress{LibraryID: id, Complete: true})
				}
				return
			}
			// Auto-after-scan: enqueue a background Enrichment pass (non-blocking).
			if enrichTrigger != nil {
				enrichTrigger(id)
			}
			// The Library's contents may have changed: nudge connected clients to
			// refetch (library-scoped, so only subscribers who can see it — and any
			// Admin — receive it).
			if broker != nil {
				broker.PublishLibraryUpdated(id)
			}
		}

		// Dispatch the scan on a request-independent context (context.Background):
		// the request returns 202 immediately, so the scan must outlive it. The
		// Library is validated and marked running before this returns, so the 202
		// status read below reflects "running" and an unknown Library is 404.
		err := scan.StartScan(context.Background(), id, mode, onProgress, done)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
			return
		case errors.Is(err, scanner.ErrScanInProgress):
			// A scan is already running for this Library: don't start a second one or
			// reset its counters — just report the in-flight status (idempotent).
			st, sErr := status.ScanStatusByLibrary(id)
			if sErr != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "scan failed", nil)
				return
			}
			writeJSON(w, http.StatusAccepted, toScanStatus(st))
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "scan failed", nil)
			return
		}
		st, err := status.ScanStatusByLibrary(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "scan failed", nil)
			return
		}
		writeJSON(w, http.StatusAccepted, toScanStatus(st))
	}
}

// toScanEvent maps a scanner.Progress snapshot onto the SSE payload shape,
// shared by the manual handler and (via the app loop) the scheduled scan path —
// mirrors toEnrichEvent. The producer (scanner) stays free of any events import;
// the conversion happens here at the transport boundary.
func toScanEvent(p scanner.Progress) events.ScanProgress {
	return events.ScanProgress{
		LibraryID:   p.LibraryID,
		TitlesFound: p.TitlesFound,
		FilesFound:  p.FilesFound,
		Complete:    p.Complete,
		Added:       p.Added,
		Removed:     p.Removed,
	}
}

// handleScanStatus returns the pollable scan status for a Library (authenticated).
func handleScanStatus(status ScanStatusReader, exists LibraryExister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := pathParam(r.URL.Path, "/libraries/", "/scan")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		ok, err := exists.LibraryExists(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to read scan status", nil)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
			return
		}
		st, err := status.ScanStatusByLibrary(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to read scan status", nil)
			return
		}
		writeJSON(w, http.StatusOK, toScanStatus(st))
	}
}

// handleListTitles returns a cursor-paginated, sortable list of a Library's
// Title summaries (authenticated). Each summary is decorated with the calling
// User's watch state (resume + watched) via one bulk read, so a client sees
// resume markers without an extra round-trip per Title.
func handleListTitles(svc *catalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		id := pathParam(r.URL.Path, "/libraries/", "/titles")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}

		// A TV Library's top-level grid is Shows, not Titles (api-contract.md). The
		// Movie grid is unchanged. Music (later) would branch to Artists here.
		kind, err := svc.LibraryKind(id)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list titles", nil)
			return
		}
		if kind == "tv" {
			handleListShows(svc, id)(w, r)
			return
		}
		// A Music Library's top-level list is Artists, not Titles (api-contract.md).
		if kind == "music" {
			handleListArtists(svc, id)(w, r)
			return
		}

		q := r.URL.Query()
		in := catalog.ListInput{
			LibraryID: id,
			Sort:      parseSort(q.Get("sort")),
			Cursor:    q.Get("cursor"),
			Limit:     parseLimit(q.Get("limit")),
			// filter[genre] narrows the grid to one enriched genre (external-
			// metadata-enrichment). Absent → no filter.
			Genre: q.Get("filter[genre]"),
		}
		page, err := svc.ListTitles(scope, in)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
			return
		case errors.Is(err, catalog.ErrBadCursor):
			writeError(w, http.StatusBadRequest, codeBadRequest, "invalid cursor", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list titles", nil)
			return
		}

		ids := make([]string, 0, len(page.Titles))
		for _, t := range page.Titles {
			ids = append(ids, t.ID)
		}
		states, err := svc.WatchStatesForTitles(ident.User.ID, ids)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list titles", nil)
			return
		}
		// Enriched genres for the page in one bulk read (no N+1), so each summary
		// can carry its genres for browse/filter chips.
		genres, err := svc.GenresForTitles(ids)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list titles", nil)
			return
		}
		// Per-Title artwork cache-bust version (one bulk read), so a browse client
		// reloads only the posters whose artwork actually changed when it live-
		// refreshes during a scan/enrich pass (realtime-events web slice).
		versions, err := svc.ArtworkVersionsForTitles(ids)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list titles", nil)
			return
		}

		out := titlesResponse{
			Titles:     make([]titleSummaryJSON, 0, len(page.Titles)),
			NextCursor: page.NextCursor,
		}
		for _, t := range page.Titles {
			js := toTitleSummary(t, states[t.ID], genres[t.ID])
			js.ArtworkVersion = versions[t.ID]
			out.Titles = append(out.Titles, js)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// --- Unmatched (Admin attention surface) ------------------------------------

type unmatchedFileJSON struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Reason  string `json:"reason,omitempty"`
	AddedAt string `json:"addedAt,omitempty"`
}

type unmatchedResponse struct {
	Files []unmatchedFileJSON `json:"files"`
}

// handleListUnmatched returns a Library's Unmatched files — recognized media
// the scanner could not turn into a Title (CONTEXT.md). Admin-only attention
// surface. Unknown Library → 404 (hide-existence).
func handleListUnmatched(svc *catalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := pathParam(r.URL.Path, "/libraries/", "/unmatched")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		files, err := svc.ListUnmatched(id)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list unmatched", nil)
			return
		}
		out := unmatchedResponse{Files: make([]unmatchedFileJSON, 0, len(files))}
		for _, f := range files {
			out.Files = append(out.Files, unmatchedFileJSON{
				ID:      f.ID,
				Path:    f.Path,
				Reason:  f.Reason,
				AddedAt: formatTimestamp(f.AddedAt),
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// --- Enrichment attention (Admin attention surface) -------------------------

// enrichmentAttentionTitleJSON is one Title that Enrichment could not match — a
// lean entry for the Admin's hand-match list. It browses/plays fine; only its
// descriptive metadata is missing, so it is kept distinct from the identity
// Unmatched files (no Title at all) and from needsReview.
type enrichmentAttentionTitleJSON struct {
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	Title            string `json:"title"`
	Year             int    `json:"year,omitempty"`
	EnrichmentStatus string `json:"enrichmentStatus"`
}

type enrichmentAttentionResponse struct {
	Titles []enrichmentAttentionTitleJSON `json:"titles"`
}

// handleListEnrichmentAttention returns a Library's Titles whose enrichment
// status is 'unmatched' or 'failed' (CONTEXT.md) — the Admin attention surface
// for correcting a wrong/missing metadata match via PUT
// /titles/{id}/enrichmentMatch. It is a NEW dimension on the attention surface,
// separate from the identity Unmatched files and the needs-review list. Admin-only.
// Unknown Library → 404 (hide-existence).
func handleListEnrichmentAttention(svc *catalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := pathParam(r.URL.Path, "/libraries/", "/enrichment-attention")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		titles, err := svc.TitlesNeedingMatch(id)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list enrichment attention", nil)
			return
		}
		out := enrichmentAttentionResponse{Titles: make([]enrichmentAttentionTitleJSON, 0, len(titles))}
		for _, t := range titles {
			out.Titles = append(out.Titles, enrichmentAttentionTitleJSON{
				ID:               t.ID,
				Kind:             t.Kind,
				Title:            t.Title,
				Year:             t.Year,
				EnrichmentStatus: t.EnrichmentStatus,
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// --- Needs-review attention (Admin attention surface) -----------------------

// needsReviewItemJSON is one entry on the identity needs-review list: a Title
// (Movie / Episode / Track) or a Show the scanner flagged as an uncertain parse
// (no year, non-SxxExx episode numbering, or a tag-less Track). FolderPath is the
// path a fix-match override must be keyed to, set for the kinds a folder override
// can fix — a Movie (its folder / the file itself), a Show (its top-level folder),
// and a Track (its album folder) — so the client can offer fix-identity. It is
// omitted for an Episode (a numbering problem only Enrichment maps), which gets
// only "Mark reviewed". This is the identity attention list, distinct from the
// enrichment metadata-match list above.
type needsReviewItemJSON struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"` // "movie" | "episode" | "track" | "show"
	Title      string `json:"title"`
	Year       int    `json:"year,omitempty"`
	FolderPath string `json:"folderPath,omitempty"`
}

type needsReviewResponse struct {
	Items []needsReviewItemJSON `json:"items"`
}

// handleListNeedsReview returns a Library's still-flagged needs-review items —
// Movies, Episodes, Tracks, and Shows whose needs_review is set and which an Admin
// has not yet dismissed. This is the server-side replacement for the old client
// page-walk: it works uniformly across Movie / TV / Music libraries (the walk
// silently returned nothing for TV/Music, whose listings are Shows/Artists, not
// Titles) and hands back the Movie folder so a fix-match can be driven inline.
// Admin-only; unknown Library → 404 (hide-existence).
func handleListNeedsReview(svc *catalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := pathParam(r.URL.Path, "/libraries/", "/needs-review")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		items, err := svc.NeedsReview(id)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list needs review", nil)
			return
		}
		out := needsReviewResponse{Items: make([]needsReviewItemJSON, 0, len(items))}
		for _, it := range items {
			out.Items = append(out.Items, needsReviewItemJSON{
				ID:         it.ID,
				Kind:       it.Kind,
				Title:      it.Title,
				Year:       it.Year,
				FolderPath: it.Anchor,
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleReviewTitle dismisses a Title's (Movie / Episode / Track) needs_review
// flag — POST /titles/{id}/review, Admin-only. Idempotent confirmation that the
// uncertain parse is fine; the dismissal sticks across rescans (migration 0012).
// Unknown Title → 404. titleID is parsed by the caller (subtree router).
func handleReviewTitle(svc *catalog.Service, titleID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch err := svc.MarkTitleReviewed(titleID); {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to mark reviewed", nil)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// handleReviewShow is handleReviewTitle for a Show — POST /shows/{id}/review.
func handleReviewShow(svc *catalog.Service, showID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch err := svc.MarkShowReviewed(showID); {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "show not found", nil)
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to mark reviewed", nil)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// handleGetTitle returns one Title with its nested Editions/Files/Streams,
// Extras, and Artwork (authenticated). Unknown id → 404. It also dispatches the
// artwork sub-resource (/titles/{id}/artwork/{role}).
func handleGetTitle(svc *catalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/titles/")
		if rest == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		// /titles/{id}/artwork/{role}
		if i := strings.Index(rest, "/artwork/"); i > 0 {
			titleID := rest[:i]
			role := rest[i+len("/artwork/"):]
			handleTitleArtwork(svc, scope, titleID, role)(w, r)
			return
		}
		if strings.Contains(rest, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		d, err := svc.GetTitle(scope, rest)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to get title", nil)
			return
		}
		// Surface the calling User's watch state (resume + watched) on the detail.
		ws, err := svc.WatchStateFor(ident.User.ID, d.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to get title", nil)
			return
		}
		out := toTitleDetail(d, ws)
		// For an Episode, attach its Show/Season/episode parent context (a Movie
		// has none — ErrNotFound — so the field stays omitted).
		if d.Kind == "episode" {
			if c, err := svc.EpisodeContext(d.ID); err == nil {
				out.Episode = toEpisodeContext(c)
			}
		}
		// For a Track, attach its Artist/Album/disc/track parent context.
		if d.Kind == "track" {
			if c, err := svc.TrackContext(d.ID); err == nil {
				out.Track = toTrackContext(c)
			}
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleTitleArtwork serves the local artwork image bytes for a Title+role
// (poster|background). Local-on-disk wins; no external fetch (ADR-0001). A Title
// or role with no artwork → 404.
func handleTitleArtwork(svc *catalog.Service, scope access.Scope, titleID, role string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if titleID == "" || role == "" || strings.Contains(role, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		art, err := svc.Artwork(scope, titleID, role)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "artwork not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to get artwork", nil)
			return
		}
		// ServeFile streams the image with correct content-type/caching and handles
		// range requests. ResolveArtworkPath re-roots a cache-relative (fetched/
		// uploaded) path onto the artwork dir; a local path passes through absolute.
		http.ServeFile(w, r, svc.ResolveArtworkPath(art.Path))
	}
}

// handleLibrarySubtree dispatches every route under "/libraries/{id}...". It
// runs behind requireAuth (so the identity is attached), then routes by
// sub-resource and applies the right scope per leaf:
//
//   - {id}/titles  GET            → browse (any authenticated User)
//   - {id}/scan    POST           → trigger scan (Admin)
//   - {id}/scan    GET            → scan status (any authenticated User)
//   - {id}         GET            → single Library (scoped)
//   - {id}         PATCH / DELETE → single-Library admin ops (Admin)
//
// Sub-resources are matched first so they aren't shadowed by the single-Library
// handler (which rejects any path containing a "/").
func handleLibrarySubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/libraries/")

		switch {
		case strings.HasSuffix(rest, "/titles"):
			requireMethod(http.MethodGet, requireScope(deps.Access, handleListTitles(deps.Catalog)))(w, r)
			return
		case strings.HasSuffix(rest, "/unmatched"):
			// Admin attention surface: the Unmatched list is Admin-only.
			requireMethod(http.MethodGet, requireAdmin(handleListUnmatched(deps.Catalog)))(w, r)
			return
		case strings.HasSuffix(rest, "/overrides"):
			// Admin attention surface: Match overrides (orphans surfaced here).
			requireMethod(http.MethodGet, requireAdmin(handleListOverrides(deps.Match)))(w, r)
			return
		case strings.HasSuffix(rest, "/enrichment-attention"):
			// Admin attention surface: Titles whose Enrichment is unmatched/failed,
			// awaiting a hand-match (distinct from the identity Unmatched bucket).
			requireMethod(http.MethodGet, requireAdmin(handleListEnrichmentAttention(deps.Catalog)))(w, r)
			return
		case strings.HasSuffix(rest, "/needs-review"):
			// Admin attention surface: Movies/Episodes/Tracks/Shows the scanner
			// flagged as an uncertain parse, each resolvable via mark-reviewed (or a
			// Movie fix-match). Server-side so it works for TV/Music too.
			requireMethod(http.MethodGet, requireAdmin(handleListNeedsReview(deps.Catalog)))(w, r)
			return
		case strings.HasSuffix(rest, "/fix-match"):
			// Admin identity correction (fix-match), keyed to a folder path. An id-only
			// fix resolves its canonical title/year from the provider (deps.Enrich).
			requireMethod(http.MethodPost, requireAdmin(handleFixMatch(deps.Match, deps.Enrich, deps.Catalog)))(w, r)
			return
		case strings.HasSuffix(rest, "/enrich"):
			// Admin: trigger an Enrichment pass over the Library (manual/re-enrich).
			requireMethod(http.MethodPost, requireAdmin(handleEnrich(deps.Enrich, deps.Events)))(w, r)
			return
		case strings.HasSuffix(rest, "/enrichment-policy"):
			// Admin: read / partially-update the Library's Enrichment policy (ADR-0027).
			switch r.Method {
			case http.MethodGet:
				requireAdmin(handleGetEnrichmentPolicy(deps))(w, r)
			case http.MethodPut:
				requireAdmin(handleUpdateEnrichmentPolicy(deps))(w, r)
			default:
				w.Header().Set("Allow", "GET, PUT")
				writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
					"method not allowed", nil)
			}
			return
		case strings.HasSuffix(rest, "/scan"):
			switch r.Method {
			case http.MethodPost:
				requireAdmin(handleScan(deps.Scanner, deps.ScanStatus, deps.EnrichTrigger, deps.Events))(w, r)
			case http.MethodGet:
				handleScanStatus(deps.ScanStatus, deps.Libraries)(w, r)
			default:
				w.Header().Set("Allow", "GET, POST")
				writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
					"method not allowed", nil)
			}
			return
		default:
			// Single-Library ops: GET /libraries/{id} is any authenticated User,
			// scoped (an ungranted Library is 404); DELETE is Admin-only.
			switch r.Method {
			case http.MethodGet:
				requireScope(deps.Access, handleGetLibrary(deps.Library))(w, r)
			case http.MethodPatch:
				requireAdmin(handleUpdateLibrary(deps.Library))(w, r)
			case http.MethodDelete:
				requireAdmin(handleDeleteLibrary(deps.Library))(w, r)
			default:
				w.Header().Set("Allow", "GET, PATCH, DELETE")
				writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
					"method not allowed", nil)
			}
		}
	}
}

// parseSort maps the sort= param to a catalog.Sort, defaulting to by-title.
// Recognized: "title" (default), "dateAdded" / "addedAt".
func parseSort(s string) catalog.Sort {
	switch s {
	case "dateAdded", "addedAt", "-addedAt", "recent":
		return catalog.SortDateAdded
	default:
		return catalog.SortTitle
	}
}

// parseLimit parses the limit= param; 0 (defaulted/clamped by the service) on
// absence or garbage.
func parseLimit(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// pathParam extracts the {id} from a path of the form prefix + id + suffix,
// returning "" if it doesn't match exactly (no extra segments). Used for the
// nested /libraries/{id}/scan and /libraries/{id}/titles routes.
func pathParam(path, prefix, suffix string) string {
	rest := strings.TrimPrefix(path, prefix)
	id := strings.TrimSuffix(rest, suffix)
	if id == rest || id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}
