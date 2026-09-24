package webref

import (
	"context"
	"errors"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

type fakeProvider struct {
	refs []pluginapi.WebReference
	err  error
	got  pluginapi.WebReferencesRequest
}

func (f *fakeProvider) Links(_ context.Context, req pluginapi.WebReferencesRequest) (pluginapi.WebReferencesResponse, error) {
	f.got = req
	// A provider scribbling on the ids it was handed must not change what the host
	// checks its answer against.
	req.IDs["imdb"] = "tt0000000"
	return pluginapi.WebReferencesResponse{References: f.refs}, f.err
}

func register(reg *pluginapi.Registry, slug string, kinds []string, p pluginapi.WebReferenceProvider) {
	reg.RegisterWebReferenceProvider(pluginapi.WebReferenceProviderRegistration{
		Descriptor: pluginapi.Descriptor{Slug: slug, Kinds: kinds},
		New:        func(pluginapi.Settings) (pluginapi.WebReferenceProvider, error) { return p, nil },
	})
}

var held = map[string]string{"imdb": "tt1160419", "tmdb": "438631"}

func ref(ns, id, label, url string) pluginapi.WebReference {
	return pluginapi.WebReference{Namespace: ns, ID: id, Label: label, URL: url}
}

// TestCollectKeepsOnlyHTTPSReferencesToHeldIDs is the host's judgment, one rule
// per row: every reference but the first is dropped.
func TestCollectKeepsOnlyHTTPSReferencesToHeldIDs(t *testing.T) {
	good := ref("imdb", "tt1160419", "IMDb", "https://www.imdb.com/title/tt1160419/")
	for _, tc := range []struct {
		name string
		ref  pluginapi.WebReference
	}{
		{"http", ref("imdb", "tt1160419", "IMDb", "http://www.imdb.com/title/tt1160419/")},
		{"no scheme", ref("imdb", "tt1160419", "IMDb", "//www.imdb.com/title/tt1160419/")},
		{"javascript", ref("imdb", "tt1160419", "IMDb", "javascript:alert(1)")},
		{"no host", ref("imdb", "tt1160419", "IMDb", "https:///title")},
		{"credentials", ref("imdb", "tt1160419", "IMDb", "https://www.imdb.com@evil.example/")},
		{"unparseable", ref("imdb", "tt1160419", "IMDb", "https://[::1")},
		{"id not held", ref("imdb", "tt9999999", "IMDb", "https://www.imdb.com/title/tt9999999/")},
		{"namespace not held", ref("trakt", "1", "Trakt", "https://trakt.tv/movies/1")},
		{"no id", ref("imdb", "", "IMDb", "https://www.imdb.com/")},
		{"no label", ref("tmdb", "438631", "", "https://www.themoviedb.org/movie/438631")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := pluginapi.NewRegistry()
			register(reg, "refs", nil, &fakeProvider{refs: []pluginapi.WebReference{good, tc.ref}})
			got := Collect(context.Background(), reg, "movie", held)
			if len(got) != 1 || got[0] != (Reference{Label: "IMDb", URL: good.URL}) {
				t.Fatalf("Collect = %+v, want only %s", got, good.URL)
			}
		})
	}
}

// TestCollectAsksOnlyProvidersServingTheKind, in order, each address once, and
// survives one that fails.
func TestCollectAsksOnlyProvidersServingTheKind(t *testing.T) {
	video := &fakeProvider{refs: []pluginapi.WebReference{
		ref("tmdb", "438631", "TMDB", "https://www.themoviedb.org/movie/438631"),
	}}
	music := &fakeProvider{refs: []pluginapi.WebReference{
		ref("imdb", "tt1160419", "Music?", "https://music.example/"),
	}}
	broken := &fakeProvider{err: errors.New("trap")}
	every := &fakeProvider{refs: []pluginapi.WebReference{
		ref("imdb", "tt1160419", "IMDb", "https://www.imdb.com/title/tt1160419/"),
		ref("tmdb", "438631", "TMDB again", "https://www.themoviedb.org/movie/438631"),
	}}
	reg := pluginapi.NewRegistry()
	register(reg, "video", []string{pluginapi.KindVideo}, video)
	register(reg, "music", []string{pluginapi.KindMusic}, music)
	register(reg, "broken", nil, broken)
	register(reg, "every", nil, every)

	got := Collect(context.Background(), reg, "movie", held)
	want := []Reference{
		{Label: "TMDB", URL: "https://www.themoviedb.org/movie/438631"},
		{Label: "IMDb", URL: "https://www.imdb.com/title/tt1160419/"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Collect = %+v, want %+v", got, want)
	}
	if music.got.Kind != "" {
		t.Fatalf("a music-only provider was asked about a movie: %+v", music.got)
	}
	if video.got.Kind != "movie" || video.got.IDs["tmdb"] != "438631" {
		t.Fatalf("the video provider was asked %+v, want the movie's kind and ids", video.got)
	}
	if held["imdb"] != "tt1160419" {
		t.Fatal("a provider changed the host's own held ids")
	}
}

// TestCollectAsksNobodyForAnItemWithNoIDs: with nothing held there is nothing a
// reference could be keyed to.
func TestCollectAsksNobodyForAnItemWithNoIDs(t *testing.T) {
	p := &fakeProvider{}
	reg := pluginapi.NewRegistry()
	register(reg, "refs", nil, p)
	if got := Collect(context.Background(), reg, "movie", nil); got != nil || p.got.Kind != "" {
		t.Fatalf("Collect = %+v (asked %+v), want nil and no call", got, p.got)
	}
}
