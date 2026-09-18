//go:build wasm

// An Obelo Plugin, whole, in one file: an Event sink AND a Metadata provider.
//
// One module may fill more than one seam — the loader looks up only the exports
// it needs, and the manifest's `provides` list is what says which — so the suite
// gets both Extension points out of one compile. The sink half is everything down
// to `mode`; the Metadata provider half (issue 11) is the fenced section below it.
//
// This is the guest half of the plugin system: a WebAssembly module that is told
// when something finished on a server and posts a signed document about it, and
// that answers what a Title is when the enrichment pass asks. The suite builds it
// from this source at test time
// (`GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared`), so the loader is
// exercised by a real module rather than a mock — and so an author can read the
// smallest complete example there is.
//
// # The ABI, in full
//
// Exports this file provides:
//
//	obelo_alloc(size u32) -> ptr u32   the host asks for a buffer to write into
//	obelo_free(ptr u32)                the host gives one back
//	deliver(ptr u32, len u32) -> i64   one event in, one answer out, as
//	                                   (ptr<<32 | len) of the response JSON — or 0
//	last_error() -> i64                why the last call answered 0
//
// Imports the host provides, in module "obelo":
//
//	http_fetch(ptr u32, len u32) -> i64   the ONLY way out of the sandbox
//	log(level u32, ptr u32, len u32)      a line in the server log
//
// The guest owns every buffer on both sides. The host never invents a pointer: it
// calls obelo_alloc, writes, and frees — which is why `pinned` can refuse a
// pointer it did not hand out, and why the fetch response comes back as an entry
// this guest already has.
//
// # What it is NOT
//
// There is no filesystem, no environment, no socket and no stdout in here, and
// none is missing: a Plugin's whole job is to answer the call it was given. The
// four misbehaving modes at the bottom exist ONLY so one module can play every
// part the test suite needs; delete them and what is left is the example.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unsafe"
)

func main() {}

// --- the shapes, copied from the contract's JSON schema ----------------------
//
// An author in another language reads pluginapi/v1/pluginapi.schema.json and
// declares these in their own. Only the fields this Plugin uses are here: the
// contract leaves additionalProperties open on every type precisely so a guest
// may ignore what it does not need.

type settings struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

type deliverRequest struct {
	// The event is kept as RAW JSON and signed as received, so the bytes this
	// Plugin signs are exactly the bytes it posts. Re-encoding it would sign one
	// document and send another.
	Event    json.RawMessage `json:"event"`
	Settings settings        `json:"settings"`
}

type deliverResponse struct {
	Delivered bool   `json:"delivered"`
	Error     string `json:"error,omitempty"`
}

type header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type fetchRequest struct {
	Method  string   `json:"method,omitempty"`
	URL     string   `json:"url"`
	Headers []header `json:"headers,omitempty"`
	Body    []byte   `json:"body,omitempty"`
}

type fetchResponse struct {
	Status  int      `json:"status,omitempty"`
	Headers []header `json:"headers,omitempty"`
	Body    []byte   `json:"body,omitempty"`
	Refused string   `json:"refused,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// --- what this Plugin actually does ------------------------------------------

//go:wasmexport deliver
func deliver(ptr, n uint32) uint64 {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail("the host passed a pointer this guest did not allocate")
	}
	var req deliverRequest
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		return fail("the request is not a SinkDeliverRequest: " + err.Error())
	}
	if answer, misbehaving := misbehave(req); misbehaving {
		return answer
	}
	return reply(post(req))
}

// post signs the event and sends it to the URL the Admin configured. The
// signature is HMAC-SHA256 over the exact bytes of the body, hex, under the
// secret the host handed over with this call — so a receiving script can reject
// anything this Plugin did not send.
func post(req deliverRequest) deliverResponse {
	body := []byte(req.Event)
	mac := hmac.New(sha256.New, []byte(req.Settings.Secret))
	mac.Write(body)

	resp := fetch(fetchRequest{
		Method: "POST",
		URL:    req.Settings.URL,
		Headers: []header{
			{Name: "Content-Type", Value: "application/json"},
			{Name: "X-Obelo-Signature", Value: "sha256=" + hex.EncodeToString(mac.Sum(nil))},
		},
		Body: body,
	})

	switch {
	case resp.Refused != "":
		// The host would not make this request and will refuse it again, so there
		// is nothing to retry and nothing to be clever about.
		logLine(levelError, "the host refused the request: "+resp.Refused)
		return deliverResponse{Error: "refused by the host: " + resp.Refused}
	case resp.Error != "":
		return deliverResponse{Error: "the request failed: " + resp.Error}
	case resp.Status < 200 || resp.Status > 299:
		return deliverResponse{Error: "the target answered " + itoa(resp.Status)}
	}
	logLine(levelInfo, "delivered one document")
	return deliverResponse{Delivered: true}
}

// --- the ABI glue an author writes once --------------------------------------

// pinned keeps every buffer the host holds a pointer to reachable, and is how a
// host-supplied pointer becomes a slice again without pointer arithmetic: a
// pointer that did not come out of alloc simply is not in here.
var pinned = map[uint32][]byte{}

//go:wasmexport obelo_alloc
func alloc(size uint32) uint32 {
	if size == 0 {
		size = 1
	}
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(unsafe.SliceData(buf))))
	pinned[ptr] = buf
	return ptr
}

//go:wasmexport obelo_free
func free(ptr uint32) {
	delete(pinned, ptr)
}

var lastError []byte

//go:wasmexport last_error
func lastErrorFn() uint64 {
	if len(lastError) == 0 {
		return 0
	}
	return emit(lastError)
}

// emit copies b into a fresh guest buffer and packs its pointer and length into
// the single i64 a wasm export may return.
func emit(b []byte) uint64 {
	ptr := alloc(uint32(len(b)))
	copy(pinned[ptr], b)
	return uint64(ptr)<<32 | uint64(len(b))
}

func fail(msg string) uint64 {
	lastError = []byte(msg)
	return 0
}

func reply(v any) uint64 {
	out, err := json.Marshal(v)
	if err != nil {
		return fail(err.Error())
	}
	lastError = nil
	return emit(out)
}

//go:wasmimport obelo http_fetch
func hostFetch(ptr, n uint32) uint64

//go:wasmimport obelo log
func hostLog(level, ptr, n uint32)

const (
	levelInfo  uint32 = 1
	levelError uint32 = 3
)

// fetch asks the HOST to perform a request. The answer comes back in a buffer the
// host obtained from alloc, so it is already one of ours.
func fetch(req fetchRequest) fetchResponse {
	in, err := json.Marshal(req)
	if err != nil {
		return fetchResponse{Error: err.Error()}
	}
	ptr := alloc(uint32(len(in)))
	copy(pinned[ptr], in)
	packed := hostFetch(ptr, uint32(len(in)))
	free(ptr)

	if packed == 0 {
		return fetchResponse{Error: "the host answered nothing"}
	}
	rptr, rlen := uint32(packed>>32), uint32(packed)
	buf, ok := pinned[rptr]
	if !ok || uint32(len(buf)) < rlen {
		return fetchResponse{Error: "the host answered with a buffer this guest does not hold"}
	}
	var resp fetchResponse
	uerr := json.Unmarshal(buf[:rlen], &resp)
	free(rptr)
	if uerr != nil {
		return fetchResponse{Error: uerr.Error()}
	}
	return resp
}

// logLine writes one line to the server log, prefixed by the host with this
// Plugin's id. It is why a guest needs no stdout.
func logLine(level uint32, msg string) {
	b := []byte(msg)
	ptr := alloc(uint32(len(b)))
	copy(pinned[ptr], b)
	hostLog(level, ptr, uint32(len(b)))
	free(ptr)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d [8]byte
	i := len(d)
	for n > 0 {
		i--
		d[i] = byte('0' + n%10)
		n /= 10
	}
	return string(d[i:])
}

// --- the four parts this one module plays for the suite ----------------------
//
// EVERYTHING BELOW THIS LINE IS TEST SCAFFOLDING, not an example. A real Plugin
// has none of it: the suite needs a guest that panics, one that never returns and
// two that reach for hosts they were not given, and building four modules to get
// them would be four compiles and four things to keep in step. The mode is read
// out of the target URL the test configures, which is the one thing a test can
// set that reaches this far in.

var spun uint64

func misbehave(req deliverRequest) (uint64, bool) {
	switch mode(req.Settings.URL) {
	case "panic":
		panic("this plugin fails on every call, on purpose")
	case "hang":
		// Never returns. The host's deadline has to stop it, because nothing in
		// here is going to.
		for {
			spun++
		}
	case "forbidden":
		return reply(reportRefusal(fetch(fetchRequest{URL: "https://not-allowed.example.test/steal"}))), true
	case "metadata":
		// A host the manifest DOES allowlist, whose address is one no plugin may
		// reach. The allowlist is a claim; the address rule is the answer.
		return reply(reportRefusal(fetch(fetchRequest{URL: "http://169.254.169.254/latest/meta-data/"}))), true
	}
	return 0, false
}

// reportRefusal hands whatever the host said back through the delivery answer, so
// a test can see the refusal a guest actually received.
func reportRefusal(resp fetchResponse) deliverResponse {
	if resp.Refused != "" {
		return deliverResponse{Error: "refused: " + resp.Refused}
	}
	if resp.Error != "" {
		return deliverResponse{Error: "error: " + resp.Error}
	}
	return deliverResponse{Error: "the host allowed a fetch it should have refused, answering " + itoa(resp.Status)}
}

// =============================================================================
// THE METADATA PROVIDER HALF (.scratch/plugin-system issue 11)
//
// One module can fill two seams: the loader only looks up the exports it needs,
// and the manifest's `provides` list is what says which. Everything from here to
// the end of the file is the Metadata provider Extension point — eight exports,
// three of them mandatory — plus the two host functions a provider uses that a
// sink does not.
//
// The example an author copies is the shapes and the ABI glue. The `obelo-mode=`
// switch inside metadata_lookup is test scaffolding, exactly as `misbehave` above
// is: one module has to play every part the suite needs, and the mode arrives in
// the url the test configures because that is the one thing a test can set that
// reaches this far in.
//
// Exports:
//
//	metadata_lookup(ptr, len) -> i64              a LookupRequest in, a LookupResponse out
//	metadata_search(ptr, len) -> i64              search, behind capability "search"
//	metadata_artwork_candidates(ptr, len) -> i64  behind capability "artwork-candidates"
//	metadata_external_ref(ptr, len) -> i64        behind capability "external-ref"
//
// (episode-list and album-tracklist are not exported here: a module that does not
// export an optional call is told "unavailable" by the host, which is the same
// thing an undeclared capability answers, and NOT exporting them is how this file
// demonstrates that.)
//
// Imports used only by this half, in module "obelo":
//
//	settings_get() -> i64          the Settings the host resolved for THIS call
//	kv_get(ptr, len) -> i64        this Plugin's own namespace
//	kv_set(ptr, len) -> i64
//	kv_delete(ptr, len) -> i64
// =============================================================================

//go:wasmimport obelo settings_get
func hostSettingsGet() uint64

//go:wasmimport obelo kv_get
func hostKVGet(ptr, n uint32) uint64

//go:wasmimport obelo kv_set
func hostKVSet(ptr, n uint32) uint64

//go:wasmimport obelo kv_delete
func hostKVDelete(ptr, n uint32) uint64

// --- the shapes, copied from the contract's JSON schema ----------------------

type mediaRef struct {
	Kind   string `json:"kind"`
	Title  string `json:"title,omitempty"`
	Year   int    `json:"year,omitempty"`
	Artist string `json:"artist,omitempty"`
	Album  string `json:"album,omitempty"`
	Track  string `json:"track,omitempty"`
}

type artworkRef struct {
	Role string `json:"role"`
	URL  string `json:"url"`
}

type metadataRecord struct {
	Matched    bool         `json:"matched"`
	Name       string       `json:"name,omitempty"`
	Year       int          `json:"year,omitempty"`
	Overview   string       `json:"overview,omitempty"`
	Genres     []string     `json:"genres,omitempty"`
	Artwork    []artworkRef `json:"artwork,omitempty"`
	ExternalID string       `json:"externalId,omitempty"`
	Source     string       `json:"source,omitempty"`
	FromSearch bool         `json:"fromSearch,omitempty"`
}

type lookupRequest struct {
	Ref mediaRef `json:"ref"`
}

type lookupResponse struct {
	Outcome string         `json:"outcome"`
	Record  metadataRecord `json:"record"`
	Detail  string         `json:"detail,omitempty"`
}

type searchRequest struct {
	Kind  string `json:"kind"`
	Query string `json:"query"`
}

type searchCandidate struct {
	ExternalID string `json:"externalId"`
	Title      string `json:"title,omitempty"`
	Year       int    `json:"year,omitempty"`
	Kind       string `json:"kind,omitempty"`
}

type searchResponse struct {
	Outcome    string            `json:"outcome"`
	Candidates []searchCandidate `json:"candidates,omitempty"`
}

type artworkCandidatesRequest struct {
	Ref  mediaRef `json:"ref"`
	Role string   `json:"role"`
}

type artworkCandidate struct {
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Source string `json:"source,omitempty"`
}

type artworkCandidatesResponse struct {
	Outcome    string             `json:"outcome"`
	Candidates []artworkCandidate `json:"candidates,omitempty"`
}

type externalRefRequest struct {
	Kind   string `json:"kind"`
	Pasted string `json:"pasted"`
}

type externalRefResponse struct {
	Outcome    string `json:"outcome"`
	ExternalID string `json:"externalId,omitempty"`
	GotKind    string `json:"gotKind,omitempty"`
	WantKind   string `json:"wantKind,omitempty"`
}

// The Outcome enum, as far as this Plugin needs it.
const (
	outcomeMatched           = "matched"
	outcomeNoMatch           = "no-match"
	outcomeRefKindMismatch   = "ref-kind-mismatch"
	outcomeRefUnsupportedKnd = "ref-unsupported-kind"
)

// settings is the same struct the sink half declares, plus the two fields only a
// provider is handed.
type providerSettings struct {
	Enabled  bool   `json:"enabled"`
	Secret   string `json:"secret"`
	URL      string `json:"url"`
	URL2     string `json:"url2"`
	Language string `json:"language"`
	// Values is the SECOND VARIANT of the settings field: the values of the
	// settings this Plugin's own manifest declared, keyed by the field key that
	// declared them, in the JSON shape that field's type names. A plugin that
	// declares no fields never sees this key at all.
	Values map[string]any `json:"values"`
}

type kvGetRequest struct {
	Key string `json:"key"`
}

type kvGetResponse struct {
	Found bool   `json:"found"`
	Value []byte `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

type kvSetRequest struct {
	Key   string `json:"key"`
	Value []byte `json:"value,omitempty"`
}

type kvDeleteRequest struct {
	Key string `json:"key"`
}

type kvWriteResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// --- what this Plugin does as a Metadata provider ----------------------------

// The strings a test greps for. Nothing else in the server writes them, so an
// item carrying one was decorated by this guest and by nothing else.
const (
	guestOverview    = "Filled from inside the sandbox by an Installed plugin."
	guestWrongTitle  = "An Entirely Different Record"
	guestArtworkPath = "/art/poster.jpg"
)

//go:wasmexport metadata_lookup
func metadataLookup(ptr, n uint32) uint64 {
	var req lookupRequest
	if !takeRequest(ptr, n, &req) {
		return fail("the request is not a LookupRequest")
	}
	s := currentSettings()

	switch mode(s.URL) {
	case "video-supplement":
		// A fill-only Supplement: it contributes an overview and one poster and
		// NEVER a name, because identity is the host's (ADR-0002). The host merges
		// only what the Authoritative provider left empty.
		if !videoKind(req.Ref.Kind) {
			return reply(lookupResponse{Outcome: outcomeNoMatch})
		}
		return reply(lookupResponse{Outcome: outcomeMatched, Record: metadataRecord{
			Matched:  true,
			Overview: guestOverview,
			Artwork:  []artworkRef{{Role: "poster", URL: s.URL2 + guestArtworkPath}},
			Source:   "installed",
		}})

	case "music-search-hit":
		// A relevance-ranked SEARCH hit whose title is not the local one — for the
		// TRACK only, so the artist and album above it resolve normally and the
		// rejection under test is the track's own.
		//
		// The guest states the FACT that it searched and says nothing about whether
		// the answer is right; the host applies the ADR-0050 acceptance test to it.
		if !musicKind(req.Ref.Kind) {
			return reply(lookupResponse{Outcome: outcomeNoMatch})
		}
		if req.Ref.Kind == "track" {
			return reply(lookupResponse{Outcome: outcomeMatched, Record: metadataRecord{
				Matched:    true,
				Name:       guestWrongTitle,
				Overview:   guestOverview,
				ExternalID: "guest-search-hit",
				Source:     "installed",
				FromSearch: true,
			}})
		}
		return reply(lookupResponse{Outcome: outcomeMatched, Record: metadataRecord{
			Matched:    true,
			Name:       req.Ref.Title,
			Overview:   guestOverview,
			ExternalID: "guest-" + req.Ref.Kind,
			Source:     "installed",
		}})

	case "echo-settings":
		// The manifest-declared settings, read back through settings_get and put
		// where a black-box test can see them: the record's overview.
		//
		// It is a JSON object rather than a formatted sentence because that is what
		// is being proved — an integer comes back as a number and not "7", a
		// multi-select as an array, a bool as true — and because encoding/json sorts
		// a map's keys, so what a test compares against is stable.
		if !musicKind(req.Ref.Kind) && !videoKind(req.Ref.Kind) {
			return reply(lookupResponse{Outcome: outcomeNoMatch})
		}
		return reply(lookupResponse{Outcome: outcomeMatched, Record: metadataRecord{
			Matched:  true,
			Name:     req.Ref.Title,
			Overview: string(encode(s.Values)),
			Source:   "installed",
		}})

	case "kv":
		// The plugin-scoped namespace: write this Plugin's own secret under a key
		// every guest in the test uses, read it back, and report what came out. Two
		// Plugins doing this must not see each other's value.
		if !kvSet("shared-key", []byte(s.Secret)) {
			return fail("kv_set was refused")
		}
		value, found, err := kvGet("shared-key")
		if err != "" {
			return fail("kv_get: " + err)
		}
		if !found {
			return fail("kv_get answered absent for a key this guest just wrote")
		}
		return reply(lookupResponse{Outcome: outcomeMatched, Record: metadataRecord{
			Matched: true, Name: req.Ref.Title, Overview: string(value), Source: "installed",
		}})

	default:
		// The ordinary Full provider: resolve by lookup, answer a record. Music and
		// video alike, so one manifest can point it at either kind.
		if !musicKind(req.Ref.Kind) && !videoKind(req.Ref.Kind) {
			return reply(lookupResponse{Outcome: outcomeNoMatch})
		}
		return reply(lookupResponse{Outcome: outcomeMatched, Record: metadataRecord{
			Matched:    true,
			Name:       req.Ref.Title,
			Overview:   guestOverview,
			Genres:     []string{"Test Genre"},
			Artwork:    []artworkRef{{Role: artworkRole(req.Ref.Kind), URL: s.URL2 + guestArtworkPath}},
			ExternalID: "guest-" + req.Ref.Kind,
			Source:     "installed",
		}})
	}
}

// metadata_album_tracklist answers what an Album holds (capability
// "album-tracklist"). This guest has no tracklist of its own, so it says so — and
// says it two different ways, because the two mean different things to the host:
//
//   - no-match is SETTLED: "this album can name none of its contents", which
//     sends the Admin to the Album.
//   - unavailable is "I cannot answer this call at all right now", an outage
//     rather than a diagnosis, which leaves the album tier with nothing to say and
//     lets each Track fall back to its own search this pass (ADR-0050).
//
//go:wasmexport metadata_album_tracklist
func metadataAlbumTracklist(ptr, n uint32) uint64 {
	var req map[string]any
	if !takeRequest(ptr, n, &req) {
		return fail("the request is not a TracklistRequest")
	}
	if mode(currentSettings().URL) == "music-search-hit" {
		return reply(map[string]any{"outcome": "unavailable"})
	}
	return reply(map[string]any{"outcome": outcomeNoMatch})
}

//go:wasmexport metadata_search
func metadataSearch(ptr, n uint32) uint64 {
	var req searchRequest
	if !takeRequest(ptr, n, &req) {
		return fail("the request is not a SearchRequest")
	}
	return reply(searchResponse{Outcome: outcomeMatched, Candidates: []searchCandidate{{
		ExternalID: "guest-" + req.Kind,
		Title:      req.Query,
		Kind:       req.Kind,
	}}})
}

//go:wasmexport metadata_artwork_candidates
func metadataArtworkCandidates(ptr, n uint32) uint64 {
	var req artworkCandidatesRequest
	if !takeRequest(ptr, n, &req) {
		return fail("the request is not an ArtworkCandidatesRequest")
	}
	s := currentSettings()
	return reply(artworkCandidatesResponse{Outcome: outcomeMatched, Candidates: []artworkCandidate{{
		URL: s.URL2 + guestArtworkPath, Width: 600, Height: 900, Source: "installed",
	}}})
}

// metadata_external_ref reads a string an Admin pasted. The two refusals here are
// the two the host renders as distinct 400s: a link naming a real entity of the
// WRONG kind (with both kinds, so the message can name them), and a link naming an
// entity kind this server does not pin at all.
//
//go:wasmexport metadata_external_ref
func metadataExternalRef(ptr, n uint32) uint64 {
	var req externalRefRequest
	if !takeRequest(ptr, n, &req) {
		return fail("the request is not an ExternalRefRequest")
	}
	switch {
	case strings.Contains(req.Pasted, "/artist/") && req.Kind != "artist":
		return reply(externalRefResponse{
			Outcome: outcomeRefKindMismatch, GotKind: "artist", WantKind: req.Kind,
		})
	case strings.Contains(req.Pasted, "/work/"), strings.Contains(req.Pasted, "/label/"):
		return reply(externalRefResponse{Outcome: outcomeRefUnsupportedKnd})
	default:
		return reply(externalRefResponse{Outcome: outcomeMatched, ExternalID: "guest-pinned"})
	}
}

// --- the ABI glue for the provider half --------------------------------------

// settings asks the host for the Settings it resolved for THIS call: the Admin's
// enabled flag, the decrypted secret, the effective URLs and the server's
// preferred language. It is answered only while a call is on the stack, which is
// what "secrets at call time only" means from in here.
func currentSettings() providerSettings {
	var s providerSettings
	packed := hostSettingsGet()
	if packed == 0 {
		return s
	}
	rptr, rlen := uint32(packed>>32), uint32(packed)
	buf, ok := pinned[rptr]
	if !ok || uint32(len(buf)) < rlen {
		return s
	}
	_ = json.Unmarshal(buf[:rlen], &s)
	free(rptr)
	return s
}

func kvGet(key string) (value []byte, found bool, errMsg string) {
	var resp kvGetResponse
	if !hostRoundTrip(hostKVGet, kvGetRequest{Key: key}, &resp) {
		return nil, false, "the host answered nothing"
	}
	return resp.Value, resp.Found, resp.Error
}

func kvSet(key string, value []byte) bool {
	var resp kvWriteResponse
	if !hostRoundTrip(hostKVSet, kvSetRequest{Key: key, Value: value}, &resp) {
		return false
	}
	return resp.OK
}

// kvDelete is exercised by the suite's namespace test; a real Plugin uses it to
// evict a cache entry it knows is stale.
func kvDelete(key string) bool {
	var resp kvWriteResponse
	if !hostRoundTrip(hostKVDelete, kvDeleteRequest{Key: key}, &resp) {
		return false
	}
	return resp.OK
}

// hostRoundTrip is fetch's shape for the request-response host functions: marshal
// into a buffer this guest allocated, call, read the answer out of a buffer the
// host obtained from the same allocator, free both.
func hostRoundTrip(call func(ptr, n uint32) uint64, req any, out any) bool {
	in, err := json.Marshal(req)
	if err != nil {
		return false
	}
	ptr := alloc(uint32(len(in)))
	copy(pinned[ptr], in)
	packed := call(ptr, uint32(len(in)))
	free(ptr)
	if packed == 0 {
		return false
	}
	rptr, rlen := uint32(packed>>32), uint32(packed)
	buf, ok := pinned[rptr]
	if !ok || uint32(len(buf)) < rlen {
		return false
	}
	uerr := json.Unmarshal(buf[:rlen], out)
	free(rptr)
	return uerr == nil
}

// takeRequest turns the host's (ptr, len) into the request document. A pointer
// this guest did not allocate is refused, which is what makes "the guest owns
// every buffer on both sides" checkable from in here.
func takeRequest(ptr, n uint32, out any) bool {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return false
	}
	return json.Unmarshal(buf[:n], out) == nil
}

func videoKind(kind string) bool {
	switch kind {
	case "movie", "show", "season", "episode":
		return true
	}
	return false
}

func musicKind(kind string) bool {
	switch kind {
	case "artist", "album", "track":
		return true
	}
	return false
}

// artworkRole is the one the host will accept for a Title of any kind. A real
// source names the role its image IS; this guest offers a poster for everything
// because the suite only needs one image to travel, and because a role the host
// does not file for that kind is refused at the store rather than silently kept.
func artworkRole(string) string { return "poster" }

func mode(target string) string {
	const marker = "obelo-mode="
	i := strings.Index(target, marker)
	if i < 0 {
		return ""
	}
	m := target[i+len(marker):]
	if j := strings.IndexAny(m, "&#"); j >= 0 {
		m = m[:j]
	}
	return m
}

// =============================================================================
// The Subtitle provider seam (.scratch/plugin-system issue 12).
// =============================================================================
//
// ONE module, two Extension points. The manifest's `provides` list says which
// seams this Plugin fills and the host looks up only the exports those seams
// need, so a module can be an Event sink and a Subtitle provider at once and the
// two never see each other. That is why this section is simply appended: nothing
// above it changes.
//
// Two more exports, and they are the whole of this seam:
//
//	obelo_subtitle_search(ptr u32, len u32) -> i64     a SubtitleSearchCall in,
//	                                                   a SubtitleSearchResponse out
//	obelo_subtitle_download(ptr u32, len u32) -> i64   a SubtitleDownloadCall in,
//	                                                   a SubtitleDownloadResponse out
//
// Everything here but the two lines marked as scaffolding is the example: this
// is what a Subtitle provider author writes. Note what it does NOT do. It never
// opens a file and never reads a frame of anybody's library — the release-exact
// content hash arrives in the request, computed by the HOST, because a guest has
// no filesystem and needs none. And it never decides how big a download may be:
// the host states maxBytes on the way in and checks the answer on the way out.

// --- the shapes, copied from the contract's JSON schema ----------------------

type subtitleRef struct {
	Title     string `json:"title,omitempty"`
	Year      int    `json:"year,omitempty"`
	IMDBID    string `json:"imdbId,omitempty"`
	MovieHash string `json:"movieHash,omitempty"`
	FileSize  int64  `json:"fileSize,omitempty"`
}

type subtitleSearchRequest struct {
	Ref      subtitleRef `json:"ref"`
	Language string      `json:"language"`
}

// The settings ride WITH the call, exactly as they do for a delivery, so the API
// key exists in here for the duration of one search and dies with the instance.
type subtitleSearchCall struct {
	Request  subtitleSearchRequest `json:"request"`
	Settings settings              `json:"settings"`
}

type subtitleCandidate struct {
	ID        string `json:"id"`
	Language  string `json:"language,omitempty"`
	Format    string `json:"format,omitempty"`
	Release   string `json:"release,omitempty"`
	MatchedBy string `json:"matchedBy,omitempty"`
	Downloads int    `json:"downloads,omitempty"`
}

type subtitleSearchResponse struct {
	Outcome    string              `json:"outcome"`
	Candidates []subtitleCandidate `json:"candidates,omitempty"`
	Detail     string              `json:"detail,omitempty"`
}

type subtitleDownloadRequest struct {
	Candidate subtitleCandidate `json:"candidate"`
	MaxBytes  int64             `json:"maxBytes,omitempty"`
}

type subtitleDownloadCall struct {
	Request  subtitleDownloadRequest `json:"request"`
	Settings settings                `json:"settings"`
}

type subtitleDownloadResponse struct {
	Outcome     string `json:"outcome"`
	Data        []byte `json:"data,omitempty"`
	Format      string `json:"format,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// sourceSubtitle is one entry of the SOURCE's own answer — the shape of the
// service this Plugin wraps, not the contract's. Translating between the two is
// the entire job of a provider Plugin.
type sourceSubtitle struct {
	ID        string `json:"id"`
	Language  string `json:"language"`
	Format    string `json:"format"`
	Release   string `json:"release"`
	Downloads int    `json:"downloads"`
}

// --- what this Plugin actually does ------------------------------------------

//go:wasmexport obelo_subtitle_search
func subtitleSearch(ptr, n uint32) uint64 {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail("the host passed a pointer this guest did not allocate")
	}
	var call subtitleSearchCall
	if err := json.Unmarshal(buf[:n], &call); err != nil {
		return fail("the request is not a SubtitleSearchCall: " + err.Error())
	}
	if mode(call.Settings.Secret) == "nomatch" {
		return reply(subtitleSearchResponse{Outcome: "no-match", Detail: "this source has nothing for that release"})
	}

	resp := fetch(fetchRequest{
		Method: "POST",
		URL:    call.Settings.URL + "/search",
		Headers: []header{
			{Name: "Content-Type", Value: "application/json"},
			{Name: "Api-Key", Value: call.Settings.Secret},
		},
		Body: encode(call.Request),
	})
	if detail, bad := unusable(resp); bad {
		// A source that cannot be reached is 'unavailable', NOT 'no-match'. The
		// two are different facts and the host renders them differently: one is
		// "there is nothing", the other is "I could not ask".
		logLine(levelError, "the search could not be made: "+detail)
		return reply(subtitleSearchResponse{Outcome: "unavailable", Detail: detail})
	}

	var found struct {
		Subtitles []sourceSubtitle `json:"subtitles"`
	}
	if err := json.Unmarshal(resp.Body, &found); err != nil {
		return reply(subtitleSearchResponse{Outcome: "unavailable", Detail: "the source's answer is not the shape this plugin knows: " + err.Error()})
	}
	if len(found.Subtitles) == 0 {
		return reply(subtitleSearchResponse{Outcome: "no-match"})
	}

	// Which signal produced these candidates is the host's to display and this
	// Plugin's to report: a release-exact hash match is worth more to a viewer
	// than a title query, and only the Plugin knows which one it used.
	matched := matchedBy(call.Request.Ref)
	out := make([]subtitleCandidate, 0, len(found.Subtitles))
	for _, s := range found.Subtitles {
		out = append(out, subtitleCandidate{
			ID:        s.ID,
			Language:  s.Language,
			Format:    s.Format,
			Release:   s.Release,
			MatchedBy: matched,
			Downloads: s.Downloads,
		})
	}
	return reply(subtitleSearchResponse{Outcome: "matched", Candidates: out})
}

//go:wasmexport obelo_subtitle_download
func subtitleDownload(ptr, n uint32) uint64 {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail("the host passed a pointer this guest did not allocate")
	}
	var call subtitleDownloadCall
	if err := json.Unmarshal(buf[:n], &call); err != nil {
		return fail("the request is not a SubtitleDownloadCall: " + err.Error())
	}
	if answer, misbehaving := misbehaveDownload(call); misbehaving {
		return answer
	}

	resp := fetch(fetchRequest{
		URL:     call.Settings.URL + "/download/" + call.Request.Candidate.ID,
		Headers: []header{{Name: "Api-Key", Value: call.Settings.Secret}},
	})
	if detail, bad := unusable(resp); bad {
		logLine(levelError, "the download could not be made: "+detail)
		return reply(subtitleDownloadResponse{Outcome: "unavailable", Detail: detail})
	}
	if len(resp.Body) == 0 {
		// The candidate is gone from the source since the search. That is a
		// no-match, not a failure: nothing is broken and nothing should be retried.
		return reply(subtitleDownloadResponse{Outcome: "no-match", Detail: "the source no longer has that candidate"})
	}
	if int64(len(resp.Body)) > call.Request.MaxBytes && call.Request.MaxBytes > 0 {
		// The host said how much it would take. Answering with more is refusing
		// the contract, so the honest answer is to refuse the download instead.
		return reply(subtitleDownloadResponse{Outcome: "unavailable", Detail: "the file is larger than the host will accept"})
	}

	format := call.Request.Candidate.Format
	if format == "" {
		format = "srt"
	}
	logLine(levelInfo, "downloaded one subtitle")
	return reply(subtitleDownloadResponse{
		Outcome:     "matched",
		Data:        resp.Body,
		Format:      format,
		ContentType: "application/x-subrip",
	})
}

// matchedBy names the signal these candidates came from, in the order a source
// is worth asking in: a release-exact content hash first, then the enrichment id,
// then the title query nothing better was available for.
func matchedBy(ref subtitleRef) string {
	switch {
	case ref.MovieHash != "":
		return "moviehash"
	case ref.IMDBID != "":
		return "imdb"
	default:
		return "query"
	}
}

// unusable folds the three ways a fetch can fail to produce an answer into one
// sentence, because this Plugin does the same thing about all three.
func unusable(resp fetchResponse) (string, bool) {
	switch {
	case resp.Refused != "":
		return "refused by the host: " + resp.Refused, true
	case resp.Error != "":
		return "the request failed: " + resp.Error, true
	case resp.Status < 200 || resp.Status > 299:
		return "the source answered " + itoa(resp.Status), true
	}
	return "", false
}

func encode(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return out
}

// --- scaffolding, again ------------------------------------------------------
//
// One mode, for the one thing a well-behaved Plugin cannot demonstrate: a guest
// that answers with MORE bytes than the host said it would take. It cannot be
// provoked through the source, because the host's fetch cap already refuses an
// oversize body on the way in — so the bytes have to be invented in here. The
// mode is read out of the SECRET, which for a provider is the one string a test
// can set that reaches this far in.

func misbehaveDownload(call subtitleDownloadCall) (uint64, bool) {
	if mode(call.Settings.Secret) != "oversize" {
		return 0, false
	}
	// Exactly one byte past the cap the host stated, whatever it is.
	return reply(subtitleDownloadResponse{
		Outcome: "matched",
		Data:    make([]byte, call.Request.MaxBytes+1),
		Format:  "srt",
	}), true
}
