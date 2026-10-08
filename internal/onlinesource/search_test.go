package onlinesource

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// search() on the source page (ADR-0068; issue 05): the same judgment as a row page,
// and never cached.

func searchProvider() *pagedProvider {
	prov := &pagedProvider{}
	prov.search = func(req pluginapi.OnlineSearchRequest) pluginapi.OnlineSearchResponse {
		return pluginapi.OnlineSearchResponse{Items: []pluginapi.OnlineItem{goodItem("hit-" + req.Query)}}
	}
	return prov
}

func TestTheSameSearchTwiceMakesTwoPluginCalls(t *testing.T) {
	prov := searchProvider()
	s, _ := pagedService(map[string]*pagedProvider{"tube": prov})
	for i := 0; i < 2; i++ {
		items, err := s.Search(context.Background(), "tube", "cats")
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(items); got != "hit-cats" {
			t.Fatalf("search %d = %q, want the Plugin's hit", i, got)
		}
	}
	if n := len(prov.searches); n != 2 {
		t.Fatalf("Plugin search calls = %d, want 2 (a search is never cached)", n)
	}
}

func TestSearchResultsAreCappedAndMalformedItemsDropped(t *testing.T) {
	prov := searchProvider()
	prov.search = func(pluginapi.OnlineSearchRequest) pluginapi.OnlineSearchResponse {
		items := []pluginapi.OnlineItem{
			goodItem("ok"),
			{ID: "bad/id", Title: "No", ThumbnailURL: "https://media.example/x.jpg"},
			{ID: "plain", Title: "No", ThumbnailURL: "http://media.example/x.jpg"},
			{ID: "neg", Title: "No", ThumbnailURL: "https://media.example/x.jpg", DurationMs: -1},
		}
		for j := 0; j < maxPageItems+5; j++ {
			items = append(items, goodItem(fmt.Sprintf("m%d", j)))
		}
		items[0].Title = strings.Repeat("é", maxTitleLen+50)
		return pluginapi.OnlineSearchResponse{Items: items}
	}
	s, _ := pagedService(map[string]*pagedProvider{"tube": prov})
	items, err := s.Search(context.Background(), "tube", "  ")
	if err != nil || len(items) != 0 {
		t.Fatalf("a blank query = %v, %v, want no items and no error", items, err)
	}
	if len(prov.searches) != 0 {
		t.Fatal("a blank query reached the Plugin")
	}
	items, err = s.Search(context.Background(), "tube", "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != maxPageItems {
		t.Fatalf("items = %d, want the cap %d", len(items), maxPageItems)
	}
	if items[0].ID != "ok" || len([]rune(items[0].Title)) != maxTitleLen {
		t.Fatalf("first = %q (%d chars), want ok truncated to %d", items[0].ID, len([]rune(items[0].Title)), maxTitleLen)
	}
	for _, it := range items {
		if it.ID == "bad/id" || it.ID == "plain" || it.ID == "neg" {
			t.Fatalf("malformed item %q survived", it.ID)
		}
	}
	if unresolvable(s, items) != 0 {
		t.Fatal("a search result has no thumbnail address, so its image could not load")
	}
}

func TestTheQueryIsTrimmedAndBounded(t *testing.T) {
	prov := searchProvider()
	s, _ := pagedService(map[string]*pagedProvider{"tube": prov})
	if _, err := s.Search(context.Background(), "tube", "  cats \n"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Search(context.Background(), "tube", strings.Repeat("é", maxQueryLen+50)); err != nil {
		t.Fatal(err)
	}
	if got := prov.searches[0].Query; got != "cats" {
		t.Fatalf("query = %q, want it trimmed", got)
	}
	if got := len([]rune(prov.searches[1].Query)); got != maxQueryLen {
		t.Fatalf("query = %d characters, want it cut to %d", got, maxQueryLen)
	}
}

func TestAFailingSearchIsUnavailableAndAnUnknownSourceIsNoSource(t *testing.T) {
	prov := searchProvider()
	prov.fail = errors.New("boom")
	s, _ := pagedService(map[string]*pagedProvider{"tube": prov})
	if _, err := s.Search(context.Background(), "tube", "cats"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a failing Plugin = %v, want ErrUnavailable", err)
	}
	if _, err := s.Search(context.Background(), "nope", "cats"); !errors.Is(err, ErrNoSource) {
		t.Fatalf("an unknown source = %v, want ErrNoSource", err)
	}
}
