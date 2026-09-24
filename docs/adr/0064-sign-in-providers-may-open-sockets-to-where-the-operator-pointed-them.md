# Sign-in providers may open sockets to where the operator pointed them

[ADR-0058](./0058-an-installed-plugin-is-a-wasm-guest-called-through-a-hand-rolled-abi-on-wazero.md)
decision 5 closed the host-function set at four (later five) and named the boundary
explicitly: "no raw sockets, no host function that returns a handle." An LDAP-style Sign-in
provider ([ADR-0063](./0063-a-sign-in-provider-proves-who-someone-is-and-the-server-decides-what-that-is-worth.md))
cannot be built on that boundary — LDAP is not HTTP, and `http_fetch` cannot reach a
directory. This ADR grants a narrow, purpose-built exception.

## Decisions

**1. A new host function opens a raw TCP connection, granted only to a Plugin providing a
Sign-in provider.** No other Extension point may request it; the grant is checked against
what the manifest declares it provides, the same place every other capability grant is
checked (ADR-0057 decision 3's "the host owns every judgment," applied here to a wire, not
a wire's content).

**2. The target is the host:port the operator typed into that Plugin's settings, and
nothing else.** Never a manifest-declared host, never a target the guest names at call time.
The one address a Sign-in provider Plugin may reach is the one an Admin wrote down for it,
exactly as `network.hosts` bounds `http_fetch` to a manifest's own list — here the equivalent
list has exactly one entry, set by the operator instead of the author, because nobody but
the operator knows where their own directory lives.

**3. TLS is on by default, terminated and certificate-verified by the host.** The connection
is upgraded either by dialing straight into implicit TLS or by an in-place StartTLS
negotiation, and the host does the verifying — a guest never sees a raw negotiated TLS
session or holds a private key. An Admin may opt a Plugin out of TLS per-plugin, shown with
a warning ("allow unencrypted connection (plaintext passwords)"), and may set a per-plugin
trusted-CA. The channel this function opens carries plaintext passwords in the LDAP bind
case, which is exactly why TLS defaults on and is the host's job, not left to a guest's own
(absent) TLS stack.

**4. A connection is a handle valid only within the call that opened it, closed at that
call's deadline, and never persists across calls.** This is the one narrow amendment to
ADR-0058 decision 5's "no host function that returns a handle": the handle exists, but it
cannot outlive the call, which is the property that rule existed to protect. A handle's
lifetime is bounded by the call that created it, full stop — no connection pooling across
guest invocations.

## Why

`http_fetch` is deliberately the only way out of the sandbox for every other Extension
point, and that stays true for a plain metadata or subtitle lookup. LDAP simply is not an
HTTP protocol, so a Sign-in provider that needs to bind against a directory needs a
different kind of exit, not a wider version of the same one.

### Considered

- **LDAP as a Built-in.** The orchestrator's recommendation, and the safer shape: no new host
  function, no sandbox exception. Rejected by the maintainer, who wants LDAP to remain an
  Installed plugin like any other Sign-in provider rather than special-cased compiled-in
  code.
- **An `ldap_bind` host function that speaks the protocol itself.** Rejected: it would move
  LDAP's wire format and error handling into the host, which is exactly the kind of
  protocol-specific logic ADR-0058's "request-response and JSON-shaped, like the contract
  itself" was written to keep out.
- **Dropping LDAP entirely, avoiding the need for this exception altogether.** Rejected: the
  maintainer wants LDAP available as an ordinary Installed plugin like any other Sign-in
  provider, which this ADR's narrow socket grant exists to make possible.

## Consequences

- ADR-0058 decision 5's "no raw sockets" and "no host function that returns a handle" are
  each amended, narrowly, by this ADR: a raw socket now exists, gated to one Extension point
  and one operator-typed target, and a handle now exists, gated to one call's lifetime.
  Every other Extension point, and every other target a Sign-in provider might imagine, is
  unaffected.
- The socket host function joins `http_fetch`, `log`, `kv_get`/`kv_set`/`kv_delete` and
  `settings_get` as the sixth host function, closed the same way the other five are: by
  Extension point, checked host-side, never guest-configurable beyond what the operator
  wrote into settings.
- An audit line records a refused socket attempt the same way a refused `http_fetch` host
  does, for the same reason: a Plugin reaching somewhere the operator did not point it is an
  event worth seeing.
