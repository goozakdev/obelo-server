# Docker-first modular monolith on Linux x86_64/arm64

The server ships primarily as a Docker image bundling FFmpeg, targeting Linux x86_64 and arm64 (servers, NASes, Raspberry Pi). Media directories and a single writable data directory are mounted in.

It is a single long-running process — a modular monolith with clean internal module boundaries (scanner, playback, API, web), not a set of microservices. Transcoding runs as child FFmpeg processes, not a separate service.

Native per-OS single binaries are a planned later convenience, not v1.

## Why
Self-hosted adoption is gated by install friction; one container that runs identically across hobbyist hardware is the lowest-friction path. A single process is far simpler to operate, which is the whole point of self-hosting; internal module boundaries preserve the option to split services later.

## Consequences
- FFmpeg is bundled in the image (reinforces [ADR-0003](./0003-three-tier-playback-with-capability-negotiation.md)).
- Module boundaries must be respected in code so a future split stays possible.
