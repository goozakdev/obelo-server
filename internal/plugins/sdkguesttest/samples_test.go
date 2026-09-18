package sdkguesttest_test

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

// The guarantee that the authoring guide's SDK section cannot drift
// (.scratch/bundled-plugins issue 03).
//
// It is the sibling of internal/plugins/discordtest/samples_test.go and works the
// same way, because the argument is the same: a sample in prose is a thing that
// rots — the contract moves, the sample does not, and the first person to find out
// is the author who copied it. So every code block in §11 is marked with the file
// and region it came from, and this test re-extracts each one and compares it BYTE
// FOR BYTE.
//
// It is a SECOND test rather than an extension of the Discord one because the two
// read different sources. The Discord test holds the guide to the reference plugin
// in its sibling repository; this one holds it to the SDK's own guest and provider
// inside pluginsdk. Sharing one test would mean one source directory, and there
// are two.
//
// The marker syntax is deliberately DIFFERENT — `sdk-sample:` rather than
// `sample:` — so neither test ever sees the other's blocks.

// guidePath is the document both tests hold to source.
const guidePath = "docs/plugins/authoring.md"

// sdkSampleSources maps the short file name a marker names onto its path under
// pluginsdk. The short names are what an author reads in the guide; the paths are
// where the code actually lives, and three different directories is why this is a
// table rather than a join.
var sdkSampleSources = map[string]string{
	"guest/main.go":           "testdata/guest/main.go",
	"testprovider.go":         "internal/testprovider/testprovider.go",
	"provider_native_test.go": "provider_native_test.go",
}

// A marker, immediately above the fenced block it governs:
//
//	<!-- sdk-sample: guest/main.go serve -->
var (
	sdkMarkerRE = regexp.MustCompile(`^<!--\s*sdk-sample:\s*(\S+)\s+(\S+)\s*-->$`)
	sdkFenceRE  = regexp.MustCompile("^```")
)

type sdkGuideSample struct {
	line   int
	file   string
	region string
	body   string
}

// TestEverySDKSampleInTheAuthoringGuideComesFromTheSDK is the mechanism.
func TestEverySDKSampleInTheAuthoringGuideComesFromTheSDK(t *testing.T) {
	samples := sdkParseGuide(t)
	if len(samples) == 0 {
		t.Fatalf("%s carries no marked SDK samples; a section that teaches by example must be held to the example",
			guidePath)
	}
	for _, s := range samples {
		path, ok := sdkSampleSources[s.file]
		if !ok {
			t.Errorf("%s:%d: the marker names %q, which is not a file this test knows. "+
				"Add it to sdkSampleSources, or fix the marker.", guidePath, s.line, s.file)
			continue
		}
		want, err := sdkExtract(filepath.Join(sdkPluginSDKDir(t), filepath.FromSlash(path)), s.region)
		if err != nil {
			t.Errorf("%s:%d: %v", guidePath, s.line, err)
			continue
		}
		if s.body != want {
			t.Errorf("%s:%d: the sample <!-- sdk-sample: %s %s --> has drifted from %s.\n"+
				"Copy the region out of the source instead of editing the guide.\n\n"+
				"--- the guide has ---\n%s\n--- the source has ---\n%s",
				guidePath, s.line, s.file, s.region, path, s.body, want)
		}
	}
}

// TestEveryMarkedSDKRegionIsShownInTheGuide is the other direction: a region
// marked in the source and never used is a marker nobody is maintaining, and the
// next person to add one will not know whether they may remove it.
func TestEveryMarkedSDKRegionIsShownInTheGuide(t *testing.T) {
	shown := map[string]bool{}
	for _, s := range sdkParseGuide(t) {
		shown[s.file+" "+s.region] = true
	}
	var files []string
	for name := range sdkSampleSources {
		files = append(files, name)
	}
	sort.Strings(files)

	for _, name := range files {
		body, err := os.ReadFile(filepath.Join(sdkPluginSDKDir(t), filepath.FromSlash(sdkSampleSources[name])))
		if err != nil {
			t.Fatalf("reading %s: %v", sdkSampleSources[name], err)
		}
		for _, region := range sdkRegionNames(string(body)) {
			if !shown[name+" "+region] {
				t.Errorf("%s marks the region %q, which %s never shows. Show it, or drop the markers.",
					name, region, guidePath)
			}
		}
	}
}

// sdkParseGuide finds every SDK marker in the guide and the fenced block under it.
func sdkParseGuide(t *testing.T) []sdkGuideSample {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sdkRepoRoot(t), filepath.FromSlash(guidePath)))
	if err != nil {
		t.Fatalf("reading the authoring guide: %v", err)
	}
	var out []sdkGuideSample
	lines := strings.Split(string(raw), "\n")
	for i := 0; i < len(lines); i++ {
		m := sdkMarkerRE.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if m == nil {
			continue
		}
		// The fence must open on the very next line. Anything else — a blank line,
		// a sentence — means the marker is not attached to the block it claims.
		if i+1 >= len(lines) || !sdkFenceRE.MatchString(lines[i+1]) {
			t.Fatalf("%s:%d: the marker for %q is not immediately followed by a fenced block",
				guidePath, i+1, m[1])
		}
		var body []string
		j := i + 2
		for ; j < len(lines); j++ {
			if sdkFenceRE.MatchString(lines[j]) {
				break
			}
			body = append(body, lines[j])
		}
		if j >= len(lines) {
			t.Fatalf("%s:%d: the block for %q is never closed", guidePath, i+1, m[1])
		}
		out = append(out, sdkGuideSample{line: i + 1, file: m[1], region: m[2], body: strings.Join(body, "\n")})
		i = j
	}
	return out
}

// sdkExtract reads one region out of a source file.
func sdkExtract(path, region string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	begin := "// sdk-sample:begin " + region
	end := "// sdk-sample:end " + region

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

// sdkRegionNames is every region a source marks, in file order.
func sdkRegionNames(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "// sdk-sample:begin "); ok && !strings.Contains(name, " ") {
			out = append(out, name)
		}
	}
	return out
}

// sdkRepoRoot is the repository root, found from this file rather than from a
// working directory.
func sdkRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source")
	}
	// .../internal/plugins/sdkguesttest/samples_test.go -> the repository root.
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file))))
	if _, err := os.Stat(filepath.Join(root, "go.work")); err != nil {
		t.Fatalf("no go.work at %s: %v", root, err)
	}
	return root
}

func sdkPluginSDKDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(sdkRepoRoot(t), "pluginsdk")
}
