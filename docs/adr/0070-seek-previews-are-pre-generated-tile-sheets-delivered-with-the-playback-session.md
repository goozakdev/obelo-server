# Seek previews are pre-generated tile sheets, delivered with the Playback session

A **Seek preview** is a small still of a File shown above the seek bar while someone scrubs.
This ADR decides how they are made, what shape clients receive them in, and under which
credential, because that contract is consumed by three clients (web, Apple, Android) built in
parallel and is expensive to change once they ship.

## Decisions

**1. Made ahead of time, never on demand.** Seek preview generation is a background job that
extracts one frame every **10 seconds**, **320px wide** (display-aspect-correct, HDR
tone-mapped to SDR), from each video File in Movie and TV Libraries. A File with no set yet
shows no picture; there is no on-demand fallback. Interval and size are fixed in v1 but
carried in the manifest, so changing them later needs no client change.

**2. Nearest keyframe, CPU decode.** Frame *i* is the keyframe at or before *i × interval*,
decoded on the CPU regardless of the Transcode backend. A frame may be a few seconds early;
the seek itself still lands on the exact time.

**3. Scheduled exactly like Marker detection (ADR-0065 §4).** Runs only when no Transcode is
running, yields at the next safe point when one starts, and never counts against the
ADR-0009 cap. Seek preview generation and Marker detection share **one background lane**, so
only one heavy background job runs at a time, previews ahead of Markers. Backfill order:
a File someone starts playing jumps the queue; otherwise most recently added first.

**4. Tile sheets plus a JSON manifest.** Frames are packed row-major into JPEG grid sheets
(frame *i* = time *i × interval*). The manifest states interval, frame size, grid and sheet
URLs. Clients fetch sheets lazily (on the first scrub, the needed sheet and its neighbour) and
crop locally.

**5. Delivered with the Playback session.** The playback decision carries an optional
`seekPreviews` manifest, absent when the File has none, with sheets under
`/api/v1/stream/<stream token>/seekPreviews/`. Access, Rating ceiling and the Linked-Library
relay (ADR-0056 §5) come from the session; a Linked Library's previews are always the origin's.

**6. A bearer File route, for downloads only.** `GET /files/{id}/seekPreviews` returns the same
manifest with sheet URLs under the same File route, behind the same Scope check and the same
single-404 collapse as `/files/{id}/download` (no previews yet is also that 404). It takes the
Bearer header only; `?token=` stays exclusive to the download route. A client
downloading a File fetches the sheets after the bytes and stores them with the download;
a download made before its File had previews is not topped up later.

**7. Previews belong to the bytes.** A set is stale when the File's mtime or size changes
(the Scanner's own change signal, which updates the File row in place): it is deleted at once,
stops being served, and is requeued. It is kept while the File is Missing (ADR-0008), and kept **and still served**
while the library toggle is off — the toggle governs generation only; it is deleted when the File is purged or an Admin deletes a
library's previews. A failure is recorded and not retried until the File changes or an Admin
retries. Sets live under `<data>/seekpreviews/<fileID>/` (ADR-0007).

## Considered

- **On-demand frame extraction** on hover. Rejected: every scrub costs a seek and decode,
  laggy on slow disks and remote links, and it competes with live playback at exactly the
  moment someone is watching.
- **HLS I-frame playlists.** Native AVKit scrubbing understands them, but they exist only for
  HLS sessions (not Direct play), hls.js on the web gains nothing, and both native clients
  draw their own scrubber anyway (`AVPlayerLayer`, Compose `Slider`).
- **WebVTT thumbnail track** (`#xywh=`). Pays off only with a player library that consumes it
  natively; we use none, and native clients would parse VTT for no gain.
- **One image per frame.** ~720 requests for a two-hour film; bad over a Tailnet or Relay.
- **Exact-frame decode, or hardware decode.** Exact is many times slower for a multi-day
  CPU backfill on a NAS; hardware decode is shared with the live Transcodes this job is meant
  to stay out of the way of.
- **A bearer route for everything.** Would need its own access checks and relay path for
  playback; the session already has both. The bearer route exists only because a download
  plays with no session.
- **Importing Jellyfin `.trickplay` / Plex BIF files.** Layouts vary by version, and generation
  is automatic anyway.

## Consequences

- The manifest shape, URL patterns and tile layout are frozen in a contract doc with a
  checked-in fixture **before** any generation code, so the three clients build in parallel.
  Changes after the freeze are made by the server side and announced to all three clients.
- `internal/markerdetect`'s own queue moves under a coordinator shared with Seek preview
  generation.
- Library settings gain the toggle, a coverage line, failed Files with Retry, and "Delete
  seek previews"; a Title/Show gains "Generate seek previews now". A failed preview is **not**
  an attention kind: it loses only a nicety.
- CONTEXT.md gains **Seek preview** and **Seek preview generation**.
