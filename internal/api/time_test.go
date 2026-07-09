package api

import (
	"testing"
	"time"
)

func TestFormatTimestamp(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty stays empty", "", ""},
		{"sqlite datetime -> rfc3339", "2026-06-22 21:00:17", "2026-06-22T21:00:17Z"},
		{"already rfc3339 normalized to utc", "2026-06-22T21:00:17Z", "2026-06-22T21:00:17Z"},
		{"rfc3339 with offset -> utc", "2026-06-22T16:00:17-05:00", "2026-06-22T21:00:17Z"},
		{"unrecognized passes through", "not a date", "not a date"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatTimestamp(tc.in); got != tc.want {
				t.Errorf("formatTimestamp(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// guard: the canonical output parses back as RFC3339.
func TestFormatTimestampOutputIsRFC3339(t *testing.T) {
	got := formatTimestamp("2026-06-22 21:00:17")
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Errorf("output %q is not RFC3339-parseable: %v", got, err)
	}
}
