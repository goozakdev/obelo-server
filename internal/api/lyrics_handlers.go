package api

import (
	"errors"
	"net/http"

	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/lyrics"
)

// Lyrics. One GET leaf on the title subtree, open to ANY authenticated User —
// Members included, because reading along is part of listening:
//
//	GET /titles/{id}/lyrics → { "lyrics": null | { "kind", "source", "lines", "text" } }
//
// "lyrics" is null when the Title has none, which is an empty state and never an
// error. A Synced answer carries "lines" ([{ "startMs", "text" }], "text" empty);
// a Plain one carries "text" ("lines" empty).

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
		l, found, err := deps.Catalog.TitleLyrics(scope, titleID)
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
		out := &lyricsJSON{Kind: string(l.Kind), Source: "local", Lines: []lyricLineJSON{}}
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
