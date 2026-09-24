package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/lyricfetch"
	"github.com/goozakdev/obelo-server/internal/lyrics"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Lyrics. One GET leaf on the title subtree, open to ANY authenticated User —
// Members included, because reading along is part of listening:
//
//	GET /titles/{id}/lyrics → { "lyrics": null | { "kind", "source", "lines", "text" } }
//
// "lyrics" is null when the Title has none, which is an empty state and never an
// error. A Synced answer carries "lines" ([{ "startMs", "text" }], "text" empty);
// a Plain one carries "text" ("lines" empty).
//
// This GET is what opening the lyrics view does, so it is also where a Track with
// no usable Local lyrics first asks its Lyric providers (internal/lyricfetch).
// "source" is "local" or "fetched"; a fetched answer also carries "id", which
// names it for "wrong lyrics".
//
//	POST /titles/{id}/lyrics/wrong { "id" } → the GET's shape, after the rejection
//
// "Wrong lyrics": the fetched answer the Track shows, named by the "id" the view
// was given, is rejected for that Track — for every User, Members included, not
// just the one who pressed it — and the Lyric providers are asked again passing
// over every answer rejected for it (lyricfetch.Service.Reject). A 409 changes
// nothing and carries what the Track shows now in details.lyrics:
// NO_FETCHED_LYRICS when that is not a provider's answer, LYRICS_CHANGED when it
// is another one than the id names (a stale view, or a second press). The
// remote role — a linked Server, not a person reading along — is refused.

// lyricsFetchTimeout bounds one open's asking of every Lyric provider. Each call
// has its own budget inside it; this is the ceiling on the whole page.
const lyricsFetchTimeout = 30 * time.Second

type lyricLineJSON struct {
	StartMs int64  `json:"startMs"`
	Text    string `json:"text"`
}

type lyricsJSON struct {
	Kind   string          `json:"kind"`
	Source string          `json:"source"`
	ID     string          `json:"id,omitempty"`
	Lines  []lyricLineJSON `json:"lines"`
	Text   string          `json:"text"`
}

type lyricsResponse struct {
	Lyrics *lyricsJSON `json:"lyrics"`
}

func handleTitleLyrics(deps Deps, titleID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		a, found, err := titleLyrics(r.Context(), deps, scope, titleID)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to load lyrics", nil)
			return
		}
		writeJSON(w, http.StatusOK, lyricsResponse{Lyrics: lyricsBody(a, found)})
	}
}

// lyricsBody is what a Track shows in the GET's shape, nil when it shows none.
func lyricsBody(a lyricfetch.Answer, found bool) *lyricsJSON {
	if !found {
		return nil
	}
	out := &lyricsJSON{Kind: string(a.Lyrics.Kind), Source: a.Source, ID: a.ID, Lines: []lyricLineJSON{}}
	if a.Lyrics.Kind == lyrics.Synced {
		for _, ln := range a.Lyrics.Lines {
			out.Lines = append(out.Lines, lyricLineJSON{StartMs: ln.StartMs, Text: ln.Text})
		}
	} else {
		out.Text = a.Lyrics.Text
	}
	return out
}

func handleWrongLyrics(deps Deps, titleID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id, ok := identityFrom(r.Context()); ok && id.User.Role == auth.RoleRemote {
			writeError(w, http.StatusForbidden, codeForbidden, "a linked server cannot mark lyrics wrong", nil)
			return
		}
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		var in struct {
			ID string `json:"id"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if in.ID == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest, "id of the lyrics shown is required", nil)
			return
		}
		d, err := deps.Catalog.GetTitle(scope, titleID)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to load title", nil)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), lyricsFetchTimeout)
		defer cancel()
		err = lyricfetch.ErrNothingToReject
		var a lyricfetch.Answer
		var found bool
		if deps.Lyrics != nil && d.Kind == "track" {
			a, found, err = deps.Lyrics.Reject(ctx, lyricTrack(deps, d), in.ID)
		}
		code, message := "", ""
		switch {
		case errors.Is(err, lyricfetch.ErrNothingToReject):
			code, message = codeNoFetchedLyrics, "these lyrics did not come from a lyric provider"
		case errors.Is(err, lyricfetch.ErrNotShown):
			code, message = codeLyricsChanged, "these lyrics have changed since they were shown"
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to reject lyrics", nil)
			return
		default:
			writeJSON(w, http.StatusOK, lyricsResponse{Lyrics: lyricsBody(a, found)})
			return
		}
		// Nothing was rejected: answer with what the Track shows now, so the view
		// shows that instead of the answer it pressed on.
		a, found, err = titleLyrics(r.Context(), deps, scope, titleID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to load lyrics", nil)
			return
		}
		writeError(w, http.StatusConflict, code, message, map[string]any{"lyrics": lyricsBody(a, found)})
	}
}

// titleLyrics is the lyrics a Title shows and their source. With no Lyric
// provider service wired it is the Local lyrics alone; otherwise a Track is
// resolved through the service, which may ask the providers.
func titleLyrics(ctx context.Context, deps Deps, scope access.Scope, titleID string) (lyricfetch.Answer, bool, error) {
	if deps.Lyrics == nil {
		l, found, err := deps.Catalog.TitleLyrics(scope, titleID)
		return lyricfetch.Answer{Lyrics: l, Source: lyricfetch.SourceLocal}, found, err
	}
	d, err := deps.Catalog.GetTitle(scope, titleID)
	if err != nil {
		return lyricfetch.Answer{}, false, err
	}
	if d.Kind != "track" {
		return lyricfetch.Answer{}, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, lyricsFetchTimeout)
	defer cancel()
	return deps.Lyrics.Lyrics(ctx, lyricTrack(deps, d))
}

// lyricTrack is what a Lyric provider is asked about a Track: its artist, its
// display title, its Album, its longest File's duration, and the recording id
// this server holds — the enrichment record's, else the one the file's tags
// assert.
func lyricTrack(deps Deps, d store.TitleDetail) lyricfetch.Track {
	t := lyricfetch.Track{ID: d.ID, Title: d.Title.Title, RecordingID: d.MusicbrainzID}
	if d.EnrichedTitle != "" {
		t.Title = d.EnrichedTitle
	}
	if t.RecordingID == "" {
		t.RecordingID = d.MusicbrainzRecordingID
	}
	if c, err := deps.Catalog.TrackContext(d.ID); err == nil {
		t.Artist, t.Album = c.ArtistName, c.AlbumTitle
	}
	for _, e := range d.Editions {
		for _, f := range e.Files {
			if f.DurationMs > t.DurationMs {
				t.DurationMs = f.DurationMs
			}
		}
	}
	return t
}
