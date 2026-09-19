// Package opensubtitles is the Obelo Subtitle provider for OpenSubtitles' REST v1
// API, as a plugin (ADR-0021, ADR-0059, .scratch/bundled-plugins issue 09).
//
// It is a port and not a rewrite. The ADR-0021 match order — moviehash, then
// imdb_id, then a filename query — the query parameters, the JSON shapes, the
// two-step download and the format fallback all came from
// internal/builtins/opensubtitles, which this file replaces: the same requests go
// out and the same candidates come back. What changed is where the bytes come
// from. There is no net/http here, no client, no timeout and no redirect policy:
// the provider asks a [pluginsdk.Host], and the host applies its allowlist, its
// fetch policy (a redirect into private address space is refused, exactly as
// safefetch refused it for the Built-in), its User-Agent and its byte cap.
//
// # Where an error goes
//
// A Subtitle provider's Go error is a STRIKE against the plugin, and three in a
// row disable it (ADR-0058 decision 7; the 2026-09-18 amendment that stopped
// counting a clean error applies to Metadata providers only). So the line
// [pluginsdk.Unavailable] draws matters more here than anywhere:
//
//   - "OpenSubtitles could not answer right now" — the host refused the fetch,
//     the fetch ran out of the call's budget, or OpenSubtitles answered 408, 429
//     or a 5xx — is OutcomeUnavailable with the reason in Detail. The subtitle
//     domain degrades that to "nothing found" (ADR-0001), and it costs nothing.
//   - The DOWNLOAD QUOTA is the same kind of answer and gets the same treatment.
//     OpenSubtitles meters downloads per account per day and says so with a 406;
//     a viewer who fetches one subtitle too many has done nothing wrong, and
//     under the plain 4xx rule three such presses would take the whole provider
//     off the server until tomorrow and an Admin's click.
//   - A 401, a 403, or a document this code cannot read describe OUR REQUEST, and
//     only an operator can fix them. Those stay Go errors, which is what puts
//     "status 401" on the Plugins screen where an Admin will read it.
//
// # The byte cap
//
// The host states MaxBytes on every download and refuses an answer over it
// whole. This plugin never gets as far as answering one: its manifest asks for
// an 8 MiB fetch limit, the host refuses a body over that before the guest sees
// a byte, and a body under it but over a smaller stated MaxBytes is answered as
// unavailable here, naming both numbers — the SDK's advice for a provider that
// cannot fit under the cap.
package opensubtitles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

// Source is the plugin's id, and the key its settings row is written under.
const Source = "opensubtitles"

// DefaultBaseURL is the public OpenSubtitles REST API host, used when the
// Settings carry no URL. The host normally supplies the manifest's defaultUrl,
// so this is a fallback for a test or a caller that did not.
const DefaultBaseURL = "https://api.opensubtitles.com/api/v1"

// fallbackMaxBytes bounds a download when the request states no cap. The host
// always states one (ADR-0057 decision 2); this exists so the plugin can never
// be talked into answering an unbounded body. It is the Built-in's number.
const fallbackMaxBytes = 8 << 20

// statusQuotaReached is what OpenSubtitles answers POST /download with once the
// account's daily download allowance is spent.
const statusQuotaReached = 406

// Provider is the OpenSubtitles plugin. It holds the host and nothing else: the
// key and the base URL arrive with each call, in Settings.
type Provider struct {
	host pluginsdk.Host
}

var _ pluginapi.SubtitleProvider = (*Provider)(nil)

// New builds the provider over a host — pluginsdk.Sandbox() inside the module,
// an sdktest.Host in a native test.
func New(host pluginsdk.Host) *Provider {
	return &Provider{host: host}
}

// SearchSubtitles tries the match narrowings in order and returns the first
// non-empty set. Each narrowing is a distinct query against /subtitles; one that
// yields no candidates falls through to the next, and all of them exhausted is
// OutcomeNoMatch, which is an answer.
func (p *Provider) SearchSubtitles(ctx context.Context, req pluginapi.SubtitleSearchRequest) (pluginapi.SubtitleSearchResponse, error) {
	lang := languageParam(req.Language)
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
		cands, err := p.searchOnce(ctx, n.params, n.matchedBy)
		if err != nil {
			if detail, ok := pluginsdk.Unavailable(err); ok {
				return pluginapi.SubtitleSearchResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
			}
			return pluginapi.SubtitleSearchResponse{}, fmt.Errorf("opensubtitles: search: %w", err)
		}
		if len(cands) > 0 {
			return pluginapi.SubtitleSearchResponse{Outcome: pluginapi.OutcomeMatched, Candidates: cands}, nil
		}
	}
	return pluginapi.SubtitleSearchResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
}

// searchResponse is the part of a /subtitles document this plugin reads.
type searchResponse struct {
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

// searchOnce issues one /subtitles query and parses the candidate list.
func (p *Provider) searchOnce(ctx context.Context, params url.Values, matchedBy string) ([]pluginapi.SubtitleCandidate, error) {
	var out searchResponse
	if err := pluginsdk.GetJSON(ctx, p.host, p.baseURL()+"/subtitles", params, &out, p.headers()...); err != nil {
		return nil, err
	}
	var cands []pluginapi.SubtitleCandidate
	for _, d := range out.Data {
		a := d.Attributes
		if len(a.Files) == 0 {
			continue
		}
		f := a.Files[0]
		cands = append(cands, pluginapi.SubtitleCandidate{
			ID: strconv.Itoa(f.FileID),
			// The source's own code ("en", "pt-BR"), lowercased. Mapping it onto
			// the server's ISO 639-1 vocabulary is the host's job, done once for
			// every Subtitle provider in internal/subfetch.
			Language:        strings.ToLower(strings.TrimSpace(a.Language)),
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

// DownloadSubtitle performs OpenSubtitles' two-step download: POST /download
// with the candidate's file id for a time-limited link, then GET the link for
// the bytes, whole. The format is the candidate's, falling back to the file
// name's extension.
func (p *Provider) DownloadSubtitle(ctx context.Context, req pluginapi.SubtitleDownloadRequest) (pluginapi.SubtitleDownloadResponse, error) {
	candidate := req.Candidate
	fileID, err := strconv.Atoi(candidate.ID)
	if err != nil {
		return pluginapi.SubtitleDownloadResponse{}, fmt.Errorf("opensubtitles: bad candidate id %q: %w", candidate.ID, err)
	}

	body, _ := json.Marshal(map[string]any{"file_id": fileID})
	linkReq := pluginapi.FetchRequest{
		Method:  "POST",
		URL:     p.baseURL() + "/download",
		Headers: append(p.headers(), pluginsdk.Header("Content-Type", "application/json")),
		Body:    body,
	}
	var dl struct {
		Link     string `json:"link"`
		FileName string `json:"file_name"`
		Message  string `json:"message"`
	}
	resp, err := pluginsdk.Do(ctx, p.host, linkReq)
	if err != nil {
		return unavailableOr(err, quotaDetail(resp, err), "download request")
	}
	if err := json.Unmarshal(resp.Body, &dl); err != nil {
		return pluginapi.SubtitleDownloadResponse{}, fmt.Errorf("opensubtitles: decoding download link: %w", err)
	}
	if dl.Link == "" {
		// The candidate has vanished since the search — an answer, not a failure.
		return pluginapi.SubtitleDownloadResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
	}

	// The link is the PROVIDER'S string, not ours. The host checks its target
	// against the manifest allowlist and every redirect off it against the fetch
	// policy, so a link pointed inward is refused rather than followed.
	file, err := pluginsdk.Do(ctx, p.host, pluginapi.FetchRequest{URL: dl.Link})
	if err != nil {
		return unavailableOr(err, "", "fetching subtitle bytes")
	}
	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = fallbackMaxBytes
	}
	if int64(len(file.Body)) > maxBytes {
		return pluginapi.SubtitleDownloadResponse{
			Outcome: pluginapi.OutcomeUnavailable,
			Detail:  fmt.Sprintf("the subtitle is %d bytes, over the %d this server accepts", len(file.Body), maxBytes),
		}, nil
	}

	format := candidate.Format
	if format == "" {
		format = formatFromFilename(dl.FileName)
	}
	return pluginapi.SubtitleDownloadResponse{
		Outcome:     pluginapi.OutcomeMatched,
		Data:        file.Body,
		Format:      format,
		ContentType: headerValue(file.Headers, "Content-Type"),
	}, nil
}

// unavailableOr answers a failed fetch: OutcomeUnavailable when it was the
// source or the host not answering (or the download quota, when quota is set),
// a Go error otherwise.
func unavailableOr(err error, quota, what string) (pluginapi.SubtitleDownloadResponse, error) {
	if quota != "" {
		return pluginapi.SubtitleDownloadResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: quota}, nil
	}
	if detail, ok := pluginsdk.Unavailable(err); ok {
		return pluginapi.SubtitleDownloadResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
	}
	return pluginapi.SubtitleDownloadResponse{}, fmt.Errorf("opensubtitles: %s: %w", what, err)
}

// quotaDetail is the sentence for a spent download quota, and empty for any
// other failure. OpenSubtitles says how many downloads remain and when they
// reset in the body's "message"; that is what an operator wants to read.
func quotaDetail(resp pluginapi.FetchResponse, err error) string {
	var fe *pluginsdk.FetchError
	if !errors.As(err, &fe) || fe.Status != statusQuotaReached {
		return ""
	}
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(resp.Body, &body) == nil && strings.TrimSpace(body.Message) != "" {
		return "the OpenSubtitles download quota is spent: " + strings.TrimSpace(body.Message)
	}
	return "the OpenSubtitles download quota is spent"
}

// baseURL is the operator's configured URL, or the public host, without a
// trailing slash.
func (p *Provider) baseURL() string {
	base := strings.TrimSpace(p.host.Settings().URL)
	if base == "" {
		base = DefaultBaseURL
	}
	return strings.TrimRight(base, "/")
}

// headers are OpenSubtitles' auth and content headers. The User-Agent is the
// host's to set, and it does (ADR-0059 decision 7).
func (p *Provider) headers() []pluginapi.FetchHeader {
	return []pluginapi.FetchHeader{
		pluginsdk.Header("Api-Key", p.host.Settings().Secret),
		pluginsdk.Header("Accept", "application/json"),
	}
}

// languageParam is the language as OpenSubtitles' `languages` parameter wants
// it. The host sends an ISO 639-1 code already; this only refuses to send an
// empty one.
func languageParam(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// headerValue is the first value of a response header, case-insensitively.
func headerValue(headers []pluginapi.FetchHeader, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// formatFromFilename derives the subtitle format token from a filename's
// extension (srt/ass/ssa/vtt/sub), defaulting to "srt" — OpenSubtitles'
// overwhelmingly common text format — when there is no usable extension.
func formatFromFilename(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 || i == len(name)-1 {
		return "srt"
	}
	switch ext := strings.ToLower(name[i+1:]); ext {
	case "srt", "ass", "ssa", "vtt", "sub":
		return ext
	default:
		return "srt"
	}
}
