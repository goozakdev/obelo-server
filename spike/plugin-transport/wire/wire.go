// Package wire is a COPY of the Subtitle provider wire structs this spike needs,
// taken from internal/pluginapi/v1 at 68fd7cb.
//
// It is a copy on purpose. The spike is its own Go module so the root go.mod
// never learns about a wasm runtime, and a separate module cannot import an
// internal/ package of the server anyway. Issue 07 is moving the real contract to
// pluginapi/v1 at the module root in parallel with this spike; when a loader
// actually ships (issue 09) it imports THAT package, and this copy dies with the
// rest of the spike.
package wire

// Outcome is the contract's outcome enum, abridged to what two calls need.
type Outcome string

const (
	OutcomeMatched Outcome = "matched"
	OutcomeNoMatch Outcome = "no-match"
)

// Page is the contract's only paging shape.
type Page struct {
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
}

// SubtitleRef is everything a Subtitle provider may key a search by.
type SubtitleRef struct {
	Title     string `json:"title,omitempty"`
	Year      int    `json:"year,omitempty"`
	IMDBID    string `json:"imdbId,omitempty"`
	MovieHash string `json:"movieHash,omitempty"`
	FileSize  int64  `json:"fileSize,omitempty"`
}

// SubtitleSearchRequest asks for the candidates a source offers for one Title in
// one language.
type SubtitleSearchRequest struct {
	Ref      SubtitleRef `json:"ref"`
	Language string      `json:"language"`
	Page
}

// SubtitleCandidate is one subtitle a source offers.
type SubtitleCandidate struct {
	ID              string `json:"id"`
	Language        string `json:"language,omitempty"`
	Format          string `json:"format,omitempty"`
	Release         string `json:"release,omitempty"`
	HearingImpaired bool   `json:"hearingImpaired,omitempty"`
	Forced          bool   `json:"forced,omitempty"`
	MatchedBy       string `json:"matchedBy,omitempty"`
	Downloads       int    `json:"downloads,omitempty"`
}

// SubtitleSearchResponse is what a search answers.
type SubtitleSearchResponse struct {
	Outcome    Outcome             `json:"outcome"`
	Candidates []SubtitleCandidate `json:"candidates,omitempty"`
	Detail     string              `json:"detail,omitempty"`
}

// SubtitleDownloadRequest asks for one candidate's bytes, whole.
type SubtitleDownloadRequest struct {
	Candidate SubtitleCandidate `json:"candidate"`
	MaxBytes  int64             `json:"maxBytes,omitempty"`
}

// SubtitleDownloadResponse carries the subtitle file itself.
type SubtitleDownloadResponse struct {
	Outcome     Outcome `json:"outcome"`
	Data        []byte  `json:"data,omitempty"`
	Format      string  `json:"format,omitempty"`
	ContentType string  `json:"contentType,omitempty"`
	Detail      string  `json:"detail,omitempty"`
}
