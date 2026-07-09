// Package subfetch is the external subtitle-fetching domain (ADR-0021): the
// optional step that goes to an external provider (OpenSubtitles first) for a
// Subtitle track in a language a Title lacks, matched to the exact release. It
// mirrors internal/enrich verbatim — a narrow provider seam, DB-backed settings
// hot-swapped by a Manager, a live test-connection probe, and identity-keyed
// filesystem caching — so subtitle fetching reuses one mental model. Like
// enrichment it NEVER affects identity and degrades gracefully offline: a
// disabled/absent provider makes zero outbound calls and surfaces nothing to
// playback (ADR-0001).
//
// The network is isolated behind one seam — SubtitleProvider — mirroring how the
// scanner fakes the Prober and enrich fakes MetadataProvider. app.New wires the
// real OpenSubtitles provider; tests inject a fake, so the black-box HTTP tests
// drive the whole fetch flow with zero network. The Service depends only on this
// interface + a Store + a cache dir, never on net/http (ADR-0006).
package subfetch

import (
	"context"
	"errors"
)

// ErrNoMatch is the normal "the provider has no subtitle for this release in this
// language" outcome — a provider returns it (or an empty candidate slice) rather
// than a fatal error. The fetch reports "nothing found" and playback is unaffected.
var ErrNoMatch = errors.New("subfetch: no external subtitle match")

// ErrProviderDisabled is the "no provider is configured/enabled" outcome: the
// Service short-circuits with it BEFORE any network call so a disabled or offline
// server does zero outbound work (ADR-0001). The API maps it to an empty result,
// never an error surfaced to the viewer.
var ErrProviderDisabled = errors.New("subfetch: subtitle provider disabled")

// SubtitleRef is everything a provider may key a search by, gathered from the
// Title and its played File. A provider tries them in the ADR-0021 match order —
// MovieHash (release-exact) → IMDBID → a Title/Year filename query — using
// whichever are present; an un-enriched Title with no IMDBID and an unreadable
// file still gets the filename query.
type SubtitleRef struct {
	// Title/Year are the parsed identity, always present; the last-resort query.
	Title string
	Year  int
	// IMDBID is the enrichment-assigned id ("tt…"), empty on an un-enriched Title.
	IMDBID string
	// MovieHash is the OpenSubtitles moviehash of the played File (subtitle.MovieHash),
	// computed lazily by the Service; empty when the file is unreadable/too small.
	MovieHash string
	// FileSize is the played File's size in bytes, sent alongside MovieHash (the
	// OpenSubtitles hash query pairs the two). 0 when unknown.
	FileSize int64
}

// Candidate is one subtitle the provider offers for a language: enough to show the
// viewer a choice (Label/Release, whether it's a hearing-impaired/forced variant)
// and to download it later (the opaque provider ID + file id). It carries no bytes
// — Download fetches those on the pick.
type Candidate struct {
	// ID is the opaque provider handle for THIS candidate, echoed back to Download
	// and recorded as the fetched row's provider_id (the pick-lock key).
	ID string
	// Language is the normalized ISO-639-1 code of the subtitle (matches the request).
	Language string
	// Format is the subtitle format token the download will be ("srt", "ass",
	// "vtt", "sub"), driving the WebVTT conversion / image classification.
	Format string
	// Release is the human release name the candidate is synced to (e.g.
	// "Dune.2021.1080p.BluRay"), shown in the picker so the viewer recognizes a match.
	Release string
	// HearingImpaired/Forced are disposition hints for labeling the choice.
	HearingImpaired bool
	Forced          bool
	// MatchedBy records which signal produced this candidate ("moviehash" | "imdb" |
	// "query"), so the UI (and tests) can tell a release-exact hash match from a
	// looser filename match. Informational only.
	MatchedBy string
	// Downloads is the provider's popularity count, used to order candidates best-first.
	Downloads int
}

// SubtitleProvider is the external-subtitle seam (ADR-0021, analogue of
// enrich.MetadataProvider): Search finds candidates for a Title+language in the
// match order the provider implements, and Download fetches one candidate's bytes.
// A no-match is ErrNoMatch (or an empty slice), never fatal; a disabled provider
// is the nil provider the builder yields, whose methods return ErrProviderDisabled.
type SubtitleProvider interface {
	// Search returns the candidate subtitles for ref in the wanted ISO-639-1
	// language, best-first. An empty slice (or ErrNoMatch) means the provider has
	// nothing — a normal outcome. It never returns an error for "disabled"; the
	// Service gates that before calling.
	Search(ctx context.Context, ref SubtitleRef, lang string) ([]Candidate, error)
	// Download fetches the bytes of one candidate and reports its format token
	// (subtitle.TextFormat input). ErrNoMatch when the candidate has vanished.
	Download(ctx context.Context, candidate Candidate) (data []byte, format string, err error)
}

// disabledProvider is the nil-object provider the builder yields when no source is
// configured/enabled. Every method short-circuits with ErrProviderDisabled and
// makes NO network call, so an offline server does zero outbound work (ADR-0001).
type disabledProvider struct{}

func (disabledProvider) Search(context.Context, SubtitleRef, string) ([]Candidate, error) {
	return nil, ErrProviderDisabled
}

func (disabledProvider) Download(context.Context, Candidate) ([]byte, string, error) {
	return nil, "", ErrProviderDisabled
}
