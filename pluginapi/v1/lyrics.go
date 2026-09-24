package v1

import "context"

// The Lyric provider Extension point (ADR-0063's note on it, behind ADR-0057):
// find a track's words by artist, title and duration — and, when the host holds
// one, its MusicBrainz recording id — answering either Synced lyrics (each line
// timed) or Plain lyrics (just the text). One call, request-response.
//
// The host asks lazily: only when a track has no Local lyrics or only Plain ones,
// and only the first time someone opens the lyrics view for it. Hits and misses
// are both remembered, so a Plugin is not asked the same question twice.
//
// It decides nothing. The HOST judges every answer: a Synced answer timed for a
// recording more than a few seconds longer or shorter than the track is kept only
// as Plain, and an answer naming a different recording than the one the host sent
// is dropped whole. That is why a response states the duration its lines were
// timed for and the recording it is for.

// Lyric kinds, the two shapes a Lyric provider may answer in.
const (
	LyricsSynced = "synced"
	LyricsPlain  = "plain"
)

// LyricsRequest asks for one track's words.
type LyricsRequest struct {
	// Artist and Title name the track as this server files it.
	Artist string `json:"artist"`
	Title  string `json:"title"`
	// Album is the track's Album title, for a source that disambiguates by it.
	Album string `json:"album,omitempty"`
	// DurationMs is the track's own length, in milliseconds; 0 when the host does
	// not know it.
	DurationMs int64 `json:"durationMs,omitempty"`
	// RecordingID is the MusicBrainz recording id the host holds for the track,
	// absent when it holds none. When present, an answer for any other recording
	// is rejected.
	RecordingID string `json:"recordingId,omitempty"`
}

// LyricLine is one timed line of a Synced answer: the text shown from StartMs
// until the next line begins. An empty Text is a deliberate gap.
type LyricLine struct {
	StartMs int64  `json:"startMs"`
	Text    string `json:"text"`
}

// LyricsResponse is what the call answers. A response with no Kind is the normal
// "nothing found", and carries no Outcome for the reason WebReferencesResponse
// carries none: the only judgment is the host's.
type LyricsResponse struct {
	// Kind is LyricsSynced (Lines holds the words) or LyricsPlain (Text does).
	Kind  string      `json:"kind,omitempty"`
	Lines []LyricLine `json:"lines,omitempty"`
	Text  string      `json:"text,omitempty"`
	// DurationMs is the length of the recording the lines were timed against. A
	// Synced answer that states none, or one more than a few seconds off the
	// track's own length, is kept only as Plain.
	DurationMs int64 `json:"durationMs,omitempty"`
	// RecordingID is the MusicBrainz recording the answer is for, when the source
	// knows it. One that differs from the request's is rejected outright.
	RecordingID string `json:"recordingId,omitempty"`
}

// LyricProvider is the Go call surface of the Lyric provider Extension point. An
// error means the Plugin failed: the host shows the track without that Plugin's
// answer and does not remember the failure as a miss.
type LyricProvider interface {
	Lyrics(ctx context.Context, req LyricsRequest) (LyricsResponse, error)
}

// LyricProviderFactory builds a Lyric provider from the Settings the host
// resolved. Settings.Enabled is true for anything the host builds, URL is the
// Descriptor's default, and Values carries whatever an Installed plugin's
// manifest declared for itself.
type LyricProviderFactory func(Settings) (LyricProvider, error)

// LyricProviderRegistration is what a Lyric provider hands the host: what it is,
// and how to build it.
type LyricProviderRegistration struct {
	Descriptor Descriptor
	New        LyricProviderFactory
}

// LyricsCall is what the host hands an INSTALLED Lyric provider for one call: the
// request a Built-in would receive and the Settings the host resolved, travelling
// together for the reason SubtitleSearchCall's do. The response is un-enveloped —
// a plain LyricsResponse.
type LyricsCall struct {
	Request  LyricsRequest `json:"request"`
	Settings Settings      `json:"settings"`
}
