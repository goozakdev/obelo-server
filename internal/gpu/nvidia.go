package gpu

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultTTL bounds how often a poll can spawn nvidia-smi. The Transcoding tab
// polls every few seconds, and several admins (or an accidental rapid refresh)
// could poll at once; the cache coalesces that burst into at most one nvidia-smi
// per TTL. It is deliberately short so the readout still feels live — and the
// SampledAt stamp makes any staleness visible regardless.
const DefaultTTL = 2 * time.Second

// nvidiaSMIQuery is the fixed --query-gpu column list, in the order parseCSV
// expects: utilization %, VRAM used, VRAM total, encoder session count, driver
// version. --format=csv,noheader,nounits yields a single bare, comma-separated row.
const nvidiaSMIQuery = "utilization.gpu,memory.used,memory.total,encoder.stats.sessionCount,driver_version"

// NvidiaSMIProbe is the production Probe: it shells out to nvidia-smi (never linked
// via NVML/cgo — ADR-0006 single-static-binary posture) and TTL-caches the result.
// A missing binary or a query error surfaces as unavailable (ok=false), never an
// error to the caller — the /transcoding block is best-effort.
type NvidiaSMIProbe struct {
	ttl time.Duration
	now func() time.Time
	// run executes the nvidia-smi query and returns its stdout. It is a field so a
	// unit test can drive the cache with a fake runner + clock and no process spawn;
	// production points it at execNvidiaSMI.
	run func(ctx context.Context) (string, error)

	mu        sync.Mutex
	cached    Telemetry
	ok        bool
	hasCache  bool
	fetchedAt time.Time
}

// NewNvidiaSMIProbe builds the production probe. binary names the nvidia-smi
// executable (empty defaults to "nvidia-smi" on PATH); ttl<=0 uses DefaultTTL.
func NewNvidiaSMIProbe(binary string, ttl time.Duration) *NvidiaSMIProbe {
	if binary == "" {
		binary = "nvidia-smi"
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &NvidiaSMIProbe{
		ttl: ttl,
		now: time.Now,
		run: func(ctx context.Context) (string, error) { return execNvidiaSMI(ctx, binary) },
	}
}

// Sample implements Probe. It returns the cached sample while it is within the
// TTL; otherwise it re-runs nvidia-smi, parses, and caches. The lock is held
// across the run so concurrent polls collapse to one spawn rather than a herd.
func (p *NvidiaSMIProbe) Sample(ctx context.Context) (Telemetry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	if p.hasCache && now.Sub(p.fetchedAt) < p.ttl {
		return p.cached, p.ok
	}

	out, err := p.run(ctx)
	p.fetchedAt = now
	p.hasCache = true
	if err != nil {
		p.cached, p.ok = Telemetry{}, false
		return p.cached, p.ok
	}
	p.cached, p.ok = parseCSV(out, now)
	return p.cached, p.ok
}

// execNvidiaSMI runs the fixed nvidia-smi query and returns its stdout. A missing
// binary or a non-zero exit is returned as an error (→ unavailable upstream).
func execNvidiaSMI(ctx context.Context, binary string) (string, error) {
	cmd := exec.CommandContext(ctx, binary,
		"--query-gpu="+nvidiaSMIQuery,
		"--format=csv,noheader,nounits")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// parseCSV parses one nvidia-smi CSV row (noheader,nounits) into a Telemetry
// stamped with sampledAt. ok is false when the output has no usable row or the
// wrong column count — an all-or-nothing unavailable. Within a well-formed row, an
// individual column the tool reports as unavailable ("[N/A]"/"[Not Supported]")
// becomes a nil field rather than failing the whole sample. Exposed to the
// package's unit test as a pure string→struct function (no process spawn).
func parseCSV(out string, sampledAt time.Time) (Telemetry, bool) {
	line := firstNonEmptyLine(out)
	if line == "" {
		return Telemetry{}, false
	}
	fields := strings.Split(line, ",")
	if len(fields) != 5 {
		return Telemetry{}, false
	}
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}
	return Telemetry{
		UtilizationPct:  parseIntField(fields[0]),
		VRAMUsedMB:      parseIntField(fields[1]),
		VRAMTotalMB:     parseIntField(fields[2]),
		EncoderSessions: parseIntField(fields[3]),
		DriverVersion:   parseStringField(fields[4]),
		SampledAt:       sampledAt,
	}, true
}

// firstNonEmptyLine returns the first line of out with content, trimmed. nvidia-smi
// emits one data row; guarding for blank leading lines keeps parsing robust.
func firstNonEmptyLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}

// parseIntField parses a numeric column, returning nil for an unavailable or
// non-numeric value so it renders as a JSON null rather than a misleading 0.
func parseIntField(s string) *int {
	if unavailableField(s) {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	return &n
}

// parseStringField returns a non-empty, available string column, or nil.
func parseStringField(s string) *string {
	if unavailableField(s) {
		return nil
	}
	return &s
}

// unavailableField reports the sentinel values nvidia-smi uses for a column it
// can't report (and the empty string), which map to a nil field.
func unavailableField(s string) bool {
	switch strings.ToLower(s) {
	case "", "[n/a]", "n/a", "[not supported]", "not supported", "[unknown error]":
		return true
	default:
		return false
	}
}
