package discovery

import (
	"strings"
	"testing"
)

func TestPortFrom(t *testing.T) {
	// Advertising the wrong port yields a server that is discoverable and
	// unreachable — strictly worse than not being discoverable. So parse, never
	// guess.
	ok := map[string]int{
		":8080":          8080,
		"0.0.0.0:8099":   8099,
		"127.0.0.1:8080": 8080,
		"  :8080  ":      8080,
		"[::]:8080":      8080,
		"[::1]:9000":     9000,
	}
	for addr, want := range ok {
		got, err := portFrom(addr)
		if err != nil {
			t.Errorf("portFrom(%q) errored: %v", addr, err)
			continue
		}
		if got != want {
			t.Errorf("portFrom(%q) = %d, want %d", addr, got, want)
		}
	}

	for _, addr := range []string{"", "8080", "0.0.0.0", ":", ":0", ":notaport", ":99999"} {
		if got, err := portFrom(addr); err == nil {
			t.Errorf("portFrom(%q) = %d, want an error", addr, got)
		}
	}
}

func TestInstanceNameEscapesDots(t *testing.T) {
	// The instance name is assembled into <instance>.<service>.<domain>, so a raw
	// dot splits the label and produces a malformed record.
	got := instanceName("living.room")
	if strings.Contains(got, ".") {
		t.Fatalf("instanceName(%q) = %q, still contains a dot", "living.room", got)
	}
}

func TestInstanceNameNeverEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", ".", "..."} {
		if got := instanceName(in); got == "" {
			t.Errorf("instanceName(%q) = %q, want a fallback", in, got)
		}
	}
}

func TestInstanceNameFitsDNSLabel(t *testing.T) {
	// DNS labels cap at 63 octets.
	got := instanceName(strings.Repeat("a", 200))
	if len(got) > 63 {
		t.Fatalf("instanceName length = %d, want <= 63", len(got))
	}
}

func TestInstanceNameTruncatesOnRuneBoundary(t *testing.T) {
	// Cutting a multi-byte name at byte 63 would emit invalid UTF-8 into a record
	// that Bonjour then rejects — a failure that would look like "discovery just
	// doesn't work" rather than a name bug.
	got := instanceName(strings.Repeat("é", 100))
	if len(got) > 63 {
		t.Fatalf("length = %d, want <= 63", len(got))
	}
	for i, r := range got {
		if r == '�' {
			t.Fatalf("invalid UTF-8 at byte %d in %q", i, got)
		}
	}
}

func TestInstanceNameKeepsOrdinaryNames(t *testing.T) {
	if got := instanceName("Living Room"); got != "Living Room" {
		t.Fatalf("instanceName(%q) = %q, want it unchanged", "Living Room", got)
	}
}

func TestServiceTypeIsThePublishedContract(t *testing.T) {
	// Clients browse for exactly this string. Changing it breaks discovery for
	// every deployed client, silently — they just find nothing. This test exists
	// to make that change deliberate rather than incidental.
	if ServiceType != "_juicebox._tcp" {
		t.Fatalf("ServiceType = %q — this is a published interface (ADR-0034); "+
			"changing it strands every deployed client", ServiceType)
	}
	if APIPath != "/api/v1" {
		t.Fatalf("APIPath = %q, want /api/v1 (must mirror internal/api's prefix)", APIPath)
	}
}
