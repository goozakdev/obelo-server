package subfetch

import (
	"context"
	"errors"
	"fmt"

	pluginapi "github.com/goozakdev/obelo-server/internal/pluginapi/v1"
	"github.com/goozakdev/obelo-server/internal/store"
)

// The contract edge of the subtitle domain (ADR-0057). A Subtitle provider is a
// Plugin now: the server asks it through pluginapi's wire types and it answers
// with an Outcome. This file is the adapter that turns such a Plugin back into the
// SubtitleProvider interface the Service has always called, mapping each Outcome
// to the sentinel the Service already matches on — so the Service, the API
// handlers and every test that reads ErrNoMatch are untouched by the move.
//
// Nothing above this file knows the contract exists; nothing below it knows the
// server's sentinels exist. That is the whole point: when Phase 2 puts a sandbox
// boundary under pluginapi, this file is the only thing between it and the domain.

// SlugOpenSubtitles is the stable provider slug persisted in the subtitle settings
// rows and used in the settings API routes. It is the same string the OpenSubtitles
// Built-in registers under (opensubtitles.Slug) — the subtitle domain keeps its own
// copy because a settings row it seeds must not depend on which Plugins happen to
// be registered, and a row whose slug no Plugin claims is simply never built.
const SlugOpenSubtitles = "opensubtitles"

// maxSubtitleBytes is the cap the HOST states on a download (ADR-0057 decision 2:
// byte payloads come back whole and size-capped by the caller). Generous for any
// subtitle file; a Plugin that answers with more is refused here rather than
// trusted, because the cap is the host's to enforce.
const maxSubtitleBytes = 8 << 20 // 8 MiB

// errOutcomeNotInPoint is what a Plugin gets for answering with an Outcome that
// belongs to a different Extension point — a metadata external-ref outcome from a
// Subtitle provider, say, or a value this build has never heard of. It is a
// programming error in the Plugin, not a domain outcome, so it surfaces as a real
// error rather than quietly reading as "nothing found".
var errOutcomeNotInPoint = errors.New("subfetch: outcome is not part of the Subtitle provider extension point")

// ProviderFromPlugin adapts a contract-level Subtitle provider Plugin to the
// SubtitleProvider interface the fetch Service calls. It is the ONLY place the
// subtitle domain translates between the two.
func ProviderFromPlugin(p pluginapi.SubtitleProvider) SubtitleProvider {
	if p == nil {
		return disabledProvider{}
	}
	return pluginProvider{plugin: p}
}

// pluginProvider is the adapter itself: wire types out, domain types and sentinels
// back.
type pluginProvider struct{ plugin pluginapi.SubtitleProvider }

// Search asks the Plugin for candidates and maps its Outcome to a sentinel. A Go
// error from the Plugin is a transport failure and is passed through untouched —
// the host treats it as transient rather than a settled non-answer (ADR-0048).
func (a pluginProvider) Search(ctx context.Context, ref SubtitleRef, lang string) ([]Candidate, error) {
	resp, err := a.plugin.SearchSubtitles(ctx, pluginapi.SubtitleSearchRequest{
		Ref: pluginapi.SubtitleRef{
			Title:     ref.Title,
			Year:      ref.Year,
			IMDBID:    ref.IMDBID,
			MovieHash: ref.MovieHash,
			FileSize:  ref.FileSize,
		},
		Language: lang,
	})
	if err != nil {
		return nil, err
	}
	if err := outcomeError(resp.Outcome); err != nil {
		return nil, err
	}
	// A matched outcome with an empty list means the same thing a no-match does.
	if len(resp.Candidates) == 0 {
		return nil, ErrNoMatch
	}
	out := make([]Candidate, 0, len(resp.Candidates))
	for _, c := range resp.Candidates {
		out = append(out, Candidate{
			ID:              c.ID,
			Language:        c.Language,
			Format:          c.Format,
			Release:         c.Release,
			HearingImpaired: c.HearingImpaired,
			Forced:          c.Forced,
			MatchedBy:       c.MatchedBy,
			Downloads:       c.Downloads,
		})
	}
	return out, nil
}

// Download asks the Plugin for one candidate's bytes under the host's cap and maps
// its Outcome to a sentinel. The cap is re-checked here: the contract says the
// caller caps, so the caller — not the Plugin's good manners — is what bounds what
// reaches the subtitle cache.
func (a pluginProvider) Download(ctx context.Context, candidate Candidate) ([]byte, string, error) {
	resp, err := a.plugin.DownloadSubtitle(ctx, pluginapi.SubtitleDownloadRequest{
		Candidate: pluginapi.SubtitleCandidate{
			ID:              candidate.ID,
			Language:        candidate.Language,
			Format:          candidate.Format,
			Release:         candidate.Release,
			HearingImpaired: candidate.HearingImpaired,
			Forced:          candidate.Forced,
			MatchedBy:       candidate.MatchedBy,
			Downloads:       candidate.Downloads,
		},
		MaxBytes: maxSubtitleBytes,
	})
	if err != nil {
		return nil, "", err
	}
	if err := outcomeError(resp.Outcome); err != nil {
		return nil, "", err
	}
	if len(resp.Data) == 0 {
		return nil, "", ErrNoMatch
	}
	if int64(len(resp.Data)) > maxSubtitleBytes {
		return nil, "", fmt.Errorf("subfetch: subtitle exceeds %d bytes", maxSubtitleBytes)
	}
	format := resp.Format
	if format == "" {
		format = candidate.Format
	}
	return resp.Data, format, nil
}

// outcomeError maps a contract Outcome onto the subtitle domain's sentinels — the
// translation ADR-0057 decision 2 puts at the edge so that the Service, the API
// handlers and the existing tests keep matching on exactly the errors they always
// did. The mapping is TOTAL over pluginapi.AllOutcomes(), and the test that proves
// it ranges over that list, so a new Outcome cannot be added without this file
// deciding what it means here.
//
//   - matched      → nil, the candidates/bytes are in the response
//   - no-match     → ErrNoMatch, the normal "nothing for this release"
//   - rejected     → ErrNoMatch: acceptance is the host's judgment and the subtitle
//     domain applies none, so a source that declined its own top hit is
//     indistinguishable from having nothing, and nothing renders a reason
//   - unavailable  → ErrProviderDisabled, the same "no source is answering" the
//     nil-object provider reports, which degrades to an empty
//     candidate list rather than an error shown to a viewer (ADR-0001)
//   - the ref-* outcomes belong to the Metadata provider Extension point; a
//     Subtitle provider returning one is a Plugin bug, not an answer
func outcomeError(o pluginapi.Outcome) error {
	switch o {
	case pluginapi.OutcomeMatched:
		return nil
	case pluginapi.OutcomeNoMatch, pluginapi.OutcomeRejected:
		return ErrNoMatch
	case pluginapi.OutcomeUnavailable:
		return ErrProviderDisabled
	case pluginapi.OutcomeRefInvalid, pluginapi.OutcomeRefKindMismatch, pluginapi.OutcomeRefUnsupportedKind:
		return fmt.Errorf("%w: %q", errOutcomeNotInPoint, o)
	default:
		return fmt.Errorf("%w: %q", errOutcomeNotInPoint, o)
	}
}

// BuildProvider composes the active SubtitleProvider from the registered Subtitle
// provider Plugins and the persisted settings rows: the first Plugin whose row is
// enabled AND (if it requires a key) has one on file, built from the fixed Settings
// shape and wrapped in the adapter — otherwise the disabled nil-object provider
// that makes zero calls (ADR-0001).
//
// The registry is a VALUE passed in, not a package-level slice (ADR-0057 decision
// 5): this function has no opinion about which Plugins exist, so a test or a future
// loader hands it a different set without touching the subtitle domain.
func BuildProvider(reg *pluginapi.Registry, rows []store.SubtitleProviderRow) SubtitleProvider {
	for _, row := range rows {
		registration, ok := reg.SubtitleProvider(row.Slug)
		if !ok {
			continue // a settings row for a Plugin this build does not have
		}
		d := registration.Descriptor
		if !row.Enabled || (d.RequiresKey && row.APIKey == "") {
			continue
		}
		base := row.BaseURL
		if base == "" {
			base = d.DefaultURL
		}
		plugin, err := registration.New(pluginapi.Settings{
			Enabled: true,
			Secret:  row.APIKey,
			URL:     base,
			URL2:    d.DefaultURL2,
		})
		if err != nil || plugin == nil {
			// A Plugin that cannot be built from these settings makes no calls at
			// all rather than half-working (ADR-0001).
			continue
		}
		return ProviderFromPlugin(plugin)
	}
	return disabledProvider{}
}

// BuilderFor returns the BuildFunc the Manager rebuilds from on every settings
// save, closed over the registry the composition root built. It is what app.New
// hands to NewManager in production; a test substitutes its own BuildFunc through
// app.WithSubtitleProviderBuilder exactly as before.
func BuilderFor(reg *pluginapi.Registry) BuildFunc {
	return func(rows []store.SubtitleProviderRow) SubtitleProvider {
		return BuildProvider(reg, rows)
	}
}
