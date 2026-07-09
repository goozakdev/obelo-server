package enrich

import (
	"context"
	"errors"
	"testing"
)

// tmdbMovieResult mimics a TMDB movie lookup: full authoritative metadata —
// identity, overview, content rating, genres, and a poster.
func tmdbMovieResult() TitleMetadata {
	return TitleMetadata{
		Matched:       true,
		Name:          "Dune",
		Overview:      "Paul Atreides leads a desert rebellion.",
		ContentRating: "PG-13",
		Genres:        []string{"Science Fiction", "Adventure"},
		ExternalID:    "tmdb-1",
		Source:        "tmdb",
		Artwork:       []ArtworkRef{{Role: "poster", URL: "https://tmdb/poster.jpg"}},
	}
}

// omdbMovieResult mimics an OMDb supplement result: fill-only text fields (no
// identity), optionally an artwork ref for role-merge tests.
func omdbMovieResult() TitleMetadata {
	return TitleMetadata{
		Matched:       true,
		Source:        "omdb",
		Overview:      "OMDb plot.",
		ContentRating: "R",
		Genres:        []string{"Drama"},
	}
}

func TestVideoChainTMDBAuthoritativeFillsFirst(t *testing.T) {
	// When TMDB already carries every field, OMDb must not overwrite any of them.
	tmdb := &stubProvider{meta: tmdbMovieResult()}
	omdb := &stubProvider{meta: omdbMovieResult()}
	chain := NewVideoChainProvider(tmdb, omdb)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Dune"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Overview != "Paul Atreides leads a desert rebellion." {
		t.Errorf("overview = %q, want TMDB's kept", got.Overview)
	}
	if got.ContentRating != "PG-13" {
		t.Errorf("content rating = %q, want TMDB's PG-13 kept", got.ContentRating)
	}
	if len(got.Genres) != 2 || got.Genres[0] != "Science Fiction" {
		t.Errorf("genres = %v, want TMDB's kept", got.Genres)
	}
	// Identity stays TMDB's.
	if got.Source != "tmdb" || got.ExternalID != "tmdb-1" {
		t.Errorf("identity = %q/%q, want tmdb/tmdb-1", got.Source, got.ExternalID)
	}
}

func TestVideoChainOMDbFillsOnlyEmptyFields(t *testing.T) {
	// TMDB left overview, content rating, and genres empty; OMDb fills exactly those.
	meta := tmdbMovieResult()
	meta.Overview = ""
	meta.ContentRating = ""
	meta.Genres = nil
	tmdb := &stubProvider{meta: meta}
	omdb := &stubProvider{meta: omdbMovieResult()}
	chain := NewVideoChainProvider(tmdb, omdb)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Dune", IMDBID: "tt1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Overview != "OMDb plot." {
		t.Errorf("overview = %q, want the OMDb fill", got.Overview)
	}
	if got.ContentRating != "R" {
		t.Errorf("content rating = %q, want the OMDb fill", got.ContentRating)
	}
	if len(got.Genres) != 1 || got.Genres[0] != "Drama" {
		t.Errorf("genres = %v, want the OMDb fill", got.Genres)
	}
	// Identity/title still TMDB's, and OMDb was fed the movie ref (IMDb id passed through).
	if got.Source != "tmdb" || got.ExternalID != "tmdb-1" {
		t.Errorf("identity disturbed: %q/%q", got.Source, got.ExternalID)
	}
	if omdb.calls != 1 || omdb.last.Kind != "movie" || omdb.last.IMDBID != "tt1" {
		t.Errorf("omdb call wrong: calls=%d last=%+v", omdb.calls, omdb.last)
	}
}

func TestVideoChainMergesSupplementArtworkForMissingRole(t *testing.T) {
	// TMDB has a poster but no background; OMDb's background is merged, its poster
	// does not replace TMDB's (fill-only role-merge).
	tmdb := &stubProvider{meta: tmdbMovieResult()}
	sup := omdbMovieResult()
	sup.Artwork = []ArtworkRef{
		{Role: "poster", URL: "https://omdb/poster.jpg"},
		{Role: "background", URL: "https://omdb/bg.jpg"},
	}
	omdb := &stubProvider{meta: sup}
	chain := NewVideoChainProvider(tmdb, omdb)

	got, _ := chain.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Dune"})
	if len(got.Artwork) != 2 {
		t.Fatalf("artwork = %+v, want poster + background", got.Artwork)
	}
	if got.Artwork[0].Role != "poster" || got.Artwork[0].URL != "https://tmdb/poster.jpg" {
		t.Errorf("poster = %+v, want TMDB's kept", got.Artwork[0])
	}
	if got.Artwork[1].Role != "background" || got.Artwork[1].URL != "https://omdb/bg.jpg" {
		t.Errorf("background = %+v, want OMDb's merged", got.Artwork[1])
	}
}

func TestVideoChainSupplementErrorIsNonFatal(t *testing.T) {
	meta := tmdbMovieResult()
	meta.Overview = "" // a gap OMDb would have filled, but it errors
	tmdb := &stubProvider{meta: meta}
	omdb := &stubProvider{err: errors.New("omdb timeout")}
	chain := NewVideoChainProvider(tmdb, omdb)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Dune"})
	if err != nil {
		t.Fatalf("an OMDb error must not fail the lookup; got %v", err)
	}
	// The TMDB result survives intact (just without the gap filled).
	if got.ExternalID != "tmdb-1" || got.ContentRating != "PG-13" || got.Overview != "" {
		t.Errorf("TMDB result not preserved: %+v", got)
	}
}

func TestVideoChainSupplementNoMatchIsNonFatal(t *testing.T) {
	meta := tmdbMovieResult()
	meta.Overview = ""
	tmdb := &stubProvider{meta: meta}
	omdb := &stubProvider{err: ErrNoMatch}
	chain := NewVideoChainProvider(tmdb, omdb)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Dune"})
	if err != nil {
		t.Fatalf("an OMDb no-match must not fail the lookup; got %v", err)
	}
	if got.ExternalID != "tmdb-1" || got.Overview != "" {
		t.Errorf("TMDB result not preserved: %+v", got)
	}
}

func TestVideoChainSupplementNotServingKindPassesThrough(t *testing.T) {
	// A supplement that doesn't serve this video kind no-matches; the chain
	// consults it but the TMDB result passes through untouched. (OMDb serves movies,
	// so a show no-matches it; TheTVDB serves TV, so a movie no-matches it.)
	showMeta := TitleMetadata{Matched: true, Source: "tmdb", ExternalID: "tv-1", Name: "Show", Overview: "TMDB overview"}
	tmdb := &stubProvider{meta: showMeta}
	sup := &stubProvider{err: ErrNoMatch} // mimics a movie-only supplement on a show
	chain := NewVideoChainProvider(tmdb, sup)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "show", Title: "Show"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.ExternalID != "tv-1" || got.Source != "tmdb" || got.Name != "Show" || got.Overview != "TMDB overview" {
		t.Errorf("show result disturbed by a no-matching supplement: %+v", got)
	}
}

// tvdbEpisodeResult mimics a TheTVDB episode supplement: a display-only Name
// override plus an overview and a still, but no identity fields.
func tvdbEpisodeResult() TitleMetadata {
	return TitleMetadata{
		Matched:  true,
		Source:   "thetvdb",
		Name:     "The Wolf and the Lion",
		Overview: "Ned uncovers the truth.",
		Genres:   []string{"Drama"},
		Artwork:  []ArtworkRef{{Role: "poster", URL: "https://thetvdb/e5.jpg"}},
	}
}

func TestVideoChainTheTVDBFillsEmptyEpisodeFields(t *testing.T) {
	// TMDB matched the episode but left every descriptive field empty (the "S01E05"
	// case). TheTVDB fills the Name (display-only override), Overview, Genres, and
	// the still — all fill-only.
	tmdb := &stubProvider{meta: TitleMetadata{Matched: true, Source: "tmdb", ExternalID: "tmdb-ep-9"}}
	tvdb := &stubProvider{meta: tvdbEpisodeResult()}
	chain := NewVideoChainProvider(tmdb, tvdb)

	ref := TitleRef{Kind: "episode", Title: "Game of Thrones", SeasonNumber: 1, EpisodeNumber: 5, TheTVDBID: "121361"}
	got, err := chain.Lookup(context.Background(), ref)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Name != "The Wolf and the Lion" {
		t.Errorf("name = %q, want the TheTVDB display override", got.Name)
	}
	if got.Overview != "Ned uncovers the truth." {
		t.Errorf("overview = %q, want the TheTVDB fill", got.Overview)
	}
	if len(got.Genres) != 1 || got.Genres[0] != "Drama" {
		t.Errorf("genres = %v, want the TheTVDB fill", got.Genres)
	}
	if len(got.Artwork) != 1 || got.Artwork[0].URL != "https://thetvdb/e5.jpg" {
		t.Errorf("artwork = %+v, want the TheTVDB still", got.Artwork)
	}
	// Identity stays TMDB's — the Name override is display-only, and the ref the
	// supplement was fed is unchanged (never re-keyed).
	if got.Source != "tmdb" || got.ExternalID != "tmdb-ep-9" {
		t.Errorf("identity disturbed: %q/%q, want tmdb/tmdb-ep-9", got.Source, got.ExternalID)
	}
	if tvdb.last.TheTVDBID != "121361" || tvdb.last.SeasonNumber != 1 || tvdb.last.EpisodeNumber != 5 {
		t.Errorf("supplement fed a mutated ref: %+v", tvdb.last)
	}
}

func TestVideoChainTheTVDBNeverOverwritesEpisodeName(t *testing.T) {
	// TMDB already has the episode Name/Overview; TheTVDB must not overwrite either
	// (fill-only), and identity is untouched.
	tmdbEp := TitleMetadata{Matched: true, Source: "tmdb", ExternalID: "tmdb-ep-9", Name: "TMDB Title", Overview: "TMDB overview"}
	tmdb := &stubProvider{meta: tmdbEp}
	tvdb := &stubProvider{meta: tvdbEpisodeResult()}
	chain := NewVideoChainProvider(tmdb, tvdb)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "episode", Title: "x", SeasonNumber: 1, EpisodeNumber: 5})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Name != "TMDB Title" || got.Overview != "TMDB overview" {
		t.Errorf("TheTVDB overwrote a TMDB-set field: name=%q overview=%q", got.Name, got.Overview)
	}
	if got.ExternalID != "tmdb-ep-9" {
		t.Errorf("identity disturbed: %q", got.ExternalID)
	}
}

func TestVideoChainTheTVDBErrorSwallowedTMDBSurvives(t *testing.T) {
	// A TheTVDB error must not fail the episode lookup; the TMDB result survives.
	tmdbEp := TitleMetadata{Matched: true, Source: "tmdb", ExternalID: "tmdb-ep-9", Name: "S01E05"}
	tmdb := &stubProvider{meta: tmdbEp}
	tvdb := &stubProvider{err: errors.New("thetvdb timeout")}
	chain := NewVideoChainProvider(tmdb, tvdb)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "episode", Title: "x", SeasonNumber: 1, EpisodeNumber: 5})
	if err != nil {
		t.Fatalf("a TheTVDB error must not fail the lookup; got %v", err)
	}
	if got.ExternalID != "tmdb-ep-9" || got.Name != "S01E05" {
		t.Errorf("TMDB result not preserved: %+v", got)
	}
}

// fanartVideoResult mimics a fanart.tv video supplement: artwork-only (poster +
// background), no identity or text fields.
func fanartVideoResult() TitleMetadata {
	return TitleMetadata{
		Matched: true,
		Source:  "fanart.tv",
		Artwork: []ArtworkRef{
			{Role: "poster", URL: "https://fanart/poster.jpg"},
			{Role: "background", URL: "https://fanart/bg.jpg"},
		},
	}
}

func TestVideoChainFanartFillsOnlyEmptyArtworkRoles(t *testing.T) {
	// TMDB has a poster but no background; fanart.tv's background is merged, its poster
	// does not replace TMDB's (fill-only role-merge). No text fields are touched.
	tmdb := &stubProvider{meta: tmdbMovieResult()} // poster only
	fanart := &stubProvider{meta: fanartVideoResult()}
	chain := NewVideoChainProvider(tmdb, fanart)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Dune", TMDBID: "1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(got.Artwork) != 2 {
		t.Fatalf("artwork = %+v, want poster + background", got.Artwork)
	}
	if got.Artwork[0].Role != "poster" || got.Artwork[0].URL != "https://tmdb/poster.jpg" {
		t.Errorf("poster = %+v, want TMDB's kept (fanart never overwrites)", got.Artwork[0])
	}
	if got.Artwork[1].Role != "background" || got.Artwork[1].URL != "https://fanart/bg.jpg" {
		t.Errorf("background = %+v, want fanart.tv's merged", got.Artwork[1])
	}
	// Identity/text stays TMDB's.
	if got.Source != "tmdb" || got.ExternalID != "tmdb-1" || got.Overview != "Paul Atreides leads a desert rebellion." {
		t.Errorf("fanart.tv disturbed a TMDB field: %+v", got)
	}
}

func TestVideoChainFanartErrorSwallowedTMDBSurvives(t *testing.T) {
	// A fanart.tv error must not fail the lookup; the TMDB result (poster included)
	// survives, just without the missing background filled.
	tmdb := &stubProvider{meta: tmdbMovieResult()} // poster only, no background
	fanart := &stubProvider{err: errors.New("fanart.tv timeout")}
	chain := NewVideoChainProvider(tmdb, fanart)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Dune", TMDBID: "1"})
	if err != nil {
		t.Fatalf("a fanart.tv error must not fail the lookup; got %v", err)
	}
	if got.ExternalID != "tmdb-1" || len(got.Artwork) != 1 || got.Artwork[0].URL != "https://tmdb/poster.jpg" {
		t.Errorf("TMDB result not preserved: %+v", got)
	}
}

func TestVideoChainTMDBNoMatchPropagates(t *testing.T) {
	tmdb := &stubProvider{err: ErrNoMatch}
	omdb := &stubProvider{}
	chain := NewVideoChainProvider(tmdb, omdb)

	if _, err := chain.Lookup(context.Background(), TitleRef{Kind: "movie"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch propagated from TMDB", err)
	}
	if omdb.calls != 0 {
		t.Errorf("OMDb called after TMDB no-match (%d); want 0", omdb.calls)
	}
}
