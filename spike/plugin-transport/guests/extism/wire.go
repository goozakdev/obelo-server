//go:build wasm

// The wire structs, copied verbatim from ../../wire/wire.go into package main.
// BYTE-IDENTICAL IN BOTH GUESTS.
package main

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
