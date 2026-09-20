// Package testprovider is the Metadata provider the SDK's own proofs are built
// from: ONE source, driven two ways.
//
//   - Natively, by a sdktest.Host, in pluginsdk's own test. Milliseconds, no
//     toolchain, no sandbox.
//   - Inside wazero, compiled into pluginsdk/testdata/guest and installed through
//     the real POST /settings/plugins by internal/api's black-box suite.
//
// That the SAME type answers both is the claim the SDK makes and the reason
// [pluginsdk.Host] is an interface: the seven bundled providers of ADR-0059 keep
// the unit tests they already have AND become WebAssembly modules, rather than
// choosing.
//
// It is `internal` because it is an example and a fixture, not API. What issues
// 04–07 copy is its SHAPE — a struct holding a Host, three methods, no net/http —
// and not this file.
package testprovider

import (
	"context"
	"encoding/json"
	"strings"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

// The strings this provider writes. Nothing else in the server writes them, so a
// record carrying one was decorated by this provider and by nothing else — which
// is what lets a black-box API test recognize it without being told.
const (
	// Overview is the descriptive line an ordinary match carries.
	Overview = "Filled from inside the sandbox by a plugin built with the Obelo Go SDK."
	// ArtworkPath is appended to Settings.URL2 to make the poster URL. The HOST
	// downloads it; a plugin returns URLs and never bytes.
	ArtworkPath = "/art/poster.jpg"
	// Source is the slug this provider stamps on its records.
	Source = "sdkguest"
	// KVKey is the key the "kv" mode writes in this plugin's own namespace.
	KVKey = "sdk-guest-marker"
	// FetchPath is the path the "fetch" mode asks the operator's own URL for.
	FetchPath = "/record"
)

// Provider is a Metadata provider written the way the SDK asks for one: it holds
// a [pluginsdk.Host] and nothing else, and every outbound request, log line,
// key-value read and settings read goes through it.
//
// It implements exactly [pluginapi.MetadataProvider] — the three mandatory calls
// — and NONE of the three optional interfaces, on purpose: a manifest that
// declares `external-ref` against this provider gets OutcomeUnavailable from the
// SDK's dispatcher rather than a broken call, which is the degradation ADR-0057
// decision 3 asks for and is one of this issue's acceptance criteria.
type Provider struct {
	host pluginsdk.Host
}

var _ pluginapi.MetadataProvider = (*Provider)(nil)

// New builds the provider on a Host. In the sandbox that Host is
// pluginsdk.Sandbox(); in a test it is sdktest.New(...). The provider cannot tell,
// and that is the point.
func New(h pluginsdk.Host) *Provider { return &Provider{host: h} }

// Host is the Host this provider was built on, so a test can assert what it did.
func (p *Provider) Host() pluginsdk.Host { return p.host }

// Lookup resolves one ref to one record.
//
// The `obelo-mode=` marker in the operator's URL chooses which part this one
// provider plays, exactly as the SDK-free test guest's does: a suite needs a
// provider that reads its settings, one that round-trips a key, and one that
// fetches, and three modules would be three compiles and three things to keep in
// step. A real provider has none of this.
func (p *Provider) Lookup(ctx context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	s := p.host.Settings()
	if !serves(req.Ref.Kind) {
		return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
	}

	switch Mode(s.URL) {
	case "kv":
		// The plugin-scoped namespace, round-tripped: write this plugin's own
		// secret under a key every guest in the suite uses, read it back, and
		// report what came out. Two plugins doing this must not see each other's.
		if err := p.host.KVSet(KVKey, []byte(s.Secret)); err != nil {
			return pluginapi.LookupResponse{}, err
		}
		value, found, err := p.host.KVGet(KVKey)
		if err != nil {
			return pluginapi.LookupResponse{}, err
		}
		if !found {
			return pluginapi.LookupResponse{
				Outcome: pluginapi.OutcomeUnavailable,
				Detail:  "kv_get answered absent for a key this plugin just wrote",
			}, nil
		}
		return p.record(req.Ref, string(value), s), nil

	case "settings":
		// What the host resolved, as JSON, in a field a black-box test can read.
		// An object rather than a sentence because that is what is being proved: a
		// number comes back a number, an absent rate limit comes back absent.
		return p.record(req.Ref, string(encodeSettings(s)), s), nil

	case "fetch":
		// The only way out of the sandbox, through the SDK's Host. The target is
		// the OPERATOR's own URL, which the host allows beside the manifest
		// allowlist because an author cannot know which mirror an operator points
		// at.
		var out struct {
			Overview string `json:"overview"`
		}
		err := pluginsdk.GetJSON(ctx, p.host, baseOf(s.URL)+FetchPath, nil, &out)
		if err != nil {
			p.host.Log(pluginsdk.LevelError, "the lookup could not be made: "+err.Error())
			return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: err.Error()}, nil
		}
		return p.record(req.Ref, out.Overview, s), nil

	default:
		return p.record(req.Ref, Overview, s), nil
	}
}

// Search offers one candidate for the Edit-item box. The host caps and judges
// them; this provider only offers.
func (p *Provider) Search(_ context.Context, req pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	return pluginapi.SearchResponse{
		Outcome: pluginapi.OutcomeMatched,
		Candidates: []pluginapi.SearchCandidate{{
			ExternalID: Source + "-" + req.Kind,
			Title:      req.Query,
			Kind:       req.Kind,
		}},
	}, nil
}

// sdk-sample:begin artwork

// ArtworkCandidates lists the one image this source offers for a role — a URL on
// the image host the operator configured, which the HOST downloads.
func (p *Provider) ArtworkCandidates(_ context.Context, req pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	s := p.host.Settings()
	return pluginapi.ArtworkCandidatesResponse{
		Outcome: pluginapi.OutcomeMatched,
		Candidates: []pluginapi.ArtworkCandidate{{
			URL:    s.URL2 + ArtworkPath,
			Width:  600,
			Height: 900,
			Source: Source,
		}},
	}, nil
}

// sdk-sample:end artwork

// record is the matched answer, with the overview the mode produced.
func (p *Provider) record(ref pluginapi.MediaRef, overview string, s pluginapi.Settings) pluginapi.LookupResponse {
	return pluginapi.LookupResponse{
		Outcome: pluginapi.OutcomeMatched,
		Record: pluginapi.MetadataRecord{
			Matched:    true,
			Name:       ref.Title,
			Overview:   overview,
			Genres:     []string{"Test Genre"},
			Artwork:    []pluginapi.ArtworkRef{{Role: "poster", URL: s.URL2 + ArtworkPath}},
			ExternalID: Source + "-" + ref.Kind,
			Source:     Source,
		},
	}
}

// EchoedSettings is what the "settings" mode puts in the overview: the host-
// resolved settings a guest is allowed to see, in a shape a test can decode.
//
// The secret is NOT here. "Secrets at call time only" means a plugin may read one
// to sign a request; writing it into a record would put it in the database and on
// a screen, and a fixture that did that would be teaching the wrong thing.
type EchoedSettings struct {
	Enabled         bool           `json:"enabled"`
	HasSecret       bool           `json:"hasSecret"`
	URL             string         `json:"url"`
	URL2            string         `json:"url2"`
	Language        string         `json:"language"`
	RateLimitMillis *int           `json:"rateLimitMillis,omitempty"`
	Values          map[string]any `json:"values,omitempty"`
}

// Echo is the settings a call would report, as a value, so a native test can
// build the same document the guest writes.
func Echo(s pluginapi.Settings) EchoedSettings {
	return EchoedSettings{
		Enabled:         s.Enabled,
		HasSecret:       s.Secret != "",
		URL:             s.URL,
		URL2:            s.URL2,
		Language:        s.Language,
		RateLimitMillis: s.RateLimitMillis,
		Values:          s.Values,
	}
}

func encodeSettings(s pluginapi.Settings) []byte {
	out, err := json.Marshal(Echo(s))
	if err != nil {
		return []byte("{}")
	}
	return out
}

// Mode reads the `obelo-mode=` marker out of the operator's URL. It is the one
// string a test can set that reaches inside a guest.
func Mode(target string) string {
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

// baseOf is the operator's URL with the mode marker's query stripped, so a
// fetched path lands where the test's server serves it.
func baseOf(target string) string {
	if i := strings.Index(target, "?"); i >= 0 {
		return strings.TrimSuffix(target[:i], "/")
	}
	return strings.TrimSuffix(target, "/")
}

// serves reports whether this provider answers for a fine entity kind. A kind it
// does not serve is OutcomeNoMatch and never a guess.
func serves(kind string) bool {
	switch kind {
	case "movie", "show", "season", "episode", "artist", "album", "track":
		return true
	}
	return false
}
