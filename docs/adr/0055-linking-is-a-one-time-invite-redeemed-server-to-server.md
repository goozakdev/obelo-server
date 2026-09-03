# Linking is a one-time invite, redeemed Server to Server

[ADR-0054](./0054-a-linked-server-is-a-user-with-the-remote-role.md) says the thing that
connects is a User with the `remote` role and no password. This ADR decides how that User's
credential comes into being, how it travels between two people, and how the holding Server
reaches the granting one. It borrows the shape of the Device authorization grant
([ADR-0036](./0036-device-authorization-grant-for-tv-sign-in.md)) with the roles reversed: the
short-lived code is created by the sharer and redeemed by a Server.

## The topology this rests on

**The Link lives on the home Server, not in the apps.** A person's phone, iPad and TV talk only
to their own Server, which holds the credential, fetches the catalog and relays the bytes
(ADR-0056). This was chosen over a multi-Server client (the thing ADR-0034 said it unblocked)
for one reason that outweighs the others: **reachability becomes a single-machine problem**.
One Server has to be able to reach the other. Every phone and TV in the household does not.
That is the difference between "possible" and "not" for the tailnet path below, and it means
the Apple clients get linked libraries with no protocol change at all.

## Decisions

**1. The credential is a one-time invite code, never a password.** Creating a `remote` User
mints nothing. The sharing Admin then asks for an invite (`POST /users/{id}/invite`), and gets
a 256-bit random code, stored only as its SHA-256 like a device code, **valid for 24 hours,
single use**. The home Server redeems it once (`POST /auth/link/redeem`, unauthenticated,
rate-limited per client IP like `/auth/device/token`) and receives an ordinary Device-bound
bearer through the same `issueSession` every login uses. The code is then dead. Re-linking
means a fresh invite; nothing about the User changes.

Why not a password: a password is a durable secret that has to be typed or pasted across a
text message and stays valid wherever it landed. A code that expires in a day and burns on use
can be sent over iMessage without a second thought, and removing password login from the role
removes an entire attack surface the role never needed.

Why 24 hours and not ADR-0036's five minutes: the TV flow has a person standing at both ends.
Here the friend may open the message tomorrow.

**2. The invite is one self-contained string.** Not "a hostname and a code", which is two
fields, two mistakes, and no room for a second address. The string is

```
obelo-link:<base64url(JSON)>
```

carrying `{ "v": 1, "id": "<sharer's server id>", "name": "<sharer's server name>",
"origins": ["http://obelo.tail1a2b.ts.net", "https://media.example.org"], "code": "…",
"exp": "<RFC 3339>" }`. It is shown in the sharer's Users dialog as copyable text and as a QR
code. The origins are not secret; the code is single-use; the whole thing is safe in a chat.

The **server id** (ADR-0034) is in the string so the home Server can recognise the same Server
later — a re-key from a fresh invite with the same id updates the existing Link instead of
creating a second one, and a changed origin never orphans the mirror.

**Origins are typed by the sharing Admin at invite time**, because the Server does not know its
own public address and deliberately never emits one (ADR-0005, the retired External URL). The
dialog pre-fills the MagicDNS origin when the Tailnet node is connected, and offers a free-text
field for a public HTTPS origin. The invite carries every origin listed; the home Server tries
them in order and remembers the one that answered.

The string is **not** an `https://` URL. There is no hosted page to open it against, and the
action it triggers belongs on the *redeeming* Server, which the string cannot name.

**3. `v` is a link-protocol version, checked before anything is redeemed.** Clients branch on
the `features` map and never on the version string, and that rule has already earned its keep
once (the tvOS playbook records why). The same discipline applies between Servers:

- The sharer advertises `serverLinking: true` in `GET /server`'s `features`.
- The invite, the redeem request, the export (ADR-0056) and the relay all stamp one small
  integer, `linkProtocolVersion`. Bumping it is a deliberate act with a compatibility note.
- The home Server checks the feature and the version **before** posting the code, and refuses
  at link time with one of two clear messages — "their server needs an upgrade" or "yours
  does" — never mid-sync, never mid-film.

**4. The redeeming Server identifies itself as a Device.** The redeem request carries the home
Server's own server id as the Device `clientId` and its server name as the Device name, with
`platform: "server"`. The sharer's Users page then shows "Brandon's server", with a real
last-seen, and a re-link from the same home upserts the same Device row instead of leaving a
trail of dead ones. This is ADR-0034's identity doing the job it was minted for, one hop out.

**5. Two dialers, no second identity.** From the home Server's point of view every origin is
one of two things:

- **A plain URL over the operating system's network.** This covers a port-forwarded host with
  ADR-0041's ACME certificate, a reverse proxy, and a Tailscale Funnel address alike — all three
  are "an HTTPS origin on the internet". No new code beyond an HTTP client.
- **A tailnet name dialed through the home Server's own Tailnet node** (ADR-0043). The sharer
  *shares their Obelo machine* with the home operator's Tailscale account from the Tailscale
  admin console — machine sharing is a Tailscale feature, done by invite link, and no API exists
  for it — and the machine then resolves on the home tailnet as
  `obelo.<sharer-tailnet>.ts.net`. The home Server's `tsnet` node dials it directly over
  WireGuard: no port-forward on either side, no bandwidth cap, and the shared machine is
  quarantined by Tailscale so it cannot initiate anything back. The one requirement is that both
  Servers have the tailnet feature compiled in and connected.

The home Server picks the dialer per origin: if its Tailnet node is up and the name resolves
there, `tsnet`; otherwise the OS. Nothing is configured.

**Rejected: a second `tsnet` identity joined to the sharer's tailnet with their auth key.**
It would make the home Server a full member of another household's network, and both operators
would have to trust ACLs to contain it. Machine sharing gives exactly the one connection wanted
and nothing else.

**Not attempted: putting a phone on two tailnets.** A Tailscale client can be signed into
several accounts but only one tailnet is active at a time, and switching drops every
connection. Topology (above) is what makes this not matter.

## What the sharer cannot see, and what they can

The sharer's Server learns the home Server's id and name (on the Device row), and its address
as the TCP peer of each request. It learns which Titles are played and how many at once. It
does not learn who in the home household exists, or who is watching (ADR-0054 §3), and it
never learns the home Server's other Links.

## Consequences

- New endpoints on the sharer: `POST /users/{id}/invite` (Admin) and `POST /auth/link/redeem`
  (unauthenticated, `serverLinking`-gated, rate-limited). A new `link_invites` table with the
  same columns and reaper as `device_codes`.
- New endpoints on the home Server: `POST /links` (Admin, body is the invite string),
  `GET /links`, `POST /links/{id}/rekey`, `DELETE /links/{id}`. The home side is the whole of
  ADR-0056.
- `GET /server` gains `serverLinking` in `features` and a top-level `linkProtocolVersion`. The
  route/feature parity test covers the flag.
- The invite QR is a convenience over the string, not the mechanism — the same posture
  ADR-0036 takes toward its QR. A phone camera can read it into the web UI today; the iPhone
  and iPad apps post the decoded string to the home Server's `POST /links`, which is the
  entirety of the client-side work. The Apple TV has no camera and no role here.
- `DELETE /links/{id}` sends a best-effort `POST /auth/logout` to the sharer before deleting,
  so the sharer's Device row disappears instead of lingering as a ghost with a last-seen.

## Non-goals

- Discovering other Servers. Two people who want to link already know each other.
- Automating the Tailscale machine-share. It is a console action on the sharer's side and the
  runbook says so.
- Funnel as a first-class option. It works today as a plain URL; its relay bandwidth cap makes
  it a poor carrier for video, and it stays a plain URL until that changes.
