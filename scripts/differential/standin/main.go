// Command standin is the local stand-in for every metadata source the Obelo
// server calls, for the differential run described in
// scripts/differential/README.md.
//
// It exists to answer one question honestly: does the bundled-plugin build
// (.scratch/bundled-plugins issue 08) enrich a movie, an episode and an album
// into the SAME records, from the SAME requests, as the 2026-09-17 build did?
// Both builds are pointed at this one process, so "the same" is a diff rather
// than an argument.
//
// # One listener, on purpose
//
// Every provider is mounted as a path prefix on a SINGLE loopback listener
// rather than one listener per source. That is not laziness, it is the only
// shape that works: the plugin host permits a guest's fetch when the host is
// either in the plugin's manifest allowlist or is the host of the URL the
// operator configured (internal/plugins/hostfuncs.go, `operatorChose`). A
// second host — MusicBrainz's Cover Art Archive, which is Settings.URL2 since
// issue 06 — is NOT the operator's configured target, so a stand-in for it on
// its own port would be refused by the allowlist AND by the private-address
// rule. Sharing one host:port makes every mount the operator's own target, and
// the comparison is about records and request paths, not about ports.
//
// # Counting
//
// Every request is counted twice: once against its provider and once against
// its normalized method+path+query, with credentials redacted. The counters are
// read and zeroed over a SEPARATE control listener so that reading them never
// shows up in them.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// standinBase is this process's own "http://127.0.0.1:<port>", filled in once
// the provider listener is bound. Canned JSON embeds the literal token %%BASE%%
// wherever the real API would carry an absolute URL, and writeRawJSON swaps it
// for this — so an artwork URL a provider hands the server points back here.
var standinBase string

// --- counters ---------------------------------------------------------------

// counters is the whole point of the process. byProvider answers "did the new
// build call OMDb at all?"; byPath answers "did it call it the same way?".
type counters struct {
	mu         sync.Mutex
	byProvider map[string]int
	byPath     map[string]int
	unmatched  []string
}

var hits = counters{byProvider: map[string]int{}, byPath: map[string]int{}}

// record counts one request. key is "<provider> <METHOD> <path>[?<query>]" with
// credentials redacted, which is the granularity the differential compares as a
// multiset: it is specific enough that a dropped `inc=` parameter shows up, and
// general enough that an API key that differs between the two runs does not.
func (c *counters) record(provider, method, path string, q url.Values) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byProvider[provider]++
	c.byPath[provider+" "+method+" "+path+redactedQuery(q)]++
}

func (c *counters) noteUnmatched(provider, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unmatched = append(c.unmatched, provider+" "+detail)
}

func (c *counters) snapshot() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	byProvider := map[string]int{}
	for k, v := range c.byProvider {
		byProvider[k] = v
	}
	byPath := map[string]int{}
	for k, v := range c.byPath {
		byPath[k] = v
	}
	un := append([]string(nil), c.unmatched...)
	sort.Strings(un)
	total := 0
	for _, v := range byProvider {
		total += v
	}
	return map[string]any{
		"total":      total,
		"byProvider": byProvider,
		"byPath":     byPath,
		"unmatched":  un,
	}
}

func (c *counters) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byProvider = map[string]int{}
	c.byPath = map[string]int{}
	c.unmatched = nil
}

// secretParams are query parameters whose VALUE is a credential. They are kept
// in the key (so "called without a key" is still distinguishable from "called
// with one") but their value is replaced, because the two builds are configured
// with the same key and a differing one would be a diff about nothing.
var secretParams = map[string]bool{
	"apikey":    true,
	"api_key":   true,
	"client":    true, // AniDB's registered client name
	"clientver": true,
}

// redactedQuery renders a query string deterministically (sorted keys, sorted
// values) with credential values replaced by "<redacted>".
func redactedQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('?')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		if secretParams[strings.ToLower(k)] {
			b.WriteString("<redacted>")
			continue
		}
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		b.WriteString(strings.Join(vs, ","))
	}
	return b.String()
}

// --- helpers the per-provider files use -------------------------------------

// writeRawJSON writes body as application/json, swapping the %%BASE%% token for
// this process's own base URL so canned JSON can carry absolute links back here.
func writeRawJSON(w http.ResponseWriter, status int, body string) {
	out := strings.ReplaceAll(body, "%%BASE%%", standinBase)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(out))
}

// standinJPEG is a tiny deterministic JPEG. The server really downloads and
// caches provider artwork, so an artwork URL has to answer with bytes an image
// decoder accepts, and the same bytes on both runs.
var standinJPEG = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: uint8(16 * x), G: uint8(16 * y), B: 0x40, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}()

// writeStandinImage answers an artwork request with the canned JPEG.
func writeStandinImage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", fmt.Sprint(len(standinJPEG)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(standinJPEG)
}

// unmatched answers — and LOUDLY records — a request no canned response covers.
// An unmatched request is a hole in the stand-in, not a finding about a build,
// and the differential driver fails the run if either build produces one, so
// that a hole can never be mistaken for agreement.
func unmatched(w http.ResponseWriter, r *http.Request, provider string) {
	detail := r.Method + " " + r.URL.Path + redactedQuery(r.URL.Query())
	hits.noteUnmatched(provider, detail)
	log.Printf("standin: UNMATCHED %s %s", provider, detail)
	writeRawJSON(w, http.StatusNotFound,
		`{"__unmatched":`+mustJSONString(detail)+`}`)
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// --- mounting ---------------------------------------------------------------

// mount wraps a provider handler with the counter and strips the mount prefix,
// so a handler sees the path the REAL API would see.
func mount(mux *http.ServeMux, prefix, provider string, h http.HandlerFunc) {
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		if rest == "" {
			rest = "/"
		}
		hits.record(provider, r.Method, rest, r.URL.Query())
		r2 := r.Clone(r.Context())
		r2.URL.Path = rest
		h(w, r2)
	}
	mux.HandleFunc(prefix, wrapped)
	mux.HandleFunc(prefix+"/", wrapped)
}

func imageMount(w http.ResponseWriter, r *http.Request) { writeStandinImage(w) }

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "provider listener")
	control := flag.String("control", "127.0.0.1:0", "control listener (counters)")
	flag.Parse()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("standin: listen: %v", err)
	}
	cln, err := net.Listen("tcp", *control)
	if err != nil {
		log.Fatalf("standin: control listen: %v", err)
	}
	standinBase = "http://" + ln.Addr().String()

	mux := http.NewServeMux()
	mount(mux, "/tmdb", "tmdb", tmdbHandler)
	mount(mux, "/omdb", "omdb", omdbHandler)
	mount(mux, "/thetvdb", "thetvdb", thetvdbHandler)
	mount(mux, "/musicbrainz", "musicbrainz", musicbrainzHandler)
	mount(mux, "/coverart", "coverart", coverartHandler)
	mount(mux, "/fanart", "fanarttv", fanarttvHandler)
	mount(mux, "/theaudiodb", "theaudiodb", theaudiodbHandler)
	// Two image mounts: TMDB hands out RELATIVE file paths that the server
	// prefixes with the configured image base URL, and everybody else hands out
	// absolute URLs built from %%BASE%%. They are counted separately so an
	// artwork re-download shows up as the provider whose CDN it was.
	mount(mux, "/tmdbimg", "tmdb-images", imageMount)
	mount(mux, "/img", "images", imageMount)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hits.record("unknown", r.Method, r.URL.Path, r.URL.Query())
		unmatched(w, r, "unknown")
	})

	cmux := http.NewServeMux()
	cmux.HandleFunc("/counters", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hits.snapshot())
	})
	cmux.HandleFunc("/reset", func(w http.ResponseWriter, r *http.Request) {
		hits.reset()
		w.WriteHeader(http.StatusNoContent)
	})
	cmux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// The driver reads this line to learn both ports, so it must be the FIRST
	// thing on stdout and must be flushed before anything else is logged.
	out, _ := json.Marshal(map[string]string{
		"base":    standinBase,
		"control": "http://" + cln.Addr().String(),
	})
	fmt.Println(string(out))
	os.Stdout.Sync()

	go func() { _ = http.Serve(cln, cmux) }()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		_ = ln.Close()
		_ = cln.Close()
		os.Exit(0)
	}()

	if err := http.Serve(ln, mux); err != nil {
		log.Printf("standin: serve: %v", err)
	}
}
