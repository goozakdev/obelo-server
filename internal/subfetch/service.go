package subfetch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/subtitle"
)

// Store is the persistence the fetch Service writes a picked subtitle through.
// *store.DB satisfies it; the narrow interface keeps the domain testable.
type Store interface {
	PickTitleSubtitle(titleID, subID, lang string, forced bool, kind, codec, path, providerID string) error
}

// FetchRef is what the API layer resolves from a Title detail and hands the
// Service to key a fetch: the parsed identity, the enrichment-assigned IMDBID (if
// any), and the played File's path (for the lazy moviehash + size). The Service
// never loads a Title itself — it stays store-narrow (ADR-0006).
type FetchRef struct {
	TitleID  string
	Title    string
	Year     int
	IMDBID   string
	FilePath string // played File, for moviehash + size; may be "" (unreadable)
}

// Service owns the fetch flow (ADR-0021): compute the match ref (lazily hashing the
// media file), search the active provider, and — on a pick — download the chosen
// candidate, normalize it to WebVTT on the slice-02 conversion path, cache it
// identity-keyed under the data dir, and record a source='fetched' row that
// locks the pick. The provider is hot-swapped by the Manager via SetProvider; a
// disabled provider degrades to no candidates and no outbound call (ADR-0001).
type Service struct {
	store    Store
	cacheDir string
	// provider holds the current SubtitleProvider snapshot, swapped atomically by
	// SetProvider so an in-flight fetch never sees a half-applied configuration. The
	// value is always a providerBox so atomic.Value sees one concrete type across
	// swaps (an interface's dynamic type varies).
	provider atomic.Value // providerBox
}

// providerBox wraps the SubtitleProvider so atomic.Value stores a single concrete
// type regardless of which provider implementation is live.
type providerBox struct{ p SubtitleProvider }

// NewService wires a Service over the store and the on-disk cache directory
// (config.SubtitleCacheDir, ensured to exist by the caller). It starts with the
// disabled provider so a fetch before the first Manager.Reload makes no call.
func NewService(s Store, cacheDir string) *Service {
	svc := &Service{store: s, cacheDir: cacheDir}
	svc.provider.Store(providerBox{disabledProvider{}})
	return svc
}

// SetProvider atomically swaps the active provider (called by Manager.Reload).
func (s *Service) SetProvider(p SubtitleProvider) {
	if p == nil {
		p = disabledProvider{}
	}
	s.provider.Store(providerBox{p})
}

func (s *Service) current() SubtitleProvider {
	return s.provider.Load().(providerBox).p
}

// Search returns the provider's candidate subtitles for ref in the wanted
// language, computing the moviehash lazily from the played file first (release-exact
// matching). A disabled/offline provider yields nil candidates and no error — the
// caller shows "nothing found", never an error (graceful degradation, ADR-0001).
func (s *Service) Search(ctx context.Context, ref FetchRef, lang string) ([]Candidate, error) {
	sref := s.subtitleRef(ref)
	cands, err := s.current().Search(ctx, sref, lang)
	switch {
	case errors.Is(err, ErrProviderDisabled), errors.Is(err, ErrNoMatch):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return cands, nil
}

// Pick downloads the chosen candidate, normalizes a text subtitle to WebVTT (an
// image subtitle is cached raw for burn-in), writes it identity-keyed under the
// data dir, and records the locking source='fetched' row. It returns the created
// Subtitle so the caller can surface it immediately. A disabled provider yields
// ErrProviderDisabled (the caller reports nothing found); a genuine download/convert
// failure is a real error.
func (s *Service) Pick(ctx context.Context, ref FetchRef, candidate Candidate, lang string) (store.Subtitle, error) {
	lang = subtitle.NormalizeLang(lang)
	data, format, err := s.current().Download(ctx, candidate)
	if err != nil {
		return store.Subtitle{}, err
	}

	kind := subtitle.KindForCodec(format)
	var (
		outData []byte
		codec   string
		ext     string
	)
	if kind == "text" && subtitle.IsTextConvertible(format) {
		// Normalize to WebVTT on the same conversion path sidecars use (slice 02), so
		// a fetched subtitle serves through the existing out-of-band .vtt endpoint
		// unchanged.
		vtt, cerr := subtitle.ToWebVTT(data, format)
		if cerr != nil {
			return store.Subtitle{}, fmt.Errorf("subfetch: converting fetched subtitle: %w", cerr)
		}
		outData, codec, ext = vtt, "vtt", ".vtt"
	} else {
		// A fetched image subtitle (rare) is cached raw and burns in on transcode
		// (slice 04) like any image track.
		kind = "image"
		outData, codec, ext = data, format, "."+format
	}

	path, err := s.cache(ref.TitleID, lang, candidate, ext, outData)
	if err != nil {
		return store.Subtitle{}, err
	}

	subID := uuid.NewString()
	if err := s.store.PickTitleSubtitle(
		ref.TitleID, subID, lang, candidate.Forced, kind, codec, path, candidate.ID,
	); err != nil {
		return store.Subtitle{}, err
	}
	return store.Subtitle{
		ID:         subID,
		TitleID:    ref.TitleID,
		Source:     "fetched",
		Kind:       kind,
		Language:   lang,
		Forced:     candidate.Forced,
		Codec:      codec,
		Path:       path,
		ProviderID: candidate.ID,
	}, nil
}

// subtitleRef builds the provider match ref from a FetchRef, computing the
// OpenSubtitles moviehash lazily from the played file. A hash failure (file
// unreadable/too small) is non-fatal — the ref simply omits the hash and the
// provider falls back to imdb_id / a filename query (ADR-0021 match order).
func (s *Service) subtitleRef(ref FetchRef) SubtitleRef {
	sref := SubtitleRef{Title: ref.Title, Year: ref.Year, IMDBID: ref.IMDBID}
	if ref.FilePath != "" {
		if h, err := subtitle.MovieHash(ref.FilePath); err == nil {
			sref.MovieHash = h
			if info, serr := os.Stat(ref.FilePath); serr == nil {
				sref.FileSize = info.Size()
			}
		}
	}
	return sref
}

// cache writes the fetched subtitle bytes to the cache under a deterministic,
// identity-keyed name — <titleID>-<lang>[-forced]-<providerID><ext> — so the file
// is keyed to the stable Title id (surviving rescans and a Match override, ADR-0014,
// exactly like cacheArtwork) and re-picking overwrites in place. Returns the
// absolute path recorded in the DB row.
func (s *Service) cache(titleID, lang string, candidate Candidate, ext string, data []byte) (string, error) {
	name := titleID + "-" + lang
	if candidate.Forced {
		name += "-forced"
	}
	if candidate.ID != "" {
		name += "-" + sanitize(candidate.ID)
	}
	name += ext
	path := filepath.Join(s.cacheDir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("subfetch: writing fetched subtitle: %w", err)
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs, nil
	}
	return path, nil
}

// sanitize strips path separators from a provider id so it is safe as a filename
// component (provider ids are opaque; a stray '/' must never escape the cache dir).
func sanitize(id string) string {
	out := make([]rune, 0, len(id))
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// EnsureCacheDir creates the subtitle cache directory if absent. Called by app.New
// at boot; the dir is durable (not cleared), like the artwork cache.
func EnsureCacheDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("subfetch: creating subtitle cache %q: %w", dir, err)
	}
	return nil
}

// CandidateLabel builds a human menu label for a candidate — used by the API to
// describe a candidate in the picker (language + forced/SDH suffixes, the release
// name, and an [exact] marker for a release-exact moviehash match).
func CandidateLabel(c Candidate) string {
	label := subtitle.Label(c.Language, c.Forced)
	if c.HearingImpaired {
		label += " (SDH)"
	}
	if c.Release != "" {
		label += " — " + c.Release
	}
	if c.MatchedBy == "moviehash" {
		label += " [exact]"
	}
	return label
}
