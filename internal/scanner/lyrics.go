package scanner

import "github.com/goozakdev/obelo-server/internal/lyrics"

// trackLyrics reads the Local lyrics of the audio file at path — its sidecar
// .lrc, its embedded SYLT/USLT frames, or a lyrics tag ffprobe surfaced in
// media.Tags — for the Track the scan is filing. nil when it has none; the store
// then clears any the Track had before.
func trackLyrics(path string, media MediaInfo) *lyrics.Lyrics {
	l, ok := lyrics.Local(path, media.Tags)
	if !ok {
		return nil
	}
	return &l
}
