package discordtest_test

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/discordtest"
)

// The guarantee that the authoring guide cannot drift (.scratch/plugin-system
// issue 14).
//
// docs/plugins/authoring.md teaches by example, and an example in prose is a
// thing that rots: the contract moves, the sample does not, and the first person
// to find out is the author who copied it. So every code block in the guide is
// marked with the file and region it came from, and this test re-extracts each
// one from the Discord plugin's source and compares it BYTE FOR BYTE.
//
// A guide sample is therefore never written — it is lifted. If you want to change
// what the guide shows, change the plugin.

// guidePath is the document this test holds to the source.
const guidePath = "docs/plugins/authoring.md"

// A sample marker, immediately above the fenced block it governs:
//
//	<!-- sample: main.go deliver -->     a named region of a file
//	<!-- sample: manifest.json -->       a whole file
//
// Regions are delimited in the source by `// sample:begin NAME` and
// `// sample:end NAME` lines, which are not themselves part of the sample.
var (
	markerRE = regexp.MustCompile(`^<!--\s*sample:\s*(\S+)(?:\s+(\S+))?\s*-->$`)
	fenceRE  = regexp.MustCompile("^```")
)

type guideSample struct {
	line   int
	file   string
	region string
	body   string
}

// TestEverySampleInTheAuthoringGuideComesFromThePlugin is the whole mechanism.
func TestEverySampleInTheAuthoringGuideComesFromThePlugin(t *testing.T) {
	guide := filepath.Join(discordtest.RepoRoot(t), filepath.FromSlash(guidePath))
	raw, err := os.ReadFile(guide)
	if err != nil {
		t.Fatalf("reading the authoring guide: %v", err)
	}

	samples, err := parseGuide(string(raw))
	if err != nil {
		t.Fatalf("%s: %v", guidePath, err)
	}
	if len(samples) == 0 {
		t.Fatalf("%s carries no marked samples; a guide that teaches by example must be held to the example",
			guidePath)
	}

	source := discordtest.SourceDir(t)
	for _, s := range samples {
		want, err := extract(filepath.Join(source, s.file), s.region)
		if err != nil {
			t.Errorf("%s:%d: %v", guidePath, s.line, err)
			continue
		}
		if s.body != want {
			t.Errorf("%s:%d: the sample <!-- sample: %s %s --> has drifted from %s.\n"+
				"Copy the region out of the plugin instead of editing the guide.\n\n--- the guide has ---\n%s\n--- the plugin has ---\n%s",
				guidePath, s.line, s.file, s.region, filepath.Join(source, s.file), s.body, want)
		}
	}
}

// TestEveryMarkedRegionOfThePluginIsShownInTheGuide is the other direction: a
// region marked in the source and never used is a marker nobody is maintaining,
// and the next person to add one will not know whether they may remove it.
func TestEveryMarkedRegionOfThePluginIsShownInTheGuide(t *testing.T) {
	guide := filepath.Join(discordtest.RepoRoot(t), filepath.FromSlash(guidePath))
	raw, err := os.ReadFile(guide)
	if err != nil {
		t.Fatalf("reading the authoring guide: %v", err)
	}
	samples, err := parseGuide(string(raw))
	if err != nil {
		t.Fatalf("%s: %v", guidePath, err)
	}
	shown := map[string]bool{}
	for _, s := range samples {
		shown[s.file+" "+s.region] = true
	}

	const sourceFile = "main.go"
	body, err := os.ReadFile(filepath.Join(discordtest.SourceDir(t), sourceFile))
	if err != nil {
		t.Fatalf("reading the plugin source: %v", err)
	}
	for _, region := range regionNames(string(body)) {
		if !shown[sourceFile+" "+region] {
			t.Errorf("%s marks the region %q, which %s never shows. "+
				"Show it, or drop the markers.", sourceFile, region, guidePath)
		}
	}
}

// parseGuide finds every marker and the fenced block under it.
func parseGuide(doc string) ([]guideSample, error) {
	var out []guideSample
	lines := strings.Split(doc, "\n")
	for i := 0; i < len(lines); i++ {
		m := markerRE.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if m == nil {
			continue
		}
		// The fence must open on the very next line. Anything else — a blank line,
		// a sentence — means the marker is not attached to the block it claims.
		if i+1 >= len(lines) || !fenceRE.MatchString(lines[i+1]) {
			return nil, fmt.Errorf("line %d: the marker for %q is not immediately followed by a fenced block", i+1, m[1])
		}
		var body []string
		j := i + 2
		for ; j < len(lines); j++ {
			if fenceRE.MatchString(lines[j]) {
				break
			}
			body = append(body, lines[j])
		}
		if j >= len(lines) {
			return nil, fmt.Errorf("line %d: the block for %q is never closed", i+1, m[1])
		}
		out = append(out, guideSample{
			line:   i + 1,
			file:   m[1],
			region: m[2],
			body:   strings.Join(body, "\n"),
		})
		i = j
	}
	return out, nil
}

// extract reads one region out of a source file, or the whole file when no
// region is named.
func extract(path, region string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if region == "" {
		return strings.TrimRight(string(raw), "\n"), nil
	}
	begin := "// sample:begin " + region
	end := "// sample:end " + region

	var body []string
	inside := false
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
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
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if inside {
		return "", fmt.Errorf("%s: the region %q is never closed", filepath.Base(path), region)
	}
	return "", fmt.Errorf("%s: there is no region %q", filepath.Base(path), region)
}

// regionNames is every region the source marks, in file order.
func regionNames(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(trimmed, "// sample:begin "); ok && !strings.Contains(name, " ") {
			out = append(out, name)
		}
	}
	return out
}
