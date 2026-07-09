# Go for the server

The server is written in Go.

## Why
The workload is FFmpeg child-process orchestration, many concurrent HLS streams, and background scans in one long-lived process. Go compiles to a single static binary (serving both the Docker-first image and the future native-binary goal of [ADR-0006](./0006-docker-first-modular-monolith.md)), cross-compiles trivially to arm64, has a goroutine concurrency model that fits many simultaneous streams plus background scans, makes spawning/supervising FFmpeg clean, and has a small runtime footprint suitable for a NAS/Pi.

## Considered and rejected
- **Rust** — more performance/safety than needed here, slower to build out a large app.
- **C#/.NET** (Jellyfin's stack) — capable, but heavier runtime and larger image.
- **Node/TypeScript** — would share code with the frontend, but weakest at CPU-bound process orchestration and long-running concurrency.
