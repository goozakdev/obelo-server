// Package gpu is the best-effort GPU-telemetry seam for the admin transcoding
// surface (ADR-0029). It answers a single question — "what is the GPU doing right
// now, or nothing" — behind a narrow, fakeable Probe interface mirroring the
// transcode package's encodeProbe: the production implementation shells out to
// nvidia-smi (NVENC-only in v1), and tests inject a fake so the whole /transcoding
// projection is exercised without a real GPU.
//
// Telemetry is best-effort and all-or-nothing at the boundary: a Probe that can't
// answer (tool absent, query error) reports unavailable, which the api layer
// renders as a `null` gpu block — there is deliberately no unavailable-reason on
// the wire (ADR-0029), the operator has the backend degraded/reason story for
// "hardware isn't working". Individual fields inside a sample are nil only in the
// rare case the tool returns some columns but not others.
package gpu

import (
	"context"
	"time"
)

// Telemetry is one best-effort GPU sample. The numeric fields are pointers so a
// column the tool reports as unavailable ([N/A]/[Not Supported]) collapses to a
// nil (a JSON null) rather than a misleading zero. SampledAt is the capture time
// of this sample (the cached sample's time, not the request time), so a frozen or
// stale probe shows as an aging "as of Ns ago" stamp instead of reading live.
type Telemetry struct {
	UtilizationPct  *int
	VRAMUsedMB      *int
	VRAMTotalMB     *int
	EncoderSessions *int
	DriverVersion   *string
	SampledAt       time.Time
}

// Probe answers "current GPU telemetry, or nothing". ok is false for every
// unavailable case — the tool is absent, the query failed, or the output was
// unparseable — collapsing to one all-or-nothing state (no partial/error variant
// reaches the wire). Implementations may cache; the api layer calls Sample only
// when the active backend is NVENC.
type Probe interface {
	Sample(ctx context.Context) (telemetry Telemetry, ok bool)
}
