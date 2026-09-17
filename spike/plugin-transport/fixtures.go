package transport

import (
	"fmt"
	"os"
	"path/filepath"

	"obelo-spike/plugin-transport/wire"
)

// BuildDir is where build-guests.sh leaves the modules.
const BuildDir = "build"

// Guest names the four artifacts the spike compares.
const (
	BareGo     = "bare.go.wasm"
	BareTinyGo = "bare.tinygo.wasm"
	ExtismGo   = "extism.go.wasm"
	ExtismTiny = "extism.tinygo.wasm"
	ProbeGo    = "probe.go.wasm"
)

// LoadGuest reads a built module. A missing file means build-guests.sh has not
// run, or ran without TinyGo on PATH; callers say which.
func LoadGuest(dir, name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, fmt.Errorf("%w — run ./build-guests.sh first", err)
	}
	return b, nil
}

// SmallRequest is the "small payload" of the measurements: a realistic subtitle
// search, a few hundred bytes on the wire.
func SmallRequest() wire.SubtitleSearchRequest {
	return wire.SubtitleSearchRequest{
		Ref: wire.SubtitleRef{
			Title:     "Dune Part Two",
			Year:      2024,
			IMDBID:    "tt15239678",
			MovieHash: "8e245d9679d31e12",
			FileSize:  8_589_934_592,
		},
		Language: "en",
		Page:     wire.Page{Limit: 25},
	}
}

// LargeDownload asks the guest for a mebibyte, which is the "large payload" of the
// measurements and roughly the cap a real subtitle download would carry.
func LargeDownload() wire.SubtitleDownloadRequest {
	return wire.SubtitleDownloadRequest{
		Candidate: wire.SubtitleCandidate{ID: "spike-1", Language: "en", Format: "srt"},
		MaxBytes:  1 << 20,
	}
}

// RealisticDownload asks for 64 KiB, which is the size an actual SRT for a
// feature film comes out at. It is the number the Subtitle provider decision
// should really be read against; the mebibyte is the stress case.
func RealisticDownload() wire.SubtitleDownloadRequest {
	return wire.SubtitleDownloadRequest{
		Candidate: wire.SubtitleCandidate{ID: "spike-1", Language: "en", Format: "srt"},
		MaxBytes:  64 << 10,
	}
}

// SmallDownload asks for the few bytes the issue describes.
func SmallDownload() wire.SubtitleDownloadRequest {
	return wire.SubtitleDownloadRequest{
		Candidate: wire.SubtitleCandidate{ID: "spike-1", Language: "en", Format: "srt"},
	}
}
