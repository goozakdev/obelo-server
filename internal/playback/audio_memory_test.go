package playback

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// Unit tests for the Remembered-audio trait re-resolver (audio-streams/05, ADR-0023)
// as a pure function: exact-trait match → language match → no-match. These pin the
// meaning-keyed resolution the API/ffprobe tests exercise end to end — the logic that
// lets a pick survive a re-rip or Edition switch that shuffled stream order.

// TestResolveRememberedAudioExactTrait: a pick re-resolves to the Stream carrying the
// same language + traits even when the stream ORDER has shuffled (indexes changed).
func TestResolveRememberedAudioExactTrait(t *testing.T) {
	// Same four Streams as the fixture but reordered: the jpn 5.1 now sits first.
	shuffled := []store.Stream{
		{ID: "a-ja", Kind: "audio", Codec: "ac3", Channels: 6, Language: "jpn"},
		{ID: "a-com", Kind: "audio", Codec: "dts", Channels: 2, Language: "eng", Commentary: true, Title: "Director's Commentary"},
		{ID: "a-en", Kind: "audio", Codec: "aac", Channels: 2, Language: "eng", IsDefault: true},
		{ID: "a-und", Kind: "audio", Codec: "aac", Channels: 1},
	}

	// A pick of the Japanese 5.1 (stored as its meaning) re-resolves to a-ja.
	mem := audioMemoryOf(store.Stream{Language: "jpn", Channels: 6})
	got, ok := resolveRememberedAudioStream(shuffled, mem)
	if !ok || got.ID != "a-ja" {
		t.Fatalf("exact-trait resolve = %+v (ok=%v), want a-ja", got, ok)
	}

	// The commentary pick is distinguished from the plain English track by its label +
	// commentary disposition — it must resolve to a-com, not a-en.
	comMem := audioMemoryOf(store.Stream{Language: "eng", Channels: 2, Commentary: true, Title: "Director's Commentary"})
	if got, ok := resolveRememberedAudioStream(shuffled, comMem); !ok || got.ID != "a-com" {
		t.Fatalf("commentary resolve = %+v (ok=%v), want a-com", got, ok)
	}

	// The plain English stereo (no label, not commentary) resolves to a-en, NOT the
	// same-language commentary — the traits keep them apart.
	enMem := audioMemoryOf(store.Stream{Language: "eng", Channels: 2})
	if got, ok := resolveRememberedAudioStream(shuffled, enMem); !ok || got.ID != "a-en" {
		t.Fatalf("english-stereo resolve = %+v (ok=%v), want a-en", got, ok)
	}
}

// TestResolveRememberedAudioLanguageFallback: when no Stream matches the exact traits
// (a re-rip dropped the 5.1 layout, kept a stereo Japanese), the pick degrades to the
// first same-language Stream rather than erroring.
func TestResolveRememberedAudioLanguageFallback(t *testing.T) {
	streams := []store.Stream{
		{ID: "a-en", Kind: "audio", Codec: "aac", Channels: 2, Language: "eng", IsDefault: true},
		{ID: "a-ja2", Kind: "audio", Codec: "aac", Channels: 2, Language: "jpn"}, // stereo, not 5.1
	}
	// Remembered a Japanese 5.1 that no longer exists as such → language match on a-ja2.
	mem := audioMemoryOf(store.Stream{Language: "jpn", Channels: 6})
	got, ok := resolveRememberedAudioStream(streams, mem)
	if !ok || got.ID != "a-ja2" {
		t.Fatalf("language-fallback resolve = %+v (ok=%v), want a-ja2", got, ok)
	}
}

// TestResolveRememberedAudioNoMatch: a remembered language with no surviving Stream
// at all yields no-match (the caller then falls through to the next resolution level
// — Show memory, then preferredAudioLang → default → first), never an error.
func TestResolveRememberedAudioNoMatch(t *testing.T) {
	streams := []store.Stream{
		{ID: "a-en", Kind: "audio", Codec: "aac", Channels: 2, Language: "eng", IsDefault: true},
	}
	mem := audioMemoryOf(store.Stream{Language: "jpn", Channels: 6})
	if got, ok := resolveRememberedAudioStream(streams, mem); ok {
		t.Fatalf("no-match resolve = %+v (ok=%v), want no match", got, ok)
	}

	// A remembered UNKNOWN-language pick ("") only ever exact-matches; it must NOT
	// language-match every untagged Stream. An untagged mono is present but its traits
	// differ (channels), so it is no-match.
	unknownMem := audioMemoryOf(store.Stream{Language: "", Channels: 6})
	if _, ok := resolveRememberedAudioStream([]store.Stream{{ID: "a-und", Kind: "audio", Channels: 1}}, unknownMem); ok {
		t.Fatal("unknown-language pick language-matched an untagged Stream, want no match")
	}
}

// TestAudioMemoryOfNormalizesLanguage: the stored meaning normalizes the language to
// ISO-639-1 so a "jpn" pick matches a "ja" (or "Japanese") Stream on the next play.
func TestAudioMemoryOfNormalizesLanguage(t *testing.T) {
	mem := audioMemoryOf(store.Stream{Language: "Japanese", Channels: 6, Title: "  Commentary  ", Commentary: true})
	if mem.Language != "ja" {
		t.Errorf("normalized language = %q, want ja", mem.Language)
	}
	if mem.Label != "Commentary" {
		t.Errorf("label = %q, want trimmed \"Commentary\"", mem.Label)
	}
	// A differently-spelled same language resolves against it.
	streams := []store.Stream{{ID: "a-ja", Kind: "audio", Channels: 6, Language: "jpn", Title: "Commentary", Commentary: true}}
	if got, ok := resolveRememberedAudioStream(streams, mem); !ok || got.ID != "a-ja" {
		t.Fatalf("cross-spelling resolve = %+v (ok=%v), want a-ja", got, ok)
	}
}
