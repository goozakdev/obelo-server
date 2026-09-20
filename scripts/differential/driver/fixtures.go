package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// The fixture library: one movie, one episode under a show, and one album with
// an artist and three tracks — the three shapes the PRD's first success
// criterion names ("a movie and an album") plus the episode, because the video
// chain's second guest (TheTVDB) is only asked about a show.
//
// The names are the ones the stand-in answers to. They are real titles on
// purpose: a provider's matcher scores a title, and a nonsense name would take
// paths neither build takes in the field.
const (
	movieName  = "Back to the Future (1985)"
	showName   = "Breaking Bad"
	artistName = "Radiohead"
	albumName  = "OK Computer"
)

var albumTracks = []struct {
	num   int
	title string
}{
	{1, "Airbag"},
	{2, "Paranoid Android"},
	{3, "Subterranean Homesick Alien"},
}

// buildFixtures writes a tiny playable tree under root and returns the three
// library root folders. The files are ffmpeg-generated test patterns of a
// second or two — a few KB each, never committed.
func buildFixtures(root string) (movies, tv, music string, err error) {
	movies = filepath.Join(root, "movies")
	tv = filepath.Join(root, "tv")
	music = filepath.Join(root, "music")

	movieDir := filepath.Join(movies, movieName)
	if err = os.MkdirAll(movieDir, 0o755); err != nil {
		return
	}
	if err = makeVideo(filepath.Join(movieDir, movieName+".mkv")); err != nil {
		return
	}

	epDir := filepath.Join(tv, showName, "Season 01")
	if err = os.MkdirAll(epDir, 0o755); err != nil {
		return
	}
	if err = makeVideo(filepath.Join(epDir, showName+" - S01E01 - Pilot.mkv")); err != nil {
		return
	}

	albumDir := filepath.Join(music, artistName, albumName)
	if err = os.MkdirAll(albumDir, 0o755); err != nil {
		return
	}
	for _, tr := range albumTracks {
		name := fmt.Sprintf("%02d - %s.mp3", tr.num, tr.title)
		if err = makeAudio(filepath.Join(albumDir, name), tr.num, tr.title); err != nil {
			return
		}
	}
	return movies, tv, music, nil
}

// makeVideo writes a two-second H.264/AAC clip. The scanner probes it with
// ffprobe exactly as it probes a real file, so a synthetic clip goes through
// the same path a real one does.
func makeVideo(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x180:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest",
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg %s: %v: %s", path, err, out)
	}
	return nil
}

// makeAudio writes a one-second tagged MP3. The tags are the music scanner's
// identity source (internal/scanner/music.go), so they carry the artist, album
// and track the stand-in answers about.
func makeAudio(path string, track int, title string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-metadata", "artist="+artistName,
		"-metadata", "album_artist="+artistName,
		"-metadata", "album="+albumName,
		"-metadata", "title="+title,
		"-metadata", fmt.Sprintf("track=%d/%d", track, len(albumTracks)),
		"-metadata", "date=1997",
		"-metadata", "genre=Alternative Rock",
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg %s: %v: %s", path, err, out)
	}
	return nil
}
