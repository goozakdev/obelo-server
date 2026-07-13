# Juice Box

**A fully self-hosted media server for your household's movies, TV, and music.**

Juice Box scans the video and music files you already have on disk, organizes
them into browsable libraries, decorates them with artwork and descriptions from
public metadata sources, and streams them to your devices through a clean web
app — all from a single binary you run on your own hardware. No accounts on
someone else's servers, no phoning home, no vendor lock-in.

> Point it at a folder of media, open it in a browser, and press play.

---

## Highlights

- **Movies, TV, and Music** in one server — each library holds one kind, backed
  by one or more folders on disk (multiple folders merge into one library).
- **Identity from your file names, not a cloud database.** The scanner derives
  what each file *is* from its path and embedded tags, so your library is yours
  and stays correct offline. External metadata only ever *decorates* — it never
  decides identity.
- **Automatic artwork & metadata (enrichment).** Posters, backgrounds, logos,
  cast, descriptions, artist photos, and biographies are fetched from public
  providers (TMDB, MusicBrainz, fanart.tv, TheAudioDB, and more). Fully
  optional and off until you turn it on.
- **Adaptive streaming with three playback tiers.** Direct play when the client
  can handle the file as-is, direct stream (remux) when only the container needs
  changing, and full FFmpeg transcode when it doesn't — chosen automatically
  from what each client reports it can play.
- **Hardware-accelerated transcoding.** CPU (libx264) always works; NVENC,
  VAAPI, Quick Sync, and VideoToolbox backends are selectable, with a live
  admin view of transcode load and (on NVIDIA) GPU telemetry.
- **Multi-user with real access control.** Per-user libraries, content-rating
  ceilings, private watch state, resume/Continue-Watching, TV Up Next, and
  named per-device tokens you can revoke individually.
- **Subtitles that just work.** Embedded, sidecar (`Movie.en.srt`), and
  on-demand fetched subtitles — delivered as selectable tracks or burned in when
  the format requires it.
- **Incremental, resilient scanning.** Runs on a schedule or on demand; missing
  files are soft-deleted (watch state survives their return); a per-entity
  **targeted scan** re-checks just one movie/show/album's folders.
- **Admin niceties.** Hand-edit and lock fields, correct a wrong match, upload
  your own artwork, and curate Collections — all without the next scan undoing
  your work.
- **One process, one binary.** A Go server with the React web app embedded
  inside it (`go:embed`). SQLite for state, the filesystem for caches. Nothing
  else to stand up.

See [`CONTEXT.md`](./CONTEXT.md) for the full domain glossary and
[`docs/adr/`](./docs/adr/) for the architectural decisions behind these choices.

---

## Requirements

- **`ffmpeg`** (with `ffprobe`) on the server's `PATH` — used for probing files
  and for transcoding. This is the only runtime dependency.
- A folder (or folders) of media, and a browser to reach the web app.

That's it for running the prebuilt Docker image. To **build from source** you
additionally need **Go 1.25+** and **Node.js 22+**.

---

## Quick start with Docker (recommended)

Build the image (the build context is the repository root, not the `docker/`
directory):

```sh
docker build -f docker/Dockerfile -t juicebox .
```

Run it, mounting a writable data directory and your media (read-only):

```sh
docker run --rm -p 8080:8080 \
  -v "$PWD/data:/data" \
  -v /path/to/your/media:/media:ro \
  juicebox
```

- **`:8080`** — the web app and API (same origin). Open <http://localhost:8080>.
- **`/data`** — the one writable directory: SQLite database, artwork/subtitle
  caches, transcode scratch. Mount a volume so it survives restarts.
- **Your media** — mount anywhere read-only; you'll point libraries at these
  paths from the web UI.

Building on Apple Silicon still produces a `linux/amd64` image (the Go binary is
pure-Go and cross-compiles). If your Docker can't run amd64 locally, use buildx:

```sh
docker buildx build --platform linux/amd64 -f docker/Dockerfile -t juicebox . --load
```

### GPU-accelerated transcoding (NVIDIA / NVENC)

```sh
docker run --rm --gpus all -p 8080:8080 \
  -e JUICEBOX_HARDWARE_ACCEL=nvenc \
  -v "$PWD/data:/data" \
  -v /path/to/your/media:/media:ro \
  juicebox
```

The admin **Transcoding** tab then shows GPU telemetry (utilization, VRAM,
encoder sessions, driver version). Without `--gpus all`, or on any non-NVENC
backend, the GPU block reads "unavailable" — that's expected; the rest of the
tab still works.

More detail lives in [`docker/README.md`](./docker/README.md).

---

## Build & run from source

Prerequisites: **Go 1.25+**, **Node.js 22+**, and **`ffmpeg`** on `PATH`.

```sh
git clone https://github.com/marioquake/juicebox.git
cd juicebox
make build      # builds the web bundle first, then the Go binary that embeds it
./bin/juicebox
```

`make build` enforces the required order: the React/Vite SPA is bundled into
`internal/webui/dist` *before* `go build`, because the binary embeds it. Other
handy targets:

| Target        | What it does                                             |
| ------------- | -------------------------------------------------------- |
| `make build`  | Full build (web bundle → Go binary).                     |
| `make run`    | Build, then run the server.                              |
| `make test`   | Go unit/integration tests, then the Playwright E2E suite.|
| `make fmt`    | `gofmt` the tree.                                        |
| `make clean`  | Remove build outputs.                                    |

By default the server listens on `:8080` and stores state in `./data`.

---

## First run: create the first admin

Everything in Juice Box is authenticated, so the very first admin is
bootstrapped with a **one-time claim token** printed to the server's logs on
first start (when the database has no users yet):

```
juicebox: no users yet — first-Admin claim token: <token>
juicebox: complete setup via POST /api/v1/setup with this claimToken
```

Open the web app, and the setup wizard will ask for that token to create your
admin account. Once the first admin exists, setup permanently closes. With
Docker, read the token from `docker logs <container>`.

After logging in: create your libraries (point them at your mounted media
folders), pick a media kind for each, trigger a scan, and — if you want artwork
and descriptions — turn on enrichment and grant consent.

---

## Configuration

All configuration is via `JUICEBOX_*` environment variables. Common ones:

| Variable                             | Default   | Purpose                                                        |
| ------------------------------------ | --------- | -------------------------------------------------------------- |
| `JUICEBOX_LISTEN_ADDR`               | `:8080`   | `host:port` the server binds to.                               |
| `JUICEBOX_DATA_DIR`                  | `./data`  | Writable data directory (DB + caches).                         |
| `JUICEBOX_SCAN_INTERVAL`             | `1h`      | Scheduled incremental scan cadence (`0` disables).             |
| `JUICEBOX_HARDWARE_ACCEL`            | `off`     | `off` / `auto` / `nvenc` / `vaapi` / `qsv` / `videotoolbox`.   |
| `JUICEBOX_MAX_CONCURRENT_TRANSCODES` | `3`       | Cap on simultaneous transcodes (`0` = unlimited).              |
| `JUICEBOX_TMDB_API_KEY`              | —         | Enables Movie/TV enrichment via TMDB.                          |
| `JUICEBOX_MUSICBRAINZ_ENABLED`       | `false`   | Turns on Music enrichment (needs no key).                      |
| `JUICEBOX_FANART_TV_API_KEY`         | —         | Adds artist imagery from fanart.tv.                            |
| `JUICEBOX_THEAUDIODB_API_KEY`        | —         | Adds artist images + biographies from TheAudioDB.              |
| `JUICEBOX_OPENSUBTITLES_API_KEY`     | —         | Enables on-demand subtitle fetching.                           |
| `JUICEBOX_METADATA_LANGUAGE`         | `en-US`   | Preferred language/region for fetched metadata.               |

Provider keys and language seed the database only on **first boot** — afterward
you manage providers from the admin settings UI (no restart needed). Juice Box
is **offline-first**: with no keys configured it makes zero outbound calls and
every title simply shows as un-enriched. The full list of knobs is documented in
[`internal/config/config.go`](./internal/config/config.go).

---

## Attribution

Juice Box's *metadata enrichment* is optional and, when enabled, decorates your
library using these public sources. Juice Box is not endorsed by or affiliated
with any of them.

<table>
  <tr>
    <td width="180" align="center"><img src="assets/attribution/tmdb.svg" alt="TMDB" height="26"></td>
    <td>Movie &amp; TV metadata and artwork. <b>This product uses the TMDB API but is not endorsed or certified by <a href="https://www.themoviedb.org/">TMDB</a>.</b></td>
  </tr>
  <tr>
    <td width="180" align="center"><img src="assets/attribution/musicbrainz.svg" alt="MusicBrainz" height="52"></td>
    <td>Music metadata (artists, albums, tracks) from <a href="https://musicbrainz.org/">MusicBrainz</a>, the open music encyclopedia.</td>
  </tr>
  <tr>
    <td width="180" align="center"><img src="assets/attribution/fanarttv.png" alt="fanart.tv" height="44"></td>
    <td>Additional artist and fan artwork courtesy of <a href="https://fanart.tv/">fanart.tv</a>.</td>
  </tr>
  <tr>
    <td width="180" align="center"><img src="assets/attribution/theaudiodb.png" alt="TheAudioDB" height="26"></td>
    <td>Artist images and biographies courtesy of <a href="https://www.theaudiodb.com/">TheAudioDB</a>.</td>
  </tr>
</table>

Album cover art is additionally sourced from the
[Cover Art Archive](https://coverartarchive.org/). Logos and trademarks are the
property of their respective owners and are used here solely to credit the data
sources.

---

## Project layout

```
cmd/juicebox/        server entry point
internal/            the modular monolith (scanner, enrichment, playback, api, …)
web/                 React + TypeScript SPA (embedded into the binary)
docker/              multi-stage Dockerfile + docker notes
docs/adr/            architectural decision records
CONTEXT.md           domain glossary (the project's ubiquitous language)
```
