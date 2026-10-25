package api_test

import (
	"net/http"
	"testing"
)

// The home Server refuses a relayed session's raw-File tail unless the sharer's tier
// for it is directPlay, so a sharer that predates the raw-route guard cannot serve a
// File the Playback ceiling refused through this Server. The sharer here reports
// directStream while its raw route still serves the File.

func TestRelayRawTailRefusedWhenSharerTierIsNotDirectPlay(t *testing.T) {
	t.Parallel()
	f := linkForRelayVia(t, func(m map[string]any) { m["tier"] = "directStream" })
	titleID := f.mirroredTitle(t, "Dune")

	status, dec, body := f.play(t, f.homeAdmin, titleID, mp4Profile())
	if status != http.StatusOK {
		t.Fatalf("relayed playback = %d; body: %s", status, body)
	}
	if dec.Tier != "directStream" {
		t.Fatalf("tier = %q, want the sharer's directStream", dec.Tier)
	}
	if dec.StreamToken == "" {
		t.Fatal("no stream token on the relayed decision")
	}

	assertRawStream404(t, authStream(t, f.home, dec.StreamURL, f.homeAdmin, ""), "bearer relay tail")
	assertRawStream404(t, authStream(t, f.home, dec.StreamURL, f.homeAdmin, "bytes=0-9"), "ranged bearer relay tail")
	assertRawStream404(t, cookieGET(t, f.home, dec.StreamURL, &http.Cookie{Name: mediaCookieName, Value: f.homeAdmin}, ""), "cookie relay tail")
	assertRawStream404(t, cookieGET(t, f.home, dec.StreamURL, &http.Cookie{Name: mediaCookieName, Value: f.homeAdmin}, "bytes=0-9"), "ranged cookie relay tail")
	assertRawStream404(t, anonGET(t, f.home, tokenStream(dec.StreamToken), ""), "token relay tail")
	assertRawStream404(t, anonGET(t, f.home, tokenStream(dec.StreamToken), "bytes=0-9"), "ranged token relay tail")
}
