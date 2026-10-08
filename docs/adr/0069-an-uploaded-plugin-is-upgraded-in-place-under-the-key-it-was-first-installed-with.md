# An uploaded Plugin is upgraded in place under the key it was first installed with

[ADR-0058](./0058-an-installed-plugin-is-a-wasm-guest-called-through-a-hand-rolled-abi-on-wazero.md)
refuses a Plugin package whose id an Installed plugin already has (`PLUGIN_DUPLICATE`), so the
only way to move to a newer version is to uninstall and install again, which loses the Admin's
settings, secrets and the Plugin's data. This ADR adds the **Plugin upgrade**: same id, strictly
higher version, replaced in place. It decides what counts as one, who may publish it, what
happens to settings, when the Admin must confirm, what an upgrade may never take away, how the
swap is made safe, and which install routes it applies to. The terms are in `CONTEXT.md`:
**Plugin upgrade**, **Plugin package**.

## Decisions

**1. An upgrade is the same id and a strictly higher semver version.** It replaces the
Installed plugin in place. The same version or a lower one is refused with a message that says
so (not the generic duplicate error). A downgrade is only an uninstall followed by an install.
Uploading a package over a Bundled plugin's id follows the same rule. Bundled plugins are
signed with an Obelo release key, whose private half is injected only in the release pipeline
(the pattern of the bootstrap keys in [ADR-0032](./0032-optional-maintainer-key-rotation-endpoint.md))
and whose public half is compiled into the binary. Boot records that key like any signed
install, and also for bundled-origin rows whose on-disk bytes verify under it. On a release
build any upload over a bundled-origin row must verify under the compiled-in Obelo key,
whether or not a key was recorded for that row (rows from earlier releases have none), so it
can only be an Obelo-signed official build and keeps its bundled origin. A release build fails
its release guard if the public key is compiled in but the Bundled modules are unsigned. The
boot re-assert replaces such a copy only with a strictly newer shipped version and never
downgrades it; going back is a downgrade, so an
uninstall and install. A development build without the release key ships Bundled plugins
unsigned, which then behave as "no recorded key": an upload upgrade of one is an ordinary
no-key upgrade, always previewed as author-unconfirmed, and makes the plugin Admin-origin,
which the boot re-assert leaves alone (ADR-0059).

**2. Trust on first install.** A signature document gains the publisher's public key (an
additive field, which `pluginsign` writes). Installing a signed Plugin, on any server, records
the signer's public key: the signature is verified against the document's own key when nothing
is pinned, and against the pinned key when one is. An upgrade of a Plugin with a recorded key
must verify under that recorded key, and a mismatch is refused. A Plugin installed unsigned, or
before this feature and so with no recorded key, may be upgraded by anything; the preview is
always shown in that case and warns that the author cannot be confirmed. A pinned-publisher
policy still applies on top. Switching publisher is an uninstall and a fresh install, and the
settings go with it. There is no key rotation in v1: a package signed by a different key than
the recorded one is refused. The refusal message names both publishers and both key ids, the recorded and the new, and says to
uninstall and install; it and the plugin author guide both state that there is no key rotation
in v1.

**3. Settings follow the manifest's keys.** A setting with the same key and type is kept,
secrets and the instance URL included. A setting whose type changed is dropped. A setting
whose key was removed is deleted. A new key takes the manifest default. The response and the
Admin UI list exactly what was dropped and why. Data the Plugin itself stored is kept as is.

**4. Two steps unless nothing changes and the author is confirmed.** The upload validates and stages the package
and returns a preview: old to new version, hosts added and removed, extension points added,
a socket grant added, and settings that would be dropped or deleted. The Admin confirms to apply it.
The staged package expires after about ten minutes. Any Admin may confirm a staged upgrade, and
the audit line records both who staged it and who confirmed it. The upgrade applies in one step,
returning a summary, only when nothing widens, no setting is dropped or deleted, no Extension
point is removed, and a recorded key verified. Otherwise, including every upgrade of a Plugin
with no recorded key, the preview is shown first.

**5. An upgrade never removes what other records depend on.** An upgrade that removes an
Extension point with dependent state is refused, with counts and "uninstall to remove it".
Sign-in provider: identities and Users. Online source provider: the Users' grants. Removing an
Extension point with no dependent state is allowed, is listed in the decision 4 preview, and
requires confirmation (it is never a one-step upgrade).

**6. The new version proves itself before the swap.** It must compile, instantiate and export
every call its manifest claims, or it is refused and the old version is untouched. The swap is
the existing rebuild-and-swap: calls in flight finish on the old module. Online sessions
continue, and the cached rows are cleared through `OnChange`. There is no automatic rollback;
the normal three-strike disable applies to the new version as to any. The old module and
manifest are deleted after a successful swap, and no previous version is kept.

**7. All three install routes follow the same rule.** Upload, pasted URL and catalog each
treat a known id with a higher version as an upgrade (decisions 2 to 6), a new id as an
install, and anything else as refused. The Plugins screen gains "Upload new version" for each
uploaded plugin, the general upload detects an upgrade, and the catalog shows "Update
available". Every upgrade writes an audit line: id, old to new version, publisher, who
staged, who confirmed, and the settings dropped or deleted. There are no automatic updates. The Bundled plugins'
boot-time replacement continues, except that it never downgrades an Obelo-signed upgraded copy.

## Why

A same-id package from a different author would inherit the secrets and settings the Admin
gave the first one. Recording the signer's key at first install (decision 2) is what makes
keeping them safe on every server, pinned or not, and switching publisher is exactly the case
where they should go. Where no key was recorded the author cannot be confirmed, so the Admin is
told so every time.

A silent downgrade could put a known-vulnerable build back, so it needs the deliberate
uninstall that makes the Admin see what is lost.

An upgrade can widen reach without anyone seeing it: a new host, a new Extension point, a
socket grant ([ADR-0064](./0064-sign-in-providers-may-open-sockets-to-where-the-operator-pointed-them.md)).
The preview exists so the Admin sees that before it is live, and stays out of the way when
nothing changed.

Removing a Sign-in provider deletes the Users it created
([ADR-0063](./0063-a-sign-in-provider-proves-who-someone-is-and-the-server-decides-what-that-is-worth.md)),
so an upgrade must never be the route to that. The same refusal protects Online source grants.

Rebuild-and-swap already exists (`internal/plugins/install.go:33-43`), so the new module can be
built and checked completely before the registry is published, which is what lets a failed
upgrade leave the old one untouched.

### Considered

- **Any version replaces.** Rejected: it allows a silent downgrade to a vulnerable build.
- **Pinning-only trust.** Rely on the pinned-publisher policy and nothing else. Rejected: on
  a server with no pin, which is the default, nothing would tie an upgrade to the publisher of
  the installed version.
- **Keep every stored value.** Rejected: a setting whose type changed cannot be read by the new
  version, and a removed key is dead data.
- **Always one step.** Rejected: an upgrade that widens reach would go live unseen.
- **Partial uninstall on a removed Extension point.** Remove the dependent state along with the
  point. Rejected: it deletes Users or grants as a side effect of an upgrade.
- **Keep the previous version for rollback.** Rejected for v1: the three-strike disable
  contains a bad version, and the Admin can upload the old one only via uninstall.
- **Refuse upgrading a Bundled plugin.** Rejected: the Admin would have no way to move a
  Bundled plugin ahead of the shipped version without losing its settings.
- **Allow upgrading a Bundled plugin with only a warning.** Rejected for release builds: with
  no key to verify against, anyone could replace a shipped provider that holds the Admin's API
  keys. It is what happens on a development build, where Bundled plugins are unsigned.
- **Key rotation by a statement signed by the old key.** Rejected for v1: more signature
  machinery than the case justifies, and a lost key could not use it anyway.
- **Admin re-trust in the UI.** Rejected for v1: it is the uninstall and install path with
  the settings kept, which is what the key check exists to prevent.
- **Upload only.** Rejected: pasted URL and catalog installs would keep the dead end.

## Consequences

- ADR-0058's refusal of a same-id install now applies only when the version is not higher
  (decision 1), and `checkDuplicate` in `internal/plugins/install.go` becomes an
  upgrade-or-refuse decision. A package claiming the id of a Built-in is still refused, and so
  is an upgrade signed by a different key than the recorded one.
- The signature document format gains the publisher's public key, and the server gains a new
  piece of stored state, the recorded key of each Installed plugin.
- Bundled plugins keep ADR-0059's boot-time replacement. An uploaded same-id Plugin is still
  left alone by it; on a release build an upload-upgraded Bundled plugin stays bundled and is only ever replaced by
  a strictly newer shipped version, and on a development build it becomes Admin-origin
  (decision 1).
- A staged package expires after about ten minutes (decision 4).
- `CONTEXT.md` gains **Plugin upgrade** and an updated **Plugin package**.

## Back-pointers

[ADR-0058](./0058-an-installed-plugin-is-a-wasm-guest-called-through-a-hand-rolled-abi-on-wazero.md)
(a same-id install is no longer always refused) and
[ADR-0059](./0059-the-shipped-metadata-providers-are-bundled-plugins.md) (how an uploaded
same-id Plugin sits beside boot-time replacement) each carry a pointer back here.
