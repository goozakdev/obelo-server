package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/marioquake/juicebox/internal/gpu"
	"github.com/marioquake/juicebox/internal/testharness"
	"github.com/marioquake/juicebox/internal/transcode"
)

// Admin-scope transcode-observability black-box tests (ADR-0029): the
// GET /api/v1/transcoding snapshot, driven through the harness like
// settings_test.go / governance_test.go. This slice covers the backend block —
// the active/requested/reason/degraded projection of the setup-time
// transcode.Resolution — plus the admin gate. Load and GPU ride the same endpoint
// in later slices.

const transcodingPath = "/api/v1/transcoding"

type transcodingView struct {
	Backend struct {
		Requested string `json:"requested"`
		Active    string `json:"active"`
		Degraded  bool   `json:"degraded"`
		Reason    string `json:"reason"`
	} `json:"backend"`
	Load struct {
		Active     int  `json:"active"`
		Cap        int  `json:"cap"`
		AtCapacity bool `json:"atCapacity"`
	} `json:"load"`
	GPU *gpuView `json:"gpu"`
}

type gpuView struct {
	UtilizationPct  *int    `json:"utilizationPct"`
	VRAMUsedMb      *int    `json:"vramUsedMb"`
	VRAMTotalMb     *int    `json:"vramTotalMb"`
	EncoderSessions *int    `json:"encoderSessions"`
	DriverVersion   *string `json:"driverVersion"`
	SampledAt       string  `json:"sampledAt"`
}

// fakeGPUProbe is the injected GPU-telemetry seam (mirroring detect_test.go's
// fakeProbe): it returns fixed telemetry or "unavailable", and counts calls so a
// test can assert a non-NVENC backend never queries it.
type fakeGPUProbe struct {
	tel   gpu.Telemetry
	ok    bool
	calls int
}

func (p *fakeGPUProbe) Sample(context.Context) (gpu.Telemetry, bool) {
	p.calls++
	return p.tel, p.ok
}

func intp(n int) *int       { return &n }
func strp(s string) *string { return &s }
func nvenc() transcode.Resolution {
	return transcode.Resolution{Accel: transcode.AccelNVENC, Requested: transcode.AccelNVENC, Reason: "hardware acceleration: nvenc", Warn: false}
}

func getTranscoding(t *testing.T, srv *testharness.Server, token string) transcodingView {
	t.Helper()
	var v transcodingView
	status, body := srv.AuthGET(transcodingPath, token, &v)
	if status != http.StatusOK {
		t.Fatalf("GET transcoding = %d, want 200; body: %s", status, body)
	}
	return v
}

// TestTranscodingBackendProjection asserts the backend block across the three
// CPU-resolution stories the operator cares about (off, auto, and a degraded
// hardware fallback) plus a validated hardware backend — the states the real
// detector can't produce on a GPU-less CI box, hence WithBackendResolution.
func TestTranscodingBackendProjection(t *testing.T) {
	cases := []struct {
		name          string
		res           transcode.Resolution
		wantRequested string
		wantActive    string
		wantDegraded  bool
		wantReason    string
	}{
		{
			name:          "off resolves to cpu, not degraded",
			res:           transcode.Resolution{Accel: transcode.AccelCPU, Requested: transcode.AccelCPU, Reason: "hardware acceleration off; using CPU libx264", Warn: false},
			wantRequested: "cpu",
			wantActive:    "cpu",
			wantDegraded:  false,
			wantReason:    "hardware acceleration off; using CPU libx264",
		},
		{
			name:          "auto falls back to cpu, not degraded",
			res:           transcode.Resolution{Accel: transcode.AccelCPU, Requested: transcode.AccelAuto, Reason: "auto-detect found no working hardware encoder; using CPU libx264", Warn: false},
			wantRequested: "auto",
			wantActive:    "cpu",
			wantDegraded:  false,
			wantReason:    "auto-detect found no working hardware encoder; using CPU libx264",
		},
		{
			name:          "requested nvenc fell back to cpu is degraded",
			res:           transcode.Resolution{Accel: transcode.AccelCPU, Requested: transcode.AccelNVENC, Reason: "configured backend nvenc did not validate (encoder missing or no working device); falling back to CPU libx264", Warn: true},
			wantRequested: "nvenc",
			wantActive:    "cpu",
			wantDegraded:  true,
			wantReason:    "configured backend nvenc did not validate (encoder missing or no working device); falling back to CPU libx264",
		},
		{
			name:          "validated nvenc is active, not degraded",
			res:           transcode.Resolution{Accel: transcode.AccelNVENC, Requested: transcode.AccelNVENC, Reason: "hardware acceleration: nvenc", Warn: false},
			wantRequested: "nvenc",
			wantActive:    "nvenc",
			wantDegraded:  false,
			wantReason:    "hardware acceleration: nvenc",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testharness.New(t, testharness.WithBackendResolution(tc.res))
			token := adminToken(t, srv)

			v := getTranscoding(t, srv, token)
			if v.Backend.Requested != tc.wantRequested {
				t.Errorf("requested = %q, want %q", v.Backend.Requested, tc.wantRequested)
			}
			if v.Backend.Active != tc.wantActive {
				t.Errorf("active = %q, want %q", v.Backend.Active, tc.wantActive)
			}
			if v.Backend.Degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, want %v", v.Backend.Degraded, tc.wantDegraded)
			}
			if v.Backend.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", v.Backend.Reason, tc.wantReason)
			}
		})
	}
}

// TestTranscodingLoadIdle asserts the load block on an idle server: no transcodes
// running, the configured cap reported, and not at capacity.
func TestTranscodingLoadIdle(t *testing.T) {
	srv := testharness.New(t, testharness.WithTranscodeCap(4))
	token := adminToken(t, srv)

	v := getTranscoding(t, srv, token)
	if v.Load.Active != 0 {
		t.Errorf("idle active = %d, want 0", v.Load.Active)
	}
	if v.Load.Cap != 4 {
		t.Errorf("cap = %d, want 4", v.Load.Cap)
	}
	if v.Load.AtCapacity {
		t.Errorf("atCapacity = true on idle server, want false")
	}
}

// TestTranscodingLoadUnlimited asserts a cap of 0 is reported as unlimited and is
// never at capacity.
func TestTranscodingLoadUnlimited(t *testing.T) {
	srv := testharness.New(t, testharness.WithTranscodeCap(0))
	token := adminToken(t, srv)

	v := getTranscoding(t, srv, token)
	if v.Load.Cap != 0 {
		t.Errorf("unlimited cap = %d, want 0", v.Load.Cap)
	}
	if v.Load.AtCapacity {
		t.Errorf("atCapacity = true under unlimited cap, want false")
	}
}

// TestTranscodingLoadReflectsLiveTranscodes drives the real-ffmpeg governance
// pattern (as in governance_test.go): a live transcode fills the only slot, so the
// snapshot reports active 1 / cap 1 / atCapacity true; ending it frees the slot and
// the snapshot returns to active 0 / not-at-capacity. Only transcodes are metered,
// which the cap-of-1 saturation already proves.
func TestTranscodingLoadReflectsLiveTranscodes(t *testing.T) {
	requireFixtures(t)
	requireFFmpeg(t)
	srv := testharness.New(t, testharness.WithTranscodeCap(1))
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	bladeID := findTitle(t, list, "Blade Runner") // mkv mpeg4/mp3 — forces transcode

	// A single transcode takes the only slot.
	first := negotiateTranscodeDecision(t, srv, token, bladeID, transcodeProfile())

	v := getTranscoding(t, srv, token)
	if v.Load.Active != 1 {
		t.Errorf("active with one transcode = %d, want 1", v.Load.Active)
	}
	if v.Load.Cap != 1 {
		t.Errorf("cap = %d, want 1", v.Load.Cap)
	}
	if !v.Load.AtCapacity {
		t.Errorf("atCapacity = false at the cap, want true")
	}

	// Ending the transcode frees the slot.
	if s, b := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+first.SessionID, token, nil, nil); s != http.StatusNoContent {
		t.Fatalf("delete transcode status = %d, want 204; body: %s", s, b)
	}

	v = getTranscoding(t, srv, token)
	if v.Load.Active != 0 {
		t.Errorf("active after freeing slot = %d, want 0", v.Load.Active)
	}
	if v.Load.AtCapacity {
		t.Errorf("atCapacity = true after freeing slot, want false")
	}
}

// TestTranscodingGPUPopulatedForNVENC asserts that with an NVENC active backend
// and an available probe, the gpu block carries the full telemetry readout and a
// sampledAt reflecting the sample's capture time.
func TestTranscodingGPUPopulatedForNVENC(t *testing.T) {
	sampledAt := time.Date(2026, 7, 11, 18, 22, 4, 0, time.UTC)
	probe := &fakeGPUProbe{
		ok: true,
		tel: gpu.Telemetry{
			UtilizationPct:  intp(37),
			VRAMUsedMB:      intp(1240),
			VRAMTotalMB:     intp(8192),
			EncoderSessions: intp(2),
			DriverVersion:   strp("550.90.07"),
			SampledAt:       sampledAt,
		},
	}
	srv := testharness.New(t,
		testharness.WithBackendResolution(nvenc()),
		testharness.WithGPUProbe(probe))
	token := adminToken(t, srv)

	v := getTranscoding(t, srv, token)
	if v.GPU == nil {
		t.Fatal("gpu = null, want populated telemetry for NVENC + available probe")
	}
	if v.GPU.UtilizationPct == nil || *v.GPU.UtilizationPct != 37 {
		t.Errorf("utilizationPct = %v, want 37", v.GPU.UtilizationPct)
	}
	if v.GPU.VRAMUsedMb == nil || *v.GPU.VRAMUsedMb != 1240 {
		t.Errorf("vramUsedMb = %v, want 1240", v.GPU.VRAMUsedMb)
	}
	if v.GPU.VRAMTotalMb == nil || *v.GPU.VRAMTotalMb != 8192 {
		t.Errorf("vramTotalMb = %v, want 8192", v.GPU.VRAMTotalMb)
	}
	if v.GPU.EncoderSessions == nil || *v.GPU.EncoderSessions != 2 {
		t.Errorf("encoderSessions = %v, want 2", v.GPU.EncoderSessions)
	}
	if v.GPU.DriverVersion == nil || *v.GPU.DriverVersion != "550.90.07" {
		t.Errorf("driverVersion = %v, want 550.90.07", v.GPU.DriverVersion)
	}
	if v.GPU.SampledAt != "2026-07-11T18:22:04Z" {
		t.Errorf("sampledAt = %q, want 2026-07-11T18:22:04Z", v.GPU.SampledAt)
	}
}

// TestTranscodingGPUNullWhenProbeUnavailable asserts that an NVENC backend whose
// probe answers "unavailable" (nvidia-smi absent / query error) yields a null gpu
// block — the one all-or-nothing unavailable state.
func TestTranscodingGPUNullWhenProbeUnavailable(t *testing.T) {
	probe := &fakeGPUProbe{ok: false}
	srv := testharness.New(t,
		testharness.WithBackendResolution(nvenc()),
		testharness.WithGPUProbe(probe))
	token := adminToken(t, srv)

	v := getTranscoding(t, srv, token)
	if v.GPU != nil {
		t.Errorf("gpu = %+v, want null when the probe is unavailable", *v.GPU)
	}
	if probe.calls == 0 {
		t.Error("probe was never sampled for an NVENC backend, want it queried")
	}
}

// TestTranscodingGPUNullForNonNVENC asserts a non-NVENC active backend never
// queries the probe and reports a null gpu block, even when telemetry is available.
func TestTranscodingGPUNullForNonNVENC(t *testing.T) {
	probe := &fakeGPUProbe{ok: true, tel: gpu.Telemetry{UtilizationPct: intp(37)}}
	// Requested nvenc but fell back to CPU (degraded): the active backend is CPU, so
	// the GPU block must stay null and the probe must never be spawned.
	res := transcode.Resolution{Accel: transcode.AccelCPU, Requested: transcode.AccelNVENC, Reason: "nvenc did not validate; using CPU", Warn: true}
	srv := testharness.New(t,
		testharness.WithBackendResolution(res),
		testharness.WithGPUProbe(probe))
	token := adminToken(t, srv)

	v := getTranscoding(t, srv, token)
	if v.GPU != nil {
		t.Errorf("gpu = %+v, want null for a non-NVENC active backend", *v.GPU)
	}
	if probe.calls != 0 {
		t.Errorf("probe sampled %d times for a non-NVENC backend, want 0", probe.calls)
	}
}

// TestTranscodingRequiresAdmin asserts the endpoint is behind the admin scope: a
// Member gets 403, not a filtered view (ADR-0029).
func TestTranscodingRequiresAdmin(t *testing.T) {
	srv := testharness.New(t)
	// An admin must exist first (setup) before a member can be created.
	_ = adminToken(t, srv)
	srv.CreateMember("bob", "pw12345678")
	member := srv.LoginAs("bob", "pw12345678")

	if status, body := srv.AuthGET(transcodingPath, member, nil); status != http.StatusForbidden {
		t.Errorf("member GET = %d, want 403; body: %s", status, body)
	}
}
