package v1

import "context"

// The Subtitle provider Extension point (ADR-0021 behind ADR-0057): search an
// external source for a subtitle matched to the exact release a viewer is
// watching, then download one candidate's bytes. Two calls, both request-response,
// both plain data in and plain data out.

// SubtitleRef is everything a Subtitle provider may key a search by, gathered by
// the host from the Title and the played File. A Plugin tries whichever signals
// are present in the ADR-0021 match order — MovieHash (release-exact) → IMDBID → a
// Title/Year query — and says which one produced each candidate.
type SubtitleRef struct {
	// Title and Year are the parsed identity (ADR-0002), always present; the
	// last-resort query.
	Title string `json:"title,omitempty"`
	Year  int    `json:"year,omitempty"`
	// IMDBID is the enrichment-assigned id ("tt…"), empty on an un-enriched Title.
	IMDBID string `json:"imdbId,omitempty"`
	// MovieHash is the OpenSubtitles moviehash of the played File, computed lazily
	// by the host; empty when the file is unreadable or too small.
	MovieHash string `json:"movieHash,omitempty"`
	// FileSize is the played File's size in bytes, sent alongside MovieHash (the
	// hash query pairs the two). Zero when unknown.
	FileSize int64 `json:"fileSize,omitempty"`
}

// SubtitleSearchRequest asks for the candidates a source offers for one Title in
// one language. Page is carried for shape consistency with every other search in
// the contract; a source whose candidate list is never paged ignores it.
type SubtitleSearchRequest struct {
	Ref SubtitleRef `json:"ref"`
	// Language is the wanted subtitle language as a normalized ISO-639-1 code. The
	// host normalizes before asking, so a Plugin never has to.
	Language string `json:"language"`
	// Page is embedded, so limit and offset are flat fields on the wire.
	Page
}

// SubtitleCandidate is one subtitle a source offers: enough for the host to show
// a viewer a choice and to ask for those exact bytes later. It carries none of
// them — a download is its own call.
type SubtitleCandidate struct {
	// ID is the opaque provider handle for THIS candidate, echoed back in a
	// download request and recorded on the fetched row as the pick-lock key.
	ID string `json:"id"`
	// Language is the normalized ISO-639-1 code of the subtitle.
	Language string `json:"language,omitempty"`
	// Format is the subtitle format token the download will be ("srt", "ass",
	// "vtt", "sub"), which decides the host's conversion path.
	Format string `json:"format,omitempty"`
	// Release is the human release name the candidate is synced to (e.g.
	// "Dune.2021.1080p.BluRay"), shown in the picker.
	Release string `json:"release,omitempty"`
	// HearingImpaired and Forced are disposition hints for labeling the choice.
	HearingImpaired bool `json:"hearingImpaired,omitempty"`
	Forced          bool `json:"forced,omitempty"`
	// MatchedBy records which signal produced this candidate ("moviehash" |
	// "imdb" | "query"), so a release-exact match can be marked as such.
	MatchedBy string `json:"matchedBy,omitempty"`
	// Downloads is the source's popularity count, used to order candidates
	// best-first. Informational; the host decides the ordering it shows.
	Downloads int `json:"downloads,omitempty"`
}

// SubtitleSearchResponse is what a search answers. OutcomeMatched carries the
// candidates; OutcomeNoMatch (or an empty list) is the normal "nothing for this
// release in this language"; OutcomeUnavailable is a Plugin that cannot serve the
// call at all. A Go error alongside means the transport failed and is retried.
type SubtitleSearchResponse struct {
	Outcome    Outcome             `json:"outcome"`
	Candidates []SubtitleCandidate `json:"candidates,omitempty"`
	// Detail is optional human copy for a log line or a settings probe. It is never
	// rendered to a viewer and never carries an error identity — Outcome does that.
	Detail string `json:"detail,omitempty"`
}

// SubtitleDownloadRequest asks for one candidate's bytes, whole. MaxBytes is the
// CALLER's cap (ADR-0057 decision 2: byte payloads come back whole and size-capped
// by the caller); a Plugin must refuse rather than return more, and the host
// checks the answer anyway.
type SubtitleDownloadRequest struct {
	Candidate SubtitleCandidate `json:"candidate"`
	MaxBytes  int64             `json:"maxBytes,omitempty"`
}

// SubtitleDownloadResponse carries the subtitle file itself. Data is the complete
// file — there is no streaming in this contract — and marshals as base64 in JSON.
// OutcomeNoMatch means the candidate has vanished from the source since the search.
type SubtitleDownloadResponse struct {
	Outcome Outcome `json:"outcome"`
	Data    []byte  `json:"data,omitempty"`
	// Format is the subtitle format token of Data ("srt", "ass", "vtt", "sub"),
	// which the host converts from; it wins over the candidate's guess.
	Format string `json:"format,omitempty"`
	// ContentType is the media type the source served, informational alongside
	// Format (a source that lies about it does not change how the host converts).
	ContentType string `json:"contentType,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// SubtitleProvider is the Go call surface of the Subtitle provider Extension
// point. It is a Go interface only so a Built-in can be called in-process: every
// parameter and result is a wire type, so the same two calls survive being moved
// across a sandbox boundary in Phase 2 without the contract being redesigned.
//
// The context always carries the host's deadline. An error means the transport or
// the Plugin failed; everything else is in the response's Outcome.
type SubtitleProvider interface {
	// SearchSubtitles returns the candidates for a Title in one language,
	// best-first.
	SearchSubtitles(ctx context.Context, req SubtitleSearchRequest) (SubtitleSearchResponse, error)
	// DownloadSubtitle fetches one candidate's bytes whole, within MaxBytes.
	DownloadSubtitle(ctx context.Context, req SubtitleDownloadRequest) (SubtitleDownloadResponse, error)
}

// SubtitleProviderFactory builds a Subtitle provider Plugin from the Settings an
// Admin saved. It is the ONE thing in this package that is not wire-shaped, which
// is why it lives in the registration beside the Descriptor rather than in it: in
// Phase 1 it constructs a Built-in directly; in Phase 2 the same field becomes the
// host loading a module. An error means the Plugin cannot be built from these
// settings, and the host falls back to making no calls at all (ADR-0001).
type SubtitleProviderFactory func(Settings) (SubtitleProvider, error)

// SubtitleProviderRegistration is what a Plugin hands the host: what it is, and
// how to build it.
type SubtitleProviderRegistration struct {
	Descriptor Descriptor
	New        SubtitleProviderFactory
}

// --- the guest call, for the Subtitle provider Extension point ---------------
//
// A Built-in is handed its Settings once, by the factory that constructs it, and
// keeps them for as long as it lives. An INSTALLED plugin cannot be given them
// that way: it is a WebAssembly instance the host discards and rebuilds on any
// trap, any deadline kill and after a byte budget (ADR-0058 decision 7), so
// anything installed into it would have to be installed again — and a secret
// installed into a long-lived instance is a secret that outlives the call it was
// handed over for.
//
// So the two Subtitle provider calls travel in an envelope carrying the resolved
// Settings, exactly as SinkDeliverRequest does. That is the strongest reading of
// ADR-0058 decision 5's "secrets handed over only at call time", and it is why a
// settings_get host function is not needed for this seam: a rebuilt instance
// starts holding nobody's credential.
//
// The RESPONSES are un-enveloped. SubtitleSearchResponse and
// SubtitleDownloadResponse cross the boundary unchanged, because a guest has
// nothing to say back about the settings it was handed.

// SubtitleSearchCall is what the host hands an Installed Subtitle provider for
// one search.
type SubtitleSearchCall struct {
	// Request is the search itself, exactly as a Built-in receives it: the ref the
	// host gathered — including the content hash the HOST computed, because a
	// Plugin never reads media bytes — and the normalized language.
	Request SubtitleSearchRequest `json:"request"`
	// Settings carries URL (the source the Admin configured, or the manifest's
	// default) and Secret (the API key). Events is empty for a provider.
	Settings Settings `json:"settings"`
}

// SubtitleDownloadCall is what the host hands an Installed Subtitle provider for
// one candidate's bytes.
//
// Request.MaxBytes is the cap the HOST states, already narrowed to what this
// server will accept back from a guest. A Plugin must refuse rather than answer
// with more, and the host checks the length of what comes back anyway: an
// oversize answer is DISCARDED, never truncated, because a truncated subtitle is
// one the host would cache and a viewer would play as though it were whole.
type SubtitleDownloadCall struct {
	Request  SubtitleDownloadRequest `json:"request"`
	Settings Settings                `json:"settings"`
}
