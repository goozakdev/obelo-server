package link

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// The one string an Admin pastes (ADR-0055 §2). What is asserted here is that
// every way of getting it wrong produces ONE answer — because the operator's
// move is identical in all of them — and that an expired invite is emphatically
// NOT one of those ways: that one is a different sentence and a different fix.

// encode renders a payload the way the sharing side does, so these tests fail if
// the two encodings ever drift.
func encode(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshaling payload: %v", err)
	}
	return InviteScheme + base64.RawURLEncoding.EncodeToString(raw)
}

func goodPayload(exp time.Time) map[string]any {
	return map[string]any{
		"v":       1,
		"id":      "0f8fad5b-d9cb-469f-a165-70867728950e",
		"name":    "Amy's server",
		"origins": []string{"http://obelo.tail1a2b.ts.net", "https://media.example.org"},
		"code":    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"exp":     exp.UTC().Format(time.RFC3339),
	}
}

func TestParseInviteReadsTheWholeString(t *testing.T) {
	exp := time.Now().Add(20 * time.Hour)
	inv, err := ParseInvite(encode(t, goodPayload(exp)))
	if err != nil {
		t.Fatalf("ParseInvite: %v", err)
	}
	if inv.Version != 1 {
		t.Errorf("version = %d, want 1", inv.Version)
	}
	if inv.ServerID != "0f8fad5b-d9cb-469f-a165-70867728950e" || inv.ServerName != "Amy's server" {
		t.Errorf("identity = %q / %q", inv.ServerID, inv.ServerName)
	}
	// Order is meaningful: the home Server tries them in order (ADR-0055 §2).
	want := []string{"http://obelo.tail1a2b.ts.net", "https://media.example.org"}
	if len(inv.Origins) != 2 || inv.Origins[0] != want[0] || inv.Origins[1] != want[1] {
		t.Errorf("origins = %v, want %v", inv.Origins, want)
	}
	if inv.Code == "" {
		t.Error("no code")
	}
	if !inv.ExpiresAt.Equal(exp.UTC().Truncate(time.Second)) {
		t.Errorf("exp = %s, want %s", inv.ExpiresAt, exp.UTC().Truncate(time.Second))
	}
}

// TestParseInviteRefusesEveryMalformedString: one code for every shape failure.
// Each of these is a real way a pasted string arrives broken — a chat client
// that ate the scheme, a QR read that lost a character, a friend who pasted the
// wrong thing entirely — and none of them is distinguishable to the operator,
// so none of them may be distinguishable on the wire.
func TestParseInviteRefusesEveryMalformedString(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	drop := func(field string) string {
		p := goodPayload(exp)
		delete(p, field)
		return encode(t, p)
	}
	set := func(field string, v any) string {
		p := goodPayload(exp)
		p[field] = v
		return encode(t, p)
	}

	cases := []struct {
		name   string
		invite string
	}{
		{"empty", ""},
		{"no scheme", base64.RawURLEncoding.EncodeToString([]byte(`{"v":1}`))},
		{"an https URL, which this deliberately is not", "https://example.org/link?code=abc"},
		{"scheme but no payload", InviteScheme},
		{"payload is not base64url", InviteScheme + "not base64!!"},
		{"payload is base64 of not-JSON", InviteScheme + base64.RawURLEncoding.EncodeToString([]byte("hello"))},
		{"truncated JSON", InviteScheme + base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"id":`))},
		{"no version", drop("v")},
		{"version zero", set("v", 0)},
		{"negative version", set("v", -1)},
		{"no server id", drop("id")},
		{"empty server id", set("id", "")},
		{"no code", drop("code")},
		{"empty code", set("code", "")},
		{"no origins", drop("origins")},
		{"empty origins", set("origins", []string{})},
		{"an origin with a path", set("origins", []string{"https://example.org/obelo"})},
		{"an origin with no scheme", set("origins", []string{"example.org"})},
		{"an origin with the wrong scheme", set("origins", []string{"ftp://example.org"})},
		{"an origin carrying credentials", set("origins", []string{"https://u:p@example.org"})},
		{"an origin with a query", set("origins", []string{"https://example.org?x=1"})},
		{"no expiry", drop("exp")},
		{"expiry is not RFC 3339", set("exp", "tomorrow")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseInvite(tc.invite)
			if !errors.Is(err, ErrBadInvite) {
				t.Fatalf("ParseInvite(%q) error = %v, want ErrBadInvite", tc.invite, err)
			}
		})
	}
}

// TestParseInviteToleratesWhatTravelWellMayDoToTheString: the string is pasted
// into chats and read out of QR codes, so surrounding whitespace and base64
// padding an encoder somewhere else added are not the operator's mistake.
func TestParseInviteToleratesWhatTravelWellMayDoToTheString(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	good := encode(t, goodPayload(exp))
	for _, tc := range []struct{ name, invite string }{
		{"leading and trailing space", "  " + good + "\n"},
		{"padded base64", good + "=="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseInvite(tc.invite); err != nil {
				t.Fatalf("ParseInvite: %v", err)
			}
		})
	}
}

// TestParseInviteAcceptsUnknownFields is the forward-compatibility rule, and it
// is the OPPOSITE of the DisallowUnknownFields every request body on this server
// decodes with. A newer sharer may stamp a field this build never heard of; the
// version is what decides whether the two can talk, and a strict decode here
// would turn every additive change into an unreadable string.
func TestParseInviteAcceptsUnknownFields(t *testing.T) {
	p := goodPayload(time.Now().Add(time.Hour))
	p["somethingFromTheFuture"] = []string{"a", "b"}
	if _, err := ParseInvite(encode(t, p)); err != nil {
		t.Fatalf("ParseInvite with an unknown field: %v", err)
	}
}

// TestExpiredInviteIsNotAMalformedOne: the 24 hours running out (ADR-0055 §1) is
// a different answer from a broken string, because it is a different next move —
// ask for a fresh one rather than re-copy this one.
func TestExpiredInviteIsNotAMalformedOne(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	inv, err := ParseInvite(encode(t, goodPayload(now.Add(-time.Second))))
	if err != nil {
		t.Fatalf("an expired invite must still PARSE: %v", err)
	}
	if !inv.Expired(now) {
		t.Error("an invite whose exp has passed reports itself live")
	}
	// The boundary: exp is the instant it dies, not the last instant it lives.
	if inv.Expired(now.Add(-2 * time.Second)) {
		t.Error("an invite one second before its expiry reports itself expired")
	}
}
