package playback

import (
	"context"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
)

// The home Server enforces the hidden-track ceiling rule on a relayed play. The
// sharer's Scope (the `remote` User's) has no ceiling, so it may answer directPlay for
// a File that holds a video track above this household's ceiling; the home Server
// then asks once more naming the track it approved, so the sharer remuxes just that
// one, and refuses the play if the sharer still answers directPlay.

// hiddenTrackRelay is a sharer whose File holds rv0 (2160p, not the pick) and rv1 (720p,
// the pick). It answers directPlay until asked for a specific video Stream, then
// answers `tierWhenPicked`.
type hiddenTrackRelay struct {
	*markerRelay
	tierWhenPicked Tier
	// videoStreams overrides the list the sharer reports.
	videoStreams []any

	mu    sync.Mutex
	reqs  []RelayRequest
	ended []string
	n     int
}

func hiddenTrackStreams() []any {
	return []any{
		map[string]any{"id": "rv0", "height": float64(2160), "isDefault": false},
		map[string]any{"id": "rv1", "height": float64(720), "isDefault": true},
	}
}

func (r *hiddenTrackRelay) RelayNegotiate(_ context.Context, req RelayRequest) (RelayAnswer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	r.reqs = append(r.reqs, req)
	tier := TierDirectPlay
	if req.VideoStreamID != "" {
		tier = r.tierWhenPicked
	}
	vs := r.videoStreams
	if vs == nil {
		vs = hiddenTrackStreams()
	}
	return RelayAnswer{
		LinkID: "l1", RemoteSessionID: "rs" + string(rune('0'+r.n)), RemoteTitleID: "rt1", RemoteEditionID: "re1",
		Tier: tier, Decision: map[string]any{"videoStreams": vs},
	}, nil
}

func (r *hiddenTrackRelay) RelayEndSession(_ context.Context, _, remoteSessionID string) error {
	r.mu.Lock()
	r.ended = append(r.ended, remoteSessionID)
	r.mu.Unlock()
	return nil
}

// hiddenTrackStore maps the sharer's Stream ids onto local ones.
type hiddenTrackStore struct{ relayMarkerStore }

func (s hiddenTrackStore) LocalIDForRemote(kind, remoteID string) (string, error) {
	if kind == store.ExportStream {
		switch remoteID {
		case "rv0":
			return "lv0", nil
		case "rv1":
			return "lv1", nil
		}
		return "", store.ErrNotFound
	}
	return s.relayMarkerStore.LocalIDForRemote(kind, remoteID)
}

func hiddenTrackSetup(t *testing.T, r *hiddenTrackRelay) (*Service, Request) {
	t.Helper()
	f := mp4File(1080, 6_000_000)
	f.Path = ""
	st := hiddenTrackStore{relayMarkerStore{&markerStore{ceilingStore: ceilingStore{detail: titleWith(store.Edition{ID: "e1", Files: []store.File{f}})}}}}
	svc := NewService(st, nil, "", Governance{})
	svc.SetRelay(r)
	return svc, Request{
		UserID: "u1", DeviceID: "d1", TitleID: "t1", Profile: uhdProfile(),
		Constraints: Constraints{MaxResolution: "2160p", MaxBitrate: 100_000_000},
		Scope:       access.Scope{AllLibraries: true, MaxResolution: "1080p"},
	}
}

func TestRelayDirectPlayWithTrackAboveCeilingIsReaskedForTheApprovedTrack(t *testing.T) {
	r := &hiddenTrackRelay{markerRelay: &markerRelay{}, tierWhenPicked: TierDirectStream}
	svc, req := hiddenTrackSetup(t, r)
	dec, sess, unsup, _, err := svc.Negotiate(req)
	if err != nil || unsup != nil {
		t.Fatalf("Negotiate = unsup %v err %v, want a play", unsup, err)
	}
	if dec.Tier != TierDirectStream {
		t.Errorf("tier = %q, want directStream (the sharer remuxes the approved track)", dec.Tier)
	}
	if len(r.reqs) != 2 || r.reqs[0].VideoStreamID != "" || r.reqs[1].VideoStreamID != "lv1" || !r.reqs[1].RemuxSelectedOnly {
		t.Fatalf("sharer asked %+v, want a plain ask then one naming lv1 with remuxSelectedOnly", r.reqs)
	}
	if dec.Relay.RemoteSessionID != "rs2" || sess.ID == "" {
		t.Errorf("session wraps %q, want the second sharer session rs2", dec.Relay.RemoteSessionID)
	}
	if len(r.ended) != 1 || r.ended[0] != "rs1" {
		t.Errorf("ended sharer sessions %v, want [rs1] (the stray directPlay one)", r.ended)
	}
}

func TestRelayHiddenTrackReaskKeepsTheSwap(t *testing.T) {
	r := &hiddenTrackRelay{markerRelay: &markerRelay{}, tierWhenPicked: TierDirectStream}
	svc, req := hiddenTrackSetup(t, r)
	req.Scope.MaxStreams = 1
	first := mustNegotiate(t, svc, req)
	req.ReplacesSessionID = first.ID
	if _, _, unsup, _, err := svc.Negotiate(req); err != nil || unsup != nil {
		t.Fatalf("replacing Negotiate = unsup %v err %v", unsup, err)
	}
	last := r.reqs[len(r.reqs)-1]
	if last.ReplacesSessionID != r.reqs[len(r.reqs)-2].ReplacesSessionID || last.ReplacesSessionID == "" {
		t.Errorf("re-ask replaces %q, first ask replaces %q: want the same, non-empty id",
			last.ReplacesSessionID, r.reqs[len(r.reqs)-2].ReplacesSessionID)
	}
}

func TestRelayHiddenTrackRefusedWhenSharerInsistsOnDirectPlay(t *testing.T) {
	r := &hiddenTrackRelay{markerRelay: &markerRelay{}, tierWhenPicked: TierDirectPlay}
	svc, req := hiddenTrackSetup(t, r)
	_, _, unsup, _, err := svc.Negotiate(req)
	if err != nil {
		t.Fatalf("Negotiate err = %v", err)
	}
	if unsup == nil || unsup.Reason != ReasonResolution {
		t.Fatalf("unsupported = %+v, want a resolution refusal", unsup)
	}
	if len(r.reqs) != 2 {
		t.Errorf("sharer asked %d times, want exactly 2", len(r.reqs))
	}
	if len(r.ended) != 2 {
		t.Errorf("ended sharer sessions %v, want both rs1 and rs2", r.ended)
	}
	if n := svc.Sessions().Count(); n != 0 {
		t.Errorf("local sessions = %d, want 0", n)
	}
}

func TestRelayHiddenTrackRefusedWhenApprovedTrackIsAboveCeiling(t *testing.T) {
	r := &hiddenTrackRelay{markerRelay: &markerRelay{}, tierWhenPicked: TierDirectStream, videoStreams: []any{
		map[string]any{"id": "rv0", "height": float64(2160), "isDefault": true},
		map[string]any{"id": "rv1", "height": float64(720), "isDefault": false},
	}}
	svc, req := hiddenTrackSetup(t, r)
	_, _, unsup, _, err := svc.Negotiate(req)
	if err != nil || unsup == nil || unsup.Reason != ReasonResolution {
		t.Fatalf("Negotiate = unsup %+v err %v, want a resolution refusal", unsup, err)
	}
	if len(r.reqs) != 1 || len(r.ended) != 1 || r.ended[0] != "rs1" {
		t.Errorf("asked %d, ended %v: want one ask and rs1 ended", len(r.reqs), r.ended)
	}
}

func TestRelayWithoutCeilingIsUnchanged(t *testing.T) {
	r := &hiddenTrackRelay{markerRelay: &markerRelay{}, tierWhenPicked: TierDirectStream}
	svc, req := hiddenTrackSetup(t, r)
	req.Scope.MaxResolution = ""
	dec, _, unsup, _, err := svc.Negotiate(req)
	if err != nil || unsup != nil {
		t.Fatalf("Negotiate = unsup %v err %v", unsup, err)
	}
	if dec.Tier != TierDirectPlay || len(r.reqs) != 1 || len(r.ended) != 0 {
		t.Errorf("tier %q, asks %d, ended %v: want directPlay, one ask, nothing ended", dec.Tier, len(r.reqs), r.ended)
	}
}

func TestRelayCeilingWithAllTracksWithinIsUnchanged(t *testing.T) {
	r := &hiddenTrackRelay{markerRelay: &markerRelay{}, tierWhenPicked: TierDirectStream, videoStreams: []any{
		map[string]any{"id": "rv1", "height": float64(1080), "isDefault": true},
	}}
	svc, req := hiddenTrackSetup(t, r)
	dec, _, unsup, _, err := svc.Negotiate(req)
	if err != nil || unsup != nil {
		t.Fatalf("Negotiate = unsup %v err %v", unsup, err)
	}
	if dec.Tier != TierDirectPlay || len(r.reqs) != 1 {
		t.Errorf("tier %q, asks %d: want directPlay after one ask", dec.Tier, len(r.reqs))
	}
}
