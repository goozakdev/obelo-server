package store_test

import "testing"

// TestEnrichmentConsentRoundTrip covers the first-run consent gate (ADR-0032): a
// fresh (migrated-but-unwritten) DB reads UNDECIDED so a fresh install is gated
// off until the operator answers, and each decision round-trips with a timestamp
// and the right State() wire string.
func TestEnrichmentConsentRoundTrip(t *testing.T) {
	db := openTemp(t)

	// A fresh DB has no metadata_settings row, so consent is undecided — the state
	// that makes the server make no outbound calls and the SPA show the prompt.
	c, err := db.EnrichmentConsent()
	if err != nil {
		t.Fatalf("EnrichmentConsent (fresh): %v", err)
	}
	if c.Decided || c.Granted || c.State() != "unset" {
		t.Fatalf("fresh consent = %+v (state %q), want undecided/unset", c, c.State())
	}

	// Granting records the decision, marks it decided, and stamps a time.
	if err := db.SetEnrichmentConsent(true); err != nil {
		t.Fatalf("SetEnrichmentConsent(true): %v", err)
	}
	c, err = db.EnrichmentConsent()
	if err != nil {
		t.Fatalf("EnrichmentConsent (granted): %v", err)
	}
	if !c.Decided || !c.Granted || c.State() != "granted" {
		t.Fatalf("granted consent = %+v (state %q), want decided+granted", c, c.State())
	}
	if c.At.IsZero() {
		t.Fatalf("granted consent At is zero, want a timestamp")
	}

	// Revoking flips to declined, still decided (the prompt never fires again).
	if err := db.SetEnrichmentConsent(false); err != nil {
		t.Fatalf("SetEnrichmentConsent(false): %v", err)
	}
	c, err = db.EnrichmentConsent()
	if err != nil {
		t.Fatalf("EnrichmentConsent (declined): %v", err)
	}
	if !c.Decided || c.Granted || c.State() != "declined" {
		t.Fatalf("declined consent = %+v (state %q), want decided+not-granted", c, c.State())
	}

	// Consent lives on the shared singleton row; setting it must not disturb the
	// sibling language setting (and vice versa).
	if err := db.SetMetadataLanguage("en-GB"); err != nil {
		t.Fatalf("SetMetadataLanguage: %v", err)
	}
	if err := db.SetEnrichmentConsent(true); err != nil {
		t.Fatalf("SetEnrichmentConsent(true) after language: %v", err)
	}
	if lang, err := db.MetadataLanguage(); err != nil || lang != "en-GB" {
		t.Fatalf("language = %q (err %v), want en-GB preserved across consent write", lang, err)
	}
}
