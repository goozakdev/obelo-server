//go:build wasm

// The Subtitle provider's actual behaviour, such as it is: one fixed candidate
// for a search, and a subtitle file for a download.
//
// THIS FILE IS BYTE-IDENTICAL IN BOTH GUESTS. Only main.go differs, which is the
// whole point of the comparison.
package main

import "strconv"

func searchAnswer(req SubtitleSearchRequest) SubtitleSearchResponse {
	matchedBy := "query"
	switch {
	case req.Ref.MovieHash != "":
		matchedBy = "moviehash"
	case req.Ref.IMDBID != "":
		matchedBy = "imdb"
	}
	return SubtitleSearchResponse{
		Outcome: OutcomeMatched,
		Candidates: []SubtitleCandidate{{
			ID:        "spike-1",
			Language:  req.Language,
			Format:    "srt",
			Release:   req.Ref.Title + "." + strconv.Itoa(req.Ref.Year) + ".1080p.BluRay",
			MatchedBy: matchedBy,
			Downloads: 4211,
		}},
	}
}

// downloadAnswer returns a real (if dull) SRT. MaxBytes doubles as the REQUESTED
// size in this spike so the harness can dial the payload from a few bytes to a
// mebibyte without a second export; a shipping guest would treat it only as a cap.
func downloadAnswer(req SubtitleDownloadRequest) SubtitleDownloadResponse {
	if req.Candidate.ID != "spike-1" {
		return SubtitleDownloadResponse{Outcome: OutcomeNoMatch}
	}
	const cue = "1\n00:00:01,000 --> 00:00:04,000\nThe spike says hello.\n\n"
	data := []byte(cue)
	for req.MaxBytes > 0 && int64(len(data)) < req.MaxBytes {
		data = append(data, cue...)
	}
	if req.MaxBytes > 0 && int64(len(data)) > req.MaxBytes {
		data = data[:req.MaxBytes]
	}
	return SubtitleDownloadResponse{
		Outcome:     OutcomeMatched,
		Data:        data,
		Format:      "srt",
		ContentType: "application/x-subrip",
	}
}
