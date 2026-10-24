package link

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goozakdev/obelo-server/internal/playback"
	"github.com/goozakdev/obelo-server/internal/store"
)

// The home User's resolution ceiling goes to the sharer as ceilingResolution only if
// its handshake says features.relayCeiling; an older sharer rejects the unknown field
// with a 400. An unreadable handshake counts as "no".

func relayWithCeiling(t *testing.T, sharer *probeSharer, req playback.RelayRequest) (playback.RelayAnswer, map[string]any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(sharer.handler))
	t.Cleanup(srv.Close)
	st := &memStore{links: []store.Link{{
		ID: "l1", ServerID: "sharer", ServerName: "Sharer", Origins: []string{srv.URL},
		ActiveOrigin: srv.URL, Token: "tok", State: store.LinkStateConnected,
	}}}
	svc := newService(t, st, Options{})
	svc.relayCatalog = mirrorFake{}
	req.LibraryID, req.TitleID = "lib", "t1"
	ans, err := svc.RelayNegotiate(context.Background(), req)
	if err != nil {
		t.Fatalf("RelayNegotiate: %v", err)
	}
	sharer.mu.Lock()
	defer sharer.mu.Unlock()
	return ans, sharer.sent[len(sharer.sent)-1]
}

func ceilingRequest() playback.RelayRequest {
	return playback.RelayRequest{Constraints: playback.Constraints{CeilingResolution: "1080p"}}
}

func TestRelayForwardsTheCeilingToASharerWithRelayCeiling(t *testing.T) {
	ans, sent := relayWithCeiling(t, &probeSharer{features: map[string]bool{"serverLinking": true, "relayCeiling": true}}, ceilingRequest())
	if sent["ceilingResolution"] != "1080p" || !ans.CeilingForwarded {
		t.Errorf("ceilingResolution = %v, forwarded = %v; want 1080p, true", sent["ceilingResolution"], ans.CeilingForwarded)
	}
}

func TestRelayOmitsTheCeilingForASharerWithoutRelayCeiling(t *testing.T) {
	ans, sent := relayWithCeiling(t, &probeSharer{features: map[string]bool{"serverLinking": true, "stepDown": true}}, ceilingRequest())
	if _, present := sent["ceilingResolution"]; present || ans.CeilingForwarded {
		t.Errorf("sharer without features.relayCeiling got ceilingResolution %v (forwarded %v)", sent["ceilingResolution"], ans.CeilingForwarded)
	}
}

func TestRelayOmitsTheCeilingWhenTheHandshakeCannotBeRead(t *testing.T) {
	ans, sent := relayWithCeiling(t, &probeSharer{status: http.StatusInternalServerError}, ceilingRequest())
	if _, present := sent["ceilingResolution"]; present || ans.CeilingForwarded {
		t.Errorf("unreadable handshake still sent ceilingResolution %v (forwarded %v)", sent["ceilingResolution"], ans.CeilingForwarded)
	}
}

func TestRelayWithoutACeilingSendsNoCeilingField(t *testing.T) {
	ans, sent := relayWithCeiling(t, &probeSharer{features: map[string]bool{"relayCeiling": true}}, playback.RelayRequest{})
	if _, present := sent["ceilingResolution"]; present || ans.CeilingForwarded {
		t.Errorf("no ceiling, yet ceilingResolution %v (forwarded %v)", sent["ceilingResolution"], ans.CeilingForwarded)
	}
}

func TestOneHandshakeReadServesBothFeatures(t *testing.T) {
	sharer := &probeSharer{features: map[string]bool{"stepDown": true, "relayCeiling": true}}
	req := ceilingRequest()
	req.ReplacesSessionID = "rs1"
	_, sent := relayWithCeiling(t, sharer, req)
	if probes, _ := sharer.counts(); probes != 1 {
		t.Errorf("probes = %d, want 1 for stepDown and relayCeiling together", probes)
	}
	if sent["replacesSessionId"] != "rs1" || sent["ceilingResolution"] != "1080p" {
		t.Errorf("sent replacesSessionId %v, ceilingResolution %v; want rs1 and 1080p", sent["replacesSessionId"], sent["ceilingResolution"])
	}
}
