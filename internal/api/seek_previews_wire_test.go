package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureSeekPreviews is the ~25-minute File of testdata/seekpreviews: 1497 s at the
// fixed 10 s interval is 150 frames, in 10×8 sheets of 80 — a full sheet and a partial.
func fixtureSeekPreviews() seekPreviewsJSON {
	return seekPreviewsJSON{
		IntervalMs:  10000,
		FrameWidth:  320,
		FrameHeight: 180,
		Columns:     10,
		Rows:        8,
		FrameCount:  150,
		Sheets:      seekPreviewSheetURLs("/api/v1/stream/exampleStreamToken/seekPreviews", 2),
	}
}

func TestSeekPreviewsManifestMatchesFixture(t *testing.T) {
	dir := filepath.Join("testdata", "seekpreviews")
	got, err := json.MarshalIndent(fixtureSeekPreviews(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("manifest drifted from the frozen fixture\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestSeekPreviewsFixtureSheetsHaveGridDimensions(t *testing.T) {
	dir := filepath.Join("testdata", "seekpreviews")
	m := fixtureSeekPreviews()
	if per := m.Columns * m.Rows; len(m.Sheets) != (m.FrameCount+per-1)/per {
		t.Fatalf("%d sheets for %d frames in %d-cell grids", len(m.Sheets), m.FrameCount, per)
	}
	for i := range m.Sheets {
		f, err := os.Open(filepath.Join(dir, filepath.Base(m.Sheets[i])))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := jpeg.Decode(f)
		f.Close()
		if err != nil {
			t.Fatalf("sheet %d: %v", i, err)
		}
		b := cfg.Bounds()
		if b.Dx() != m.Columns*m.FrameWidth || b.Dy() != m.Rows*m.FrameHeight {
			t.Errorf("sheet %d is %dx%d, want %dx%d", i, b.Dx(), b.Dy(),
				m.Columns*m.FrameWidth, m.Rows*m.FrameHeight)
		}
	}
}

func TestSeekPreviewsOmittedFromDecisionWhenNone(t *testing.T) {
	b, err := json.Marshal(decisionResponse{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "seekPreviews") {
		t.Fatalf("decision without previews carries seekPreviews: %s", b)
	}
	m := fixtureSeekPreviews()
	b, err = json.Marshal(decisionResponse{SeekPreviews: &m})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"seekPreviews":{"intervalMs":10000,`) {
		t.Fatalf("decision with previews lacks seekPreviews: %s", b)
	}
}

func TestSeekPreviewSheetURLsFileRoute(t *testing.T) {
	got := seekPreviewSheetURLs("/api/v1/files/f1/seekPreviews", 2)
	if got[0] != "/api/v1/files/f1/seekPreviews/0.jpg" || got[1] != "/api/v1/files/f1/seekPreviews/1.jpg" {
		t.Fatalf("got %v", got)
	}
}

// cellStats decodes the fixture sheets and returns, per cell of the whole sequence, the
// mean luma of the frame and of its 32×32 top-left patch. The fixture's source paints that
// patch with full-range luma 255×T/1497 (T = source seconds), so the patch pins the sampling interval.
func cellStats(t *testing.T, m seekPreviewsJSON) (frame, patch []float64) {
	t.Helper()
	luma := func(img image.Image, r image.Rectangle) float64 {
		var sum float64
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				sum += float64(color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y)
			}
		}
		return sum / float64(r.Dx()*r.Dy())
	}
	for i := range m.Sheets {
		f, err := os.Open(filepath.Join("testdata", "seekpreviews", filepath.Base(m.Sheets[i])))
		if err != nil {
			t.Fatal(err)
		}
		img, err := jpeg.Decode(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		for c := 0; c < m.Columns*m.Rows; c++ {
			x, y := c%m.Columns*m.FrameWidth, c/m.Columns*m.FrameHeight
			frame = append(frame, luma(img, image.Rect(x, y, x+m.FrameWidth, y+m.FrameHeight)))
			patch = append(patch, luma(img, image.Rect(x, y, x+32, y+32)))
		}
	}
	return frame, patch
}

func TestSeekPreviewsFixtureFramesAreTenSecondsApart(t *testing.T) {
	m := fixtureSeekPreviews()
	frame, patch := cellStats(t, m)
	for i := 0; i < m.FrameCount; i++ {
		want := 255 * float64(i) * float64(m.IntervalMs) / 1000 / 1497
		if math.Abs(patch[i]-want) > 6 {
			t.Errorf("cell %d: time patch luma %.1f, want %.1f (a frame at %d s)", i, patch[i], want, i*int(m.IntervalMs/1000))
		}
		if i > 0 && frame[i] == frame[i-1] {
			t.Errorf("cell %d repeats cell %d", i, i-1)
		}
	}
}

func TestSeekPreviewsFixtureUnusedCellsAreBlack(t *testing.T) {
	m := fixtureSeekPreviews()
	frame, _ := cellStats(t, m)
	for i := m.FrameCount; i < len(frame); i++ {
		if frame[i] > 8 {
			t.Errorf("unused cell %d has mean luma %.1f, want black", i, frame[i])
		}
	}
}
