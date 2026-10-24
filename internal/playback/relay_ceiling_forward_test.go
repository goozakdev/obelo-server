package playback

import (
	"context"
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// A linked home Server forwards its User's resolution ceiling as ceilingResolution
// (features.relayCeiling). The sharer applies it as its own Scope ceiling for that one
// negotiation, so the gate, the ladder and the Remembered picks behave exactly as for a
// local capped play. It can only lower the effective ceiling.

func flacHiddenEdition() store.Edition {
	ed := hiddenTrackEdition()
	ed.Files[0].AudioCodec = "flac"
	ed.Files[0].Streams[2].Codec = "flac"
	return ed
}

func forwardedRequest() Request {
	req := d35Request()
	req.Profile.AudioCodecs = []string{"aac", "flac"}
	req.Scope.MaxResolution = ""
	req.CeilingResolution = "1080p"
	return req
}

func capped(req Request) Request {
	req.CeilingResolution = ""
	req.Scope.MaxResolution = "1080p"
	return req
}

func TestForwardedCeilingPlaysFlacHiddenTrackAsVideoCopy(t *testing.T) {
	svc := NewService(ceilingStore{detail: titleWith(flacHiddenEdition())}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	got := negotiateOK(t, svc, forwardedRequest())
	want := negotiateOK(t, svc, capped(forwardedRequest()))
	if got.Tier == TierDirectPlay {
		t.Fatalf("tier = directPlay, the raw File would carry the 2160p track")
	}
	if got.Tier != want.Tier || got.VideoStream.ID != "v1" || got.VideoStream.ID != want.VideoStream.ID {
		t.Errorf("forwarded: tier %s video %s; locally capped: tier %s video %s", got.Tier, got.VideoStream.ID, want.Tier, want.VideoStream.ID)
	}
}

func TestForwardedCeilingKeepsTheEditionRungs(t *testing.T) {
	hidden := hiddenTrackEdition()
	hidden.ID, hidden.Name = "eh", "Hidden"
	hidden.Files[0].ID = "fh"
	svc := NewService(ceilingStore{detail: titleWith(edition("el", "Lean", 720, 2_000_000), hidden)}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	got := labelsOf(negotiateOK(t, svc, forwardedRequest()).StepDown)
	want := labelsOf(negotiateOK(t, svc, capped(forwardedRequest())).StepDown)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ladder with the forwarded ceiling %v, locally capped %v", got, want)
	}
	found := false
	for _, l := range got {
		if l == "Lean · 720p" || l == "Lean · 720p*" {
			found = true
		}
	}
	if !found {
		t.Errorf("ladder %v lacks Lean · 720p", got)
	}
}

func TestForwardedCeilingNeverRaisesAStricterScopeCeiling(t *testing.T) {
	f := mp4File(1080, 6_000_000)
	svc := NewService(ceilingStore{detail: titleWith(store.Edition{ID: "e1", Files: []store.File{f}})}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	base := d35Request()
	base.Scope.MaxResolution = "720p"
	want := negotiateOK(t, svc, base)
	if want.Tier != TierTranscode {
		t.Fatalf("baseline tier %s, want transcode under a 720p Scope", want.Tier)
	}
	for _, forwarded := range []string{"1080p", "2160p", "bogus"} {
		req := base
		req.CeilingResolution = forwarded
		if got := negotiateOK(t, svc, req); got.Tier != want.Tier {
			t.Errorf("forwarded %q over a 720p Scope: tier %s, want %s", forwarded, got.Tier, want.Tier)
		}
	}
}

func TestForwardedCeilingLowersAnUncappedOrLooserScope(t *testing.T) {
	f := mp4File(1080, 6_000_000)
	svc := NewService(ceilingStore{detail: titleWith(store.Edition{ID: "e1", Files: []store.File{f}})}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	for _, scope := range []string{"", "2160p"} {
		req := d35Request()
		req.Scope.MaxResolution = scope
		if got := negotiateOK(t, svc, req); got.Tier != TierDirectPlay {
			t.Fatalf("scope %q: baseline tier %s, want directPlay", scope, got.Tier)
		}
		req.CeilingResolution = "720p"
		if got := negotiateOK(t, svc, req); got.Tier != TierTranscode {
			t.Errorf("scope %q with forwarded 720p: tier %s, want transcode", scope, got.Tier)
		}
	}
}

// forwardingRelay is a relay whose sharer took the forwarded ceiling.
type forwardingRelay struct{ *hiddenTrackRelay }

func (r forwardingRelay) RelayNegotiate(ctx context.Context, req RelayRequest) (RelayAnswer, error) {
	ans, err := r.hiddenTrackRelay.RelayNegotiate(ctx, req)
	ans.CeilingForwarded = true
	return ans, err
}

// A sharer that was sent the ceiling applied it itself, so the home Server does not
// re-ask a sound answer: no second negotiation, no ended session, no refusal.
func TestRelayDoesNotReaskWhenTheCeilingWasForwarded(t *testing.T) {
	r := &hiddenTrackRelay{markerRelay: &markerRelay{}, tierWhenPicked: TierDirectPlay,
		videoStreams: []any{map[string]any{"id": "rv1", "height": float64(720), "isDefault": true}}}
	svc, req := hiddenTrackSetup(t, r)
	svc.SetRelay(forwardingRelay{r})
	dec, _, unsup, _, err := svc.Negotiate(req)
	if err != nil || unsup != nil {
		t.Fatalf("Negotiate = unsup %v err %v, want a play", unsup, err)
	}
	if dec.Tier != TierDirectPlay || len(r.reqs) != 1 || len(r.ended) != 0 {
		t.Errorf("tier %q, asks %d, ended %v: want the sharer's answer as is after one ask", dec.Tier, len(r.reqs), r.ended)
	}
	if r.reqs[0].Constraints.CeilingResolution != "1080p" {
		t.Errorf("relay request ceiling = %q, want 1080p handed to the link", r.reqs[0].Constraints.CeilingResolution)
	}
}

// A sharer that was sent the ceiling and still answers directPlay of a File holding a
// track above it is refused (home-made 501, reason resolution) and its session ended,
// with no re-ask.
func TestRelayForwardedCeilingRefusesDirectPlayOfAHiddenTrackFile(t *testing.T) {
	r := &hiddenTrackRelay{markerRelay: &markerRelay{}, tierWhenPicked: TierDirectPlay}
	svc, req := hiddenTrackSetup(t, r)
	svc.SetRelay(forwardingRelay{r})
	_, _, unsup, _, err := svc.Negotiate(req)
	if err != nil {
		t.Fatalf("Negotiate err = %v", err)
	}
	if unsup == nil || unsup.Reason != ReasonResolution {
		t.Fatalf("unsup = %+v, want a refusal with reason resolution", unsup)
	}
	if len(r.reqs) != 1 {
		t.Errorf("sharer asked %d times, want 1 (no re-ask)", len(r.reqs))
	}
	if len(r.ended) != 1 || r.ended[0] != "rs1" {
		t.Errorf("ended sharer sessions %v, want [rs1]", r.ended)
	}
}
