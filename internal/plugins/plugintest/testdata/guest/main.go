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
	"net/url"
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
	// CallRemainingMillis is echoed back to the receiver as a header (see post
	// below), so a native test can read the number the host actually stamped on
	// this call without this guest needing an opinion about it.
	CallRemainingMillis *int `json:"callRemainingMillis,omitempty"`
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

	headers := []header{
		{Name: "Content-Type", Value: "application/json"},
		{Name: "X-Obelo-Signature", Value: "sha256=" + hex.EncodeToString(mac.Sum(nil))},
	}
	if req.Settings.CallRemainingMillis != nil {
		headers = append(headers, header{Name: "X-Obelo-Call-Remaining-Millis", Value: itoa(*req.Settings.CallRemainingMillis)})
	}
	resp := fetch(fetchRequest{
		Method:  "POST",
		URL:     req.Settings.URL,
		Headers: headers,
		Body:    body,
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

// refusals counts the calls THIS INSTANCE has answered with a clean error. It is
// package state in a guest's linear memory, so it lives exactly as long as the
// instance does — which makes it the suite's proof of whether the host kept the
// instance or rebuilt it.
var refusals uint64

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
	case "refuse":
		// The SINK's clean error: it ran to completion and answered `0` with a
		// sentence. A Metadata provider gets to do that for free (ADR-0058 decision
		// 7 as amended 2026-09-18); a sink does not, and the count in the sentence
		// is what proves it — every one of these reads "refusal 1", because the
		// instance that answered it is dropped before the next call.
		refusals++
		return fail("this sink cannot deliver and says so cleanly (refusal " + itoa(int(refusals)) + " from this instance)"), true
	case "forbidden":
		return reply(reportRefusal(fetch(fetchRequest{URL: "https://not-allowed.example.test/steal"}))), true
	case "metadata":
		// A host the manifest DOES allowlist, whose address is one no plugin may
		// reach. The allowlist is a claim; the address rule is the answer.
		return reply(reportRefusal(fetch(fetchRequest{URL: "http://169.254.169.254/latest/meta-data/"}))), true
	case "socket":
		// A socket to the receiver's own address, from a call that is not a
		// sign-in call. The host must refuse it before anything is dialled.
		u, err := url.Parse(req.Settings.URL)
		if err != nil {
			return fail(err.Error()), true
		}
		resp := hostSocketCall(socketRequest{Op: "open", Address: u.Host})
		switch {
		case resp.Refused != "":
			return reply(deliverResponse{Error: "refused: " + resp.Refused}), true
		case resp.Error != "":
			return reply(deliverResponse{Error: "error: " + resp.Error}), true
		}
		return reply(deliverResponse{Error: "the host opened a socket it should have refused, handle " + itoa(resp.Handle)}), true
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
	outcomeUnavailable       = "unavailable"
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
	// guestAgent is an identity this guest asks the host to send and the host
	// never does (ADR-0059 decision 7). A stand-in that ever sees it is a stand-in
	// looking at a bug.
	guestAgent = "guest-agent/9.9 ( nobody@example.test )"
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

	case "fetch-slow":
		// THREE fetches in one lookup, which is the shape a real provider has:
		// resolve the id, decorate the record, ask for artwork. The stand-in the
		// test points URL2 at answers each one slowly, so this is where the host's
		// call budget is either enough or it is not.
		//
		// A fetch that comes back as an ERROR is "the source is not answering right
		// now", and this guest says so — `unavailable`, which sends the item to the
		// host's backoff — rather than `no-match`, which would be a claim about the
		// source's catalogue it is in no position to make. That the guest gets to
		// answer at all is the property under test: before ADR-0059 decision 6 the
		// call deadline killed the instance mid-fetch and the Plugin took the blame.
		for i := 1; i <= 3; i++ {
			resp := fetch(fetchRequest{URL: s.URL2})
			if resp.Refused != "" {
				return reply(lookupResponse{Outcome: outcomeUnavailable, Detail: "fetch " + itoa(i) + " refused: " + resp.Refused})
			}
			if resp.Error != "" {
				return reply(lookupResponse{Outcome: outcomeUnavailable, Detail: "fetch " + itoa(i) + " failed: " + resp.Error})
			}
		}
		return reply(lookupResponse{Outcome: outcomeMatched, Record: metadataRecord{
			Matched: true, Name: req.Ref.Title, Overview: guestOverview, Source: "installed",
		}})

	case "fetch-once":
		// ONE fetch, carrying headers of this guest's own — including a User-Agent,
		// which the host drops in favour of its own identity, and an ordinary one,
		// which travels. What came back is reported as a sentence the test reads:
		// how many bytes arrived, or the refusal that stopped them.
		resp := fetch(fetchRequest{URL: s.URL2, Headers: []header{
			{Name: "User-Agent", Value: guestAgent},
			{Name: "X-Guest-Header", Value: "yes"},
		}})
		detail := "bytes=" + itoa(len(resp.Body))
		switch {
		case resp.Refused != "":
			detail = "refused: " + resp.Refused
		case resp.Error != "":
			detail = "failed: " + resp.Error
		}
		return reply(lookupResponse{Outcome: outcomeMatched, Detail: detail, Record: metadataRecord{
			Matched: true, Name: req.Ref.Title, Overview: detail, Source: "installed",
		}})

	case "refuse":
		// A CLEAN error: the guest ran to completion, decided it cannot answer this
		// call, and came back through the ABI's `0` with a sentence — which is what
		// a real provider does with a rejected key (401) or a document it cannot
		// parse. For a Metadata provider the host keeps this instance and counts no
		// strike, so the sentence carries `refusals`, which counts the calls THIS
		// instance has answered and starts again from 1 in a rebuilt one. A test
		// reading 1, 2, 3, 4, 5 is reading proof that one instance answered them all.
		refusals++
		return fail("the source rejected this credential: status 401 (refusal " +
			itoa(int(refusals)) + " from this instance)")

	case "spin":
		// A lookup that never returns. `hang` above is the sink's; this is the
		// provider's, and it is what a deadline kill must still mean now that a
		// slow SOURCE no longer produces one.
		for {
			spun++
		}

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
	switch mode(call.Settings.Secret) {
	case "nomatch":
		return reply(subtitleSearchResponse{Outcome: "no-match", Detail: "this source has nothing for that release"})
	case "spin":
		// A search that never returns, so a test can read the seam's call budget
		// off the wall clock (.scratch/bundled-plugins issue 09).
		for {
			spun++
		}
	}
	if answer, probing := probeNamespace(call); probing {
		return answer
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
// Two modes. The first is for the one thing a well-behaved Plugin cannot
// demonstrate: a guest that answers with MORE bytes than the host said it would
// take. It cannot be provoked through the source, because the host's fetch cap
// already refuses an oversize body on the way in — so the bytes have to be
// invented in here. The second reports what this Plugin's own key-value
// namespace holds. Both modes are read out of the SECRET, which for a provider is
// the one string a test can set that reaches this far in.

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

// The strings the kv-probe mode reports with (.scratch/plugin-system issue 16).
const (
	kvProbeKey     = "install-marker"
	kvProbeValue   = "written-by-an-earlier-install"
	kvProbeAbsent  = "namespace-absent"
	kvProbePresent = "namespace-survived"
)

// probeNamespace reads one key out of this Plugin's OWN namespace, reports what
// was there, and then writes it — so asking twice tells a test that the write
// landed, and asking again after an uninstall and a reinstall under the same id
// tells it whether the namespace went with the Plugin. Only the host can answer
// that question honestly, and a guest is the only thing that can ask it.
//
// It answers through the candidate list rather than a log line or an error,
// because the candidate id is what travels untouched all the way to the player's
// "search online" — which is to say, to somewhere a black-box test can read it.
func probeNamespace(call subtitleSearchCall) (uint64, bool) {
	if mode(call.Settings.Secret) != "kv-probe" {
		return 0, false
	}
	value, found, errMsg := kvGet(kvProbeKey)
	if errMsg != "" {
		return reply(subtitleSearchResponse{Outcome: "unavailable", Detail: "kv_get: " + errMsg}), true
	}
	id, release := kvProbeAbsent, ""
	if found {
		id, release = kvProbePresent, string(value)
	}
	if !kvSet(kvProbeKey, []byte(kvProbeValue)) {
		return reply(subtitleSearchResponse{Outcome: "unavailable", Detail: "kv_set was refused"}), true
	}
	return reply(subtitleSearchResponse{Outcome: "matched", Candidates: []subtitleCandidate{{
		ID:        id,
		Language:  call.Request.Language,
		Format:    "srt",
		Release:   release,
		MatchedBy: "query",
	}}}), true
}

// =============================================================================
// The Web reference provider seam.
// =============================================================================
//
// Appended like the Subtitle provider half, for the same reason: nothing above
// changes. One export, and it is a pure computation — the ids arrive in the
// request, the answer is built from them, and nothing is fetched. The host
// refuses http_fetch for the whole of this call, and keeps only https references
// keyed to ids it sent; this guest does not have to know either rule.
//
//	web_reference_links(ptr u32, len u32) -> i64   a WebReferencesCall in,
//	                                               a WebReferencesResponse out

// --- the shapes, copied from the contract's JSON schema ----------------------

type webReferencesCall struct {
	Request struct {
		Kind string            `json:"kind"`
		IDs  map[string]string `json:"ids"`
	} `json:"request"`
	Settings struct {
		Values map[string]any `json:"values"`
	} `json:"settings"`
}

type webReference struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
	Label     string `json:"label"`
	URL       string `json:"url"`
}

type webReferencesResponse struct {
	References []webReference `json:"references,omitempty"`
}

// --- what this Plugin actually does ------------------------------------------

//go:wasmexport web_reference_links
func webReferenceLinks(ptr, n uint32) uint64 {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail("the host passed a pointer this guest did not allocate")
	}
	var call webReferencesCall
	if err := json.Unmarshal(buf[:n], &call); err != nil {
		return fail("the request is not a WebReferencesCall: " + err.Error())
	}
	if out, done := misbehaveLinks(call); done {
		return out
	}
	var refs []webReference
	for _, ns := range sortedKeys(call.Request.IDs) {
		id := call.Request.IDs[ns]
		refs = append(refs, webReference{
			Namespace: ns,
			ID:        id,
			Label:     "Example " + ns,
			URL:       "https://refs.example.test/" + call.Request.Kind + "/" + ns + "/" + id,
		})
	}
	return reply(webReferencesResponse{References: refs})
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// --- scaffolding, again ------------------------------------------------------
//
// The parts the suite needs a Web reference provider to play, chosen by a
// declared `mode` setting because the call carries no URL and no secret:
//
//	http     every held id, linked over plain http only
//	foreign  one good https reference, plus two keyed to ids the host never sent
//	fetch    reaches for http_fetch and reports what the host answered
func misbehaveLinks(call webReferencesCall) (uint64, bool) {
	mode, _ := call.Settings.Values["mode"].(string)
	ids := call.Request.IDs
	switch mode {
	case "http":
		var refs []webReference
		for _, ns := range sortedKeys(ids) {
			refs = append(refs, webReference{
				Namespace: ns, ID: ids[ns], Label: "Plain " + ns,
				URL: "http://refs.example.test/" + ns + "/" + ids[ns],
			})
		}
		return reply(webReferencesResponse{References: refs}), true
	case "foreign":
		var refs []webReference
		for _, ns := range sortedKeys(ids) {
			refs = append(refs,
				webReference{
					Namespace: ns, ID: ids[ns], Label: "Held " + ns,
					URL: "https://refs.example.test/" + ns + "/" + ids[ns],
				},
				// The right namespace, somebody else's id.
				webReference{
					Namespace: ns, ID: ids[ns] + "0", Label: "Wrong id " + ns,
					URL: "https://refs.example.test/" + ns + "/" + ids[ns] + "0",
				})
		}
		// A namespace the host sent nothing in at all.
		refs = append(refs, webReference{
			Namespace: "never-sent", ID: "1", Label: "Never sent",
			URL: "https://refs.example.test/never-sent/1",
		})
		return reply(webReferencesResponse{References: refs}), true
	case "fetch":
		resp := fetch(fetchRequest{URL: "https://refs.example.test/probe"})
		verdict := "fetched " + itoa(resp.Status)
		switch {
		case resp.Refused != "":
			verdict = "refused: " + resp.Refused
		case resp.Error != "":
			verdict = "error: " + resp.Error
		}
		var refs []webReference
		for _, ns := range sortedKeys(ids) {
			refs = append(refs, webReference{
				Namespace: ns, ID: ids[ns], Label: verdict,
				URL: "https://refs.example.test/" + ns + "/" + ids[ns],
			})
		}
		return reply(webReferencesResponse{References: refs}), true
	}
	return 0, false
}

// =============================================================================
// The Sign-in provider seam, password flow.
// =============================================================================
//
// Appended like the halves above it, for the same reason: nothing above
// changes. One export: a username and password in, accepted-with-an-identity or
// not out. The "directory" it checks against is a declared `accounts` setting,
// so a test can stand up two directories that disagree without a network:
//
//	sign_in_password(ptr u32, len u32) -> i64   a SignInPasswordCall in,
//	                                            a SignInPasswordResponse out
//
// `accounts` is `;`-separated entries of `login:password:subject:name:groups`,
// where name is the username the directory reports (empty: the login typed) and
// groups is a `,`-separated list. Two logins may share a subject — that is how
// the suite plays a person renamed at the source.

// --- the shapes, copied from the contract's JSON schema ----------------------

type signInPasswordCall struct {
	Request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"request"`
	Settings struct {
		Values map[string]any `json:"values"`
	} `json:"settings"`
}

type signInIdentity struct {
	Subject  string   `json:"subject"`
	Username string   `json:"username,omitempty"`
	Groups   []string `json:"groups,omitempty"`
}

type signInPasswordResponse struct {
	Accepted bool            `json:"accepted"`
	Identity *signInIdentity `json:"identity,omitempty"`
}

// --- what this Plugin actually does ------------------------------------------

// SignInFailsWithThePassword, as the whole `accounts` value, is a directory that
// logs the password it was handed and then fails the call with it in the error:
// the hostile answer the host must keep out of every sentence it stores or logs.
const SignInFailsWithThePassword = "fail-with-the-password"

// SignInHangs, as the whole `accounts` value, is a directory that never answers:
// only the host's deadline ends its call.
const SignInHangs = "hang"

// The directories below each put the password they were handed somewhere the
// host keeps or logs text, and then reject the login: the password in a fetched
// URL's query (after SignInFetchesWithThePassword, the URL it is appended to), as
// the name of the host fetched, in a key-value key, and in a log line.
const (
	SignInFetchesWithThePassword = "fetch-with-the-password:"
	SignInFetchesThePasswordHost = "fetch-the-password-host"
	SignInStoresThePassword      = "store-the-password"
	SignInLogsThePassword        = "log-the-password"
)

//go:wasmexport sign_in_password
func signInPassword(ptr, n uint32) uint64 {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail("the host passed a pointer this guest did not allocate")
	}
	var call signInPasswordCall
	if err := json.Unmarshal(buf[:n], &call); err != nil {
		return fail("the request is not a SignInPasswordCall: " + err.Error())
	}
	accounts, _ := call.Settings.Values["accounts"].(string)
	if accounts == SignInFailsWithThePassword {
		logLine(levelError, "checking the password "+call.Request.Password)
		return fail("the directory rejected the password " + call.Request.Password)
	}
	if accounts == SignInHangs {
		for {
			spun++
		}
	}
	switch {
	case strings.HasPrefix(accounts, SignInFetchesWithThePassword):
		fetch(fetchRequest{URL: strings.TrimPrefix(accounts, SignInFetchesWithThePassword) + call.Request.Password})
		return reply(signInPasswordResponse{})
	case accounts == SignInFetchesThePasswordHost:
		fetch(fetchRequest{URL: "http://" + call.Request.Password + ".example.test/"})
		return reply(signInPasswordResponse{})
	case accounts == SignInStoresThePassword:
		kvSet("session-"+call.Request.Password, []byte("1"))
		return reply(signInPasswordResponse{})
	case accounts == SignInLogsThePassword:
		logLine(levelInfo, "the password is "+call.Request.Password)
		return reply(signInPasswordResponse{})
	case strings.HasPrefix(accounts, SignInSocketScript):
		return reply(signInPasswordResponse{Accepted: true, Identity: &signInIdentity{
			Subject: "socket",
			Groups:  runSocketScript(strings.TrimPrefix(accounts, SignInSocketScript)),
		}})
	}
	for _, entry := range strings.Split(accounts, ";") {
		f := strings.Split(entry, ":")
		if len(f) != 5 || f[0] != call.Request.Username || f[1] != call.Request.Password {
			continue
		}
		id := &signInIdentity{Subject: f[2], Username: f[3]}
		if id.Username == "" {
			id.Username = call.Request.Username
		}
		if f[4] != "" {
			id.Groups = strings.Split(f[4], ",")
		}
		return reply(signInPasswordResponse{Accepted: true, Identity: id})
	}
	return reply(signInPasswordResponse{})
}

// =============================================================================
// The Lyric provider seam.
// =============================================================================
//
// Appended like the Web reference provider half, for the same reason. One
// export, and it is a thin wrapper: the request is POSTed as JSON to the
// source at `<settings.url>/lyrics`, and whatever the source answers is this
// Plugin's answer, verbatim. The suite's source is an httptest server, so the
// test decides every answer — well timed, mistimed, for another recording — and
// counts every question. The host judges what comes back; this guest does not
// have to know how.
//
//	lyric_provider_lyrics(ptr u32, len u32) -> i64   a LyricsCall in,
//	                                                 a LyricsResponse out

type lyricsCall struct {
	Request  json.RawMessage `json:"request"`
	Settings struct {
		URL string `json:"url"`
	} `json:"settings"`
}

//go:wasmexport lyric_provider_lyrics
func lyricProviderLyrics(ptr, n uint32) uint64 {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail("the host passed a pointer this guest did not allocate")
	}
	var call lyricsCall
	if err := json.Unmarshal(buf[:n], &call); err != nil {
		return fail("the request is not a LyricsCall: " + err.Error())
	}
	resp := fetch(fetchRequest{
		Method:  "POST",
		URL:     call.Settings.URL + "/lyrics",
		Headers: []header{{Name: "Content-Type", Value: "application/json"}},
		Body:    call.Request,
	})
	switch {
	case resp.Refused != "":
		return fail("the source was refused: " + resp.Refused)
	case resp.Error != "":
		return fail("the source could not be reached: " + resp.Error)
	case resp.Status != 200:
		return fail("the source answered " + itoa(resp.Status))
	}
	return reply(json.RawMessage(resp.Body))
}

// =============================================================================
// The Marker provider seam.
// =============================================================================
//
// Appended like the Lyric provider half, and shaped the same: the request is
// POSTed as JSON to the source at `<settings.url>/markers`, and whatever the
// source answers is this Plugin's answer, verbatim. The test's source decides
// every candidate — timed for this File's length or another's — and counts every
// question.
//
//	marker_provider_markers(ptr u32, len u32) -> i64   a MarkersCall in,
//	                                                   a MarkersResponse out

type markersCall struct {
	Request  json.RawMessage `json:"request"`
	Settings struct {
		URL string `json:"url"`
	} `json:"settings"`
}

//go:wasmexport marker_provider_markers
func markerProviderMarkers(ptr, n uint32) uint64 {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail("the host passed a pointer this guest did not allocate")
	}
	var call markersCall
	if err := json.Unmarshal(buf[:n], &call); err != nil {
		return fail("the request is not a MarkersCall: " + err.Error())
	}
	resp := fetch(fetchRequest{
		Method:  "POST",
		URL:     call.Settings.URL + "/markers",
		Headers: []header{{Name: "Content-Type", Value: "application/json"}},
		Body:    call.Request,
	})
	switch {
	case resp.Refused != "":
		return fail("the source was refused: " + resp.Refused)
	case resp.Error != "":
		return fail("the source could not be reached: " + resp.Error)
	case resp.Status != 200:
		return fail("the source answered " + itoa(resp.Status))
	}
	return reply(json.RawMessage(resp.Body))
}

// =============================================================================
// The Sign-in provider seam, redirect flow.
// =============================================================================
//
// Two exports, a plain OAuth2 provider with no network: the authorize URL is the
// declared `authorize` setting with the host's values on it, and the exchange
// reads the identity out of the code itself.
//
//	sign_in_authorize_url(ptr u32, len u32) -> i64   a SignInAuthorizeCall in,
//	                                                 a SignInAuthorizeResponse out
//	sign_in_exchange(ptr u32, len u32) -> i64        a SignInExchangeCall in,
//	                                                 a SignInExchangeResponse out
//
// A code is `subject|username|groups` (groups `,`-separated). `reject` is
// refused; a `token|` prefix also answers an ID token nobody can verify.
//
// SignInRedirectFailsWithTheSecrets, as the code or as the last path segment of
// the `authorize` setting, is a provider that logs what the call handed it —
// the code and verifier, or the state, nonce and challenge, and the
// `client_secret` setting — and then fails the call with the same in its error.

// SignInRedirectFailsWithTheSecrets is the hostile redirect provider above.
const SignInRedirectFailsWithTheSecrets = "fail-with-the-secrets"

// SignInRedirectDiscovers, as the prefix of a code, is an OpenID Connect
// provider's exchange: it reads the `issuer` setting's discovery document and
// posts to the token endpoint the document names before reading the rest of the
// code as usual, and fails saying what the host answered if either fetch is not
// a 200.
const SignInRedirectDiscovers = "discover|"

// redeemAtTheDiscoveredTokenEndpoint is SignInRedirectDiscovers' two fetches,
// answering "" when both came back 200.
func redeemAtTheDiscoveredTokenEndpoint(values map[string]any) string {
	issuer, _ := values["issuer"].(string)
	doc := fetch(fetchRequest{URL: strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"})
	if doc.Status != 200 {
		return "discovery answered " + itoa(doc.Status) + " refused=" + doc.Refused + " error=" + doc.Error
	}
	var named struct {
		Token string `json:"token_endpoint"`
	}
	if err := json.Unmarshal(doc.Body, &named); err != nil {
		return "the discovery document is not JSON: " + err.Error()
	}
	tok := fetch(fetchRequest{Method: "POST", URL: named.Token, Body: []byte("grant_type=authorization_code")})
	if tok.Status != 200 {
		return "the token endpoint " + named.Token + " answered " + itoa(tok.Status) + " refused=" + tok.Refused + " error=" + tok.Error
	}
	return ""
}

// SignInRedirectFetchesABlockedHost, as the code or as the last path segment of
// the `authorize` setting, is a provider that fetches a host no manifest lists
// and then answers as usual: the authorize URL, or a refused exchange.
const SignInRedirectFetchesABlockedHost = "fetch-a-blocked-host"

type signInAuthorizeCall struct {
	Request struct {
		State         string `json:"state"`
		CodeChallenge string `json:"codeChallenge"`
		Nonce         string `json:"nonce"`
		RedirectURI   string `json:"redirectUri"`
	} `json:"request"`
	Settings struct {
		Values map[string]any `json:"values"`
	} `json:"settings"`
}

type signInExchangeCall struct {
	Request struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"codeVerifier"`
	} `json:"request"`
	Settings struct {
		Values map[string]any `json:"values"`
	} `json:"settings"`
}

type signInExchangeResponse struct {
	Accepted     bool            `json:"accepted"`
	Identity     *signInIdentity `json:"identity,omitempty"`
	IDToken      string          `json:"idToken,omitempty"`
	RefreshToken string          `json:"refreshToken,omitempty"`
}

//go:wasmexport sign_in_authorize_url
func signInAuthorizeURL(ptr, n uint32) uint64 {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail("the host passed a pointer this guest did not allocate")
	}
	var call signInAuthorizeCall
	if err := json.Unmarshal(buf[:n], &call); err != nil {
		return fail("the request is not a SignInAuthorizeCall: " + err.Error())
	}
	base, _ := call.Settings.Values["authorize"].(string)
	if strings.HasSuffix(base, "/"+SignInRedirectFailsWithTheSecrets) {
		secret, _ := call.Settings.Values["client_secret"].(string)
		said := "the guest was handed state=" + call.Request.State + " nonce=" + call.Request.Nonce +
			" challenge=" + call.Request.CodeChallenge + " secret=" + secret
		logLine(levelError, said)
		return fail(said)
	}
	if strings.HasSuffix(base, "/"+SignInRedirectFetchesABlockedHost) {
		fetch(fetchRequest{URL: "http://blocked.example.test/"})
	}
	q := url.Values{}
	q.Set("state", call.Request.State)
	q.Set("nonce", call.Request.Nonce)
	q.Set("code_challenge", call.Request.CodeChallenge)
	q.Set("redirect_uri", call.Request.RedirectURI)
	return reply(map[string]string{"url": base + "?" + q.Encode()})
}

//go:wasmexport sign_in_exchange
func signInExchange(ptr, n uint32) uint64 {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail("the host passed a pointer this guest did not allocate")
	}
	var call signInExchangeCall
	if err := json.Unmarshal(buf[:n], &call); err != nil {
		return fail("the request is not a SignInExchangeCall: " + err.Error())
	}
	code := call.Request.Code
	if code == SignInRedirectFailsWithTheSecrets {
		secret, _ := call.Settings.Values["client_secret"].(string)
		said := "the guest was handed code=" + code + " verifier=" + call.Request.CodeVerifier + " secret=" + secret
		logLine(levelError, said)
		return fail(said)
	}
	if code == SignInRedirectFetchesABlockedHost {
		fetch(fetchRequest{URL: "http://blocked.example.test/"})
		return reply(signInExchangeResponse{})
	}
	if code == "reject" {
		return reply(signInExchangeResponse{})
	}
	if rest, ok := strings.CutPrefix(code, SignInRedirectDiscovers); ok {
		if said := redeemAtTheDiscoveredTokenEndpoint(call.Settings.Values); said != "" {
			return fail(said)
		}
		code = rest
	}
	var resp signInExchangeResponse
	refresh := false
	if rest, ok := strings.CutPrefix(code, "refresh|"); ok {
		code, refresh = rest, true
	}
	if rest, ok := strings.CutPrefix(code, "token|"); ok {
		code = rest
		resp.IDToken = "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0."
	}
	f := strings.Split(code, "|")
	if len(f) != 3 {
		return fail("the code is not subject|username|groups")
	}
	id := &signInIdentity{Subject: f[0], Username: f[1]}
	if f[2] != "" {
		id.Groups = strings.Split(f[2], ",")
	}
	resp.Accepted = true
	resp.Identity = id
	if refresh {
		resp.RefreshToken = "rt|" + f[0]
	}
	return reply(resp)
}

// =============================================================================
// The socket grant (ADR-0064).
// =============================================================================
//
// A Sign-in provider that declares `socket` may open a TCP connection, during a
// sign-in call, to the host:port the operator typed — and nowhere else. The host
// dials, encrypts and verifies; this side reads and writes the plaintext of the
// protocol it speaks.
//
//	socket(ptr u32, len u32) -> i64   a SocketRequest in, a SocketResponse out
//
// SignInSocketScript, as the prefix of the `accounts` value, is a directory that
// runs the `|`-separated steps after it and answers accepted, subject "socket",
// with one group per step saying what the host answered:
//
//	open           open the operator's address      ok:h=<handle>[:tls][:upgrade]
//	open=ADDR      open, naming ADDR
//	write=TEXT     write TEXT on the current handle ok:<bytes written>
//	read           read once                        ok:<data>[:eof]
//	read=N         read once, asking for at most N bytes
//	starttls       upgrade the current handle       ok[:tls]
//	close          close the current handle         ok
//	keep           remember the current handle for a LATER call
//	kept           make the remembered handle the current one
//	hang           never return
//
// A refusal answers "refused" and a failure "error", and the step after it runs
// anyway: a handle an open did not answer is 0, which the host refuses.

const SignInSocketScript = "socket:"

//go:wasmimport obelo socket
func hostSocket(ptr, n uint32) uint64

type socketRequest struct {
	Op      string `json:"op"`
	Address string `json:"address,omitempty"`
	Handle  int    `json:"handle,omitempty"`
	Data    []byte `json:"data,omitempty"`
	Max     int    `json:"max,omitempty"`
}

type socketResponse struct {
	Handle         int    `json:"handle,omitempty"`
	Encrypted      bool   `json:"encrypted,omitempty"`
	UpgradePending bool   `json:"upgradePending,omitempty"`
	Written        int    `json:"written,omitempty"`
	Data           []byte `json:"data,omitempty"`
	EOF            bool   `json:"eof,omitempty"`
	Refused        string `json:"refused,omitempty"`
	Error          string `json:"error,omitempty"`
}

// keptHandle outlives the call that set it: this instance's memory does. The host
// must not let it name anything in the next call.
var keptHandle int

func hostSocketCall(req socketRequest) socketResponse {
	var resp socketResponse
	if !hostRoundTrip(hostSocket, req, &resp) {
		return socketResponse{Error: "the host answered nothing"}
	}
	return resp
}

func runSocketScript(script string) []string {
	var out []string
	handle := 0
	for _, step := range strings.Split(script, "|") {
		op, arg, _ := strings.Cut(step, "=")
		var resp socketResponse
		switch op {
		case "open":
			resp = hostSocketCall(socketRequest{Op: "open", Address: arg})
			handle = resp.Handle
		case "write":
			resp = hostSocketCall(socketRequest{Op: "write", Handle: handle, Data: []byte(arg)})
		case "read":
			limit := 0
			for _, d := range arg {
				limit = limit*10 + int(d-'0')
			}
			resp = hostSocketCall(socketRequest{Op: "read", Handle: handle, Max: limit})
		case "starttls":
			resp = hostSocketCall(socketRequest{Op: "starttls", Handle: handle})
		case "close":
			resp = hostSocketCall(socketRequest{Op: "close", Handle: handle})
		case "keep":
			keptHandle = handle
			out = append(out, "ok")
			continue
		case "kept":
			handle = keptHandle
			out = append(out, "ok")
			continue
		case "hang":
			for {
				spun++
			}
		default:
			out = append(out, "unknown step "+op)
			continue
		}
		out = append(out, socketOutcome(op, resp))
	}
	return out
}

func socketOutcome(op string, resp socketResponse) string {
	switch {
	case resp.Refused != "":
		return "refused"
	case resp.Error != "":
		return "error"
	}
	out := "ok"
	switch op {
	case "open":
		out += ":h=" + itoa(resp.Handle)
	case "write":
		out += ":" + itoa(resp.Written)
	case "read":
		out += ":" + string(resp.Data)
		if resp.EOF {
			out += ":eof"
		}
	}
	if resp.Encrypted {
		out += ":tls"
	}
	if resp.UpgradePending {
		out += ":upgrade"
	}
	return out
}

// =============================================================================
// The Sign-in provider seam, re-check.
// =============================================================================
//
// Appended like the halves above it. Two exports, both answered from a declared
// `lookup` setting of `;`-separated `subject:status:groups` entries (groups
// `,`-separated); a subject the setting does not list is gone.
//
//	sign_in_lookup(ptr u32, len u32) -> i64    a SignInLookupCall in,
//	                                           a SignInLookupResponse out
//	sign_in_refresh(ptr u32, len u32) -> i64   a SignInRefreshCall in,
//	                                           a SignInRefreshResponse out
//
// A refresh token is `rt|subject`, as the exchange above hands out for a code
// prefixed `refresh|`, and is handed back unchanged.

// SignInLookupFails, as the whole `lookup` value, is a directory that fails
// every re-check: the provider unreachable.
const SignInLookupFails = "fail"

// SignInLookupFetchesABlockedHost, as the whole `lookup` value, is a directory
// that fetches a host no manifest lists on every re-check, and fails.
const SignInLookupFetchesABlockedHost = "fetch-a-blocked-host"

type signInLookupCall struct {
	Request struct {
		Subject string `json:"subject"`
	} `json:"request"`
	Settings struct {
		Values map[string]any `json:"values"`
	} `json:"settings"`
}

type signInRefreshCall struct {
	Request struct {
		RefreshToken string `json:"refreshToken"`
	} `json:"request"`
	Settings struct {
		Values map[string]any `json:"values"`
	} `json:"settings"`
}

type signInRecheckResponse struct {
	Status       string          `json:"status"`
	Identity     *signInIdentity `json:"identity,omitempty"`
	RefreshToken string          `json:"refreshToken,omitempty"`
}

// recheckAnswer is what the `lookup` directory says about subject, or false
// when the call must fail.
func recheckAnswer(values map[string]any, subject string) (signInRecheckResponse, bool) {
	lookup, _ := values["lookup"].(string)
	switch lookup {
	case SignInLookupFails:
		return signInRecheckResponse{}, false
	case SignInLookupFetchesABlockedHost:
		fetch(fetchRequest{URL: "http://blocked.example.test/"})
		return signInRecheckResponse{}, false
	}
	for _, entry := range strings.Split(lookup, ";") {
		f := strings.Split(entry, ":")
		if len(f) != 3 || f[0] != subject {
			continue
		}
		resp := signInRecheckResponse{Status: f[1]}
		if f[1] == "active" {
			resp.Identity = &signInIdentity{Subject: subject}
			if f[2] != "" {
				resp.Identity.Groups = strings.Split(f[2], ",")
			}
		}
		return resp, true
	}
	return signInRecheckResponse{Status: "gone"}, true
}

//go:wasmexport sign_in_lookup
func signInLookup(ptr, n uint32) uint64 {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail("the host passed a pointer this guest did not allocate")
	}
	var call signInLookupCall
	if err := json.Unmarshal(buf[:n], &call); err != nil {
		return fail("the request is not a SignInLookupCall: " + err.Error())
	}
	resp, ok := recheckAnswer(call.Settings.Values, call.Request.Subject)
	if !ok {
		return fail("the directory did not answer")
	}
	return reply(resp)
}

//go:wasmexport sign_in_refresh
func signInRefresh(ptr, n uint32) uint64 {
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail("the host passed a pointer this guest did not allocate")
	}
	var call signInRefreshCall
	if err := json.Unmarshal(buf[:n], &call); err != nil {
		return fail("the request is not a SignInRefreshCall: " + err.Error())
	}
	subject, ok := strings.CutPrefix(call.Request.RefreshToken, "rt|")
	if !ok {
		return reply(signInRecheckResponse{Status: "gone"})
	}
	resp, ok := recheckAnswer(call.Settings.Values, subject)
	if !ok {
		return fail("the directory did not answer")
	}
	resp.RefreshToken = call.Request.RefreshToken
	return reply(resp)
}
