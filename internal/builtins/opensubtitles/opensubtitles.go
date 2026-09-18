// Package opensubtitles is the OpenSubtitles Built-in: the Plugin that provides
// the Subtitle provider Extension point against OpenSubtitles' REST v1 API
// (ADR-0021, ADR-0057). It is compiled into the server and registered through the
// same contract an Installed plugin would use, which is what proves the contract
// honest — if pluginapi could not express OpenSubtitles' two-step download or its
// hash-first match order, that is discovered here, while the contract can still
// change freely.
//
// Everything in this package is OpenSubtitles-specific HTTP/JSON. It knows nothing
// about the server: not the fetch Service, not the store, not the settings rows.
// Its whole surface is pluginapi.SubtitleProvider — wire types in, wire types out —
// so a no-match is an Outcome rather than a sentinel error, and the subtitle
// domain's sentinels are reconstructed by the adapter on the host side.
package opensubtitles

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/goozakdev/obelo-server/internal/safefetch"
	"github.com/goozakdev/obelo-server/internal/subtitle"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Slug is the stable Plugin identity: the key persisted in the subtitle-provider
// settings row and used in the settings API routes.
const Slug = "opensubtitles"

// DefaultBaseURL is the public OpenSubtitles REST API host used when the operator
// configures no override.
const DefaultBaseURL = "https://api.opensubtitles.com/api/v1"

// maxSubtitleBytes is the fallback cap when the host states none. The host always
// states one (ADR-0057 decision 2: byte payloads are capped by the caller); this
// exists so the Plugin can never be talked into reading an unbounded body.
const maxSubtitleBytes = 8 << 20 // 8 MiB — generous for any subtitle file.

// Provider is the OpenSubtitles Plugin. It owns all OpenSubtitles REST v1
// HTTP/JSON specifics and treats a no-match or a network failure as a normal
// outcome the host degrades over, never identity (ADR-0001/0002).
//
// SearchSubtitles implements the ADR-0021 match order — moviehash → imdb_id →
// filename query — issuing each narrowing in turn and returning the first that
// yields candidates, tagged by which signal matched. DownloadSubtitle performs
// OpenSubtitles' two-step download (request a time-limited link for the file id,
// then GET the bytes).
type Provider struct {
	APIKey     string
	BaseURL    string // e.g. https://api.opensubtitles.com/api/v1
	UserAgent  string
	HTTPClient *http.Client
}

// Provider implements the Subtitle provider Extension point.
var _ pluginapi.SubtitleProvider = (*Provider)(nil)

// New builds the Plugin from the Settings an Admin saved: Secret is the API key,
// URL the effective base URL. It is the pluginapi.SubtitleProviderFactory this
// Built-in registers with. An empty URL falls back to the public host; the HTTP
// client gets a sane timeout so a slow lookup can't hang a request even if the
// host's deadline were somehow absent.
func New(s pluginapi.Settings) (pluginapi.SubtitleProvider, error) {
	return NewProvider(s.Secret, s.URL), nil
}

// NewProvider builds a Provider from a key and a base URL directly — the shape the
// settings probe wants, and what New wraps.
func NewProvider(apiKey, baseURL string) *Provider {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Provider{
		APIKey:     apiKey,
		BaseURL:    baseURL,
		UserAgent:  "obelo/1.0 (self-hosted)",
		HTTPClient: &http.Client{Timeout: 20 * time.Second},
	}
}

// client returns the HTTP client every call in this file goes through, carrying
// safefetch's redirect policy.
//
// It matters most for fetchLink: the download URL is a `link` string out of the
// provider's own JSON response, fetched server-side with the bytes written to disk
// beside the operator's media — the same shape as the artwork fetcher, and the same
// answer. It is applied to the search/download calls too, which go to the CONFIGURED
// base URL: only redirect targets are checked, so an operator's own mirror still
// works on the first hop, and a mirror that then bounces us at 169.254.169.254 does
// not. Guard copies, so a caller-supplied HTTPClient (and http.DefaultClient, which
// is process-global) is never mutated — and the policy cannot be lost by injecting
// a bare client.
func (p *Provider) client() *http.Client {
	return safefetch.Guard(p.HTTPClient)
}

// SearchSubtitles tries the match narrowings in order and returns the first
// non-empty set. Each narrowing is a distinct query against /subtitles; a
// narrowing that yields no candidates falls through to the next. All narrowings
// exhausted → OutcomeNoMatch, which is an answer, not a failure. A transport or
// status failure is a Go error, which the host treats as transient (ADR-0048).
func (p *Provider) SearchSubtitles(ctx context.Context, req pluginapi.SubtitleSearchRequest) (pluginapi.SubtitleSearchResponse, error) {
	lang := subtitle.NormalizeLang(req.Language)
	if lang == "" {
		// Can't ask OpenSubtitles for an unknown language — nothing to find, no call.
		return pluginapi.SubtitleSearchResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
	}
	ref := req.Ref

	type narrowing struct {
		matchedBy string
		params    url.Values
	}
	var order []narrowing
	if ref.MovieHash != "" {
		q := url.Values{}
		q.Set("moviehash", ref.MovieHash)
		order = append(order, narrowing{"moviehash", q})
	}
	if ref.IMDBID != "" {
		q := url.Values{}
		q.Set("imdb_id", strings.TrimPrefix(ref.IMDBID, "tt"))
		order = append(order, narrowing{"imdb", q})
	}
	if ref.Title != "" {
		q := url.Values{}
		q.Set("query", ref.Title)
		if ref.Year > 0 {
			q.Set("year", strconv.Itoa(ref.Year))
		}
		order = append(order, narrowing{"query", q})
	}

	for _, n := range order {
		n.params.Set("languages", lang)
		cands, err := p.searchOnce(ctx, n.params, lang, n.matchedBy)
		if err != nil {
			return pluginapi.SubtitleSearchResponse{}, err
		}
		if len(cands) > 0 {
			return pluginapi.SubtitleSearchResponse{
				Outcome:    pluginapi.OutcomeMatched,
				Candidates: cands,
			}, nil
		}
	}
	return pluginapi.SubtitleSearchResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
}

// searchOnce issues one /subtitles query and parses the candidate list.
func (p *Provider) searchOnce(ctx context.Context, params url.Values, lang, matchedBy string) ([]pluginapi.SubtitleCandidate, error) {
	reqURL := strings.TrimRight(p.BaseURL, "/") + "/subtitles?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("opensubtitles: building request: %w", err)
	}
	p.setHeaders(req)

	resp, err := p.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("opensubtitles: search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("opensubtitles: search: status %d", resp.StatusCode)
	}

	var out struct {
		Data []struct {
			Attributes struct {
				Language        string `json:"language"`
				HearingImpaired bool   `json:"hearing_impaired"`
				Foreign         bool   `json:"foreign_parts_only"`
				DownloadCount   int    `json:"download_count"`
				Release         string `json:"release"`
				Files           []struct {
					FileID   int    `json:"file_id"`
					FileName string `json:"file_name"`
				} `json:"files"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("opensubtitles: decoding response: %w", err)
	}

	var cands []pluginapi.SubtitleCandidate
	for _, d := range out.Data {
		a := d.Attributes
		if len(a.Files) == 0 {
			continue
		}
		f := a.Files[0]
		cands = append(cands, pluginapi.SubtitleCandidate{
			ID:              strconv.Itoa(f.FileID),
			Language:        subtitle.NormalizeLang(a.Language),
			Format:          formatFromFilename(f.FileName),
			Release:         a.Release,
			HearingImpaired: a.HearingImpaired,
			Forced:          a.Foreign,
			MatchedBy:       matchedBy,
			Downloads:       a.DownloadCount,
		})
	}
	return cands, nil
}

// DownloadSubtitle performs the OpenSubtitles two-step: POST /download with the
// candidate's file id to obtain a time-limited link, then GET the link for the
// bytes. The bytes come back WHOLE (there is no streaming in the contract) and
// within the caller's MaxBytes. The format is the candidate's, falling back to the
// download URL's extension.
func (p *Provider) DownloadSubtitle(ctx context.Context, req pluginapi.SubtitleDownloadRequest) (pluginapi.SubtitleDownloadResponse, error) {
	candidate := req.Candidate
	fileID, err := strconv.Atoi(candidate.ID)
	if err != nil {
		return pluginapi.SubtitleDownloadResponse{}, fmt.Errorf("opensubtitles: bad candidate id %q: %w", candidate.ID, err)
	}

	body, _ := json.Marshal(map[string]any{"file_id": fileID})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(p.BaseURL, "/")+"/download", strings.NewReader(string(body)))
	if err != nil {
		return pluginapi.SubtitleDownloadResponse{}, fmt.Errorf("opensubtitles: building download request: %w", err)
	}
	p.setHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client().Do(httpReq)
	if err != nil {
		return pluginapi.SubtitleDownloadResponse{}, fmt.Errorf("opensubtitles: download request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.SubtitleDownloadResponse{}, fmt.Errorf("opensubtitles: download: status %d", resp.StatusCode)
	}
	var dl struct {
		Link     string `json:"link"`
		FileName string `json:"file_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dl); err != nil {
		return pluginapi.SubtitleDownloadResponse{}, fmt.Errorf("opensubtitles: decoding download link: %w", err)
	}
	if dl.Link == "" {
		// The candidate has vanished since the search — an answer, not a failure.
		return pluginapi.SubtitleDownloadResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
	}

	data, contentType, err := p.fetchLink(ctx, dl.Link, req.MaxBytes)
	if err != nil {
		return pluginapi.SubtitleDownloadResponse{}, err
	}
	format := candidate.Format
	if format == "" {
		format = formatFromFilename(dl.FileName)
	}
	return pluginapi.SubtitleDownloadResponse{
		Outcome:     pluginapi.OutcomeMatched,
		Data:        data,
		Format:      format,
		ContentType: contentType,
	}, nil
}

// fetchLink GETs the time-limited download URL and returns the subtitle bytes,
// bounded by the caller's cap so a redirect to an error page can't balloon memory.
// The link is the PROVIDER'S string, not ours, so the fetch runs under the redirect
// policy on client() — a hop pointed inward is refused, not followed.
func (p *Provider) fetchLink(ctx context.Context, link string, maxBytes int64) ([]byte, string, error) {
	if maxBytes <= 0 {
		maxBytes = maxSubtitleBytes
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, "", fmt.Errorf("opensubtitles: building link request: %w", err)
	}
	req.Header.Set("User-Agent", p.UserAgent)
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("opensubtitles: fetching subtitle bytes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("opensubtitles: fetching subtitle bytes: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("opensubtitles: reading subtitle bytes: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("opensubtitles: subtitle exceeds %d bytes", maxBytes)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// setHeaders applies the OpenSubtitles auth + client identity headers.
func (p *Provider) setHeaders(req *http.Request) {
	req.Header.Set("Api-Key", p.APIKey)
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Accept", "application/json")
}

// formatFromFilename derives the subtitle format token from a filename's extension
// (srt/ass/ssa/vtt/sub), defaulting to "srt" — OpenSubtitles' overwhelmingly common
// text format — when there is no usable extension.
func formatFromFilename(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 || i == len(name)-1 {
		return "srt"
	}
	ext := strings.ToLower(name[i+1:])
	switch ext {
	case "srt", "ass", "ssa", "vtt", "sub":
		return ext
	default:
		return "srt"
	}
}
