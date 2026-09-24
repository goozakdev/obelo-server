# A Sign-in provider proves who someone is, and the Server decides what that is worth

[ADR-0057](./0057-plugins-implement-a-closed-set-of-extension-points-through-a-wire-shaped-contract.md)
decision 1 named authentication as a trust-model question for its own ADR, not a loader
question. This is that ADR: a fourth Extension point, **Sign-in provider**, that lets an
operator delegate credential-checking to a directory or an identity provider **they
control** — their own LDAP, their own Authentik — without delegating identity itself. The
Server still decides who exists, what role they hold, and whether a session is minted.

Two further Extension points, **Web reference provider** and **Lyric provider**, join the
Plugin set by this bucket alongside Sign-in provider and Marker provider ([ADR-0065](./0065-markers-are-local-detected-or-fetched-and-detection-is-a-core-feature.md)),
without ADRs of their own: both fit the existing contract — wire-shaped, host-owns-judgment
— unchanged. Their host-owned judgments are narrower than a Sign-in provider's: a Web
reference provider's answer is trusted only over `https` and only for an id the Server
already holds, and is shown to every role, Members included; a Lyric provider ranks below a
Local one, except it is still asked when the Local lyric is Plain-only, and a Fetched Synced
answer then wins over that Plain one — a Fetched answer whose stated duration mismatches the
track's is kept only as Plain, not rejected outright.

## Decisions

**1. One Extension point, two flows.** *Password* — the Plugin checks a username and
password it is handed. *Redirect* — the Server sends the browser away and the Plugin turns
what comes back into an identity. A Plugin may implement either or both; OIDC ships as a
**Bundled plugin** implementing redirect.

**2. The redirect contract is entirely host-owned.** The Server owns the callback route,
`state`, and PKCE, and the Plugin supplies only an authorize URL and an
`exchange(code, verifier) -> identity` call. When that exchange also returns a raw ID
token, the Server verifies its signature against JWKS fetched from the operator-typed
issuer, checks `iss`, `aud` (the client id), `nonce`, and `exp`, and takes the subject and
groups from that verified token, never from the Plugin's own say-so. A non-OIDC OAuth2
provider that issues no ID token is anchored only by `state` and PKCE, so the Server accepts
the Plugin's identity claim as given, and the Admin UI marks that provider "identity not
independently verified." No Plugin ever serves a route of its own.

**3. Identity is keyed by (Plugin id, provider subject), never by username.** A username is
chosen by the person, at a source the Server does not control, and reusing it as a key is
an account-takeover vector: two different people can hold the same one at different times,
or a hostile Plugin can hand back a username that already exists here. The keyed pairing is
called an **External identity**. A subject seen for the first time becomes a new Member
granted nothing — not a role, not a library, not a merge into anything that already exists.
An existing User gains an External identity only by attaching it explicitly while signed in
as themselves.

**4. Groups map to a role and library grants, synced at every sign-in and re-checked
periodically, and the Server never stores a password to do it.** An Admin's **Group
mapping** turns a Sign-in provider's groups into a role plus grants, re-applied on every
sign-in and again on a periodic re-check (24h default, per-plugin configurable) via refresh
token or a plugin `lookup(subject)` — the Server stores the refresh token needed to re-check,
but never a password. A Plugin that declares no `lookup` falls back to syncing at sign-in
only, plus an Admin "re-sync now" action to stand in for the periodic re-check it cannot
otherwise get. A provider saying the identity is gone or disabled revokes all of that User's
sessions, not merely the ones opened through that identity; a provider that is merely
unreachable keeps the last known state and retries — the two are not the same failure and
must not be treated as one. A re-check whose ID token fails decision 2's verification is
treated as unreachable — state kept, retried — for up to 3 consecutive failures; the third
consecutive failure revokes that User's sessions exactly as gone/disabled would, and a
success in between resets the count. The provider is flagged on the Admin page from the
first failure, and every failure is recorded as an audit line.

**5. A Local password exempts a User from every Group mapping, and the Server refuses to
lose its last one.** A User with a **Local password** signs in with the network unreachable
and every Sign-in provider unreachable, and a mapping never touches them. The Server refuses any change that
would leave zero local-password Admins; the claim-token Admin is the break-glass for a
server with none at all.

**6. The password flow tries the unchanged login form, in order.** Local password first,
then each enabled password-flow Sign-in provider in Admin-set order; first accept wins. All
refusals — a wrong Local password, every enabled provider rejecting, or an unknown username
(no Local match and no provider accepts) — take one indistinguishable path, exactly as a
wrong password does today. An accepted outside answer resolves by External identity, never
by the username on the form.

**7. A new External identity that collides with an existing username is refused, not
merged.** This can only happen once a provider has accepted the credentials, so unlike
decision 6's refusals it gets its own message: the person is directed to attach it from
their profile once signed in, or to ask an Admin. No suffix, no auto-link, no queue: a name
collision is exactly the moment a computer must not guess.

**8. The redirect flow is web-only.** A TV or an iPad reaches outside-identity sign-in
through the device authorization grant ([ADR-0036](./0036-device-authorization-grant-for-tv-sign-in.md)),
approved from a web browser. A native redirect on those clients is deferred to a client-only
change later.

**9. Auto-disable behaves as unreachable, never as revoke.** A Sign-in provider that the
Server automatically disables after repeated failures leaves every session it granted
intact and flagged, on the same reasoning as decision 4's "unreachable": a failing Plugin is
not evidence a person should be locked out.

**10. Uninstalling a Sign-in provider deletes what it left behind.** Every External identity
it issued is deleted and every session belonging to a User with no *other* sign-in path is
revoked. A User left with no sign-in path at all — no Local password, no remaining External
identity — is deleted outright; the uninstall confirmation lists them by name before it
proceeds. Their watch state is lost with them.

## Why

ADR-0001's objection was to services the operator does not control — a cloud IdP the
maintainer would have to trust on the operator's behalf. An operator's own directory or
their own Authentik instance is a service *they* control, so the principle stands even
though its literal "no delegating to an external IdP" consequence, in ADR-0001's own
Consequences, does not survive unchanged. Keying by username was rejected because it hands account takeover to whoever
controls the outside directory's naming. The Local password exemption exists because
ADR-0001 also requires the Server to function fully offline — a household must never be
locked out of its own front door by a directory that is down or was never configured this
week. Opaque, DB-backed tokens ([ADR-0015](./0015-opaque-db-backed-tokens.md)) exist
precisely so revocation is instant; the periodic re-check exists to use that property, not
to duplicate it. Owning the redirect flow host-side keeps ADR-0057 decision 3 — the host
owns every judgment — true for a fourth Extension point, and it means a Plugin never serves
a route the Server would otherwise have to trust blindly.

### Considered

- **Credential-check (password flow) only, no redirect/OIDC.** The orchestrator's
  recommendation, overridden: the maintainer chose to support both flows.
- **No Group mapping, host-only roles.** The orchestrator's recommendation. Rejected: the
  maintainer wants a directory's groups to actually govern who can do what here, not merely
  authenticate.
- **Uninstall behaving like auto-disable, keeping identities.** The orchestrator's
  recommendation, overridden. Rejected: uninstalling a Plugin is meant to evict what it
  granted, not quietly leave orphaned access behind.
- **An Admin one-time "attach on behalf" to rescue a User stranded by uninstall.** The
  orchestrator's recommendation, overridden. Rejected in favor of deletion. The consequence
  is real and is recorded here rather than hidden: a User's watch state is lost the moment
  the Sign-in provider that was their only path is uninstalled, and there is no rescue
  mechanism.

## Consequences

- CONTEXT.md gains **Sign-in provider**, **External identity**, **Group mapping**, **Local
  password**, and widens **Extension point** and **Plugin** to include it (already reflected
  in CONTEXT.md by this bucket).
- ADR-0057 decision 1's closed set grows by this ADR to include Sign-in provider (this
  document), Web reference provider and Lyric provider (both above), and Marker provider
  (ADR-0065); its "authentication is not one" sentence no longer holds and is superseded in
  part.
- ADR-0001's Consequences bullet "no delegating to an external IdP" is superseded in part:
  the Server still owns identity, session issuance and authorization outright, and delegates
  only credential-checking, and only to a source the operator points at themselves.
- Uninstalling a Sign-in provider is now a destructive operation on Users, not merely on a
  Plugin's own settings, and the confirmation UI must say so plainly.
