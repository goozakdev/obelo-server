package catalog

import (
	"crypto/sha256"
	"encoding/binary"
	"hash/fnv"
	"math/rand/v2"
	"sort"
	"strconv"
	"time"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
)

// DiscoverySource is the metadata dimension a Discovery row is drawn from (a Row
// source, CONTEXT.md).
type DiscoverySource string

const (
	SourceGenre   DiscoverySource = "genre"
	SourceStudio  DiscoverySource = "studio"
	SourceNetwork DiscoverySource = "network"
	SourceDecade  DiscoverySource = "decade"
	SourceActor   DiscoverySource = "actor"
)

// DiscoveryItem is one Movie or Show in a Discovery row. Kind is "movie" or
// "show"; a row never holds Episodes or Tracks.
type DiscoveryItem struct {
	ID        string
	LibraryID string
	Kind      string
	Title     string
	Year      int
	Watched   bool
}

// DiscoveryRow is one Discovery row: its Row source and value, the media kind it
// lists, and the Titles in the day's order. The descriptor carries what a caller
// needs to build a Library browse ("See all") and to label the row.
type DiscoveryRow struct {
	Source DiscoverySource
	// Value is the browse filter value: genre / studio / network name, the first
	// year of the decade ("1980"), or the provider person ref.
	Value string
	// DisplayValue is how to show Value (a person's name); equal to Value otherwise.
	DisplayValue string
	// Kind is "movie" or "tv".
	Kind string
	// LibraryIDs are the Libraries holding the row's Titles, sorted.
	LibraryIDs []string
	// Total is how many visible Titles matched, before the row limit.
	Total int
	Items []DiscoveryItem
}

// Discovery row tuning, fixed by product decision (not settings).
const (
	// discoveryMinTitles is how many visible Titles a value needs to qualify.
	discoveryMinTitles = 8
	// discoveryMaxRows is the most Discovery rows one Home shows.
	discoveryMaxRows = 8
)

// discoveryKey identifies a candidate row: one Row source value of one media kind.
type discoveryKey struct {
	source DiscoverySource
	value  string
	kind   string // "movie" | "tv"
}

type discoveryCandidate struct {
	row   DiscoveryRow
	items []store.DiscoveryItem
}

// DiscoveryRows returns the User's Discovery rows for the Server-local day of
// day (its calendar date in time.Local, whatever zone day carries), in display order. limit caps
// each row's Titles. The same (User, date) always yields the same rows and order;
// the next date yields a different choice. Nothing is stored: rows are recomputed
// from catalog metadata under the User's library access and Rating ceiling.
func (s *Service) DiscoveryRows(scope access.Scope, userID string, day time.Time, limit int) ([]DiscoveryRow, error) {
	items, err := s.store.DiscoveryCatalog(userID, scope.StoreFilter())
	if err != nil {
		return nil, err
	}
	seed := discoverySeed(userID, day.In(time.Local).Format("2006-01-02"))

	groups := map[discoveryKey]*discoveryCandidate{}
	add := func(k discoveryKey, display string, it store.DiscoveryItem) {
		c := groups[k]
		if c == nil {
			c = &discoveryCandidate{row: DiscoveryRow{Source: k.source, Value: k.value, DisplayValue: display, Kind: k.kind}}
			groups[k] = c
		}
		// First-seen order is not stable, so a ref with several names takes the smallest.
		if display < c.row.DisplayValue {
			c.row.DisplayValue = display
		}
		c.items = append(c.items, it)
	}
	for _, it := range items {
		kind := "movie"
		if it.Kind == "show" {
			kind = "tv"
		}
		for _, g := range uniq(it.Genres) {
			add(discoveryKey{SourceGenre, g, kind}, g, it)
		}
		for _, c := range uniq(it.Companies) {
			src := SourceStudio
			if kind == "tv" {
				src = SourceNetwork
			}
			add(discoveryKey{src, c, kind}, c, it)
		}
		if it.Year > 0 {
			d := strconv.Itoa(it.Year / 10 * 10)
			add(discoveryKey{SourceDecade, d, kind}, d, it)
		}
		seen := map[string]bool{}
		for _, p := range it.Cast {
			if !seen[p.Ref] {
				seen[p.Ref] = true
				add(discoveryKey{SourceActor, p.Ref, kind}, p.Name, it)
			}
		}
	}

	// Map iteration is random: sort to a canonical order before the seeded shuffle.
	var cands []*discoveryCandidate
	for _, c := range groups {
		if len(c.items) >= discoveryMinTitles {
			cands = append(cands, c)
		}
	}
	sort.Slice(cands, func(i, j int) bool { return candLess(cands[i].row, cands[j].row) })
	rand.New(rand.NewPCG(seed, 0x6f62656c6f)).Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })

	picked := arrangeNoAdjacent(selectFeasible(cands, discoveryMaxRows))
	rows := make([]DiscoveryRow, 0, len(picked))
	for _, c := range picked {
		rows = append(rows, c.build(seed, limit))
	}
	return rows, nil
}

func candLess(a, b DiscoveryRow) bool {
	if a.Source != b.Source {
		return a.Source < b.Source
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	return a.Value < b.Value
}

// build orders the candidate's Titles for the day (unwatched first, then watched,
// each group shuffled by the seed) and fills the descriptor.
func (c *discoveryCandidate) build(seed uint64, limit int) DiscoveryRow {
	row := c.row
	row.Total = len(c.items)
	sort.Slice(c.items, func(i, j int) bool { return c.items[i].ID < c.items[j].ID })
	h := fnv.New64a()
	h.Write([]byte(string(row.Source) + "\x00" + row.Kind + "\x00" + row.Value))
	rand.New(rand.NewPCG(seed, h.Sum64())).Shuffle(len(c.items), func(i, j int) { c.items[i], c.items[j] = c.items[j], c.items[i] })
	sort.SliceStable(c.items, func(i, j int) bool { return !c.items[i].Watched && c.items[j].Watched })

	libs := map[string]bool{}
	for _, it := range c.items {
		libs[it.LibraryID] = true
	}
	for id := range libs {
		row.LibraryIDs = append(row.LibraryIDs, id)
	}
	sort.Strings(row.LibraryIDs)
	if limit > 0 && len(c.items) > limit {
		c.items = c.items[:limit]
	}
	row.Items = make([]DiscoveryItem, len(c.items))
	for i, it := range c.items {
		row.Items[i] = DiscoveryItem{ID: it.ID, LibraryID: it.LibraryID, Kind: it.Kind, Title: it.Title, Year: it.Year, Watched: it.Watched}
	}
	return row
}

// discoverySeed hashes (User id, YYYY-MM-DD) to 64 bits.
func discoverySeed(userID, date string) uint64 {
	sum := sha256.Sum256([]byte(userID + "\x00" + date))
	return binary.BigEndian.Uint64(sum[:8])
}

func uniq(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// selectFeasible takes up to max candidates in seed order such that they can all
// be arranged with no two adjacent sharing a Row source, i.e. no source holds
// more than half (rounded up) of the chosen set. A candidate skipped on one pass
// is retried on the next, since later picks can make room for it.
func selectFeasible(cands []*discoveryCandidate, max int) []*discoveryCandidate {
	var chosen []*discoveryCandidate
	count := map[DiscoverySource]int{}
	taken := make([]bool, len(cands))
	for progress := true; progress && len(chosen) < max; {
		progress = false
		for i, c := range cands {
			if taken[i] || len(chosen) >= max {
				continue
			}
			n := len(chosen) + 1
			if count[c.row.Source]+1 > (n+1)/2 {
				continue
			}
			taken[i], progress = true, true
			chosen = append(chosen, c)
			count[c.row.Source]++
		}
	}
	return chosen
}

// arrangeNoAdjacent orders cands (already in seed order) so no two neighbours
// share a Row source, keeping seed order wherever that is allowed. A source that
// must go now to leave the rest placeable is placed first; if the rest still
// cannot be placed, the remainder is dropped rather than break adjacency.
func arrangeNoAdjacent(cands []*discoveryCandidate) []*discoveryCandidate {
	remaining := append([]*discoveryCandidate(nil), cands...)
	out := make([]*discoveryCandidate, 0, len(cands))
	var last DiscoverySource
	for len(remaining) > 0 {
		count := map[DiscoverySource]int{}
		for _, c := range remaining {
			count[c.row.Source]++
		}
		pick := -1
		for i, c := range remaining {
			if c.row.Source != last && count[c.row.Source]*2 > len(remaining) {
				pick = i // this source would not fit in the remaining slots otherwise
				break
			}
		}
		if pick < 0 {
			for i, c := range remaining {
				if c.row.Source != last {
					pick = i
					break
				}
			}
		}
		if pick < 0 {
			break
		}
		out = append(out, remaining[pick])
		last = remaining[pick].row.Source
		remaining = append(remaining[:pick], remaining[pick+1:]...)
	}
	return out
}

// TitlesByIDs bulk-reads Titles by id for a caller that already resolved the ids
// under the User's access Scope (a Discovery row's items), keyed by id.
func (s *Service) TitlesByIDs(ids []string) (map[string]store.Title, error) {
	return s.store.TitlesByIDs(ids)
}

// ShowsByIDs is TitlesByIDs for Shows.
func (s *Service) ShowsByIDs(ids []string) (map[string]store.Show, error) {
	return s.store.ShowsByIDs(ids)
}
