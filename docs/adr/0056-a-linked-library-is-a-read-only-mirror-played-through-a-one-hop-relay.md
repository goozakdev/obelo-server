# A linked Library is a read-only mirror, played through a one-hop relay

The home Server holds a token for a `remote` User on somebody else's Server (ADR-0054,
ADR-0055). This ADR decides what it does with it: how the other Server's libraries appear on
this one, who here may see them, how a play travels, and what happens when the other Server
goes away.

## Decisions

**1. Each granted remote library is a Library row here, of source `linked`.** `libraries`
gains `source` (`local` | `linked`), `link_id` and `remote_library_id`. A linked Library has no
root folders; the Scanner, the enrichment pass, artwork upload, Placement, editing — every
writer that walks libraries — skips `source = linked` by the same query filter, and the write
handlers refuse it with `409 LINKED_LIBRARY`. **The sharer's Server is the identity authority
for its own files** (ADR-0002, ADR-0019 — "local disk wins", and here local means theirs), so
nothing on this side is allowed to re-derive, correct, or enrich what arrived.

**2. Visibility is granted per User, like any Library.** A linked Library is a Library, so the
existing `user_library_access` grant applies unchanged: Admins see it by role, Members see it
when granted. The home Rating ceiling layers under the sharer's — the stricter side wins —
because each Server enforces its own ceiling on its own side and neither knows about the other.
This is what lets a home Admin say "the kids get his cartoons library and not the other one"
with a dialog that already exists.

**3. The catalog is a synced mirror, keyed by the sharer's ids.** Titles, Shows, Seasons,
Episodes, Artists, Albums, Tracks, Files, Editions and Streams from the sharer are written into
this Server's own catalog tables, each row carrying the `remote_id` it came from, under the
linked Library. Home rows, search, Collections, Playlists, counts, the access `StoreFilter`,
and Watch state keyed to Title identity (ADR-0014) then all work with **no change**, because
they are SQL over local tables and a mirrored Title is a local row.

Why not a live proxy: every aggregate this Server computes — `/home`, `/search`, a Playlist
spanning libraries — is one query with the access filter pushed into it. A live proxy makes
each of those a fan-out and a merge, and a slow friend's Server makes your home screen slow.
The mirror also degrades honestly (§6).

**Mirrored rows are matched by `remote_id`, never by title.** A rename on the sharer's side
updates the row; it never creates a second one; Watch state survives.

**4. The sharer exposes a dedicated export; the mirror never walks the browse API.**
`GET /libraries/{id}/export` — `remote` role and Admins only — is one flat, cursor-paginated
feed of every entity under a Library with its `remote_id`, parent ids, `updatedAt`, and a
tombstone for anything soft-deleted (ADR-0008 already keeps the rows). It accepts `since` for
an incremental pull. The browse endpoints are shaped for screens: nested, capped per row, and
carrying the calling User's Watch state, none of which a mirror wants, and a full walk of a TV
library through them is one request per Show then one per Season with no way to ask "what
changed". The export is also the **one place** the sharer decides what a remote peer may learn
about a Title, rather than an audit of every browse handler.

The home Server does a **full pull at link time**, then an **incremental pull on the sharer's
`libraryUpdated` SSE nudge** (the `remote` User subscribes to `/events` and is audience-gated to
its own grants, so it hears exactly the right libraries) **and on a periodic timer** as the poll
fallback — which is precisely how the contract already frames SSE: an optimisation over a
pollable resource.

**5. Playback is a pure relay: the sharer negotiates, the sharer transcodes, this Server
streams bytes.** A play on a mirrored Title goes to this Server's normal playback endpoint. This
Server forwards the client's Capability profile to the sharer's playback endpoint under the
`remote` User's bearer; the sharer clamps it to that User's Playback ceiling (ADR-0054), applies
its own three tiers and its own governance (ADR-0009), and answers with a direct-play or HLS
Decision. This Server opens a local Session wrapping the remote one, **rewrites every media URL
in the answer to a path on itself**, and streams the bytes through, Range headers included,
re-encoding nothing. `SERVER_BUSY` and `suggestedMaxBitrate` pass through verbatim. Artwork is
fetched the same way and cached in the identity-keyed artwork cache under the linked Library.

Why the sharer transcodes: they are the ones sharing with a stranger, so the transcode budget
that protects them must be theirs. This Server pays bandwidth, never CPU, for someone else's
files.

Why bytes never bypass this Server: the whole point of the topology (ADR-0055) is that the
sharer is reachable from one machine. A client-direct URL would need every phone on the
tailnet. It is also the mechanism by which §1 of ADR-0054 holds — the sharer sees one User.

Watch state for a mirrored Title is written **here, only**, against the mirrored row.

**6. When the sharer is unreachable, the mirror stays, marked unavailable.** A Link is in one
of three states:

- **connected** — the last export or relay call succeeded.
- **unreachable** — a transport failure or 5xx; transient, retried with backoff across every
  origin in the invite. The linked Library and its Titles report `available: false`; a play
  answers `503 LINK_UNREACHABLE`; nothing is deleted; Continue Watching survives the friend's
  reboot.
- **revoked** — a `401`, which the contract defines as "the token is dead". The sharer deleted
  the User or the Device. The mirror stays; the Linked servers page offers "paste a new invite"
  which, because the invite carries the same server id, re-keys the Link in place.

Only an explicit `DELETE /links/{id}` by a home Admin removes the linked Library, its mirrored
rows and their Watch state — the same shape as deleting a local Library. This mirrors how the
project already treats a File that vanished: soft-deleted and surfaced (ADR-0008, ADR-0047),
never silently gone.

**7. One hop, by construction.** A linked Library cannot be granted to a `remote` User and the
export refuses to serve one (ADR-0054 §4). Two Servers that both want a third's library each
link to it directly. The relay is therefore never a chain.

## What was considered and rejected

- **Home-side transcoding from the sharer's original file.** Simpler tiering story, and it
  bypasses the sharer's governance while burning this Server's CPU on files it does not own.
- **Dropping mirrored rows on failure.** Home rows shrink, Playlists lose members, Watch state
  has nothing to attach to, and everything reappears with fresh ids unless the mirror is more
  careful than a delete would ever be.
- **Reusing the browse API for the mirror.** See §4. It works for a ten-film library and hammers
  a friend's Server for a thousand-episode one.
- **A direct-to-client URL when the phone can reach the sharer.** Rejected for v1: it reopens
  the per-device reachability problem the topology closed, and it lets a phone present the
  home Server's `remote` token to a third party.

## Consequences

- Migration: `libraries.source`, `libraries.link_id`, `libraries.remote_library_id`; a
  nullable `remote_id` on each mirrored catalog table with a unique index on
  `(library_id, remote_id)`; a `links` table (id, server id, server name, origins JSON, active
  origin, token, protocol version, state, last synced, last error).
- The `remote` User's bearer is an **outbound credential stored at rest** on the home Server,
  in the same posture as the metadata provider keys. It grants read and play on somebody
  else's libraries and nothing else, and the sharer can kill it at any moment.
- New relay routes on the home Server for media and artwork under a session-scoped path; the
  playback service branches on `library.source` at negotiation and nowhere else.
- `GET /libraries` and the Title summaries gain `linked` and `available` fields so clients can
  badge a linked Library and grey an unreachable one without knowing why.
- A new admin surface, **Linked servers**, listing each Link with its state, origin in use,
  last sync, and the actions: sync now, re-key, unlink.
- A new runbook, *Link two servers*, covering both sides including the Tailscale machine-share
  console step and node key expiry on the sharer's side, which silently kills the tailnet path
  after 180 days and shows up here only as **unreachable**.

## Non-goals

- Merging two catalogs' identities. A film present on both Servers is two Titles.
- Bidirectional linking as one concept. Two households that share both ways create two Links,
  one per direction, each with its own `remote` User.
- Syncing Watch state, Collections, or Playlists across Servers.
- Photos, or any media kind the sharer's export does not yet carry.
