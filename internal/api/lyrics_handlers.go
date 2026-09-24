package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/goozakdev/obelo-server/internal/access"
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
// "source" is "local" or "fetched".

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
		l, source, found, err := titleLyrics(r.Context(), deps, scope, titleID)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to load lyrics", nil)
			return
		}
		if !found {
			writeJSON(w, http.StatusOK, lyricsResponse{})
			return
		}
		out := &lyricsJSON{Kind: string(l.Kind), Source: source, Lines: []lyricLineJSON{}}
		if l.Kind == lyrics.Synced {
			for _, ln := range l.Lines {
				out.Lines = append(out.Lines, lyricLineJSON{StartMs: ln.StartMs, Text: ln.Text})
			}
		} else {
			out.Text = l.Text
		}
		writeJSON(w, http.StatusOK, lyricsResponse{Lyrics: out})
	}
}

// titleLyrics is the lyrics a Title shows and their source. With no Lyric
// provider service wired it is the Local lyrics alone; otherwise a Track is
// resolved through the service, which may ask the providers.
func titleLyrics(ctx context.Context, deps Deps, scope access.Scope, titleID string) (lyrics.Lyrics, string, bool, error) {
	if deps.Lyrics == nil {
		l, found, err := deps.Catalog.TitleLyrics(scope, titleID)
		return l, lyricfetch.SourceLocal, found, err
	}
	d, err := deps.Catalog.GetTitle(scope, titleID)
	if err != nil {
		return lyrics.Lyrics{}, "", false, err
	}
	if d.Kind != "track" {
		return lyrics.Lyrics{}, "", false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, lyricsFetchTimeout)
	defer cancel()
	a, found, err := deps.Lyrics.Lyrics(ctx, lyricTrack(deps, d))
	return a.Lyrics, a.Source, found, err
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
