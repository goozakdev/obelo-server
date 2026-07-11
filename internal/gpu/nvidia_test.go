package gpu

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Pure CSV-parse coverage (no process spawn) plus the TTL-cache behavior driven
// through a fake runner + clock — the two behaviors of the production probe that
// don't need a real GPU. The endpoint's null/populated projection across backends
// is covered at the HTTP boundary (api/transcoding_test.go) with a fake Probe.

var sampleTime = time.Date(2026, 7, 11, 18, 22, 4, 0, time.UTC)

func intp(n int) *int { return &n }

func TestParseCSVPopulated(t *testing.T) {
	tel, ok := parseCSV("37, 1240, 8192, 2, 550.90.07\n", sampleTime)
	if !ok {
		t.Fatal("ok = false, want true for a well-formed row")
	}
	if got := tel.UtilizationPct; got == nil || *got != 37 {
		t.Errorf("utilizationPct = %v, want 37", got)
	}
	if got := tel.VRAMUsedMB; got == nil || *got != 1240 {
		t.Errorf("vramUsedMb = %v, want 1240", got)
	}
	if got := tel.VRAMTotalMB; got == nil || *got != 8192 {
		t.Errorf("vramTotalMb = %v, want 8192", got)
	}
	if got := tel.EncoderSessions; got == nil || *got != 2 {
		t.Errorf("encoderSessions = %v, want 2", got)
	}
	if got := tel.DriverVersion; got == nil || *got != "550.90.07" {
		t.Errorf("driverVersion = %v, want 550.90.07", got)
	}
	if !tel.SampledAt.Equal(sampleTime) {
		t.Errorf("sampledAt = %v, want %v", tel.SampledAt, sampleTime)
	}
}

func TestParseCSVPerFieldUnavailable(t *testing.T) {
	// nvidia-smi returns some columns as [N/A] — those become nil, the rest survive.
	tel, ok := parseCSV("37, [N/A], 8192, [Not Supported], 550.90.07", sampleTime)
	if !ok {
		t.Fatal("ok = false, want true (row is well-formed, only some columns N/A)")
	}
	if tel.UtilizationPct == nil || *tel.UtilizationPct != 37 {
		t.Errorf("utilizationPct = %v, want 37", tel.UtilizationPct)
	}
	if tel.VRAMUsedMB != nil {
		t.Errorf("vramUsedMb = %v, want nil ([N/A])", *tel.VRAMUsedMB)
	}
	if tel.EncoderSessions != nil {
		t.Errorf("encoderSessions = %v, want nil ([Not Supported])", *tel.EncoderSessions)
	}
	if tel.DriverVersion == nil || *tel.DriverVersion != "550.90.07" {
		t.Errorf("driverVersion = %v, want 550.90.07", tel.DriverVersion)
	}
}

func TestParseCSVUnusable(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"blank lines":   "\n  \n",
		"too few cols":  "37, 1240, 8192",
		"too many cols": "37, 1240, 8192, 2, 550.90.07, extra",
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := parseCSV(out, sampleTime); ok {
				t.Errorf("ok = true for %q, want false", out)
			}
		})
	}
}

func TestSampleCachesWithinTTL(t *testing.T) {
	now := sampleTime
	calls := 0
	p := &NvidiaSMIProbe{
		ttl: 2 * time.Second,
		now: func() time.Time { return now },
		run: func(context.Context) (string, error) {
			calls++
			return "37, 1240, 8192, 2, 550.90.07", nil
		},
	}

	if _, ok := p.Sample(context.Background()); !ok {
		t.Fatal("first sample ok = false")
	}
	// A second poll within the TTL must NOT spawn nvidia-smi again.
	now = now.Add(time.Second)
	if _, ok := p.Sample(context.Background()); !ok {
		t.Fatal("cached sample ok = false")
	}
	if calls != 1 {
		t.Errorf("run called %d times within TTL, want 1", calls)
	}

	// Past the TTL, the next poll refreshes.
	now = now.Add(2 * time.Second)
	if _, ok := p.Sample(context.Background()); !ok {
		t.Fatal("post-TTL sample ok = false")
	}
	if calls != 2 {
		t.Errorf("run called %d times after TTL, want 2", calls)
	}
}

func TestSampleRunErrorIsUnavailable(t *testing.T) {
	p := &NvidiaSMIProbe{
		ttl: time.Second,
		now: func() time.Time { return sampleTime },
		run: func(context.Context) (string, error) {
			return "", errors.New("exec: nvidia-smi not found")
		},
	}
	if _, ok := p.Sample(context.Background()); ok {
		t.Error("ok = true when the runner errored, want false (all-or-nothing null)")
	}
}
