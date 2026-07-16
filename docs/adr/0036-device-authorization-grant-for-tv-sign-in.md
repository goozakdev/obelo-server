# Signing a TV in from a phone — a Device authorization grant

A client that cannot comfortably accept typed input signs in by showing a short code and a QR of
it, while a second, already-authenticated Device approves that code. The signing-in Device never
receives a credential from the user; it polls until a session is handed to it.

The shape is [RFC 8628](https://datatracker.ietf.org/doc/html/rfc8628)'s device authorization
grant. This ADR records what we adopted, what we deliberately did not, and the one property the
whole design rests on.

## Why

Typing a password on a Siri Remote is the worst interaction in the Apple TV client, and it is the
*first* one a user meets. The on-screen keyboard is a single row of letters driven by a swipe
surface; a strong password can take a minute and several mistakes. Everything after it — browse,
play, resume — is good. The onboarding is what makes the product feel bad.

Every other option makes the server worse rather than the TV better:

- **Shorter/simpler passwords** — solving a UI problem by weakening the credential.
- **A "remember me" that never expires** — already true (tokens do not expire, ADR-0015); it does
  nothing for the *first* sign-in, which is the one that hurts.
- **An account on a third party** (Apple/Google sign-in) — contradicts ADR-0001; there is no
  vendor to depend on and no internet to require.
- **Typing the password into the web app and having the TV "find" it** — is this grant, minus the
  parts that make it safe.

## The property everything rests on

**There are two codes and only one is a secret.**

- **`deviceCode`** — 256 bits, minted per flow, held by the TV, shown to nobody, stored only as a
  SHA-256 hash (as auth tokens are, ADR-0015). It is the *only* thing that can collect a session.
- **`userCode`** — 4 characters, printed on screen and carried in the QR. It can do nothing on its
  own: presenting it requires a bearer token, because the approve endpoint is authenticated.

This is what makes a 4-character code defensible. 30^4 = 810,000 is indefensible for a
*credential*; it is fine for a code whose only power is "name a pending request to a caller who
has already proved who they are". Guessing a live user code does not yield a session. The most it
achieves is signing a stranger's TV into **the guesser's own account** — giving access away, not
taking it.

The corollary matters for support: **photographing the TV's sign-in screen accomplishes nothing**,
and neither does watching someone read the code aloud.

### What the short code does cost

- **Phishing.** Someone talked into approving an attacker's code signs the attacker's device into
  *their* account. No code length fixes this; it is the grant's inherent risk (RFC 8628 §5.3). It
  is blunted by a 5-minute TTL and by approval requiring physical possession of an authenticated
  phone. It is **not** eliminated, and we accept it — see "Approval is immediate" below, which
  removes the one control that would have blunted it further.
- **Enumeration.** Mitigated by a per-User rate limit on approve, the first rate limit in this
  server (there is none on `/auth/login` — a real gap, and not this ADR's to close).
- **Crowding.** 810k is small enough to flood, which would make code generation a retry loop and
  ultimately deny a real TV a code. Concurrent live requests are capped.

## Adopted from RFC 8628

The state machine: `code` → poll → `approve` → poll collects. The poll's pending/slow-down/expiry
answers, and the granted `interval` the client must obey.

## Deliberately not adopted

- **The wire spelling.** `authorization_pending` becomes `AUTHORIZATION_PENDING` in this API's
  error envelope. A client switching on `error.code` should not have to know that four of its
  values came from a different document than the rest. The state machine is the RFC's; the
  vocabulary is ours.
- **OAuth around it.** There is no authorization server, no scopes, no refresh token, no client
  registration. This grant issues exactly what `POST /auth/login` issues — the same opaque
  DB-backed token, in a byte-identical response — because it is a different way to *prove who you
  are*, not a different kind of session. A client therefore has one way to hold a session and two
  ways to obtain one.
- **`access_denied` / a deny operation.** See below.

## Approval is immediate — no confirmation step

Entering or scanning a code authorizes the TV with no further tap. RFC 8628 and every comparable
product (Netflix, YouTube) show a confirmation naming the device first, and that screen is the
main defense against phishing: it is where a user notices they are authorizing something they did
not start.

**We chose the faster path anyway**, on the judgement that a household LAN media server is not a
phishing target worth a tap on every sign-in. This is a product decision overriding the security
default, so it is recorded rather than assumed:

- It means the `denied` state and RFC 8628's `access_denied` do not exist here — with no
  confirmation screen, there is nothing to refuse *from*, and modelling a state nothing can reach
  would be dead code with a story attached.
- The recourse it removes is replaced by a better one that already existed: `DELETE /devices/{id}`
  revokes that Device's token instantly (ADR-0015). Noticing later and revoking beats noticing
  never.
- The approve response and the `/link` success screen **name the Device** ("Living Room TV is now
  signed in"). With no confirmation before, that line is the user's only chance to notice a
  mis-entered code — which is why it is required rather than decorative.

If a confirmation step is ever added, `access_denied` and the `denied` state come back with it.

## The verification URL is built from the request

`verificationUri` is derived from the inbound `Host` (honouring `X-Forwarded-Host`/`-Proto`), not
from config. The server cannot know its own address — only the one it was reached on — and that is
exactly the address the phone standing next to the TV needs. This is correct for all three real
deployments: a LAN IP, a hostname, and a reverse proxy on another origin (ADR-0005).

The known-bad case is loopback: a TV pointed at `127.0.0.1` (the simulator) produces a QR that
resolves to the *phone* when scanned. Not fixable here, and it does not arise on real hardware,
where the TV is a different host and must have used a routable address to reach us at all.

## The code is a path segment, not a query parameter

`/link/K7R9`, not `/link?code=K7R9`. The web app's login guard preserves only
`location.state.from.pathname` when it bounces an anonymous visitor and returns them, so a query
string would be silently dropped and the user asked to retype what they had just scanned. A path
segment survives the round trip. (That the guard drops `search` is a bug worth fixing on its own;
this design does not depend on it being fixed.)

## Consequences

- **A feature flag, `deviceAuth`.** Clients branch on it, never on a version. A server without
  these routes must send its clients to the password form, which every client keeps anyway —
  the manual path is permanent, not a fallback to be removed.
- **The first expiring rows in the database.** `auth_tokens` has no `expires_at` and nothing else
  ages out, so this feature brings the first TTL, the first sweeper, and the first SQL timestamp
  *comparison* — which is why its table stores RFC3339 written by Go rather than SQLite's
  `datetime('now')`. The two formats do not compare (`'T'` > `' '`), and a mixed table would read
  as never-expiring. See `migrations/0041_device_auth.sql`.
- **The first rate limit.** In memory, per boot, keyed by User — the same posture as the claim
  token (ADR-0013), for the same reason: it is safety state, not a record of anything.
- **Two unauthenticated endpoints.** `/auth/device/code` and `/auth/device/token` take no
  credential, which is the point: a device that could authenticate would not need this. What
  stands in for auth is the entropy of `deviceCode` and the cap on live requests.
- **The token is minted at redemption, never at approval.** Approval records only *who* approved.
  Minting at approve would park a raw, usable token in a table waiting to be collected, breaking
  ADR-0015's "only hashes are stored". Both sign-in paths share one `issueSession` so they cannot
  drift.
