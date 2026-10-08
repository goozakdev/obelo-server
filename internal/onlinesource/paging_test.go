package onlinesource

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Row paging, the caps, the malformed-entry drops and the 5-minute cache of an
// Online source's answers (ADR-0068 decisions 2, 4 and 14; issue 04).

// pagedProvider answers rows() and row() from the functions a test sets and counts
// the calls it receives.
type pagedProvider struct {
	mu        sync.Mutex
	rows      func() []pluginapi.OnlineRow
	row       func(req pluginapi.OnlineRowRequest) pluginapi.OnlineRowResponse
	fail      error
	variants  []pluginapi.OnlineVariant
	rowsCalls int
	rowCalls  []pluginapi.OnlineRowRequest
}

func (p *pagedProvider) Rows(context.Context, pluginapi.OnlineRowsRequest) (pluginapi.OnlineRowsResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rowsCalls++
	if p.fail != nil {
		return pluginapi.OnlineRowsResponse{}, p.fail
	}
	return pluginapi.OnlineRowsResponse{Rows: p.rows()}, nil
}

func (p *pagedProvider) Row(_ context.Context, req pluginapi.OnlineRowRequest) (pluginapi.OnlineRowResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rowCalls = append(p.rowCalls, req)
	if p.fail != nil {
		return pluginapi.OnlineRowResponse{}, p.fail
	}
	return p.row(req), nil
}

func (p *pagedProvider) Resolve(context.Context, pluginapi.OnlineResolveRequest) (pluginapi.OnlineResolveResponse, error) {
	return pluginapi.OnlineResolveResponse{Variants: p.variants}, nil
}

func (p *pagedProvider) calls() (rows, row int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rowsCalls, len(p.rowCalls)
}

// pagedService is a Service over the given sources (slug → provider) on a clock the
// test moves.
func pagedService(providers map[string]*pagedProvider) (*Service, *time.Time) {
	reg := pluginapi.NewRegistry()
	for slug, prov := range providers {
		prov := prov
		reg.RegisterOnlineSourceProvider(pluginapi.OnlineSourceProviderRegistration{
			Descriptor: pluginapi.Descriptor{Slug: slug, Name: slug},
			New:        func(pluginapi.Settings) (pluginapi.OnlineSourceProvider, error) { return prov, nil },
		})
	}
	s := New(reg, nil)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, &now
}

func goodItem(id string) pluginapi.OnlineItem {
	return pluginapi.OnlineItem{ID: id, Title: "Title " + id, ThumbnailURL: "https://media.example/" + id + ".jpg", DurationMs: 1000}
}

func ids(items []Item) string {
	var out []string
	for _, it := range items {
		out = append(out, it.ID)
	}
	return strings.Join(out, ",")
}

// TestOverCapRowsItemsAndTextAreTruncatedNotDropped: a Plugin answering more rows,
// more items or longer text than the caps is cut to them, and the entry is kept.
func TestOverCapRowsItemsAndTextAreTruncatedNotDropped(t *testing.T) {
	long := func(n int) string { return strings.Repeat("é", n) } // multi-byte, so a byte cut would split one
	prov := &pagedProvider{}
	prov.rows = func() []pluginapi.OnlineRow {
		var rows []pluginapi.OnlineRow
		for i := 0; i < maxRows+5; i++ {
			row := pluginapi.OnlineRow{ID: fmt.Sprintf("r%d", i), Label: "Row"}
			for j := 0; j < maxItemsPerRow+5; j++ {
				row.Items = append(row.Items, goodItem(fmt.Sprintf("i%d", j)))
			}
			rows = append(rows, row)
		}
		rows[0].Label = long(maxLabelLen + 50)
		rows[0].Items[0].Title = long(maxTitleLen + 50)
		rows[0].Items[0].Description = long(maxDescLen + 50)
		return rows
	}
	prov.row = func(pluginapi.OnlineRowRequest) pluginapi.OnlineRowResponse {
		var items []pluginapi.OnlineItem
		for j := 0; j < maxPageItems+5; j++ {
			items = append(items, goodItem(fmt.Sprintf("p%d", j)))
		}
		items[0].Title = long(maxTitleLen + 50)
		items[0].Description = long(maxDescLen + 50)
		return pluginapi.OnlineRowResponse{Items: items}
	}
	s, _ := pagedService(map[string]*pagedProvider{"tube": prov})

	rows, err := s.Rows(context.Background(), "tube")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != maxRows {
		t.Fatalf("rows = %d, want the cap %d", len(rows), maxRows)
	}
	if got := len(rows[0].Items); got != maxItemsPerRow {
		t.Fatalf("items in a row = %d, want the cap %d", got, maxItemsPerRow)
	}
	if got := []rune(rows[0].Label); len(got) != maxLabelLen {
		t.Fatalf("label = %d characters, want it truncated to %d (and kept)", len(got), maxLabelLen)
	}
	if got := []rune(rows[0].Items[0].Title); len(got) != maxTitleLen {
		t.Fatalf("title = %d characters, want %d", len(got), maxTitleLen)
	}
	if got := []rune(rows[0].Items[0].Description); len(got) != maxDescLen {
		t.Fatalf("description = %d characters, want %d", len(got), maxDescLen)
	}

	page, err := s.Row(context.Background(), "tube", "r0", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != maxPageItems {
		t.Fatalf("items in a page = %d, want the cap %d", len(page.Items), maxPageItems)
	}
	if got := []rune(page.Items[0].Title); len(got) != maxTitleLen {
		t.Fatalf("page title = %d characters, want %d", len(got), maxTitleLen)
	}
	if got := []rune(page.Items[0].Description); len(got) != maxDescLen {
		t.Fatalf("page description = %d characters, want %d", len(got), maxDescLen)
	}
}

// TestMalformedItemsAndRowsAreDroppedAndTheirSiblingsSurvive: every kind of
// malformed entry is dropped, a very long duration is not malformed, and the call
// still succeeds, for rows() and for row() alike.
func TestMalformedItemsAndRowsAreDroppedAndTheirSiblingsSurvive(t *testing.T) {
	with := func(mod func(*pluginapi.OnlineItem)) pluginapi.OnlineItem {
		it := goodItem("bad")
		mod(&it)
		return it
	}
	hours48 := pluginapi.OnlineItem{ID: "long", Title: "Two days", ThumbnailURL: "https://media.example/l.jpg", DurationMs: 48 * 3600 * 1000}
	items := func() []pluginapi.OnlineItem {
		return []pluginapi.OnlineItem{
			goodItem("ok1"),
			with(func(i *pluginapi.OnlineItem) { i.ThumbnailURL = "http://media.example/x.jpg" }),
			with(func(i *pluginapi.OnlineItem) { i.DurationMs = -1 }),
			with(func(i *pluginapi.OnlineItem) { i.ID = "has/slash" }),
			with(func(i *pluginapi.OnlineItem) { i.ID = "has space" }),
			with(func(i *pluginapi.OnlineItem) { i.ID = "" }),
			with(func(i *pluginapi.OnlineItem) { i.Title = "  " }),
			with(func(i *pluginapi.OnlineItem) { i.PublishedAt = "last tuesday" }),
			with(func(i *pluginapi.OnlineItem) { i.PublishedAt = "2026-09-01" }),
			goodItem("ok1"), // a duplicate id within the row
			hours48,
			with(func(i *pluginapi.OnlineItem) { i.ID = "dated"; i.PublishedAt = "2026-09-01T10:00:00+02:00" }),
		}
	}
	prov := &pagedProvider{}
	prov.rows = func() []pluginapi.OnlineRow {
		return []pluginapi.OnlineRow{
			{ID: "good", Label: "Good", Items: items()},
			{ID: "bad/row", Label: "Slash", Items: []pluginapi.OnlineItem{goodItem("x")}},
			{ID: "", Label: "No id", Items: []pluginapi.OnlineItem{goodItem("x")}},
			{ID: "nolabel", Label: "", Items: []pluginapi.OnlineItem{goodItem("x")}},
			{ID: "good", Label: "Dup row id", Items: []pluginapi.OnlineItem{goodItem("x")}},
			{ID: "after", Label: "After", Items: []pluginapi.OnlineItem{goodItem("y")}},
		}
	}
	prov.row = func(pluginapi.OnlineRowRequest) pluginapi.OnlineRowResponse {
		return pluginapi.OnlineRowResponse{Items: items()}
	}
	s, _ := pagedService(map[string]*pagedProvider{"tube": prov})

	rows, err := s.Rows(context.Background(), "tube")
	if err != nil {
		t.Fatalf("rows() errored because of bad siblings: %v", err)
	}
	var rowIDs []string
	for _, r := range rows {
		rowIDs = append(rowIDs, r.ID)
	}
	if got := strings.Join(rowIDs, ","); got != "good,after" {
		t.Fatalf("row ids = %q, want %q (a row with an unsafe or empty id, no label, or a repeated id is dropped)", got, "good,after")
	}
	const want = "ok1,long,dated"
	if got := ids(rows[0].Items); got != want {
		t.Fatalf("rows() items = %q, want %q", got, want)
	}
	page, err := s.Row(context.Background(), "tube", "good", "c1")
	if err != nil {
		t.Fatalf("row() errored because of bad siblings: %v", err)
	}
	if got := ids(page.Items); got != want {
		t.Fatalf("row() items = %q, want %q", got, want)
	}
	if got := page.Items[1].DurationMs; got != 48*3600*1000 {
		t.Fatalf("a 48 hour duration came back as %d ms; there is no upper cap", got)
	}
}

// TestRowPagesForwardByTheCursorTheSourceNamed: row() is handed the row id and the
// cursor, a page names the next cursor, and the last page names none.
func TestRowPagesForwardByTheCursorTheSourceNamed(t *testing.T) {
	prov := &pagedProvider{}
	prov.rows = func() []pluginapi.OnlineRow {
		return []pluginapi.OnlineRow{{ID: "recent", Label: "Recent", Items: []pluginapi.OnlineItem{goodItem("a")}, NextCursor: "c1"}}
	}
	prov.row = func(req pluginapi.OnlineRowRequest) pluginapi.OnlineRowResponse {
		switch req.Cursor {
		case "c1":
			return pluginapi.OnlineRowResponse{Items: []pluginapi.OnlineItem{goodItem("b")}, NextCursor: "c2"}
		case "c2":
			return pluginapi.OnlineRowResponse{Items: []pluginapi.OnlineItem{goodItem("c")}}
		}
		return pluginapi.OnlineRowResponse{}
	}
	s, _ := pagedService(map[string]*pagedProvider{"tube": prov})

	rows, _ := s.Rows(context.Background(), "tube")
	if rows[0].NextCursor != "c1" {
		t.Fatalf("rows() nextCursor = %q, want c1", rows[0].NextCursor)
	}
	p1, err := s.Row(context.Background(), "tube", "recent", rows[0].NextCursor)
	if err != nil || ids(p1.Items) != "b" || p1.NextCursor != "c2" {
		t.Fatalf("page 1 = %+v, %v; want item b and cursor c2", p1, err)
	}
	p2, err := s.Row(context.Background(), "tube", "recent", p1.NextCursor)
	if err != nil || ids(p2.Items) != "c" || p2.NextCursor != "" {
		t.Fatalf("page 2 = %+v, %v; want item c and no cursor", p2, err)
	}
	if got := prov.rowCalls; len(got) != 2 || got[0].RowID != "recent" || got[0].Cursor != "c1" || got[1].Cursor != "c2" {
		t.Fatalf("the Plugin was asked %+v, want recent/c1 then recent/c2", got)
	}
}

// TestACursorOrRowIdTheHostWillNotAcceptNeverReachesThePlugin: a row id outside the
// URL-safe set, an empty cursor and an over-long cursor are refused (404 to a
// client) before any Plugin call; a Plugin's own over-long cursor ends the paging.
func TestACursorOrRowIdTheHostWillNotAcceptNeverReachesThePlugin(t *testing.T) {
	prov := &pagedProvider{}
	prov.rows = func() []pluginapi.OnlineRow {
		return []pluginapi.OnlineRow{{ID: "r", Label: "R", NextCursor: strings.Repeat("x", maxCursorLen+1)}}
	}
	prov.row = func(pluginapi.OnlineRowRequest) pluginapi.OnlineRowResponse {
		return pluginapi.OnlineRowResponse{NextCursor: strings.Repeat("x", maxCursorLen+1)}
	}
	s, _ := pagedService(map[string]*pagedProvider{"tube": prov})
	for name, args := range map[string][2]string{
		"unsafe row id": {"a/b", "c1"}, "empty cursor": {"r", ""}, "long cursor": {"r", strings.Repeat("x", maxCursorLen+1)},
	} {
		if _, err := s.Row(context.Background(), "tube", args[0], args[1]); !errors.Is(err, ErrNoItem) {
			t.Errorf("%s: err = %v, want ErrNoItem", name, err)
		}
	}
	if _, n := prov.calls(); n != 0 {
		t.Fatalf("refused requests caused %d Plugin calls, want 0", n)
	}
	rows, _ := s.Rows(context.Background(), "tube")
	if rows[0].NextCursor != "" {
		t.Fatalf("a %d-byte Plugin cursor was passed on; want it dropped", maxCursorLen+1)
	}
	page, err := s.Row(context.Background(), "tube", "r", "ok")
	if err != nil || page.NextCursor != "" {
		t.Fatalf("page = %+v, %v; want an over-long next cursor dropped", page, err)
	}
}

// TestRowsAndRowAnswersAreCachedFiveMinutesPerSource: two callers inside the window
// cause one Plugin call (for rows() and for each row() cursor), the window expires
// on the clock, a failure is not cached, and a new Service starts empty.
func TestRowsAndRowAnswersAreCachedFiveMinutesPerSource(t *testing.T) {
	prov := &pagedProvider{}
	prov.rows = func() []pluginapi.OnlineRow {
		return []pluginapi.OnlineRow{{ID: "r", Label: "R", Items: []pluginapi.OnlineItem{goodItem("a")}, NextCursor: "c1"}}
	}
	prov.row = func(req pluginapi.OnlineRowRequest) pluginapi.OnlineRowResponse {
		return pluginapi.OnlineRowResponse{Items: []pluginapi.OnlineItem{goodItem("p-" + req.Cursor)}}
	}
	provs := map[string]*pagedProvider{"tube": prov}
	s, now := pagedService(provs)
	ctx := context.Background()

	for i := 0; i < 2; i++ { // two Users
		if _, err := s.Rows(ctx, "tube"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Row(ctx, "tube", "r", "c1"); err != nil {
			t.Fatal(err)
		}
	}
	if rows, row := prov.calls(); rows != 1 || row != 1 {
		t.Fatalf("two Users caused %d rows() and %d row() calls, want 1 and 1", rows, row)
	}
	if _, err := s.Row(ctx, "tube", "r", "c2"); err != nil { // another cursor is another answer
		t.Fatal(err)
	}
	if _, row := prov.calls(); row != 2 {
		t.Fatalf("a different cursor made %d row() calls in all, want 2", row)
	}

	*now = now.Add(cacheTTL - time.Second)
	s.Rows(ctx, "tube")
	if rows, _ := prov.calls(); rows != 1 {
		t.Fatalf("just inside the window: %d rows() calls, want 1", rows)
	}
	*now = now.Add(2 * time.Second)
	s.Rows(ctx, "tube")
	s.Row(ctx, "tube", "r", "c1")
	if rows, row := prov.calls(); rows != 2 || row != 3 {
		t.Fatalf("after 5 minutes: %d rows() and %d row() calls, want 2 and 3", rows, row)
	}

	prov.fail = errors.New("boom")
	*now = now.Add(cacheTTL + time.Second)
	if _, err := s.Rows(ctx, "tube"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	prov.fail = nil
	if _, err := s.Rows(ctx, "tube"); err != nil {
		t.Fatalf("a failure was cached: %v", err)
	}

	fresh, _ := pagedService(provs) // a restart
	before, _ := prov.calls()
	fresh.Rows(ctx, "tube")
	if after, _ := prov.calls(); after != before+1 {
		t.Fatalf("a new Service answered from a cache it cannot have: %d calls, want %d", after, before+1)
	}
}

// TestTheCacheIsKeyedPerSource: the same row id and cursor on two sources are two
// answers, so one source's rows can never be served as another's.
func TestTheCacheIsKeyedPerSource(t *testing.T) {
	mk := func(tag string) *pagedProvider {
		p := &pagedProvider{}
		p.rows = func() []pluginapi.OnlineRow {
			return []pluginapi.OnlineRow{{ID: "r", Label: tag, Items: []pluginapi.OnlineItem{goodItem(tag)}}}
		}
		p.row = func(pluginapi.OnlineRowRequest) pluginapi.OnlineRowResponse {
			return pluginapi.OnlineRowResponse{Items: []pluginapi.OnlineItem{goodItem("page-" + tag)}}
		}
		return p
	}
	s, _ := pagedService(map[string]*pagedProvider{"alpha": mk("alpha"), "beta": mk("beta")})
	ctx := context.Background()
	for _, src := range []string{"alpha", "beta", "alpha", "beta"} {
		rows, err := s.Rows(ctx, src)
		if err != nil || rows[0].Label != src || ids(rows[0].Items) != src {
			t.Fatalf("%s rows = %+v, %v; want its own", src, rows, err)
		}
		page, err := s.Row(ctx, src, "r", "c")
		if err != nil || ids(page.Items) != "page-"+src {
			t.Fatalf("%s page = %+v, %v; want its own", src, page, err)
		}
	}
}

// thumbService is a Service over a source whose every row() page names maxPageItems
// items of its own.
func thumbService() (*Service, *time.Time) {
	prov := &pagedProvider{}
	prov.rows = func() []pluginapi.OnlineRow {
		return []pluginapi.OnlineRow{{ID: "r", Label: "R", Items: []pluginapi.OnlineItem{goodItem("first")}}}
	}
	prov.row = func(req pluginapi.OnlineRowRequest) pluginapi.OnlineRowResponse {
		var items []pluginapi.OnlineItem
		for j := 0; j < maxPageItems; j++ {
			items = append(items, goodItem(fmt.Sprintf("%s-%d", req.Cursor, j)))
		}
		return pluginapi.OnlineRowResponse{Items: items}
	}
	return pagedService(map[string]*pagedProvider{"tube": prov})
}

func thumbCount(s *Service) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.thumbs["tube"] == nil {
		return 0
	}
	return len(s.thumbs["tube"].items)
}

// unresolvable counts the items among rows that the thumbnail proxy has no address for.
func unresolvable(s *Service, items ...[]Item) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, list := range items {
		for _, it := range list {
			if l := s.thumbs["tube"]; l == nil || l.items[it.ID] == nil {
				n++
			}
		}
	}
	return n
}

// TestAFullSizeRowsAnswerKeepsEveryThumbnail: the largest answer the caps allow (50
// rows of 100 items) must have every one of its thumbnails resolvable.
func TestAFullSizeRowsAnswerKeepsEveryThumbnail(t *testing.T) {
	prov := &pagedProvider{rows: func() []pluginapi.OnlineRow {
		var rs []pluginapi.OnlineRow
		for r := 0; r < maxRows; r++ {
			row := pluginapi.OnlineRow{ID: fmt.Sprintf("r%d", r), Label: "L"}
			for i := 0; i < maxItemsPerRow; i++ {
				row.Items = append(row.Items, goodItem(fmt.Sprintf("r%d-i%d", r, i)))
			}
			rs = append(rs, row)
		}
		return rs
	}}
	s, _ := pagedService(map[string]*pagedProvider{"tube": prov})
	rows, err := s.Rows(context.Background(), "tube")
	if err != nil {
		t.Fatal(err)
	}
	var all [][]Item
	for _, r := range rows {
		all = append(all, r.Items)
	}
	if n := unresolvable(s, all...); n != 0 {
		t.Fatalf("%d of %d thumbnails of one full answer are unresolvable", n, maxRows*maxItemsPerRow)
	}
}

// TestACachedAnswerServedAfterHeavyPagingStillResolvesItsThumbnails: serving an
// answer from the cache refreshes its thumbnails, so paging far past the cap cannot
// leave a still-cached page showing images the proxy has forgotten.
func TestACachedAnswerServedAfterHeavyPagingStillResolvesItsThumbnails(t *testing.T) {
	prov := &pagedProvider{
		rows: func() []pluginapi.OnlineRow {
			row := pluginapi.OnlineRow{ID: "r", Label: "L", NextCursor: "c"}
			for i := 0; i < 20; i++ {
				row.Items = append(row.Items, goodItem(fmt.Sprintf("home%d", i)))
			}
			return []pluginapi.OnlineRow{row}
		},
		row: func(req pluginapi.OnlineRowRequest) pluginapi.OnlineRowResponse {
			var out pluginapi.OnlineRowResponse
			for i := 0; i < maxPageItems; i++ {
				out.Items = append(out.Items, goodItem(fmt.Sprintf("%s-%d", req.Cursor, i)))
			}
			return out
		},
	}
	s, now := pagedService(map[string]*pagedProvider{"tube": prov})
	ctx := context.Background()
	s.Rows(ctx, "tube")
	for i := 0; i < maxCacheEntries-1 && i < 4*maxThumbsPerSource/maxPageItems; i++ {
		if _, err := s.Row(ctx, "tube", "r", fmt.Sprintf("p%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(time.Minute) // the rows cache entry is still valid
	rows, err := s.Rows(ctx, "tube")
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := prov.calls(); r != 1 {
		t.Fatalf("rows() Plugin calls = %d, want the cached answer (1)", r)
	}
	if n := unresolvable(s, rows[0].Items); n != 0 {
		t.Fatalf("%d of the cached page's %d thumbnails are unresolvable", n, len(rows[0].Items))
	}
}

// TestAThumbnailOutlivesTheRowsCache: a page left open past the 5 minute rows cache
// must still be able to load its thumbnails.
func TestAThumbnailOutlivesTheRowsCache(t *testing.T) {
	s, now := thumbService()
	ctx := context.Background()
	s.Rows(ctx, "tube")
	*now = now.Add(6 * time.Minute)
	if _, _, err := s.Thumbnail(ctx, "tube", "first"); errors.Is(err, ErrNoItem) {
		t.Fatal("a thumbnail named by a page fetched 6 minutes ago no longer resolves")
	}
}

// TestTheThumbnailMapIsBoundedBySizeAndKeepsWhatIsUsed: paging through many distinct
// cursors never grows the map past its cap, and the entry that keeps being used
// survives the eviction of the idle ones.
func TestTheThumbnailMapIsBoundedBySizeAndKeepsWhatIsUsed(t *testing.T) {
	s, _ := thumbService()
	ctx := context.Background()
	s.Rows(ctx, "tube")
	for i := 0; i < maxThumbsPerSource/maxPageItems+30; i++ {
		s.Thumbnail(ctx, "tube", "first") // in use: refreshed on access
		if _, err := s.Row(ctx, "tube", "r", fmt.Sprintf("c%d", i)); err != nil {
			t.Fatal(err)
		}
		if got := thumbCount(s); got > maxThumbsPerSource {
			t.Fatalf("after %d pages the thumbnail map holds %d entries, cap %d", i+1, got, maxThumbsPerSource)
		}
	}
	if got := thumbCount(s); got != maxThumbsPerSource {
		t.Fatalf("thumbnail entries = %d, want the map full at its cap %d", got, maxThumbsPerSource)
	}
	if _, _, err := s.Thumbnail(ctx, "tube", "first"); errors.Is(err, ErrNoItem) {
		t.Fatal("the thumbnail in constant use was evicted")
	}
	if _, _, err := s.Thumbnail(ctx, "tube", "c0-0"); !errors.Is(err, ErrNoItem) {
		t.Fatalf("the oldest idle thumbnail: err = %v, want it evicted (ErrNoItem)", err)
	}
}

// TestClearCacheForgetsASourcesRowsOnly: after a source's settings change or its
// Plugin is replaced, its next page is asked of the Plugin afresh; another source's
// cache is untouched.
func TestClearCacheForgetsASourcesRowsOnly(t *testing.T) {
	mk := func() *pagedProvider {
		p := &pagedProvider{}
		p.rows = func() []pluginapi.OnlineRow { return []pluginapi.OnlineRow{{ID: "r", Label: "R"}} }
		p.row = func(pluginapi.OnlineRowRequest) pluginapi.OnlineRowResponse { return pluginapi.OnlineRowResponse{} }
		return p
	}
	a, b := mk(), mk()
	s, _ := pagedService(map[string]*pagedProvider{"a": a, "b": b})
	ctx := context.Background()
	for _, src := range []string{"a", "b"} {
		s.Rows(ctx, src)
		s.Row(ctx, src, "r", "c")
	}
	s.ClearCache("a")
	for _, src := range []string{"a", "b"} {
		s.Rows(ctx, src)
		s.Row(ctx, src, "r", "c")
	}
	if r, p := a.calls(); r != 2 || p != 2 {
		t.Fatalf("source a: %d rows() and %d row() calls, want 2 and 2 after its cache was cleared", r, p)
	}
	if r, p := b.calls(); r != 1 || p != 1 {
		t.Fatalf("source b: %d rows() and %d row() calls, want 1 and 1 (not cleared)", r, p)
	}
	s.ClearCache("")
	s.Rows(ctx, "b")
	if r, _ := b.calls(); r != 2 {
		t.Fatalf("ClearCache(\"\") left source b cached: %d rows() calls, want 2", r)
	}
}

// TestSessionTitlesShareTheThumbnailBoundAndSurviveACachedPage: the item → title map
// that labels a session is held to the same per-source bound as the thumbnails, and
// a page served from the cache refreshes it, so an item on a still-cached page is
// labelled with its title after heavy paging.
func TestSessionTitlesShareTheThumbnailBoundAndSurviveACachedPage(t *testing.T) {
	prov := &pagedProvider{
		variants: []pluginapi.OnlineVariant{{
			URL: "https://cdn.example.test/v.mp4", Container: "mp4", Codecs: []string{"h264", "aac"}, Resolution: "720p",
		}},
		rows: func() []pluginapi.OnlineRow {
			return []pluginapi.OnlineRow{{ID: "r", Label: "L", NextCursor: "c", Items: []pluginapi.OnlineItem{goodItem("home")}}}
		},
		row: func(req pluginapi.OnlineRowRequest) pluginapi.OnlineRowResponse {
			var out pluginapi.OnlineRowResponse
			for i := 0; i < maxPageItems; i++ {
				out.Items = append(out.Items, goodItem(fmt.Sprintf("%s-%d", req.Cursor, i)))
			}
			return out
		},
	}
	s, now := pagedService(map[string]*pagedProvider{"tube": prov})
	s.SetMediaHostPolicy(func(_, host string) bool { return host == "cdn.example.test" })
	s.lookup = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
	ctx := context.Background()
	s.Rows(ctx, "tube")
	for i := 0; i < maxCacheEntries-1; i++ {
		if _, err := s.Row(ctx, "tube", "r", fmt.Sprintf("p%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if got := thumbCount(s); got > maxThumbsPerSource {
		t.Fatalf("remembered items = %d, cap %d", got, maxThumbsPerSource)
	}
	*now = now.Add(time.Minute) // the rows cache entry is still valid
	if _, err := s.Rows(ctx, "tube"); err != nil {
		t.Fatal(err)
	}
	sess, unsup, err := s.Play(ctx, PlayInput{
		UserID: "u1", SourceID: "tube", ItemID: "home", Profile: canPlayMP4(),
	})
	if err != nil || unsup != nil {
		t.Fatalf("Play = %v, %v", unsup, err)
	}
	if sess.ItemTitle != "Title home" {
		t.Fatalf("session item title = %q, want the title from the cached page", sess.ItemTitle)
	}
}
