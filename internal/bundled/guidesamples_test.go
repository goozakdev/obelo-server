package bundled_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// The guarantee that the authoring guide's BUNDLED-PLUGIN samples cannot drift
// (.scratch/bundled-plugins: issue 08).
//
// It is the third of its kind and the argument has not changed: a sample in prose
// is a thing that rots — the contract moves, the sample does not, and the first
// person to find out is the author who copied it. internal/plugins/discordtest
// holds the guide to the reference plugin in its sibling repository;
// internal/plugins/sdkguesttest holds it to the SDK's own guest; this one holds
// the two sections issue 08 added — "Pace yourself" and "Declare a probe" — to
// the plugins this server SHIPS, which is the strongest source available for
// either claim. An author reading "pace like this" is reading the code that paces
// MusicBrainz on the maintainer's own server.
//
// The marker syntax is deliberately different again — `bundled-sample:` — so no
// test ever sees another's blocks. The three regexes anchor on their own prefix.
//
// It lives in internal/bundled because this package is already the one that is
// about plugins/ as a set: it embeds what `make plugins` builds out of those
// directories and it knows the shipped ids.

const guidePath = "docs/plugins/authoring.md"

// bundledSampleSources maps the name a marker uses onto a path under plugins/.
// The names are the paths, which is the shape an author will go looking for.
var bundledSampleSources = map[string]string{
	"musicbrainz/main.go":       "musicbrainz/main.go",
	"musicbrainz/manifest.json": "musicbrainz/manifest.json",
}

// A marker, immediately above the fenced block it governs:
//
//	<!-- bundled-sample: musicbrainz/main.go pace -->
//
// The region is either a name marked in the source with `// bundled-sample:begin
// <name>` … `// bundled-sample:end <name>`, or the word `whole`, which is the
// entire file. `whole` exists for JSON, which has no comments to hide a marker in
// — and a manifest is short enough and interesting enough to show entire.
var (
	bundledMarkerRE = regexp.MustCompile(`^<!--\s*bundled-sample:\s*(\S+)\s+(\S+)\s*-->$`)
	bundledFenceRE  = regexp.MustCompile("^```")
)

const wholeFile = "whole"

type bundledGuideSample struct {
	line   int
	file   string
	region string
	body   string
}

// TestEveryBundledSampleInTheAuthoringGuideComesFromAShippedPlugin is the
// mechanism: every marked block is re-extracted from source and compared BYTE FOR
// BYTE.
func TestEveryBundledSampleInTheAuthoringGuideComesFromAShippedPlugin(t *testing.T) {
	samples := parseGuideForBundledSamples(t)
	if len(samples) == 0 {
		t.Fatalf("%s carries no marked bundled-plugin samples; two sections that teach by "+
			"example must be held to the example", guidePath)
	}
	for _, s := range samples {
		rel, ok := bundledSampleSources[s.file]
		if !ok {
			t.Errorf("%s:%d: the marker names %q, which is not a file this test knows. "+
				"Add it to bundledSampleSources, or fix the marker.", guidePath, s.line, s.file)
			continue
		}
		path := filepath.Join(pluginsDir(t), filepath.FromSlash(rel))
		want, err := extractBundledRegion(path, s.region)
		if err != nil {
			t.Errorf("%s:%d: %v", guidePath, s.line, err)
			continue
		}
		if s.body != want {
			t.Errorf("%s:%d: the sample <!-- bundled-sample: %s %s --> has drifted from plugins/%s.\n"+
				"Copy the region out of the source instead of editing the guide.\n\n"+
				"--- the guide has ---\n%s\n--- the source has ---\n%s",
				guidePath, s.line, s.file, s.region, rel, s.body, want)
		}
	}
}

// TestEveryMarkedBundledRegionIsShownInTheGuide is the other direction: a region
// marked in a plugin's source and never shown is a marker nobody is maintaining,
// and the next person to touch that file will not know whether they may remove it.
func TestEveryMarkedBundledRegionIsShownInTheGuide(t *testing.T) {
	shown := map[string]bool{}
	for _, s := range parseGuideForBundledSamples(t) {
		shown[s.file+" "+s.region] = true
	}
	names := make([]string, 0, len(bundledSampleSources))
	for name := range bundledSampleSources {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(pluginsDir(t), filepath.FromSlash(bundledSampleSources[name])))
		if err != nil {
			t.Fatalf("reading plugins/%s: %v", bundledSampleSources[name], err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			region, ok := strings.CutPrefix(strings.TrimSpace(line), "// bundled-sample:begin ")
			if !ok || strings.Contains(region, " ") {
				continue
			}
			if !shown[name+" "+region] {
				t.Errorf("plugins/%s marks the region %q, which %s never shows. Show it, or drop the markers.",
					name, region, guidePath)
			}
		}
	}
}

func parseGuideForBundledSamples(t *testing.T) []bundledGuideSample {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRootForSamples(t), filepath.FromSlash(guidePath)))
	if err != nil {
		t.Fatalf("reading the authoring guide: %v", err)
	}
	var out []bundledGuideSample
	lines := strings.Split(string(raw), "\n")
	for i := 0; i < len(lines); i++ {
		m := bundledMarkerRE.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if m == nil {
			continue
		}
		// The fence must open on the very next line. Anything else — a blank line, a
		// sentence — means the marker is not attached to the block it claims.
		if i+1 >= len(lines) || !bundledFenceRE.MatchString(lines[i+1]) {
			t.Fatalf("%s:%d: the marker for %q is not immediately followed by a fenced block",
				guidePath, i+1, m[1])
		}
		var body []string
		j := i + 2
		for ; j < len(lines); j++ {
			if bundledFenceRE.MatchString(lines[j]) {
				break
			}
			body = append(body, lines[j])
		}
		if j >= len(lines) {
			t.Fatalf("%s:%d: the block for %q is never closed", guidePath, i+1, m[1])
		}
		out = append(out, bundledGuideSample{line: i + 1, file: m[1], region: m[2], body: strings.Join(body, "\n")})
		i = j
	}
	return out
}

// extractBundledRegion reads one region out of a source file, or the whole file.
func extractBundledRegion(path, region string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if region == wholeFile {
		return strings.Trim(string(raw), "\n"), nil
	}
	begin := "// bundled-sample:begin " + region
	end := "// bundled-sample:end " + region

	var body []string
	inside := false
	for _, line := range strings.Split(string(raw), "\n") {
		switch strings.TrimSpace(line) {
		case begin:
			if inside {
				return "", fmt.Errorf("%s: the region %q begins twice", filepath.Base(path), region)
			}
			inside = true
			continue
		case end:
			if !inside {
				return "", fmt.Errorf("%s: the region %q ends before it begins", filepath.Base(path), region)
			}
			return strings.Trim(strings.Join(body, "\n"), "\n"), nil
		}
		if inside {
			body = append(body, line)
		}
	}
	if inside {
		return "", fmt.Errorf("%s: the region %q is never closed", filepath.Base(path), region)
	}
	return "", fmt.Errorf("%s: there is no region %q", filepath.Base(path), region)
}

// repoRootForSamples is the repository root, found from this file rather than
// from a working directory.
func repoRootForSamples(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source")
	}
	// .../internal/bundled/guidesamples_test.go -> the repository root.
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "go.work")); err != nil {
		t.Fatalf("no go.work at %s: %v", root, err)
	}
	return root
}

func pluginsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRootForSamples(t), "plugins")
}
