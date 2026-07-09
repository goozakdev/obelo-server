package scanner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Real-ffprobe integration for the EXTENDED Prober (issue tv-music/03 acceptance
// criterion: "real-ffprobe integration verifies tag reading through the extended
// Prober"). It generates a tagged audio file with `ffmpeg -metadata` and probes
// it with the real FFprobe, asserting the metadata tags surface on MediaInfo.Tags
// — exercising the no-new-dependency, no-new-seam tag extraction against the
// actual binary. Extends the existing sanctioned real-ffprobe allowance; skips
// when ffmpeg/ffprobe are not on PATH.

func TestFFprobeReadsMusicTags(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH")
	}

	dir := t.TempDir()
	// Write tiny tagged audio files across containers so we cover BOTH tag
	// placements ffprobe uses: format.tags (FLAC/MP3/MP4) and stream.tags (Vorbis
	// comments in OGG/Opus) — collectTags merges both.
	for _, ext := range []string{"flac", "mp3", "m4a", "ogg"} {
		out := filepath.Join(dir, "track."+ext)
		cmd := exec.Command("ffmpeg",
			"-y", "-loglevel", "error",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
			"-metadata", "artist=Track Artist",
			"-metadata", "album_artist=Album Artist Name",
			"-metadata", "album=Greatest Hits",
			"-metadata", "title=My Track",
			"-metadata", "track=3/12",
			"-metadata", "disc=1/2",
			"-metadata", "date=2021",
			"-metadata", "genre=Rock",
			out,
		)
		if err := cmd.Run(); err != nil {
			t.Fatalf("ffmpeg generating %s fixture: %v", ext, err)
		}

		media, err := (FFprobe{}).Probe(context.Background(), out)
		if err != nil {
			t.Fatalf("Probe(%s): %v", ext, err)
		}
		// The identity-bearing tags must surface (case-insensitive Tag accessor).
		checks := map[string]string{
			"artist":       "Track Artist",
			"album_artist": "Album Artist Name",
			"album":        "Greatest Hits",
			"title":        "My Track",
			"track":        "3/12",
			"disc":         "1/2",
			"date":         "2021",
			"genre":        "Rock",
		}
		for k, want := range checks {
			if got := media.Tag(k); got != want {
				t.Errorf("%s: Tag(%q) = %q, want %q (tags: %v)", ext, k, got, want, media.Tags)
			}
		}

		// And the extended Prober still surfaces the technical audio stream.
		if a, ok := media.PrimaryAudio(); !ok || a.Codec == "" {
			t.Errorf("%s: no audio stream from Probe (%+v)", ext, media)
		}
	}
}

// TestFFprobeTaglessMusicHasNoTags: an audio file written with NO metadata
// surfaces a nil/empty Tags map, so the music mapper falls back to the path.
func TestFFprobeTaglessMusicHasNoTags(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	out := filepath.Join(t.TempDir(), "bare.mp3")
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1", out)
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg: %v", err)
	}
	media, err := (FFprobe{}).Probe(context.Background(), out)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	// ffmpeg writes an "encoder" tag; the identity-bearing tags must be absent.
	for _, k := range []string{"artist", "album", "album_artist", "title"} {
		if got := media.Tag(k); got != "" {
			t.Errorf("tagless file Tag(%q) = %q, want empty", k, got)
		}
	}
	_ = os.Remove(out)
}
