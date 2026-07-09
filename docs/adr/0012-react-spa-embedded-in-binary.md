# React+TS SPA, hls.js player, embedded in the Go binary

The management web app is a single-page app in React + TypeScript that consumes the public API scope ([ADR-0010](./0010-unified-two-scope-api.md)) like any other client. In-browser playback uses hls.js (MSE) on Chrome/Firefox/Edge and native HLS on Safari, so the browser exercises the same HLS path as native clients ([ADR-0004](./0004-hls-for-adaptive-progressive-for-direct-play.md)).

The built static assets are embedded into the Go binary (`go:embed`) and served by the monolith on the same process and port — nothing extra to deploy.

## Why
Embedding keeps the "one container, one process, one port" promise of [ADR-0006](./0006-docker-first-modular-monolith.md). React+TS has the most mature ecosystem for the constraint that matters most — an HLS browser player (hls.js).

## Consequences
- The frontend build is a step in producing the Go binary/image.
- The browser is a real playback client, validating the public API and HLS delivery end to end.
