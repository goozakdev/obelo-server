package subfetch

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// fakeProvider is a canned SubtitleProvider: it returns preset candidates and
// download bytes, recording what it was asked, so a test drives the whole fetch
// flow with zero network (mirrors the enrich MetadataProvider fake).
type fakeProvider struct {
	candidates  []Candidate
	data        []byte
	format      string
	searchErr   error
	downloadErr error

	lastRef  SubtitleRef
	lastLang string
}

func (f *fakeProvider) Search(_ context.Context, ref SubtitleRef, lang string) ([]Candidate, error) {
	f.lastRef, f.lastLang = ref, lang
	return f.candidates, f.searchErr
}

func (f *fakeProvider) Download(context.Context, Candidate) ([]byte, string, error) {
	if f.downloadErr != nil {
		return nil, "", f.downloadErr
	}
	return f.data, f.format, nil
}

// fakeStore captures the PickTitleSubtitle call the Service makes.
type fakeStore struct {
	picked store.Subtitle
	called bool
}

func (s *fakeStore) PickTitleSubtitle(titleID, subID, lang string, forced bool, kind, codec, path, providerID string) error {
	s.called = true
	s.picked = store.Subtitle{
		ID: subID, TitleID: titleID, Source: "fetched", Kind: kind,
		Language: lang, Forced: forced, Codec: codec, Path: path, ProviderID: providerID,
	}
	return nil
}

const sampleSRT = "1\n00:00:01,000 --> 00:00:02,000\nHello world\n"

// TestServicePickCachesTextOriginal: a fetched text subtitle is cached in its
// ORIGINAL format with its canonical codec recorded (ADR-0033) — the row behaves
// like a Sidecar at serve time (converted to WebVTT on demand; original served
// raw to a capable client). The pick still validates convertibility up front, so
// a malformed download is rejected here, not at first serve.
func TestServicePickCachesTextOriginal(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProvider{data: []byte(sampleSRT), format: "srt"}
	st := &fakeStore{}
	svc := NewService(st, dir)
	svc.SetProvider(prov)

	ref := FetchRef{TitleID: "title-1", Title: "Dune", Year: 2021}
	cand := Candidate{ID: "555", Language: "en", Format: "srt", Release: "Dune.2021"}

	sub, err := svc.Pick(context.Background(), ref, cand, "en")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if sub.Source != "fetched" || sub.Kind != "text" || sub.Language != "en" {
		t.Fatalf("unexpected subtitle: %+v", sub)
	}
	if sub.ProviderID != "555" {
		t.Fatalf("provider id = %q, want 555", sub.ProviderID)
	}
	if sub.Codec != "srt" {
		t.Fatalf("codec = %q, want srt (original format preserved, ADR-0033)", sub.Codec)
	}
	if !st.called {
		t.Fatalf("store.PickTitleSubtitle was not called")
	}
	// The cached file must exist under the data dir and be the ORIGINAL bytes.
	data, err := os.ReadFile(sub.Path)
	if err != nil {
		t.Fatalf("reading cached subtitle: %v", err)
	}
	if string(data) != sampleSRT {
		t.Fatalf("cached file is not the original SRT bytes:\n%s", data)
	}
	if !strings.HasPrefix(sub.Path, dir) {
		t.Fatalf("cached path %q not under cache dir %q", sub.Path, dir)
	}
	if !strings.HasSuffix(sub.Path, ".srt") {
		t.Fatalf("cached path %q should carry the original .srt extension", sub.Path)
	}
}

// TestServicePickNormalizesFormatAlias: a provider reporting "subrip" folds to the
// canonical "srt" codec and extension, so serve-time format matching (which keys
// on subtitle.TextFormat) always lines up.
func TestServicePickNormalizesFormatAlias(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProvider{data: []byte(sampleSRT), format: "subrip"}
	st := &fakeStore{}
	svc := NewService(st, dir)
	svc.SetProvider(prov)

	sub, err := svc.Pick(context.Background(),
		FetchRef{TitleID: "title-1", Title: "Dune", Year: 2021},
		Candidate{ID: "556", Language: "en", Format: "subrip"}, "en")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if sub.Codec != "srt" {
		t.Fatalf("codec = %q, want srt (canonicalized from subrip)", sub.Codec)
	}
	if !strings.HasSuffix(sub.Path, ".srt") {
		t.Fatalf("cached path %q should carry the canonical .srt extension", sub.Path)
	}
}

func TestServiceSearchDegradesWhenDisabled(t *testing.T) {
	svc := NewService(&fakeStore{}, t.TempDir())
	// A brand-new Service starts on the disabled provider (no SetProvider call).
	cands, err := svc.Search(context.Background(), FetchRef{Title: "Dune"}, "en")
	if err != nil {
		t.Fatalf("Search on disabled provider should not error, got %v", err)
	}
	if cands != nil {
		t.Fatalf("disabled provider should yield no candidates, got %v", cands)
	}
}

func TestServiceSearchComputesMovieHashFromFile(t *testing.T) {
	dir := t.TempDir()
	// A file large enough to hash (>= 64 KiB).
	media := dir + "/movie.mkv"
	if err := os.WriteFile(media, make([]byte, 128*1024), 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	prov := &fakeProvider{candidates: []Candidate{{ID: "1", Language: "en"}}}
	svc := NewService(&fakeStore{}, dir)
	svc.SetProvider(prov)

	_, err := svc.Search(context.Background(), FetchRef{Title: "Dune", FilePath: media}, "en")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if prov.lastRef.MovieHash == "" {
		t.Fatalf("expected a moviehash to be computed from the file")
	}
	if prov.lastRef.FileSize != 128*1024 {
		t.Fatalf("file size = %d, want %d", prov.lastRef.FileSize, 128*1024)
	}
}
