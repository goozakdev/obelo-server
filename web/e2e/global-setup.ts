import type { FullConfig } from "@playwright/test";

// The suite is NOT parallel-safe (issue 12): play.spec.ts's transcode-cap test
// holds the server's single concurrent-transcode slot
// (OBELO_MAX_CONCURRENT_TRANSCODES=1, boot-server.mjs) while music.spec.ts's
// FLAC transcode can land in the other worker and race it — measured once at
// --workers 2: 61 passed / 1 failed / 1 skipped, music.spec.ts's transcode
// getting a real 503 SERVER_BUSY instead of the 200 it expects. Making the
// suite parallel-safe is out of scope; refuse to run with more than one
// worker instead of silently racing.
export default function globalSetup(config: FullConfig) {
  if (config.workers > 1) {
    throw new Error(
      `e2e suite must run with a single worker (got ${config.workers}): play.spec.ts's ` +
        `transcode-cap test and music.spec.ts's FLAC transcode both exercise the server's ` +
        `single concurrent-transcode slot and race each other under more than one worker ` +
        `(issue 12). Run with --workers=1 (playwright.config.ts also pins workers: 1 by default).`,
    );
  }
}
