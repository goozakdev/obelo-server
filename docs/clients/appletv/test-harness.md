# Test harness — disposable backend for tvOS development

How to boot an isolated, throwaway Juice Box server with generated media, so the tvOS app (simulator or device) develops against something real without touching a production instance. Everything below is scriptable; wire it into the tvOS repo as a `make backend` target or an Xcode pre-action.

## Option A — Docker (no Go/Node toolchain needed)

The server repo ships a multi-stage Dockerfile building a static linux binary with ffmpeg in the runtime image.

```bash
# from a checkout of the juicebox repo:
docker build -f docker/Dockerfile -t juicebox .

docker run --rm -p 8099:8080 \
  -v "$PWD/harness/data:/data" -v "$PWD/harness/media:/media" \
  -e JUICEBOX_DATA_DIR=/data \
  juicebox
```

## Option B — build from source (needs Go + Node; matches server HEAD)

```bash
# from a checkout of the juicebox repo:
make web                                   # SPA bundle (go:embed prerequisite)
go build -o /tmp/jb-harness/juicebox ./cmd/juicebox

JUICEBOX_DATA_DIR=/tmp/jb-harness/data \
JUICEBOX_LISTEN_ADDR=0.0.0.0:8099 \
  /tmp/jb-harness/juicebox > /tmp/jb-harness/server.log 2>&1 &
echo $! > /tmp/jb-harness/server.pid
```

`0.0.0.0` (not `127.0.0.1`) if a physical Apple TV on the LAN must reach it; the simulator can use `127.0.0.1`.

## First boot: claim the admin

First boot logs a one-time claim token:

```
juicebox: no users yet — first-Admin claim token: <TOKEN>
```

```bash
CLAIM=$(grep -o 'claim token: .*' /tmp/jb-harness/server.log | awk '{print $3}')
B=http://127.0.0.1:8099/api/v1

curl -s -X POST $B/setup \
  -d "{\"claimToken\":\"$CLAIM\",\"username\":\"admin\",\"password\":\"harness\"}"

TOKEN=$(curl -s -X POST $B/auth/login \
  -d '{"username":"admin","password":"harness","device":{"name":"Harness","platform":"cli","clientId":"harness-1"}}' \
  | sed -E 's/.*"token":"([^"]+)".*/\1/')
```

Also create a **member** user early — the tvOS app should be tested against a capped, library-granted member, not just the admin (access filtering, 404-hiding, and rating ceilings only show up there):

```bash
curl -s -X POST $B/users -H "Authorization: Bearer $TOKEN" \
  -d '{"username":"kid","password":"harness","role":"member"}'
# then PUT /users/{id}/libraryAccess and /users/{id}/ratingCeiling as needed
```

## Media fixtures

ffmpeg test clips scan fine when named by convention (identity is derived from paths — the naming *is* the identity):

```bash
M=harness/media
mkdir -p "$M/Movies/Back to the Future (1985)" "$M/TV/The Wire/Season 01" \
         "$M/Music/Daft Punk/Discovery"

clip() { ffmpeg -y -loglevel error \
  -f lavfi -i "testsrc=duration=${2:-10}:size=640x360:rate=24" \
  -f lavfi -i "sine=frequency=440:duration=${2:-10}" \
  -c:v libx264 -pix_fmt yuv420p -c:a aac -shortest "$1"; }

clip "$M/Movies/Back to the Future (1985)/Back to the Future (1985).mkv" 30
clip "$M/TV/The Wire/Season 01/The Wire - S01E01 - The Target.mkv" 30
clip "$M/TV/The Wire/Season 01/The Wire - S01E02 - The Detail.mkv" 30
# music: identity comes from embedded tags, so set them:
ffmpeg -y -loglevel error -f lavfi -i "sine=frequency=330:duration=15" \
  -metadata artist="Daft Punk" -metadata album="Discovery" \
  -metadata title="One More Time" -metadata track=1 \
  -c:a aac "$M/Music/Daft Punk/Discovery/01 - One More Time.m4a"
```

Useful variants: with the libmpv profile both `.mkv` and `.mp4` direct-play, so add a second file in one movie folder named `… - 1080p.mkv` vs `… - 2160p.mkv` to exercise Editions, and drop **subtitle sidecars** next to a movie to exercise ADR-0033 original-format delivery — `Movie.en.srt` (plain SRT text) and `Movie.de.ass` (a real ASS script with an override tag like `{\i1}`): negotiating with `textSubtitleFormats: ["ass","srt","vtt"]` must return `.ass`/`.srt` URLs with `format` tags, while `["vtt"]` must return `.vtt` conversions. To exercise the transcode tier despite the broad profile, send a low `constraints.maxBitrate` (e.g. `500000`).

## Libraries + scan

```bash
# Parse the id properly — do NOT reach for sed here. libraryJSON is
#   { "id", "name", "kind", "rootFolders": [ { "id", "path" } ] }
# and `sed -E 's/.*"id":"([^"]+)".*/\1/'` is GREEDY: `.*` runs to the LAST "id" in
# the body, so it returns the root folder's id, not the library's. The scan below
# then POSTs to a library that doesn't exist, 404s, and you get an empty library
# with no error anywhere. (The `"token"` extraction above is safe only because a
# login response has exactly one `"token"` key.)
MLIB=$(curl -s -X POST $B/libraries -H "Authorization: Bearer $TOKEN" \
  -d "{\"name\":\"Movies\",\"kind\":\"movie\",\"rootFolders\":[\"$PWD/$M/Movies\"]}" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')

# Expect 202. Anything else (404 = wrong id, 409 = the library already exists) means
# the scan never ran.
curl -s -o /dev/null -w '%{http_code}\n' -X POST $B/libraries/$MLIB/scan \
  -H "Authorization: Bearer $TOKEN"
# poll: GET $B/libraries/$MLIB/scan until state:"idle"
# repeat for TV (kind "tv") and Music (kind "music")
```

Scans are async (202 + poll); the harness fixtures scan in well under a second. Without a TMDB key, enrichment stays sparse — fine for API testing (`enrichmentStatus: "pending"`, no artwork). To exercise artwork/metadata paths, set `JUICEBOX_TMDB_API_KEY` at boot and `POST /libraries/{id}/enrich`.

## Teardown / reset

```bash
kill $(cat /tmp/jb-harness/server.pid)
rm -rf /tmp/jb-harness/data        # full reset: next boot prints a fresh claim token
```

Resetting the data dir is the cheapest way back to a known state — cheaper than unwinding watch state/playlists through the API.

## Gotchas

- **Simulator vs device**: the simulator shares the Mac's localhost; a physical Apple TV needs the Mac's LAN IP and `JUICEBOX_LISTEN_ADDR=0.0.0.0:…`, plus local-network permission in the tvOS app.
- **Discovery**: the harness advertises `_juicebox._tcp` like any instance (ADR-0034). Name it with `JUICEBOX_SERVER_NAME="Harness"` so it is obvious in a picker next to a real server. Verify from the Mac with `dns-sd -B _juicebox._tcp local` — if the app sees nothing, check that its Info.plist declares `NSBonjourServices`, since without it the browse returns empty and looks like a server fault. Note a **simulator does not join the LAN's multicast** the way a device does; test discovery on real hardware and use manual entry in the simulator.
- **HTTP on tvOS**: plain-HTTP LAN backends need an ATS exception (`NSAllowsLocalNetworking`) in the app's Info.plist.
- The server binds plain HTTP; TLS is a reverse proxy's job in production. Testing the `Secure`-cookie path needs a proxy setting `X-Forwarded-Proto: https`.
- Progress keepalive: an idle session is reaped after **90 s** (`JUICEBOX_SESSION_IDLE_TIMEOUT`); set it low (e.g. `10s`) to test the app's reaped-session recovery quickly.
- The transcode tier needs ffmpeg on the server host (bundled in the Docker image; on macOS `brew install ffmpeg`).
