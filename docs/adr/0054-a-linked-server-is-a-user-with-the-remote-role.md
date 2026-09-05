# A linked Server is a User with the `remote` role

Two households each run an Obelo. One wants to let the other browse and play a few of its
libraries. There is no account service, no directory, no relay ([ADR-0001](./0001-fully-self-hosted-no-vendor-dependency.md)),
so whatever "letting them in" means, it has to be something one Server can grant and the other
can hold, with nothing in between.

This ADR decides **what the thing that logs in is**. Two companion ADRs decide how it gets its
credential ([ADR-0055](./0055-linking-is-a-one-time-invite-redeemed-server-to-server.md)) and what
it sees once it has one ([ADR-0056](./0056-a-linked-library-is-a-read-only-mirror-played-through-a-one-hop-relay.md)).

## Why not a new entity

The obvious design is a `peers` or `shares` table: the connecting thing is a Server, not a
person, and a person-shaped row feels wrong for it. It was rejected because everything a
sharing operator would want to say about a peer — *these libraries, up to this rating, at most
this quality* — is already something the Server says about a User, and every handler already
enforces it through one seam:

- `access.Service.Resolve(userID)` turns a User into a fail-closed `Scope`
  (`internal/access/service.go`), and `requireScope` puts it on every request.
- The per-User grant set (`user_library_access`) is replaced atomically as a whole and rejects
  unknown libraries without touching the prior set.
- Outside-scope reads answer **404, never 403** (api-contract §Access control). A peer must not
  be able to enumerate what it was not granted, and the 404 posture already guarantees that.
- A **Device** row is the thing a token binds to, individually revocable, upserted by a stable
  `clientId` (ADR-0015, ADR-0034).
- Deleting a User cascades its Devices and tokens.

A separate entity would need its own grant table, its own scope resolution, its own token
binding, and its own audit of every browse handler — or it would shadow a User and get all of
that for free while pretending not to. So: **the peer is a User**.

## Decisions

**1. A third role, `remote`.** Beside `admin` and `member`. The role is a label the sharing
Admin picks at creation ("Brandon's server"); the username is that label. A `remote` User:

- **has no password.** `password_hash` is empty, `POST /auth/login` refuses the role outright
  (before the KDF, before the rate limiter — there is nothing to verify), and the web app never
  offers it a sign-in. Its only credential is the token minted by redeeming an invite
  (ADR-0055).
- **cannot be promoted.** Role changes to or from `remote` are rejected, the way a grant to an
  Admin is rejected today (`ADMIN_GRANT`). A `remote` User is created `remote` and dies `remote`.
- **is not in the roster.** The household roster and instant switch list people. A `remote`
  User is excluded from every "who is on this Server" surface a non-Admin can see. It appears
  on the Admin Users page, with its Device and last-seen, because that page is where the
  operator manages what it can reach.
- **never writes watch state.** Watch state belongs to the person watching, and that person is
  on the other Server, which keeps it there (ADR-0056). Relay playback under a `remote` User
  starts and ends sessions, feeds `nowPlaying` for the sharer's transcode observability, and
  writes nothing per-Title.
- **resolves a Scope like a Member.** Exactly the granted set, empty = nothing, plus a Rating
  ceiling, plus the ceilings below. No new enforcement code.

**2. A per-User Playback ceiling, for every role except Admin.** Three columns beside
`rating_ceiling`: `max_resolution` (e.g. `1080p`), `max_bitrate` (bits/sec) and `max_streams`,
zero or empty meaning uncapped. The first two are **clamped into the session's
`Constraints`** (`internal/playback/profile.go`) before negotiation: the effective constraint is
the stricter of what the client asked for and what the User is allowed. The existing tiering
then does what it always does — a 1080p Edition direct-plays, a 4K-only File transcodes down
under ADR-0009 governance and is refused with `SERVER_BUSY` when the budget is full. Titles are
never hidden by a quality ceiling; a ceiling changes how something plays, not whether it exists.

`max_streams` is enforced at negotiation: a User at their cap gets a new error code,
`STREAM_LIMIT`, and the sessions that count are the ones the reaper has not ended. It is the
sharer's one lever over *load* from a household they cannot see into.

The ceiling is general because the mechanism is: a household Admin capping a kid's iPad at 720p
is the same code path. `remote` is merely the role where it matters most.

**3. What the sharer sees is the Link, not the person.** Every play from the other household
arrives as the one `remote` User. The sharer's session list and `nowPlaying` events show
"Brandon's server — Title X", one session per concurrent stream. No display name, device name,
or anything else about who in that household is watching crosses the wire. The sharer's
legitimate interests are load and content; both are served. `max_streams` exists precisely so
they do not need a third one.

**4. A linked Library is never grantable to a `remote` User.** A Library the sharer itself
mirrored from somebody else (ADR-0056) cannot be placed in a `remote` User's grant set: the
replace-set rejects it (`LINKED_GRANT`), the export endpoint refuses to serve it, and the Users
dialog does not list it for the role. The owner of the files decided who sees them; a hop later,
that decision would be made by somebody they never met, and paid for in transcode CPU by the
owner. One hop, by construction.

## What was considered and rejected

- **A `remote` flag on a Member.** A flag on a Member invites "a Member who is also remote",
  and every roster/watch-state/login check becomes a two-field test. A role is one field and
  the guards read as `role == remote`, exactly like `ADMIN_GRANT` reads today.
- **Sharing the sharer's own Admin token.** Immediate, and catastrophic: full mutation rights on
  another household's Server, forever.
- **Hiding Titles that exceed the quality ceiling.** Zero CPU cost for the sharer, and a friend
  with a 4K-only library shares nothing. The transcode governor is the sharer's real protection
  and it already exists.
- **Per-person attribution across the Link.** Leaks the home household's user list to a
  stranger's Server for no benefit to the home household.

## Consequences

- `CONTEXT.md`'s **User** widens from "a person with credentials on this Server" to "a person,
  or a linked Server, with credentials on this Server". **Remote** and **Playback ceiling** are
  new glossary terms.
- Migration: `users` gains `max_resolution TEXT`, `max_bitrate INTEGER`, `max_streams INTEGER`,
  all defaulting to unset. `role` gains the value `remote`; `password_hash` becomes allowed-empty
  for that role only (a CHECK, so a `member` can never be created without one by accident).
- `POST /users` accepts `role: "remote"` with no password and returns the User; the invite is
  minted separately (ADR-0055) so re-keying does not mean re-creating.
- `PUT /users/{id}/playbackCeiling` is the admin endpoint; `422 ADMIN_CEILING` applies to it as
  it does to the Rating ceiling.
- The SSE subscribe-time audience resolution is unchanged: a `remote` User subscribing to
  `/events` sees only `libraryUpdated` for its granted set, which is exactly the nudge the
  mirror on the other side wants.
- Deleting the `remote` User is the sharer's kill switch. The cascade removes its Device and
  token; the next request from the other Server 401s and that Server marks its Link **revoked**
  (ADR-0056).

## Non-goals

- Accounts that span Servers. A person is a User on their home Server and nothing on anyone
  else's.
- Any change to how Admins and Members authenticate.
- Federated identity, invitations by email, or a directory of Servers. There is nobody to run
  one (ADR-0001).
