package subfetch

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

	"github.com/marioquake/juicebox/internal/subtitle"
)

// OpenSubtitlesProvider is the first concrete SubtitleProvider (ADR-0021): it owns
// all OpenSubtitles REST v1 HTTP/JSON specifics behind the SubtitleProvider seam
// and treats a no-match or network failure as a normal outcome the fetch degrades
// over, never identity (ADR-0001/0002).
//
// Search implements the ADR-0021 match order — moviehash → imdb_id → filename query
// — issuing each narrowing in turn and returning the first that yields candidates,
// tagged by which signal matched. Download performs OpenSubtitles' two-step
// download (request a time-limited link for the file id, then GET the bytes).
type OpenSubtitlesProvider struct {
	APIKey     string
	BaseURL    string // e.g. https://api.opensubtitles.com/api/v1
	UserAgent  string
	HTTPClient *http.Client
}

// defaultOpenSubtitlesBaseURL is the public host used when no override is set.
const defaultOpenSubtitlesBaseURL = "https://api.opensubtitles.com/api/v1"

// NewOpenSubtitlesProvider builds a provider from settings. An empty base URL falls
// back to the public host; the HTTP client gets a sane timeout so a slow lookup
// can't hang a request.
func NewOpenSubtitlesProvider(apiKey, baseURL string) *OpenSubtitlesProvider {
	if baseURL == "" {
		baseURL = defaultOpenSubtitlesBaseURL
	}
	return &OpenSubtitlesProvider{
		APIKey:     apiKey,
		BaseURL:    baseURL,
		UserAgent:  "juicebox/1.0 (self-hosted)",
		HTTPClient: &http.Client{Timeout: 20 * time.Second},
	}
}

func (p *OpenSubtitlesProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

// Search tries the match narrowings in order and returns the first non-empty set.
// Each narrowing is a distinct query against /subtitles; a narrowing that yields no
// candidates falls through to the next. All narrowings exhausted → ErrNoMatch.
func (p *OpenSubtitlesProvider) Search(ctx context.Context, ref SubtitleRef, lang string) ([]Candidate, error) {
	lang = subtitle.NormalizeLang(lang)
	if lang == "" {
		return nil, ErrNoMatch // can't ask OpenSubtitles for an unknown language
	}

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
			return nil, err
		}
		if len(cands) > 0 {
			return cands, nil
		}
	}
	return nil, ErrNoMatch
}

// searchOnce issues one /subtitles query and parses the candidate list.
func (p *OpenSubtitlesProvider) searchOnce(ctx context.Context, params url.Values, lang, matchedBy string) ([]Candidate, error) {
	reqURL := strings.TrimRight(p.BaseURL, "/") + "/subtitles?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("subfetch: building opensubtitles request: %w", err)
	}
	p.setHeaders(req)

	resp, err := p.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("subfetch: opensubtitles search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("subfetch: opensubtitles search: status %d", resp.StatusCode)
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
		return nil, fmt.Errorf("subfetch: decoding opensubtitles response: %w", err)
	}

	var cands []Candidate
	for _, d := range out.Data {
		a := d.Attributes
		if len(a.Files) == 0 {
			continue
		}
		f := a.Files[0]
		cands = append(cands, Candidate{
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

// Download performs the OpenSubtitles two-step: POST /download with the candidate's
// file id to obtain a time-limited link, then GET the link for the bytes. The
// format is inferred from the candidate (falling back to the download URL's
// extension).
func (p *OpenSubtitlesProvider) Download(ctx context.Context, candidate Candidate) ([]byte, string, error) {
	fileID, err := strconv.Atoi(candidate.ID)
	if err != nil {
		return nil, "", fmt.Errorf("subfetch: bad candidate id %q: %w", candidate.ID, err)
	}

	body, _ := json.Marshal(map[string]any{"file_id": fileID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(p.BaseURL, "/")+"/download", strings.NewReader(string(body)))
	if err != nil {
		return nil, "", fmt.Errorf("subfetch: building download request: %w", err)
	}
	p.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("subfetch: opensubtitles download request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("subfetch: opensubtitles download: status %d", resp.StatusCode)
	}
	var dl struct {
		Link     string `json:"link"`
		FileName string `json:"file_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dl); err != nil {
		return nil, "", fmt.Errorf("subfetch: decoding download link: %w", err)
	}
	if dl.Link == "" {
		return nil, "", ErrNoMatch
	}

	data, err := p.fetchLink(ctx, dl.Link)
	if err != nil {
		return nil, "", err
	}
	format := candidate.Format
	if format == "" {
		format = formatFromFilename(dl.FileName)
	}
	return data, format, nil
}

// fetchLink GETs the time-limited download URL and returns the subtitle bytes,
// bounded so a redirect to an error page can't balloon memory.
func (p *OpenSubtitlesProvider) fetchLink(ctx context.Context, link string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, fmt.Errorf("subfetch: building link request: %w", err)
	}
	req.Header.Set("User-Agent", p.UserAgent)
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("subfetch: fetching subtitle bytes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("subfetch: fetching subtitle bytes: status %d", resp.StatusCode)
	}
	const maxBytes = 8 << 20 // 8 MiB — generous for any subtitle file.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("subfetch: reading subtitle bytes: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("subfetch: subtitle exceeds %d bytes", maxBytes)
	}
	return data, nil
}

// setHeaders applies the OpenSubtitles auth + client identity headers.
func (p *OpenSubtitlesProvider) setHeaders(req *http.Request) {
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
