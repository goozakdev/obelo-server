package api

import "strconv"

// seekPreviewsJSON is the Seek preview manifest (ADR-0070, docs/api-contract.md
// "Seek previews"): the grid of frames a File's tile sheets hold. The playback
// decision carries it as the optional `seekPreviews` (a pointer, so omitempty drops
// it entirely when the File has none) with sheet URLs under the session's stream
// token; GET /files/{id}/seekPreviews returns the same shape with File-route URLs.
//
// Frame i is the still at time i×IntervalMs; sheet = i ÷ (Columns×Rows), cell = the
// remainder, row-major. Every sheet is Columns×FrameWidth by Rows×FrameHeight pixels;
// the last may be partial, its unused cells unspecified.
type seekPreviewsJSON struct {
	IntervalMs  int      `json:"intervalMs"`
	FrameWidth  int      `json:"frameWidth"`
	FrameHeight int      `json:"frameHeight"`
	Columns     int      `json:"columns"`
	Rows        int      `json:"rows"`
	FrameCount  int      `json:"frameCount"`
	Sheets      []string `json:"sheets"`
}

// seekPreviewSheetURLs names n sheets as prefix + "/<n>.jpg", from 0. The prefix is
// "/api/v1/stream/<stream token>/seekPreviews" for a session or
// "/api/v1/files/<id>/seekPreviews" for the bearer File route.
func seekPreviewSheetURLs(prefix string, n int) []string {
	urls := make([]string, n)
	for i := range urls {
		urls[i] = prefix + "/" + strconv.Itoa(i) + ".jpg"
	}
	return urls
}
