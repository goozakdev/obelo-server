// boot-server.mjs — the Playwright webServer command.
//
// It stands up the REAL embedded server the way production does, so the browser
// specs exercise UI + typed API client + Go server as one black box:
//
//   1. Build the frontend (vite build) into ../internal/webui/dist.
//   2. `go build` the juicebox binary, which embeds that bundle via go:embed.
//   3. Boot the binary against a FRESH temp data dir on E2E_PORT.
//
// The process stays in the foreground; Playwright waits for /api/v1/server to
// answer (see playwright.config.ts) and kills this process when the run ends,
// which terminates the child server. The temp data dir is cleaned on exit.
//
// Requires a Go toolchain on PATH (the harness builds the real binary). Network
// is only needed for the one-time `npm install` / `npx playwright install` the
// caller runs beforehand — booting itself is offline.

import { spawn, spawnSync } from "node:child_process";
import { createServer } from "node:http";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const webDir = resolve(here, "..");
const repoRoot = resolve(webDir, "..");
const port = process.env.E2E_PORT ?? "8099";

// A local TMDB stub so the enrichment E2E exercises the real Enrichment pass
// with NO live network: the server's TMDBProvider + ArtworkFetcher are pointed
// at this stub via env. It serves a canned movie-details payload, a search hit,
// and a 1×1 PNG for any /img/* poster/backdrop request.
const stubPng = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M8AAAMBAQDJ/pLvAAAAAElFTkSuQmCC",
  "base64",
);
const stubMovie = JSON.stringify({
  id: 777,
  title: "Dune",
  overview: "An epic tale of dunes and destiny.",
  tagline: "Fear is the mind-killer.",
  release_date: "2021-10-22",
  runtime: 155,
  genres: [{ name: "Science Fiction" }, { name: "Adventure" }],
  production_companies: [{ name: "Legendary" }],
  poster_path: "/img/poster.png",
  backdrop_path: "/img/backdrop.png",
  images: { logos: [{ file_path: "/img/logo.png", width: 800, height: 310 }] },
  credits: { cast: [{ name: "Timothée Chalamet", character: "Paul Atreides" }] },
  release_dates: { results: [{ iso_3166_1: "US", release_dates: [{ certification: "PG-13" }] }] },
});
// TV (issue 03): a show with genres/network/US content rating, a season poster,
// and an episode with a canonical name + still — all images point at /img/*.
const stubTV = JSON.stringify({
  id: 1399,
  overview: "Noble families vie for the Iron Throne.",
  genres: [{ name: "Drama" }, { name: "Fantasy" }],
  networks: [{ name: "HBO" }],
  poster_path: "/img/tv-poster.png",
  backdrop_path: "/img/tv-bg.png",
  images: { logos: [{ file_path: "/img/tv-logo.png", width: 800, height: 310 }] },
  content_ratings: { results: [{ iso_3166_1: "US", rating: "TV-MA" }] },
});
const stubSeason = JSON.stringify({ poster_path: "/img/season.png", overview: "Season one." });
// Edit-item image picker (item-editing/03): the TMDB /images payload the Fix-label
// poster/background chooser lists. Two posters + one backdrop + a still, all
// pointing at /img/* (served as a real PNG above).
const stubImages = JSON.stringify({
  posters: [
    { file_path: "/img/poster-a.png", width: 2000, height: 3000 },
    { file_path: "/img/poster-b.png", width: 1000, height: 1500 },
  ],
  backdrops: [{ file_path: "/img/back-a.png", width: 3840, height: 2160 }],
  stills: [{ file_path: "/img/still.png", width: 1920, height: 1080 }],
  logos: [{ file_path: "/img/logo-a.png", width: 800, height: 310 }],
});
const stubEpisode = JSON.stringify({
  name: "The Suitcase",
  overview: "Carmy opens up about the restaurant.",
  still_path: "/img/still.png",
});

// Music (issue 03): MusicBrainz artist/release-group/recording + Cover Art Archive
// front images (served as PNG from /release-group/{id}/front).
const stubArtist = JSON.stringify({
  artists: [
    {
      id: "mb-artist",
      type: "Group",
      disambiguation: "English rock band",
      area: { name: "Oxford" },
      tags: [{ name: "alternative rock" }, { name: "art rock" }],
    },
  ],
});
const stubReleaseGroup = JSON.stringify({
  "release-groups": [
    { id: "mb-rg", "first-release-date": "1997-05-21", tags: [{ name: "alternative rock" }] },
  ],
});
const stubRecording = JSON.stringify({ recordings: [{ id: "mb-rec", title: "Canonical Title" }] });

const tmdbStub = createServer((req, res) => {
  const url = req.url ?? "";
  const json = (body) => {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(body);
  };
  if (url.startsWith("/img/") || /\/release-group\/[^/]+\/front/.test(url)) {
    // Both TMDB image paths and Cover Art Archive front images return a real PNG.
    res.writeHead(200, { "Content-Type": "image/png" });
    res.end(stubPng);
  } else if (url.startsWith("/subtitles?") || url === "/subtitles") {
    // OpenSubtitles search (subtitles/05): one candidate in the requested language,
    // so the "search online → pick → track appears" e2e drives a real fetch with no
    // live network. The candidate's file_id keys the download below.
    const lang = new URL(url, "http://stub").searchParams.get("languages") ?? "en";
    json(
      JSON.stringify({
        data: [
          {
            attributes: {
              language: lang,
              hearing_impaired: false,
              download_count: 1234,
              release: "E2E.Release.1080p",
              files: [{ file_id: 5150, file_name: `online.${lang}.srt` }],
            },
          },
        ],
      }),
    );
  } else if (url === "/download") {
    // OpenSubtitles two-step download: hand back a link to the SRT bytes below.
    json(JSON.stringify({ link: `http://127.0.0.1:${tmdbPort}/subfile.srt`, file_name: "online.srt" }));
  } else if (url.startsWith("/subfile.srt")) {
    res.writeHead(200, { "Content-Type": "text/plain" });
    res.end("1\n00:00:00,000 --> 00:00:05,000\nFetched online cue\n");
  } else if (/\/(movie|tv)\/.*\/images/.test(url)) {
    // The Edit-item image picker lists a role's provider images (item-editing/03).
    json(stubImages);
  } else if (url.startsWith("/search/movie")) {
    // A title the operator hasn't curated has no metadata record: the enrichment-
    // match attention E2E (issue 05) drives a deliberate no-match for "Nomatch
    // Movie", then corrects it by id (/movie/{id} below always resolves).
    if (url.includes("Nomatch")) {
      json(JSON.stringify({ results: [] }));
    } else {
      // Carry the display fields the Edit-item "Fix info" picker shows (title/
      // year/poster/overview) so the enrichment-override E2E can render + pick a
      // candidate; the by-name Lookup path only reads the id, so this stays
      // backward-compatible with the other enrichment specs.
      json(
        JSON.stringify({
          results: [
            {
              id: 777,
              title: "Dune",
              release_date: "2021-10-22",
              poster_path: "/img/poster.png",
              overview: "An epic tale of dunes and destiny.",
            },
          ],
        }),
      );
    }
  } else if (url.startsWith("/movie/")) {
    json(stubMovie);
  } else if (url.startsWith("/search/tv")) {
    // Carry the display fields the parent Edit-item "Fix info" picker shows
    // (name/first_air_date/poster/overview) so the Show enrichment-override E2E can
    // render + pick a candidate; the by-name Lookup path only reads the id.
    json(
      JSON.stringify({
        results: [
          {
            id: 1399,
            name: "Game of Thrones",
            first_air_date: "2011-04-17",
            poster_path: "/img/tv-poster.png",
            overview: "Noble families vie for the Iron Throne.",
          },
        ],
      }),
    );
  } else if (/\/tv\/[^/]+\/season\/\d+\/episode\/\d+/.test(url)) {
    json(stubEpisode);
  } else if (/\/tv\/[^/]+\/season\/\d+/.test(url)) {
    json(stubSeason);
  } else if (url.startsWith("/tv/")) {
    json(stubTV);
  } else if (url.startsWith("/artist")) {
    json(stubArtist);
  } else if (url.startsWith("/release-group")) {
    // The album Fix-info search (item-editing/search-improvements) sends the user's
    // terms RELEVANCE-RANKED, not wrapped as an exact releasegroup:"…" phrase. For
    // the Anastasia soundtrack we mimic REAL MusicBrainz: its canonical release-group
    // title is just "Anastasia" (secondary-type Soundtrack), so the bare relevance
    // terms FIND it but an exact-phrase query (`releasegroup:"Anastasia Soundtrack"`,
    // the old bug) matches NOTHING. This makes the album-relevance e2e a real
    // regression guard — reverting the phrase fix makes the candidate vanish. Any
    // other query (the enrichment auto-match by tag, etc.) keeps the old stub reply.
    const rgQuery = new URL(url, "http://stub").searchParams.get("query") ?? "";
    if (/anastasia/i.test(rgQuery)) {
      if (/releasegroup:"[^"]*"/i.test(rgQuery)) {
        json(JSON.stringify({ "release-groups": [] }));
      } else {
        json(
          JSON.stringify({
            "release-groups": [
              {
                id: "mb-anastasia",
                title: "Anastasia",
                "first-release-date": "1997-11-18",
                "primary-type": "Album",
                "secondary-types": ["Soundtrack"],
                "artist-credit": [{ name: "David Newman" }],
              },
            ],
          }),
        );
      }
    } else {
      json(stubReleaseGroup);
    }
  } else if (url.startsWith("/recording")) {
    json(stubRecording);
  } else {
    res.writeHead(404, { "Content-Type": "application/json" });
    res.end("{}");
  }
});
await new Promise((ready) => tmdbStub.listen(0, "127.0.0.1", ready));
const tmdbPort = tmdbStub.address().port;
console.log(`[boot-server] TMDB stub on :${tmdbPort}`);

// The auth E2E needs the one-time first-Admin claim token, which the server
// prints to its logs on a fresh data dir (ADR-0013) and never exposes over the
// API. We capture it from the child's stdout and write it to this file so the
// setup spec can read it (mirroring an operator reading the logs). Cleared at
// boot so a stale token from a previous run can't be mistaken for the new one.
const claimTokenFile = join(here, ".claim-token");

function run(cmd, args, opts = {}) {
  const label = `${cmd} ${args.join(" ")}`;
  console.log(`[boot-server] ${label}`);
  const res = spawnSync(cmd, args, { stdio: "inherit", ...opts });
  if (res.status !== 0) {
    throw new Error(`[boot-server] step failed (${res.status}): ${label}`);
  }
}

// 1. Build the frontend into the Go embed directory.
run("npm", ["run", "build"], { cwd: webDir });

// 2. Build the server binary that embeds the freshly built bundle.
const binPath = join(mkdtempSync(join(tmpdir(), "juicebox-bin-")), "juicebox");
run("go", ["build", "-o", binPath, "./cmd/juicebox"], { cwd: repoRoot });

// 3. Boot the binary against a fresh temp data dir. Scheduled scan + session
//    reaper are disabled so the smoke run is deterministic and quiet.
const dataDir = mkdtempSync(join(tmpdir(), "juicebox-data-"));
console.log(`[boot-server] starting server on :${port} (data dir: ${dataDir})`);

// Start each run with no stale claim token on disk.
try {
  rmSync(claimTokenFile, { force: true });
} catch {
  // best effort
}

// Pipe stdout/stderr so we can scrape the claim-token line; everything is still
// forwarded to our own streams so the logs stay visible.
const child = spawn(binPath, [], {
  stdio: ["inherit", "pipe", "pipe"],
  env: {
    ...process.env,
    JUICEBOX_LISTEN_ADDR: `127.0.0.1:${port}`,
    JUICEBOX_DATA_DIR: dataDir,
    JUICEBOX_SCAN_INTERVAL: "0",
    JUICEBOX_SESSION_IDLE_TIMEOUT: "0",
    // Cap concurrent transcodes at 1 so the play spec can deterministically
    // provoke a 503 SERVER_BUSY: occupy the single slot via the API, then drive
    // the browser to a transcode-requiring title and assert the busy/retry UX
    // (ADR-0009 / TRANSCODE issue 05). Direct play and remux are unmetered, so
    // every other E2E spec is unaffected.
    JUICEBOX_MAX_CONCURRENT_TRANSCODES: "1",
    // Enrichment ON, pointed at the local TMDB stub above (no live network), so
    // the enrichment specs can drive a real pass and assert the decorated detail
    // + the live SSE-driven grid update. Auto-after-scan and the scheduled sweep
    // are OFF so a scan never races a spec's MANUAL POST /enrich (the specs assert
    // exact pass counts and drive the live-update from the manual pass).
    JUICEBOX_TMDB_API_KEY: "e2e-key",
    JUICEBOX_TMDB_BASE_URL: `http://127.0.0.1:${tmdbPort}`,
    JUICEBOX_TMDB_IMAGE_BASE_URL: `http://127.0.0.1:${tmdbPort}`,
    // Music enrichment (issue 03): MusicBrainz + Cover Art Archive pointed at the
    // same local stub, so the TV/Music enrichment spec runs with no live network.
    JUICEBOX_MUSICBRAINZ_BASE_URL: `http://127.0.0.1:${tmdbPort}`,
    JUICEBOX_COVERART_BASE_URL: `http://127.0.0.1:${tmdbPort}`,
    JUICEBOX_AUTO_ENRICH: "false",
    JUICEBOX_ENRICH_INTERVAL: "0",
    // External subtitle fetching (subtitles/05) ON, pointed at the same local stub
    // (the /subtitles + /download + /subfile.srt routes above), so the "search
    // online" e2e drives a real provider fetch with no live network. A key is set so
    // SeedIfEmpty enables the provider on first boot.
    JUICEBOX_OPENSUBTITLES_API_KEY: "e2e-sub-key",
    JUICEBOX_OPENSUBTITLES_BASE_URL: `http://127.0.0.1:${tmdbPort}`,
  },
});

// The log line is: "...first-Admin claim token: <token>". Capture the token and
// persist it for the setup spec.
// Go's `log` package writes to STDERR, so we scan both streams — scanning only
// stdout would never see the token line.
const claimTokenRe = /claim token:\s*(\S+)/;
function captureToken(text) {
  const m = text.match(claimTokenRe);
  if (m) {
    writeFileSync(claimTokenFile, m[1], "utf8");
    console.log(`[boot-server] captured claim token → ${claimTokenFile}`);
  }
}
child.stdout.on("data", (buf) => {
  const text = buf.toString();
  process.stdout.write(text);
  captureToken(text);
});
child.stderr.on("data", (buf) => {
  const text = buf.toString();
  process.stderr.write(text);
  captureToken(text);
});

function shutdown(signal) {
  try {
    child.kill("SIGTERM");
  } catch {
    // already gone
  }
  try {
    rmSync(dataDir, { recursive: true, force: true });
  } catch {
    // best effort
  }
  try {
    rmSync(claimTokenFile, { force: true });
  } catch {
    // best effort
  }
  try {
    tmdbStub.close();
  } catch {
    // best effort
  }
  if (signal) process.exit(0);
}

child.on("exit", (code) => {
  shutdown(null);
  process.exit(code ?? 0);
});
process.on("SIGTERM", () => shutdown("SIGTERM"));
process.on("SIGINT", () => shutdown("SIGINT"));
