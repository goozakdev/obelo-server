package api

import (
	"net/http"
	"time"

	"github.com/marioquake/juicebox/internal/gpu"
	"github.com/marioquake/juicebox/internal/transcode"
)

// transcodingResponse is the camelCase JSON shape of GET /api/v1/transcoding
// (ADR-0029): a single admin-only, read-only snapshot of the transcode
// subsystem. A later slice adds `gpu` alongside `backend` and `load`; it rides the
// same poll automatically.
type transcodingResponse struct {
	Backend backendBlock `json:"backend"`
	Load    loadBlock    `json:"load"`
	// GPU is the best-effort GPU-telemetry block, or null in every unavailable case
	// (non-NVENC active backend, nvidia-smi absent, probe error) — one all-or-nothing
	// state, deliberately without an unavailable-reason (ADR-0029).
	GPU *gpuBlock `json:"gpu"`
}

// backendBlock projects the setup-time transcode.Resolution (ADR-0009): the
// operator's Requested backend, the resolved-and-validated Active backend in
// force, the Degraded flag (requested hardware but running on CPU), and the
// human-readable Reason for the resolution.
type backendBlock struct {
	Requested string `json:"requested"`
	Active    string `json:"active"`
	Degraded  bool   `json:"degraded"`
	Reason    string `json:"reason"`
}

// loadBlock projects playback's transcode-governance counters (ADR-0009): the
// live full-transcode Active count against the concurrency Cap (0 = unlimited),
// and the derived AtCapacity flag. Direct play and direct stream carry no load and
// are never counted. AtCapacity is always false under an unlimited cap.
type loadBlock struct {
	Active     int  `json:"active"`
	Cap        int  `json:"cap"`
	AtCapacity bool `json:"atCapacity"`
}

// gpuBlock is the best-effort GPU-telemetry readout (ADR-0029), NVENC-only in v1.
// The numeric fields are pointers so a column nvidia-smi reports as unavailable
// renders as a JSON null rather than a misleading 0; SampledAt is the capture time
// of the (possibly cached) sample, so a frozen probe shows as an aging stamp.
type gpuBlock struct {
	UtilizationPct  *int    `json:"utilizationPct"`
	VRAMUsedMb      *int    `json:"vramUsedMb"`
	VRAMTotalMb     *int    `json:"vramTotalMb"`
	EncoderSessions *int    `json:"encoderSessions"`
	DriverVersion   *string `json:"driverVersion"`
	SampledAt       string  `json:"sampledAt"`
}

// handleTranscoding serves the admin transcode-observability snapshot. It only
// projects state that already exists (the boot-time backend Resolution, the live
// governance counters, and a best-effort GPU sample) — it changes nothing about
// playback, negotiation, or governance.
func handleTranscoding(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res := deps.Backend
		load := deps.Playback.TranscodeLoad()
		writeJSON(w, http.StatusOK, transcodingResponse{
			Backend: backendBlock{
				Requested: backendWire(res.Requested),
				Active:    backendWire(res.Accel),
				Degraded:  res.Warn,
				Reason:    res.Reason,
			},
			Load: loadBlock{
				Active:     load.Active,
				Cap:        load.Cap,
				AtCapacity: load.Cap > 0 && load.Active >= load.Cap,
			},
			GPU: gpuTelemetry(r, deps.Backend.Accel, deps.GPU),
		})
	}
}

// gpuTelemetry projects a best-effort GPU sample, or nil for the one uniform
// unavailable state. The probe is queried ONLY when the active backend is NVENC —
// CPU/VAAPI/QSV/VideoToolbox never spawn nvidia-smi — and a nil probe or an
// unavailable answer collapses to nil (a null gpu block on the wire).
func gpuTelemetry(r *http.Request, active transcode.Accel, probe gpu.Probe) *gpuBlock {
	if active != transcode.AccelNVENC || probe == nil {
		return nil
	}
	tel, ok := probe.Sample(r.Context())
	if !ok {
		return nil
	}
	return &gpuBlock{
		UtilizationPct:  tel.UtilizationPct,
		VRAMUsedMb:      tel.VRAMUsedMB,
		VRAMTotalMb:     tel.VRAMTotalMB,
		EncoderSessions: tel.EncoderSessions,
		DriverVersion:   tel.DriverVersion,
		SampledAt:       tel.SampledAt.UTC().Format(time.RFC3339),
	}
}

// backendWire maps a transcode.Accel to its wire vocabulary. AccelCPU is the
// empty-string zero value internally but is reported as the explicit "cpu" so the
// active backend is always a concrete, non-empty name (`off` resolves to `cpu`;
// `auto` never appears as an active value — the detector resolves it to a concrete
// backend before it reaches here).
func backendWire(a transcode.Accel) string {
	if a == transcode.AccelCPU {
		return "cpu"
	}
	return string(a)
}
