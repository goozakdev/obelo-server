package transcode

// Setup-time ffmpeg AVAILABILITY resolution — the simpler sibling of detect.go's
// backend detection, and deliberately a different question. detect.go asks "WHICH
// video encoder should the transcode tier use", and its answer always exists (the
// CPU libx264 path is the guaranteed fallback). This file asks the question below
// that one: "is there a usable ffmpeg on this host AT ALL" — and unlike the
// backend, that one has a real "no". A deployment with no ffmpeg cannot serve the
// remux or transcode halves of ADR-0003 at all, only direct play.
//
// It follows ADR-0009's posture for its sibling ("detection/validation is a
// setup-time concern, not per-stream"): the answer is resolved ONCE at startup and
// handed to whoever needs it — the handshake's features.transcode flag, and the
// Runner that will actually spawn ffmpeg. Nothing re-probes per request, and the
// handshake in particular must never shell out (it is an unauthenticated endpoint
// every client hits on connect).
//
// The seam mirrors Detector: an AvailabilityProbe interface, the real
// FFmpegAvailabilityProbe that satisfies it by shelling out through the same
// ffmpegProbe the detector uses, and a StaticAvailability the app can be wired
// against so both answers are drivable in tests on any host — with or without a
// real ffmpeg installed.

import (
	"context"
	"fmt"
	"os/exec"
)

// Availability is the setup-time verdict on whether this deployment can run
// ffmpeg. It is the single resolved fact behind the handshake's
// features.transcode flag, so a client that hides its transcode affordance when
// the flag is false is hiding it exactly when the server genuinely cannot serve
// that tier.
type Availability struct {
	// Available reports whether an ffmpeg executable was both RESOLVED and ran.
	// Resolution alone is not enough — a file on PATH that cannot execute (a
	// truncated download, a wrong-architecture binary, a dangling symlink into a
	// vanished container layer) resolves fine and then fails at the first
	// playback, which is precisely the state the flag exists to warn clients away
	// from. False is a legitimate, supported deployment: direct play still works.
	Available bool

	// Binary is the executable the transcode Runner should invoke, carried here so
	// the whole server agrees on ONE ffmpeg rather than resolving it again per
	// component. Empty means "ffmpeg on PATH" — FFmpeg.Binary's own convention, and
	// what the production probe leaves it as, so the resolved-from-PATH case invokes
	// ffmpeg byte-for-byte as it did before this seam existed.
	Binary string

	// Reason is a human-readable explanation for the one startup log line: where the
	// binary was found, or why the answer is no. An operator who sees clients hiding
	// transcode needs this line to know whether ffmpeg is missing or merely broken.
	Reason string
}

// AvailabilityProbe answers the "is there a usable ffmpeg" question once at
// startup. It is an interface — with a static implementation below, mirroring
// Detector/StaticDetector — so the app can be wired against a PINNED answer: the
// feature flag must be assertable in both directions on a developer box that has
// ffmpeg and on a CI box that does not, and neither test may depend on installing
// or removing a binary.
type AvailabilityProbe interface {
	// Resolve reports whether ffmpeg is usable. It is called ONCE at startup and
	// must never fail-fast: an absent or broken ffmpeg is a degraded deployment
	// (direct play only), not a boot failure.
	Resolve(ctx context.Context) Availability
}

// StaticAvailability is an AvailabilityProbe that returns a fixed Availability
// without touching the filesystem or spawning anything. It is the test/harness
// seam for the transcode feature flag and for the no-ffmpeg failure path: pinning
// {Available: false, Binary: "<a path that does not exist>"} produces a server
// that both ADVERTISES no transcode tier and genuinely cannot spawn ffmpeg, so the
// advertisement and the behaviour can be asserted together.
type StaticAvailability struct{ Availability Availability }

// Resolve implements AvailabilityProbe, returning the pinned Availability.
func (a StaticAvailability) Resolve(context.Context) Availability { return a.Availability }

// FFmpegAvailabilityProbe is the production AvailabilityProbe: it looks the
// executable up and runs it.
type FFmpegAvailabilityProbe struct {
	// binary is the configured executable name/path, empty for "ffmpeg on PATH".
	// It is preserved verbatim into the resolved Availability so the Runner keeps
	// invoking exactly what was configured.
	binary string
	probe  ffmpegProbe
}

// NewAvailabilityProbe builds the production probe. ffmpegBinary names the ffmpeg
// executable (empty defaults to "ffmpeg" on PATH, matching FFmpeg/FFprobe and
// NewDetector).
func NewAvailabilityProbe(ffmpegBinary string) *FFmpegAvailabilityProbe {
	return &FFmpegAvailabilityProbe{binary: ffmpegBinary, probe: ffmpegProbe{binary: ffmpegBinary}}
}

// Resolve implements AvailabilityProbe with a two-step check that mirrors the
// detector's validate() in spirit: the executable must be (1) findable and
// (2) able to actually run. Step 2 is what separates "a file with that name
// exists" from "this host can transcode", and it is cheap — `ffmpeg -version`
// exits in milliseconds and decodes nothing.
//
// Step 1 is skipped for an explicitly-pathed binary (LookPath is a PATH search;
// an absolute or relative path is simply run, and step 2 catches it if it cannot
// be). Its only other job is to name the resolved location in the log line.
func (p *FFmpegAvailabilityProbe) Resolve(ctx context.Context) Availability {
	located, lookErr := exec.LookPath(p.probe.bin())
	if lookErr != nil {
		return Availability{
			Binary: p.binary,
			Reason: fmt.Sprintf("no ffmpeg executable found for %q (%v); the transcode and direct-stream tiers are unavailable — direct play only", p.probe.bin(), lookErr),
		}
	}
	if err := p.probe.versionRuns(ctx); err != nil {
		return Availability{
			Binary: p.binary,
			Reason: fmt.Sprintf("ffmpeg at %s could not be run (%v); the transcode and direct-stream tiers are unavailable — direct play only", located, err),
		}
	}
	return Availability{
		Available: true,
		Binary:    p.binary,
		Reason:    fmt.Sprintf("ffmpeg resolved at %s; the transcode delivery tier is available", located),
	}
}

// versionRuns runs `ffmpeg -hide_banner -version` and reports whether it completed
// cleanly. It is the cheapest possible proof that the binary is genuinely
// executable on this host: it opens no input, decodes nothing, and touches no
// device, so it says nothing about which ENCODERS are present (that is the
// detector's two-step validate) — only that ffmpeg itself runs.
func (p ffmpegProbe) versionRuns(ctx context.Context) error {
	return exec.CommandContext(ctx, p.bin(), "-hide_banner", "-version").Run()
}
