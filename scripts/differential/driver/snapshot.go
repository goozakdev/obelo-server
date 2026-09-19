package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// A snapshot is everything about a running server this differential compares:
// the provider settings an operator sees, the installed plugins, the per-Library
// enrichment policy, and every enriched record the catalog will show.
//
// It is deliberately built out of PUBLIC API responses and nothing else. Reading
// SQLite directly would let this harness assert things an operator cannot see,
// and the PRD's criteria are about what an operator sees.
type snapshot struct {
	Providers   []map[string]any `json:"providers"`
	Settings    map[string]any   `json:"providerSettings"`
	Plugins     []map[string]any `json:"plugins"`
	Libraries   []map[string]any `json:"libraries"`
	dataDir     string
	rawPolicies map[string]map[string]any
}

// volatileKeys hold values that legitimately differ between two runs or two data
// directories and say nothing about enrichment: clocks, install times, and the
// artwork cache-bust token, which is derived from when the bytes landed.
var volatileKeys = map[string]bool{
	"addedAt":        true,
	"createdAt":      true,
	"installedAt":    true,
	"updatedAt":      true,
	"startedAt":      true,
	"finishedAt":     true,
	"enrichedAt":     true,
	"lastSeenAt":     true,
	"grantedAt":      true,
	"artworkVersion": true,
	"photoVersion":   true,
}

// scrub replaces volatile values in place, recursively, with a marker that still
// distinguishes "there was one" from "there was not".
func scrub(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if volatileKeys[k] {
				if val == nil || val == "" || val == float64(0) {
					t[k] = "<absent>"
				} else {
					t[k] = "<volatile>"
				}
				continue
			}
			t[k] = scrub(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = scrub(t[i])
		}
		return t
	default:
		return v
	}
}

// --- collecting -------------------------------------------------------------

func collect(c *client, dataDir string) (*snapshot, error) {
	s := &snapshot{dataDir: dataDir, rawPolicies: map[string]map[string]any{}}

	var provResp map[string]any
	if err := c.do(http.MethodGet, "/settings/metadata-providers", nil, &provResp); err != nil {
		return nil, fmt.Errorf("provider settings: %w", err)
	}
	if raw, ok := provResp["providers"].([]any); ok {
		for _, p := range raw {
			if m, ok := p.(map[string]any); ok {
				s.Providers = append(s.Providers, m)
			}
		}
		sort.Slice(s.Providers, func(i, j int) bool {
			return fmt.Sprint(s.Providers[i]["slug"]) < fmt.Sprint(s.Providers[j]["slug"])
		})
	}
	delete(provResp, "providers")
	s.Settings = provResp

	// The plugins list only exists on a build that has installed plugins; the
	// 2026-09-17 build answers with an empty list, which is itself a fact worth
	// recording rather than an error.
	var plugResp map[string]any
	if err := c.do(http.MethodGet, "/settings/plugins", nil, &plugResp); err == nil {
		if raw, ok := plugResp["plugins"].([]any); ok {
			for _, p := range raw {
				if m, ok := p.(map[string]any); ok {
					s.Plugins = append(s.Plugins, m)
				}
			}
		}
	}

	var libResp struct {
		Libraries []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"libraries"`
	}
	if err := c.do(http.MethodGet, "/libraries", nil, &libResp); err != nil {
		return nil, fmt.Errorf("libraries: %w", err)
	}
	libs := libResp.Libraries
	sort.Slice(libs, func(i, j int) bool { return libs[i].Name < libs[j].Name })

	for _, lib := range libs {
		entry := map[string]any{"name": lib.Name, "kind": lib.Kind}

		var policy map[string]any
		if err := c.do(http.MethodGet, "/libraries/"+lib.ID+"/enrichment-policy", nil, &policy); err != nil {
			return nil, fmt.Errorf("policy for %s: %w", lib.Name, err)
		}
		entry["enrichmentPolicy"] = policy
		s.rawPolicies[lib.Name] = policy

		var listing map[string]any
		if err := c.do(http.MethodGet, "/libraries/"+lib.ID+"/titles?limit=200", nil, &listing); err != nil {
			return nil, fmt.Errorf("listing %s: %w", lib.Name, err)
		}

		switch lib.Kind {
		case "movie":
			var titles []any
			for _, t := range asList(listing["titles"]) {
				m := asMap(t)
				var detail map[string]any
				if err := c.do(http.MethodGet, "/titles/"+fmt.Sprint(m["id"]), nil, &detail); err != nil {
					return nil, err
				}
				titles = append(titles, detail)
			}
			entry["titles"] = titles
		case "tv":
			var shows []any
			for _, sh := range asList(listing["shows"]) {
				m := asMap(sh)
				showEntry := map[string]any{"summary": m}
				var seasonsResp map[string]any
				if err := c.do(http.MethodGet, "/shows/"+fmt.Sprint(m["id"])+"/seasons", nil, &seasonsResp); err != nil {
					return nil, err
				}
				// The `show` object from THIS response, not the one in the
				// library listing: only handleShowSeasons decorates a Show with
				// its entity enrichment, and `enrichmentOverride` — the item pin
				// criterion B is about — lives on that decoration.
				showEntry["show"] = asMap(seasonsResp["show"])
				var seasons []any
				for _, se := range asList(seasonsResp["seasons"]) {
					sm := asMap(se)
					seasonEntry := map[string]any{"season": sm}
					var epResp map[string]any
					if err := c.do(http.MethodGet, "/seasons/"+fmt.Sprint(sm["id"])+"/episodes", nil, &epResp); err != nil {
						return nil, err
					}
					var eps []any
					for _, ep := range asList(epResp["episodes"]) {
						em := asMap(ep)
						var detail map[string]any
						if err := c.do(http.MethodGet, "/titles/"+fmt.Sprint(em["id"]), nil, &detail); err != nil {
							return nil, err
						}
						eps = append(eps, detail)
					}
					seasonEntry["episodes"] = eps
					seasons = append(seasons, seasonEntry)
				}
				showEntry["seasons"] = seasons
				shows = append(shows, showEntry)
			}
			entry["shows"] = shows
		case "music":
			var artists []any
			for _, ar := range asList(listing["artists"]) {
				m := asMap(ar)
				artistEntry := map[string]any{"summary": m}
				var albumsResp map[string]any
				if err := c.do(http.MethodGet, "/artists/"+fmt.Sprint(m["id"])+"/albums", nil, &albumsResp); err != nil {
					return nil, err
				}
				// Same reason as the Show above: the decorated Artist is the one
				// in this response.
				artistEntry["artist"] = asMap(albumsResp["artist"])
				var albums []any
				for _, al := range asList(albumsResp["albums"]) {
					am := asMap(al)
					albumEntry := map[string]any{"album": am}
					var trResp map[string]any
					if err := c.do(http.MethodGet, "/albums/"+fmt.Sprint(am["id"])+"/tracks", nil, &trResp); err != nil {
						return nil, err
					}
					var tracks []any
					for _, tr := range asList(trResp["tracks"]) {
						tm := asMap(tr)
						var detail map[string]any
						if err := c.do(http.MethodGet, "/titles/"+fmt.Sprint(tm["id"]), nil, &detail); err != nil {
							return nil, err
						}
						tracks = append(tracks, detail)
					}
					albumEntry["tracks"] = tracks
					albums = append(albums, albumEntry)
				}
				artistEntry["albums"] = albums
				artists = append(artists, artistEntry)
			}
			entry["artists"] = artists
		}
		s.Libraries = append(s.Libraries, entry)
	}
	return s, nil
}

func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// --- normalizing ------------------------------------------------------------

var (
	uuidRE = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	hexRE  = regexp.MustCompile(`\b[0-9a-f]{16,}\b`)
	// A parent's artwork URL carries its cache-bust token in the QUERY string
	// rather than in a field of its own (`?v=2026-09-19+04%3A46%3A25`), so the
	// volatileKeys sweep cannot reach it. It is the moment the bytes landed —
	// two runs minutes apart differ by construction — and the artwork itself is
	// still compared, because the URL's path and role are untouched.
	cacheBustRE = regexp.MustCompile(`\?v=[^"]*`)
)

// knownMBIDs are the MusicBrainz identifiers the stand-in serves. They ARE
// UUIDs, so the id-aliasing below would erase them — and they are exactly the
// enrichment result this run exists to compare. They are protected by name.
var knownMBIDs = []string{
	"a74b1b7f-71a5-4011-9441-d0b5e4122711", // Radiohead
	"b1392450-e666-3926-a536-22c65f834433", // OK Computer (release group)
	"0b6b4ba0-d36f-47bd-b4ea-6a5b91842d29", // OK Computer (release)
	"d8c4b2a6-1f51-4b3e-9e0c-8b0d8c2f0001",
	"d8c4b2a6-1f51-4b3e-9e0c-8b0d8c2f0002",
	"d8c4b2a6-1f51-4b3e-9e0c-8b0d8c2f0003",
}

// normalize renders a snapshot as canonical JSON text with everything that
// legitimately differs between two data directories replaced by a stable token.
//
// Row ids are aliased by ORDER OF FIRST APPEARANCE in that canonical text. That
// works because both snapshots are built by the same traversal in the same
// order, so if the two servers hold the same records the two id sequences line
// up; and if they do not line up, the structures differ, which is the very
// thing this is looking for.
func normalize(s *snapshot, doc map[string]any) string {
	scrub(doc)
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "MARSHAL ERROR: " + err.Error()
	}
	text := string(b)

	// The data directory and the media root are absolute paths that differ per
	// run; the media root is shared between the two builds, so only the data
	// directory needs a token.
	if s.dataDir != "" {
		text = strings.ReplaceAll(text, s.dataDir, "<dataDir>")
	}

	// Protect the MusicBrainz ids from the id aliaser.
	for i, mbid := range knownMBIDs {
		text = strings.ReplaceAll(text, mbid, fmt.Sprintf("<mbid-%d>", i))
	}

	seen := map[string]string{}
	var order int
	text = uuidRE.ReplaceAllStringFunc(text, func(m string) string {
		if alias, ok := seen[m]; ok {
			return alias
		}
		order++
		alias := fmt.Sprintf("<id-%d>", order)
		seen[m] = alias
		return alias
	})
	// Artwork cache paths are content hashes; they embed nothing an operator
	// reads, and two servers that fetched the same bytes still write them under
	// paths derived from row ids.
	text = hexRE.ReplaceAllString(text, "<hash>")
	text = cacheBustRE.ReplaceAllString(text, "?v=<volatile>")
	return text
}

func toAnyList(in []map[string]any) []any {
	out := make([]any, 0, len(in))
	for _, m := range in {
		out = append(out, m)
	}
	return out
}

// diffText returns a compact unified-ish diff of two normalized snapshots, and
// the number of differing lines. It is hand-rolled because this harness has no
// dependencies and a line diff is twenty lines of code.
func diffText(aName, a, bName, b string) (string, int) {
	al := strings.Split(a, "\n")
	bl := strings.Split(b, "\n")
	lcs := longestCommon(al, bl)
	var out strings.Builder
	changed := 0
	i, j := 0, 0
	for _, k := range lcs {
		for i < k.a {
			out.WriteString("- [" + aName + "] " + strings.TrimSpace(al[i]) + "\n")
			changed++
			i++
		}
		for j < k.b {
			out.WriteString("+ [" + bName + "] " + strings.TrimSpace(bl[j]) + "\n")
			changed++
			j++
		}
		i, j = k.a+1, k.b+1
	}
	for i < len(al) {
		out.WriteString("- [" + aName + "] " + strings.TrimSpace(al[i]) + "\n")
		changed++
		i++
	}
	for j < len(bl) {
		out.WriteString("+ [" + bName + "] " + strings.TrimSpace(bl[j]) + "\n")
		changed++
		j++
	}
	return out.String(), changed
}

type pair struct{ a, b int }

// longestCommon is a plain LCS over lines. The snapshots are a few thousand
// lines, so O(n·m) is milliseconds and readability wins.
func longestCommon(a, b []string) []pair {
	n, m := len(a), len(b)
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}
	var out []pair
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, pair{i, j})
			i, j = i+1, j+1
		case table[i+1][j] >= table[i][j+1]:
			i++
		default:
			j++
		}
	}
	return out
}

// --- request counters -------------------------------------------------------

type counterSnapshot struct {
	Total      int            `json:"total"`
	ByProvider map[string]int `json:"byProvider"`
	ByPath     map[string]int `json:"byPath"`
	Unmatched  []string       `json:"unmatched"`
}

func readCounters(control string) (*counterSnapshot, error) {
	resp, err := http.Get(control + "/counters")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out counterSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func resetCounters(control string) error {
	resp, err := http.Post(control+"/reset", "application/json", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// diffCounters compares two per-path multisets and returns a human-readable
// report plus the number of paths that differ.
func diffCounters(aName string, a map[string]int, bName string, b map[string]int) (string, int) {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out strings.Builder
	diffs := 0
	for _, k := range sorted {
		if a[k] != b[k] {
			fmt.Fprintf(&out, "  %-72s %s=%d  %s=%d\n", k, aName, a[k], bName, b[k])
			diffs++
		}
	}
	return out.String(), diffs
}

// formatPaths renders the per-path multiset, which is the record of exactly
// what a first enrichment pass asks each source for. It is printed in full
// rather than summarized because it IS the evidence for criterion A's second
// half, and a reader of the run's output should not have to take "identical
// multisets" on trust.
func formatPaths(c *counterSnapshot) string {
	keys := make([]string, 0, len(c.ByPath))
	for k := range c.ByPath {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&out, "      %s ×%d\n", k, c.ByPath[k])
	}
	if len(keys) == 0 {
		out.WriteString("      (none)\n")
	}
	return out.String()
}

func formatCounters(c *counterSnapshot) string {
	keys := make([]string, 0, len(c.ByProvider))
	for k := range c.ByProvider {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&out, "    %-16s %d\n", k, c.ByProvider[k])
	}
	if len(keys) == 0 {
		out.WriteString("    (no requests)\n")
	}
	return out.String()
}
