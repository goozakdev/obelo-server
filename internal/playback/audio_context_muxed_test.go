package playback

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// A copied-HEVC fMP4 remux whose audio is muxed (a single-audio File, or a
// remux-selected multi-audio File that keeps only the selected Stream) must reach the
// master with NO audio renditions (they have no builder and would 404) and a CODECS
// audio part naming the one Stream actually muxed.
func TestSessionAudioContextMuxedFMP4HasNoRenditions(t *testing.T) {
	hevc := store.Stream{ID: "v1", Kind: "video", Codec: "hevc"}
	eac3 := store.Stream{ID: "a-en", Kind: "audio", Codec: "eac3", Channels: 6, Language: "eng", IsDefault: true}
	aac := store.Stream{ID: "a-ja", Kind: "audio", Codec: "aac", Channels: 2, Language: "jpn"}
	for _, tc := range []struct {
		name       string
		streams    []store.Stream
		selected   store.Stream
		remuxOnly  bool
		wantCodecs string
	}{
		{"single audio", []store.Stream{hevc, eac3}, eac3, false, "ec-3"},
		{"remux-selected multi audio", []store.Stream{hevc, aac, eac3}, eac3, true, "ec-3"},
		{"remux-selected pick of aac", []store.Stream{hevc, eac3, aac}, aac, true, "mp4a.40.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := store.File{ID: "f1", EditionID: "e1", DurationMs: 10_000, VideoCodec: "hevc", Present: true, Streams: tc.streams}
			detail := store.TitleDetail{}
			detail.Title.ID = "t1"
			detail.Editions = []store.Edition{{ID: "e1", Files: []store.File{f}}}
			svc := NewService(fakeSubStore{detail: detail}, nil, "", Governance{})
			sess := svc.Sessions().Create(CreateInput{UserID: "u1", TitleID: "t1"}, Decision{
				Tier:              TierDirectStream,
				Edition:           store.Edition{ID: "e1"},
				File:              f,
				VideoStream:       hevc,
				AudioStream:       tc.selected,
				RemuxSelectedOnly: tc.remuxOnly,
			})
			if !sess.FMP4 {
				t.Fatal("fixture is not an fMP4 session")
			}
			ctx, err := svc.SessionAudioContext("u1", sess.ID)
			if err != nil {
				t.Fatalf("SessionAudioContext: %v", err)
			}
			if ctx.Demuxed || len(ctx.Renditions) != 0 {
				t.Errorf("Demuxed=%v renditions=%d, want a muxed session with no AUDIO group", ctx.Demuxed, len(ctx.Renditions))
			}
			if ctx.AudioCodec != tc.wantCodecs {
				t.Errorf("AudioCodec = %q, want %q (the muxed Stream)", ctx.AudioCodec, tc.wantCodecs)
			}
		})
	}
}
