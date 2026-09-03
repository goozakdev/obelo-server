package playback

import (
	"errors"
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
)

// The per-User Playback ceiling (ADR-0054 §2, .scratch/linked-servers issue 02):
// the clamp, its interaction with tiering, and the concurrent-stream cap.

// uhdProfile is a client that can decode 4K, so the DEVICE is never the limiter
// in these tests — only the ceiling (or the client's own Constraints) is.
func uhdProfile() DeviceProfile {
	return DeviceProfile{
		Containers:       []string{"mp4", "mkv"},
		VideoCodecs:      []VideoCodecSupport{{Codec: "h264", MaxResolution: "2160p"}},
		AudioCodecs:      []string{"aac", "ac3"},
		MaxAudioChannels: 8,
	}
}

// ceilingStore is a minimal TitleStore + WatchStateStore over one Title detail.
type ceilingStore struct{ detail store.TitleDetail }

func (s ceilingStore) TitleByID(id string) (store.TitleDetail, error) {
	if id != s.detail.ID {
		return store.TitleDetail{}, store.ErrNotFound
	}
	return s.detail, nil
}
func (s ceilingStore) WatchStateFor(string, string) (store.WatchState, error) {
	return store.WatchState{}, nil
}
func (s ceilingStore) SaveWatchState(string, string, int64, bool, bool) error { return nil }

func titleWith(editions ...store.Edition) store.TitleDetail {
	d := store.TitleDetail{Editions: editions}
	d.Title.ID = "t1"
	return d
}

// TestClampToCeilingStricterWins is the whole rule in one table: the stricter of
// the client's Constraints and the User's ceiling wins, and zero/"" on either
// side means "the other one" rather than a limit of zero.
func TestClampToCeilingStricterWins(t *testing.T) {
	cases := []struct {
		name      string
		cons      Constraints
		scope     access.Scope
		wantRes   string
		wantRate  int64
		wantBound bool
	}{
		{
			name:    "no ceiling leaves the request alone",
			cons:    Constraints{MaxResolution: "2160p", MaxBitrate: 50_000_000},
			scope:   access.Scope{},
			wantRes: "2160p", wantRate: 50_000_000, wantBound: false,
		},
		{
			name:    "ceiling tightens a looser request",
			cons:    Constraints{MaxResolution: "2160p", MaxBitrate: 50_000_000},
			scope:   access.Scope{MaxResolution: "1080p", MaxBitrate: 8_000_000},
			wantRes: "1080p", wantRate: 8_000_000, wantBound: true,
		},
		{
			name:    "a stricter client keeps its own numbers",
			cons:    Constraints{MaxResolution: "720p", MaxBitrate: 3_000_000},
			scope:   access.Scope{MaxResolution: "1080p", MaxBitrate: 8_000_000},
			wantRes: "720p", wantRate: 3_000_000, wantBound: false,
		},
		{
			name:    "an unconstrained client inherits the ceiling",
			cons:    Constraints{},
			scope:   access.Scope{MaxResolution: "1080p", MaxBitrate: 8_000_000},
			wantRes: "1080p", wantRate: 8_000_000, wantBound: true,
		},
		{
			name:    "each dimension is independent",
			cons:    Constraints{MaxResolution: "720p", MaxBitrate: 50_000_000},
			scope:   access.Scope{MaxResolution: "1080p", MaxBitrate: 8_000_000},
			wantRes: "720p", wantRate: 8_000_000, wantBound: true,
		},
		{
			name:    "an unreadable ceiling token is no ceiling, not a block",
			cons:    Constraints{MaxResolution: "2160p"},
			scope:   access.Scope{MaxResolution: "gigantic"},
			wantRes: "2160p", wantRate: 0, wantBound: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, bound := clampToCeiling(tc.cons, tc.scope)
			if got.MaxResolution != tc.wantRes || got.MaxBitrate != tc.wantRate {
				t.Errorf("clamped = %q/%d, want %q/%d",
					got.MaxResolution, got.MaxBitrate, tc.wantRes, tc.wantRate)
			}
			if bound != tc.wantBound {
				t.Errorf("bound = %v, want %v", bound, tc.wantBound)
			}
		})
	}
}

// TestClampPreservesUnrelatedConstraints: the clamp touches the two quality
// dimensions and nothing else — the language preferences ride through.
func TestClampPreservesUnrelatedConstraints(t *testing.T) {
	got, _ := clampToCeiling(
		Constraints{PreferredAudioLang: "ja", PreferredSubtitleLang: "en"},
		access.Scope{MaxResolution: "1080p"})
	if got.PreferredAudioLang != "ja" || got.PreferredSubtitleLang != "en" {
		t.Errorf("clamp dropped a language preference: %+v", got)
	}
}

// TestNegotiate4KOnlyUnderCeilingTranscodesDown is the issue's first acceptance:
// a 4K-only File under a 1080p ceiling negotiates the TRANSCODE tier (the client
// could have played it — the ceiling is what refuses) and the Title is still
// perfectly findable: a ceiling changes how something plays, never whether it
// exists.
func TestNegotiate4KOnlyUnderCeilingTranscodesDown(t *testing.T) {
	detail := titleWith(store.Edition{ID: "uhd", Name: "2160p", Files: []store.File{mp4File(2160, 40_000_000)}})
	svc := NewService(ceilingStore{detail: detail}, nil, "", Governance{})

	req := Request{
		UserID: "u1", TitleID: "t1", Profile: uhdProfile(),
		Constraints: Constraints{MaxResolution: "2160p", MaxBitrate: 100_000_000},
		Scope:       access.Scope{AllLibraries: true, MaxResolution: "1080p"},
	}
	dec, sess, unsup, busy, err := svc.Negotiate(req)
	if err != nil || unsup != nil || busy != nil {
		t.Fatalf("capped negotiate: err=%v unsup=%v busy=%v", err, unsup, busy)
	}
	if dec.Tier != TierTranscode {
		t.Errorf("tier = %q, want transcode (4K source under a 1080p ceiling)", dec.Tier)
	}
	if !dec.UserCeiling {
		t.Error("decision.UserCeiling = false, want true (the USER's ceiling bound this session)")
	}
	if !sess.UserCeiling {
		t.Error("session.UserCeiling = false, want true (the marker must reach the session)")
	}

	// Control: the SAME request from an uncapped Scope direct-plays, so the
	// transcode above is the ceiling's doing and not the fixture's.
	req.Scope = access.Scope{AllLibraries: true}
	dec, _, unsup, busy, err = svc.Negotiate(req)
	if err != nil || unsup != nil || busy != nil {
		t.Fatalf("uncapped negotiate: err=%v unsup=%v busy=%v", err, unsup, busy)
	}
	if dec.Tier != TierDirectPlay {
		t.Errorf("uncapped tier = %q, want directPlay", dec.Tier)
	}
	if dec.UserCeiling {
		t.Error("uncapped decision.UserCeiling = true, want false (nothing capped it)")
	}
}

// TestNegotiateCeilingDirectPlaysAvailable1080pEdition is the second half of that
// acceptance: where a 1080p Edition exists, a 1080p ceiling picks it and DIRECT
// PLAYS — the ceiling costs no CPU when the library already has a fitting file.
func TestNegotiateCeilingDirectPlaysAvailable1080pEdition(t *testing.T) {
	detail := titleWith(
		store.Edition{ID: "uhd", Name: "2160p", Files: []store.File{mp4File(2160, 40_000_000)}},
		store.Edition{ID: "hd", Name: "1080p", Files: []store.File{mp4File(1080, 6_000_000)}},
	)
	svc := NewService(ceilingStore{detail: detail}, nil, "", Governance{})

	dec, _, unsup, busy, err := svc.Negotiate(Request{
		UserID: "u1", TitleID: "t1", Profile: uhdProfile(),
		Constraints: Constraints{MaxResolution: "2160p", MaxBitrate: 100_000_000},
		Scope:       access.Scope{AllLibraries: true, MaxResolution: "1080p"},
	})
	if err != nil || unsup != nil || busy != nil {
		t.Fatalf("negotiate: err=%v unsup=%v busy=%v", err, unsup, busy)
	}
	if dec.Tier != TierDirectPlay || dec.Edition.ID != "hd" {
		t.Errorf("tier/edition = %q/%q, want directPlay/hd", dec.Tier, dec.Edition.ID)
	}
	if !dec.UserCeiling {
		t.Error("decision.UserCeiling = false, want true (the ceiling still chose the edition)")
	}
}

// TestNegotiateBitrateCeilingBinds: the bitrate half of the ceiling escalates a
// file the client would otherwise have direct-played, with the same marker.
func TestNegotiateBitrateCeilingBinds(t *testing.T) {
	detail := titleWith(store.Edition{ID: "hd", Name: "1080p", Files: []store.File{mp4File(1080, 20_000_000)}})
	svc := NewService(ceilingStore{detail: detail}, nil, "", Governance{})

	dec, _, unsup, busy, err := svc.Negotiate(Request{
		UserID: "u1", TitleID: "t1", Profile: uhdProfile(),
		Constraints: Constraints{MaxResolution: "2160p", MaxBitrate: 100_000_000},
		Scope:       access.Scope{AllLibraries: true, MaxBitrate: 4_000_000},
	})
	if err != nil || unsup != nil || busy != nil {
		t.Fatalf("negotiate: err=%v unsup=%v busy=%v", err, unsup, busy)
	}
	if dec.Tier != TierTranscode || !dec.UserCeiling {
		t.Errorf("tier=%q userCeiling=%v, want transcode/true", dec.Tier, dec.UserCeiling)
	}
}

// TestNegotiateStreamLimitRefusesAndFrees is the third acceptance: a third
// concurrent negotiation under maxStreams:2 is refused with the counts, every
// tier counts (these are all direct plays), and ENDING a session frees the slot.
func TestNegotiateStreamLimitRefusesAndFrees(t *testing.T) {
	detail := titleWith(store.Edition{ID: "hd", Name: "1080p", Files: []store.File{mp4File(1080, 6_000_000)}})
	svc := NewService(ceilingStore{detail: detail}, nil, "", Governance{})

	req := Request{
		UserID: "u1", TitleID: "t1", Profile: uhdProfile(),
		Constraints: Constraints{MaxResolution: "1080p", MaxBitrate: 100_000_000},
		Scope:       access.Scope{AllLibraries: true, MaxStreams: 2},
	}
	first, second := mustNegotiate(t, svc, req), mustNegotiate(t, svc, req)
	if first.ID == "" || second.ID == "" || first.ID == second.ID {
		t.Fatalf("want two distinct sessions, got %q and %q", first.ID, second.ID)
	}

	// The third is refused — and refused BEFORE a session exists.
	_, _, _, _, err := svc.Negotiate(req)
	if !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("third negotiate err = %v, want ErrStreamLimit", err)
	}
	var limit *StreamLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("third negotiate error does not carry the counts: %v", err)
	}
	if limit.Active != 2 || limit.Limit != 2 {
		t.Errorf("limit = %d of %d, want 2 of 2", limit.Active, limit.Limit)
	}
	if n := svc.Sessions().Count(); n != 2 {
		t.Errorf("sessions after the refusal = %d, want 2 (the refusal minted none)", n)
	}

	// Another User is unaffected — the cap is per-User, not server-wide.
	other := req
	other.UserID = "u2"
	if s := mustNegotiate(t, svc, other); s.ID == "" {
		t.Error("a different User was blocked by u1's stream limit")
	}

	// Ending one of u1's sessions frees the slot.
	if !svc.Sessions().End(first.ID) {
		t.Fatal("ending the first session reported nothing removed")
	}
	if s := mustNegotiate(t, svc, req); s.ID == "" {
		t.Error("negotiate after freeing a slot minted no session")
	}
}

// TestNegotiateUncappedStreamsNeverRefuses: maxStreams 0 is uncapped, not a
// limit of zero — the failure mode that would lock every uncapped User out.
func TestNegotiateUncappedStreamsNeverRefuses(t *testing.T) {
	detail := titleWith(store.Edition{ID: "hd", Name: "1080p", Files: []store.File{mp4File(1080, 6_000_000)}})
	svc := NewService(ceilingStore{detail: detail}, nil, "", Governance{})
	req := Request{
		UserID: "u1", TitleID: "t1", Profile: uhdProfile(),
		Constraints: Constraints{MaxResolution: "1080p", MaxBitrate: 100_000_000},
		Scope:       access.Scope{AllLibraries: true},
	}
	for i := 0; i < 5; i++ {
		mustNegotiate(t, svc, req)
	}
	if n := svc.Sessions().Count(); n != 5 {
		t.Errorf("sessions = %d, want 5 (uncapped)", n)
	}
}

func mustNegotiate(t *testing.T, svc *Service, req Request) Session {
	t.Helper()
	_, sess, unsup, busy, err := svc.Negotiate(req)
	if err != nil || unsup != nil || busy != nil {
		t.Fatalf("negotiate: err=%v unsup=%v busy=%v", err, unsup, busy)
	}
	return sess
}
