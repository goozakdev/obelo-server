// Command differential is the harness behind `make differential`.
//
// It closes two success criteria of the bundled-plugins PRD
// (.scratch/bundled-plugins/PRD.md) that the issue-08 walk could only ARGUE:
//
//	A. "Enriches a movie and an album on the first pass exactly as the
//	   2026-09-17 build does."
//	B. "An existing server upgraded across issue 08 keeps every provider key,
//	   every per-Library override and every item pin; the Cover Art Archive row
//	   is gone; nothing re-enriches."
//
// It does it the only way those can be proven: by running BOTH binaries — the
// 2026-09-17 build (commit cf5da34) and the bundled build — against the same
// stand-in sources and the same fixture library, and diffing.
//
// Nothing here touches the developer's server, their data directory or their
// port. Both builds are handed a data directory under this run's own temp root
// and a loopback port chosen at runtime.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// --- the result table -------------------------------------------------------

type check struct {
	criterion string
	name      string
	pass      bool
	skipped   bool
	detail    string
}

type report struct {
	checks   []check
	findings []string
}

func (r *report) add(criterion, name string, pass bool, format string, args ...any) {
	r.checks = append(r.checks, check{criterion: criterion, name: name, pass: pass,
		detail: fmt.Sprintf(format, args...)})
}

func (r *report) skip(criterion, name string, format string, args ...any) {
	r.checks = append(r.checks, check{criterion: criterion, name: name, skipped: true,
		detail: fmt.Sprintf(format, args...)})
}

// finding records something the run observed that is neither a pass nor a
// mechanical failure — a real difference in behaviour, reported with enough
// detail to act on. A finding is a successful outcome of this harness.
func (r *report) finding(format string, args ...any) {
	r.findings = append(r.findings, fmt.Sprintf(format, args...))
}

func (r *report) failed() bool {
	for _, c := range r.checks {
		if !c.pass && !c.skipped {
			return true
		}
	}
	return false
}

func (r *report) print() {
	fmt.Println()
	fmt.Println("================================ RESULTS ================================")
	width := 0
	for _, c := range r.checks {
		if len(c.name) > width {
			width = len(c.name)
		}
	}
	for _, c := range r.checks {
		status := "PASS"
		if c.skipped {
			status = "SKIP"
		} else if !c.pass {
			status = "FAIL"
		}
		fmt.Printf("%-4s  %-2s  %-*s  %s\n", status, c.criterion, width, c.name, c.detail)
	}
	if len(r.findings) > 0 {
		fmt.Println()
		fmt.Println("--------------------------------- FINDINGS ------------------------------")
		for i, f := range r.findings {
			fmt.Printf("%2d. %s\n", i+1, f)
		}
	}
	fmt.Println("=========================================================================")
}

// --- the run ----------------------------------------------------------------

const (
	tmdbKey       = "differential-tmdb-key"
	omdbKey       = "differential-omdb-key"
	tvdbKey       = "differential-tvdb-key"
	fanartKey     = "differential-fanart-key"
	theaudiodbKey = "differential-theaudiodb-key"
)

func main() {
	oldBin := flag.String("old", "", "path to the 2026-09-17 binary (built from cf5da34)")
	newBin := flag.String("new", "", "path to the bundled-plugins binary (built from this tree)")
	standinBin := flag.String("standin", "", "path to the stand-in provider server binary")
	work := flag.String("work", "", "working directory (default: a fresh temp dir)")
	keep := flag.Bool("keep", false, "keep the working directory even on success")
	flag.Parse()

	if *oldBin == "" || *newBin == "" || *standinBin == "" {
		fmt.Fprintln(os.Stderr, "differential: -old, -new and -standin are all required")
		os.Exit(2)
	}

	started := time.Now()
	root := *work
	if root == "" {
		d, err := os.MkdirTemp("", "obelo-differential-")
		if err != nil {
			fatal(err)
		}
		root = d
	}
	// Absolute throughout: a Library root folder must be absolute, and a data
	// directory that is relative would be relative to the SERVER's working
	// directory rather than this driver's.
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	fmt.Printf("differential: working directory %s\n", root)

	r := &report{}
	err := run(r, root, *oldBin, *newBin, *standinBin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\ndifferential: the run did not complete: %v\n", err)
		r.add("--", "the run completes", false, "%v", err)
	}
	r.print()
	fmt.Printf("\nwall time: %s\n", time.Since(started).Round(time.Second))

	if r.failed() || err != nil {
		fmt.Printf("working directory KEPT for inspection: %s\n", root)
		os.Exit(1)
	}
	if *keep {
		fmt.Printf("working directory KEPT (-keep): %s\n", root)
		return
	}
	if *work == "" {
		_ = os.RemoveAll(root)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "differential:", err)
	os.Exit(1)
}

// standin is the running stand-in provider process.
type standin struct {
	cmd     *exec.Cmd
	base    string
	control string
}

func startStandin(bin, logPath string) (*standin, error) {
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin)
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("stand-in did not announce its ports: %w", err)
	}
	var addrs struct {
		Base    string `json:"base"`
		Control string `json:"control"`
	}
	if err := json.Unmarshal([]byte(line), &addrs); err != nil {
		return nil, fmt.Errorf("stand-in announced %q: %w", line, err)
	}
	// Keep draining stdout so the pipe can never block the process.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	return &standin{cmd: cmd, base: addrs.Base, control: addrs.Control}, nil
}

func (s *standin) stop() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
		_, _ = s.cmd.Process.Wait()
	}
}

// providerEnv is the identical environment both builds are configured with.
// Every URL points at the one stand-in; the keys are the same strings. OMDb and
// TheTVDB have no environment variables at all (config.ProviderEnvTable says so
// explicitly), so they are configured through the settings API below, on both.
func providerEnv(base string) map[string]string {
	return map[string]string{
		"OBELO_TMDB_API_KEY":         tmdbKey,
		"OBELO_TMDB_BASE_URL":        base + "/tmdb",
		"OBELO_TMDB_IMAGE_BASE_URL":  base + "/tmdbimg",
		"OBELO_MUSICBRAINZ_ENABLED":  "true",
		"OBELO_MUSICBRAINZ_BASE_URL": base + "/musicbrainz",
		"OBELO_COVERART_BASE_URL":    base + "/coverart",
		"OBELO_FANART_TV_API_KEY":    fanartKey,
		"OBELO_FANART_TV_BASE_URL":   base + "/fanart",
		"OBELO_THEAUDIODB_API_KEY":   theaudiodbKey,
		"OBELO_THEAUDIODB_BASE_URL":  base + "/theaudiodb",
	}
}

type libIDs struct{ movie, tv, music string }

// configure applies the settings neither build can take from the environment,
// and it applies them identically to both.
func configure(c *client, base string) error {
	return c.do(http.MethodPut, "/settings/metadata-providers", map[string]any{
		"providers": []map[string]any{
			{"slug": "omdb", "enabled": true, "apiKey": omdbKey, "baseURL": base + "/omdb"},
			{"slug": "thetvdb", "enabled": true, "apiKey": tvdbKey, "baseURL": base + "/thetvdb"},
		},
		// Pacing off on BOTH builds. On the 2026-09-17 build this knob reached
		// MusicBrainz only; on the bundled build it reaches every Metadata
		// provider through the fixed Settings (ADR-0059 decision 5). Zero is the
		// documented "no limit" value, which is the only setting that means the
		// same thing on both sides.
		"musicBrainzRateLimitMs": 0,
		"autoEnrichAfterScan":    true,
	}, nil)
}

func createLibraries(c *client, movies, tv, music string) (libIDs, error) {
	var ids libIDs
	mk := func(name, kind, path string) (string, error) {
		var out struct {
			ID string `json:"id"`
		}
		err := c.do(http.MethodPost, "/libraries", map[string]any{
			"name": name, "kind": kind, "rootFolders": []string{path},
		}, &out)
		return out.ID, err
	}
	var err error
	if ids.movie, err = mk("Movies", "movie", movies); err != nil {
		return ids, err
	}
	if ids.tv, err = mk("Shows", "tv", tv); err != nil {
		return ids, err
	}
	if ids.music, err = mk("Music", "music", music); err != nil {
		return ids, err
	}
	return ids, nil
}

// freshRun boots a binary on a brand-new data directory, configures it,
// scans and enriches the fixture library, and returns its snapshot.
func freshRun(bin, dataDir, logPath, base string, r *report, label string) (*snapshot, libIDs, *server, error) {
	var ids libIDs
	s, err := startServer(bin, dataDir, logPath, providerEnv(base))
	if err != nil {
		return nil, ids, nil, err
	}
	c := newClient(s.base)
	claim, err := s.claimToken()
	if err != nil {
		s.stop()
		return nil, ids, nil, err
	}
	if err := c.setupAndLogin(claim); err != nil {
		s.stop()
		return nil, ids, nil, err
	}
	if err := configure(c, base); err != nil {
		s.stop()
		return nil, ids, nil, err
	}
	movies, tv, music := fixtureRoots(dataDir)
	ids, err = createLibraries(c, movies, tv, music)
	if err != nil {
		s.stop()
		return nil, ids, nil, err
	}
	for _, lib := range []struct{ name, id string }{
		{"Movies", ids.movie}, {"Shows", ids.tv}, {"Music", ids.music},
	} {
		res, err := c.scanAndSettle(lib.id, "")
		if err != nil {
			s.stop()
			return nil, ids, nil, fmt.Errorf("%s/%s: %w", label, lib.name, err)
		}
		if res != nil {
			fmt.Printf("  %-4s %-7s pass: total=%d matched=%d unmatched=%d failed=%d retrying=%d\n",
				label, lib.name, res.Total, res.Matched, res.Unmatched, res.Failed, res.Retrying)
			if res.Failed > 0 || res.Retrying > 0 {
				r.finding("%s %s: the enrichment pass reported failed=%d retrying=%d — a stand-in answered badly or a provider errored; see the server log",
					label, lib.name, res.Failed, res.Retrying)
			}
		}
	}
	snap, err := collect(c, dataDir)
	if err != nil {
		s.stop()
		return nil, ids, nil, err
	}
	return snap, ids, s, nil
}

// fixtureRoots is where buildFixtures put the three library roots. They are
// SHARED by both builds on purpose: two copies would differ by path, and a path
// is the one thing in these records that must not be normalized away.
var sharedMovies, sharedTV, sharedMusic string

func fixtureRoots(string) (string, string, string) { return sharedMovies, sharedTV, sharedMusic }

func run(r *report, root, oldBin, newBin, standinBin string) error {
	media := filepath.Join(root, "media")
	var err error
	sharedMovies, sharedTV, sharedMusic, err = buildFixtures(media)
	if err != nil {
		return fmt.Errorf("building fixtures: %w", err)
	}
	fmt.Printf("differential: fixtures under %s\n", media)

	sd, err := startStandin(standinBin, filepath.Join(root, "standin.log"))
	if err != nil {
		return fmt.Errorf("starting the stand-in: %w", err)
	}
	defer sd.stop()
	fmt.Printf("differential: stand-in on %s (control %s)\n", sd.base, sd.control)

	// ---------------------------------------------------------------- phase 1
	fmt.Println("\n=== phase 1: the 2026-09-17 build, fresh ===")
	oldDir := filepath.Join(root, "data-old")
	oldLog := filepath.Join(root, "old.log")
	oldSnap, oldIDs, oldSrv, err := freshRun(oldBin, oldDir, oldLog, sd.base, r, "OLD")
	if err != nil {
		return fmt.Errorf("phase 1: %w", err)
	}
	oldCounters, err := readCounters(sd.control)
	if err != nil {
		oldSrv.stop()
		return err
	}
	fmt.Print(formatCounters(oldCounters))
	fmt.Println("    the requests a first pass makes, in full:")
	fmt.Print(formatPaths(oldCounters))

	// ---------------------------------------------------------------- phase 2
	fmt.Println("\n=== phase 2: the upgrade fixtures, written by the OLD build ===")
	oldClient := newClient(oldSrv.base)
	if err := oldClient.login(); err != nil {
		oldSrv.stop()
		return err
	}
	up, err := writeUpgradeFixtures(oldClient, oldIDs, sd.base, r)
	if err != nil {
		oldSrv.stop()
		return fmt.Errorf("phase 2: %w", err)
	}
	baseline, err := collect(oldClient, oldDir)
	if err != nil {
		oldSrv.stop()
		return err
	}
	oldSrv.stop()
	fmt.Println("  OLD build stopped cleanly")

	oldDB := filepath.Join(oldDir, "obelo.db")
	if up.coverartPlanted {
		if err := plantCoverArtOverride(oldDB, up.musicLibID); err != nil {
			return fmt.Errorf("phase 2: %w", err)
		}
		up.coverartOverride = true
		fmt.Println("  planted a `coverart` library_provider_override row directly (the 2026-09-17 API refuses to create one)")
	}
	before, beforeCA, err := countProviderOverrides(oldDB, up.musicLibID)
	if err != nil {
		return err
	}
	fmt.Printf("  Music Library override rows before the upgrade: %d (of which coverart: %d)\n", before, beforeCA)

	// ---------------------------------------------------------------- phase 3
	fmt.Println("\n=== phase 3: the bundled build, fresh ===")
	if err := resetCounters(sd.control); err != nil {
		return err
	}
	newDir := filepath.Join(root, "data-new")
	newLog := filepath.Join(root, "new.log")
	newSnap, _, newSrv, err := freshRun(newBin, newDir, newLog, sd.base, r, "NEW")
	if err != nil {
		return fmt.Errorf("phase 3: %w", err)
	}
	newCounters, err := readCounters(sd.control)
	if err != nil {
		newSrv.stop()
		return err
	}
	fmt.Print(formatCounters(newCounters))
	newSrv.stop()

	// --------------------------------------------------- criterion A verdicts
	fmt.Println("\n=== criterion A ===")
	assertCriterionA(r, oldSnap, newSnap, oldCounters, newCounters, oldDir, newDir)

	// ---------------------------------------------------------------- phase 4
	fmt.Println("\n=== phase 4: the bundled build, on the OLD build's data directory ===")
	if err := resetCounters(sd.control); err != nil {
		return err
	}
	upLog := filepath.Join(root, "upgraded.log")
	upSrv, err := startServer(newBin, oldDir, upLog, providerEnv(sd.base))
	if err != nil {
		return fmt.Errorf("phase 4: the bundled build would not boot on the 2026-09-17 data directory: %w", err)
	}
	defer upSrv.stop()
	upClient := newClient(upSrv.base)
	if err := upClient.login(); err != nil {
		return fmt.Errorf("phase 4 login: %w", err)
	}
	// Boot-time work (bundled install, the Cover Art migration) is done by the
	// time /api/v1/server answers, but give the first enrichment scheduler tick
	// a moment so anything it would do lands inside the counters we read.
	time.Sleep(2 * time.Second)

	fmt.Println("  rescanning and enriching the way an Admin would (default mode)")
	for _, lib := range []struct{ name, id string }{
		{"Movies", oldIDs.movie}, {"Shows", oldIDs.tv}, {"Music", oldIDs.music},
	} {
		res, err := upClient.scanAndSettle(lib.id, "")
		if err != nil {
			return fmt.Errorf("phase 4 %s: %w", lib.name, err)
		}
		if res != nil {
			fmt.Printf("  UP   %-7s pass: total=%d matched=%d unmatched=%d failed=%d retrying=%d\n",
				lib.name, res.Total, res.Matched, res.Unmatched, res.Failed, res.Retrying)
		}
	}
	upCounters, err := readCounters(sd.control)
	if err != nil {
		return err
	}
	fmt.Println("  provider requests during the upgraded boot + pass:")
	fmt.Print(formatCounters(upCounters))
	fmt.Print(formatPaths(upCounters))

	after, err := collect(upClient, oldDir)
	if err != nil {
		return err
	}
	assertCriterionB(r, baseline, after, upCounters, up, oldDir)
	return nil
}

// --- criterion A ------------------------------------------------------------

func assertCriterionA(r *report, oldSnap, newSnap *snapshot,
	oldC, newC *counterSnapshot, oldDir, newDir string) {

	if len(oldC.Unmatched) > 0 || len(newC.Unmatched) > 0 {
		r.add("A", "the stand-in covered every request", false,
			"OLD unmatched=%v NEW unmatched=%v", oldC.Unmatched, newC.Unmatched)
		r.finding("the stand-in did not cover every request either build made; a request it answered 404 to is a hole in the harness, NOT evidence about a build. OLD: %v NEW: %v",
			oldC.Unmatched, newC.Unmatched)
	} else {
		r.add("A", "the stand-in covered every request", true,
			"no unmatched paths in either run (%d + %d requests)", oldC.Total, newC.Total)
	}

	oldText := normalize(oldSnap, map[string]any{"libraries": toAnyList(oldSnap.Libraries)})
	newText := normalize(newSnap, map[string]any{"libraries": toAnyList(newSnap.Libraries)})
	_ = oldDir
	_ = newDir
	d, n := diffText("OLD", oldText, "NEW", newText)
	if n == 0 {
		r.add("A", "every enriched record is identical", true,
			"%d normalized lines, byte-identical", len(strings.Split(oldText, "\n")))
	} else {
		r.add("A", "every enriched record is identical", false, "%d differing lines", n)
		r.finding("the enriched records DIFFER between the two builds (%d lines). The diff follows in full:\n%s", n, d)
	}

	cd, cn := diffCounters("OLD", oldC.ByPath, "NEW", newC.ByPath)
	if cn == 0 {
		r.add("A", "every provider request path matches", true,
			"%d distinct paths, identical multisets", len(oldC.ByPath))
	} else {
		r.add("A", "every provider request path matches", false, "%d differing paths", cn)
		r.finding("the two builds made DIFFERENT provider requests (%d paths differ):\n%s", cn, cd)
	}
}

// --- criterion B ------------------------------------------------------------

// upgradeFixtures is what the OLD build wrote before it was stopped, so the
// upgraded build can be asked whether it still has it.
type upgradeFixtures struct {
	providers       []map[string]any
	moviePolicy     map[string]any
	musicPolicy     map[string]any
	showPin         any
	showName        string
	coverartBaseURL string
	musicLibID      string
	// coverartOverride is true once a `library_provider_override` row naming
	// `coverart` exists on the Music Library, however it got there;
	// coverartPlanted says it had to be written directly because the
	// 2026-09-17 API refuses to create one (see coverartrow.go).
	coverartOverride bool
	coverartPlanted  bool
}

func writeUpgradeFixtures(c *client, ids libIDs, base string, r *report) (*upgradeFixtures, error) {
	up := &upgradeFixtures{coverartBaseURL: base + "/coverart"}

	// (1) A NON-DEFAULT Cover Art base URL, written through the settings API so
	// it is an operator's row and not merely an environment seed.
	if err := c.do(http.MethodPut, "/settings/metadata-providers", map[string]any{
		"providers": []map[string]any{
			{"slug": "coverart", "enabled": true, "baseURL": up.coverartBaseURL},
		},
	}, nil); err != nil {
		r.finding("the 2026-09-17 build refused a settings write for the `coverart` provider row (%v); the Cover Art migration is still exercised through the row the first boot seeded from OBELO_COVERART_BASE_URL, which is non-default by construction", err)
	}

	// (2) A per-Library override that names omdb, on the movie Library.
	if err := c.do(http.MethodPut, "/libraries/"+ids.movie+"/enrichment-policy", map[string]any{
		"providerOverrides": map[string]any{"omdb": false},
	}, nil); err != nil {
		return nil, fmt.Errorf("setting the omdb override: %w", err)
	}
	// (3) A per-Library override that names coverart, ALONGSIDE one that does
	// not — so the migration's "scrub the coverart row, keep the Library's other
	// keys" can be told apart from "drop the Library's overrides".
	if err := c.do(http.MethodPut, "/libraries/"+ids.music+"/enrichment-policy", map[string]any{
		"providerOverrides": map[string]any{"coverart": false, "theaudiodb": false},
	}, nil); err != nil {
		r.finding("THE 2026-09-17 BUILD'S OWN API REFUSES TO CREATE A PER-LIBRARY OVERRIDE NAMING `coverart`: %v. `Catalog.SupplementProvidersForKind` offers only providers with RequiresKey, and the Cover Art Archive is keyless, so no server driven through its API can hold such a row and issue 06's scrub of it is DEFENSIVE rather than load-bearing. The harness plants the row directly instead, with the server stopped, so the scrub is still exercised — see the SKIP/PASS line and coverartrow.go.", err)
		up.coverartPlanted = true
		// Still set the sibling override, so the surviving-key half is real.
		if err2 := c.do(http.MethodPut, "/libraries/"+ids.music+"/enrichment-policy", map[string]any{
			"providerOverrides": map[string]any{"theaudiodb": false},
		}, nil); err2 != nil {
			return nil, fmt.Errorf("setting the theaudiodb override: %w", err2)
		}
	} else {
		up.coverartOverride = true
	}
	up.musicLibID = ids.music

	// (4) An item pin, the way the Edit-item flow writes one: a Show pinned to
	// an authoritative external id. A Show is used because a Show's pin is the
	// one the browse API shows back (showSummaryJSON.enrichmentOverride).
	var shows map[string]any
	if err := c.do(http.MethodGet, "/libraries/"+ids.tv+"/titles", nil, &shows); err != nil {
		return nil, err
	}
	list := asList(shows["shows"])
	if len(list) == 0 {
		return nil, errors.New("no Show to pin — the TV fixture did not scan")
	}
	show := asMap(list[0])
	up.showName = fmt.Sprint(show["title"])
	if err := c.do(http.MethodPut, "/shows/"+fmt.Sprint(show["id"])+"/enrichmentOverride", map[string]any{
		"externalId": "1396",
	}, nil); err != nil {
		return nil, fmt.Errorf("pinning the Show: %w", err)
	}

	// Every one of those writes can kick off a pass; let them all settle before
	// the baseline is taken.
	for _, id := range []string{ids.movie, ids.tv, ids.music} {
		if _, err := c.waitEnrich(id); err != nil {
			return nil, err
		}
	}

	// Read back what we just wrote, so the assertions compare like with like.
	var provResp map[string]any
	if err := c.do(http.MethodGet, "/settings/metadata-providers", nil, &provResp); err != nil {
		return nil, err
	}
	for _, p := range asList(provResp["providers"]) {
		up.providers = append(up.providers, asMap(p))
	}
	if err := c.do(http.MethodGet, "/libraries/"+ids.movie+"/enrichment-policy", nil, &up.moviePolicy); err != nil {
		return nil, err
	}
	if err := c.do(http.MethodGet, "/libraries/"+ids.music+"/enrichment-policy", nil, &up.musicPolicy); err != nil {
		return nil, err
	}
	// Read the pin back from /shows/{id}/seasons, NOT from the library listing:
	// only handleShowSeasons decorates a Show with its entity enrichment, so the
	// listing's row carries no enrichmentOverride on either build.
	var seasonsAfter map[string]any
	if err := c.do(http.MethodGet, "/shows/"+fmt.Sprint(show["id"])+"/seasons", nil, &seasonsAfter); err != nil {
		return nil, err
	}
	up.showPin = asMap(seasonsAfter["show"])["enrichmentOverride"]
	fmt.Printf("  wrote: coverart baseURL=%s, omdb override on Movies, coverart+theaudiodb overrides on Music, Show pin=%v\n",
		up.coverartBaseURL, up.showPin)
	return up, nil
}

// supplementOverride digs the override state for one provider slug out of an
// enrichment-policy response (the read side of providerOverrides is the
// supplements[] array).
func supplementOverride(policy map[string]any, slug string) (any, bool) {
	for _, s := range asList(policy["supplements"]) {
		m := asMap(s)
		if fmt.Sprint(m["slug"]) == slug {
			return m["override"], true
		}
	}
	return nil, false
}

func assertCriterionB(r *report, baseline, after *snapshot, c *counterSnapshot,
	up *upgradeFixtures, dataDir string) {

	// --- every provider key row intact -------------------------------------
	oldRows := map[string]map[string]any{}
	for _, p := range up.providers {
		oldRows[fmt.Sprint(p["slug"])] = p
	}
	newRows := map[string]map[string]any{}
	for _, p := range after.Providers {
		newRows[fmt.Sprint(p["slug"])] = p
	}
	var lost []string
	for slug, old := range oldRows {
		if slug == "coverart" {
			continue
		}
		nw, ok := newRows[slug]
		if !ok {
			lost = append(lost, slug+" (row gone)")
			continue
		}
		for _, f := range []string{"enabled", "hasKey", "baseURL"} {
			if fmt.Sprint(old[f]) != fmt.Sprint(nw[f]) {
				lost = append(lost, fmt.Sprintf("%s.%s %v→%v", slug, f, old[f], nw[f]))
			}
		}
	}
	sort.Strings(lost)
	if len(lost) == 0 {
		r.add("B", "every provider key row survives", true,
			"%d rows, enabled+hasKey+baseURL unchanged", len(oldRows)-1)
	} else {
		r.add("B", "every provider key row survives", false, "%s", strings.Join(lost, "; "))
		r.finding("provider rows changed across the upgrade: %s", strings.Join(lost, "; "))
	}

	// --- the Cover Art Archive row is gone ---------------------------------
	if _, still := newRows["coverart"]; still {
		r.add("B", "the coverart provider row is gone", false, "a `coverart` row is still listed")
		r.finding("the upgraded server still lists a `coverart` metadata provider row")
	} else {
		r.add("B", "the coverart provider row is gone", true, "no `coverart` row")
	}

	// --- musicbrainz URL2 carries the mirrored Cover Art host ---------------
	mb := newRows["musicbrainz"]
	got := fmt.Sprint(mb["imageBaseURL"])
	if got == up.coverartBaseURL {
		r.add("B", "musicbrainz URL2 is the mirrored Cover Art host", true, "%s", got)
	} else {
		r.add("B", "musicbrainz URL2 is the mirrored Cover Art host", false,
			"want %s, got %s", up.coverartBaseURL, got)
		r.finding("the Cover Art base URL the operator had set (%s) did not land in musicbrainz.imageBaseURL (%s)",
			up.coverartBaseURL, got)
	}

	// --- the per-Library overrides -----------------------------------------
	newMovie := after.rawPolicies["Movies"]
	newMusic := after.rawPolicies["Music"]

	wantOMDb, _ := supplementOverride(up.moviePolicy, "omdb")
	gotOMDb, present := supplementOverride(newMovie, "omdb")
	if present && fmt.Sprint(wantOMDb) == fmt.Sprint(gotOMDb) && fmt.Sprint(gotOMDb) == "false" {
		r.add("B", "the omdb per-Library override survives", true, "Movies: omdb override = false")
	} else {
		r.add("B", "the omdb per-Library override survives", false,
			"want false, got %v (present=%v)", gotOMDb, present)
		r.finding("the per-Library `omdb` override on the Movies Library did not survive the upgrade: want false, got %v", gotOMDb)
	}

	gotADB, adbPresent := supplementOverride(newMusic, "theaudiodb")
	if adbPresent && fmt.Sprint(gotADB) == "false" {
		r.add("B", "the Library's other override survives the scrub", true,
			"Music: theaudiodb override = false")
	} else {
		r.add("B", "the Library's other override survives the scrub", false,
			"want false, got %v (present=%v)", gotADB, adbPresent)
		r.finding("the sibling `theaudiodb` override on the Music Library did not survive the Cover Art migration: want false, got %v", gotADB)
	}

	// The scrub is asserted where it happens — the `library_provider_override`
	// table — because the API view cannot distinguish "the row was scrubbed"
	// from "the provider left the catalog, so no supplement row is rendered".
	total, coverart, dbErr := countProviderOverrides(filepath.Join(dataDir, "obelo.db"), up.musicLibID)
	how := "written through the API"
	if up.coverartPlanted {
		how = "planted directly; the 2026-09-17 API refuses to create one"
	}
	switch {
	case !up.coverartOverride:
		r.skip("B", "the coverart per-Library override is scrubbed",
			"no `coverart` override row could be created at all")
	case dbErr != nil:
		r.add("B", "the coverart per-Library override is scrubbed", false, "%v", dbErr)
	case coverart != 0:
		r.add("B", "the coverart per-Library override is scrubbed", false,
			"%d `coverart` rows remain on the Music Library", coverart)
		r.finding("the Music Library still holds %d `library_provider_override` row(s) naming `coverart` after the upgrade", coverart)
	case total == 0:
		r.add("B", "the coverart per-Library override is scrubbed", false,
			"the scrub took the Library's OTHER override rows with it (0 rows left)")
		r.finding("the Cover Art migration removed every `library_provider_override` row for the Music Library, not only the `coverart` one")
	default:
		r.add("B", "the coverart per-Library override is scrubbed", true,
			"0 coverart rows, %d sibling row(s) survive (%s)", total, how)
	}

	// --- the item pin -------------------------------------------------------
	var gotPin any
	for _, lib := range after.Libraries {
		if fmt.Sprint(lib["kind"]) != "tv" {
			continue
		}
		if l := asList(lib["shows"]); len(l) > 0 {
			gotPin = asMap(asMap(l[0])["show"])["enrichmentOverride"]
		}
	}
	wantPin, _ := json.Marshal(up.showPin)
	havePin, _ := json.Marshal(gotPin)
	switch {
	case up.showPin == nil:
		r.skip("B", "the item pin survives",
			"the 2026-09-17 build did not surface the Show pin on its browse row, so there is nothing to compare")
	case string(wantPin) == string(havePin):
		r.add("B", "the item pin survives", true, "Show %q pin = %s", up.showName, havePin)
	default:
		r.add("B", "the item pin survives", false, "want %s, got %s", wantPin, havePin)
		r.finding("the Show pin written by the 2026-09-17 build changed across the upgrade: %s → %s", wantPin, havePin)
	}

	// --- seven bundled plugins ---------------------------------------------
	bundled := 0
	var names []string
	for _, p := range after.Plugins {
		if fmt.Sprint(p["origin"]) == "bundled" {
			bundled++
			names = append(names, fmt.Sprint(p["id"]))
		}
	}
	if bundled == 7 {
		r.add("B", "seven plugins with origin bundled", true, "%s", strings.Join(names, " "))
	} else {
		r.add("B", "seven plugins with origin bundled", false,
			"%d bundled of %d installed: %s", bundled, len(after.Plugins), strings.Join(names, " "))
	}

	// --- every previously-enriched record byte-identical --------------------
	beforeText := normalize(baseline, map[string]any{"libraries": toAnyList(baseline.Libraries)})
	afterText := normalize(after, map[string]any{"libraries": toAnyList(after.Libraries)})
	// The enrichment-policy block is part of the libraries doc and legitimately
	// differs: the coverart supplement leaves the list. Records are compared
	// with the policy blocks removed, and the policy is asserted above.
	beforeText = stripPolicies(beforeText)
	afterText = stripPolicies(afterText)
	d, n := diffText("BEFORE", beforeText, "AFTER", afterText)
	if n == 0 {
		r.add("B", "every enriched record is unchanged", true,
			"%d normalized lines, byte-identical", len(strings.Split(beforeText, "\n")))
	} else {
		r.add("B", "every enriched record is unchanged", false, "%d differing lines", n)
		r.finding("records changed across the upgrade (%d lines):\n%s", n, d)
	}

	// --- nothing re-enriches ------------------------------------------------
	if c.Total == 0 {
		r.add("B", "nothing re-enriches", true, "0 provider requests during boot and the pass")
	} else {
		r.add("B", "nothing re-enriches", false, "%d provider requests", c.Total)
		keys := make([]string, 0, len(c.ByPath))
		for k := range c.ByPath {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&b, "    %s ×%d\n", k, c.ByPath[k])
		}
		r.finding("the upgraded server made %d provider requests where it should have made none:\n%s", c.Total, b.String())
	}

}

// stripPolicies removes the enrichmentPolicy block from a normalized snapshot
// text. It is a line filter rather than a tree edit because the text is already
// canonical and indentation makes the block's extent unambiguous.
func stripPolicies(text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	skipIndent := -1
	for _, ln := range lines {
		indent := len(ln) - len(strings.TrimLeft(ln, " "))
		if skipIndent >= 0 {
			if indent > skipIndent {
				continue
			}
			skipIndent = -1
		}
		if strings.HasPrefix(strings.TrimSpace(ln), `"enrichmentPolicy":`) {
			skipIndent = indent
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}
