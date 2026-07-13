# Runbook: rotate a leaked default metadata key

**When:** a bundled default TMDB or fanart.tv key is being abused against our quota
(scraped from a binary, leaked, whatever). This rotates it for **every install that
polls**, same-day, **without cutting a release**. Governing decision: ADR-0032.

**Precondition:** you have the base64 `kAppEncKey` (the app encryption key baked into
official builds) in your maintainer secret store, and `wrangler` is authenticated to
the Cloudflare account hosting the rotation Worker (`deploy/rotation-worker/`). If you
haven't created `kAppEncKey` yet, see below.

## Generating `kAppEncKey` (one-time setup)

`kAppEncKey` is a **32-byte AES-256 key, standard-base64-encoded** — nothing more.
The client requires exactly 32 decoded bytes (`internal/rotation/rotation.go`,
`newGCM`), so generate it with:

```sh
openssl rand -base64 32          # 44 chars, ends in '='
#   head -c 32 /dev/urandom | base64   # equivalent
```

It is **symmetric**: the same value both encrypts (maintainer side, `keytool`) and
decrypts (server side, in every official binary). So store the one value in **two**
places, and they must match exactly:

- **CI secret** `JUICEBOX_APP_ENC_KEY` — injected into official binaries via
  `-ldflags -X` (Make/Docker); verbatim, since it is already base64.
- **Your own vault** (password manager) — you pass it as `JUICEBOX_APP_ENC_KEY` when
  running `keytool` at rotation time (step 2 below).

Generate it **once** and keep it stable — it is empty in source and never committed
(`internal/config/bootstrap.go`). Regenerating it forces a release (see the asymmetry
section), so only do so on a deeper compromise, not for routine provider-key rotation.

## The three steps

### 1. Revoke on the provider dashboard

Kill the compromised key at its source first, so the abuse stops even for installs
that haven't polled yet:

- **TMDB:** Settings → API → revoke/regenerate the leaked key.
- **fanart.tv:** account → API keys → revoke the leaked project key.

Generate the replacement key(s) here; you'll seal them in step 2.

### 2. Encrypt the replacement (`cmd/keytool`)

Seal the new key(s) into the versioned envelope the server fetches. Supply
`kAppEncKey` via the env var (not a flag) so it stays out of your shell history:

```sh
JUICEBOX_APP_ENC_KEY=<base64-kAppEncKey> \
  go run ./cmd/keytool \
    -tmdb   <new-tmdb-key> \
    -fanart <new-fanart-key> \
    -o envelope.json
```

- Pass only the key(s) you're rotating; omit a flag to leave that provider's default
  unset in the payload. (Publishing an **all-empty** payload is refused — it would
  strip every install's default keys.)
- Every run uses a **fresh random nonce**, so re-running is safe and two envelopes
  over the same keys differ.
- `-min-app-version` (default `0.x` = any build) marks a payload only newer builds
  should adopt — use it only when a payload depends on a client change.

### 3. Publish to KV (`wrangler`)

```sh
cd deploy/rotation-worker
wrangler kv key put --binding=KEYS_KV envelope --path=../../envelope.json
rm ../../envelope.json      # clean up the sealed file
```

Installs pick up the new key on their **next poll** (startup + every N hours). No
release, no Docker re-pull. The client fails safe throughout: while KV is mid-update
or unreachable, servers fall through to their bootstrap key and log once — never a
crash, never a retry storm (`internal/rotation`, issue 03).

## The asymmetry: rotating `kAppEncKey` DOES need a release

The three steps above rotate a **provider key** — release-free, because the provider
keys live only in the encrypted payload the Worker serves.

`kAppEncKey` itself is different: it's baked into the binary via `-ldflags -X`
(`internal/config/bootstrap.go`). If **it** is compromised, you must:

1. Generate a new `kAppEncKey` (same command as one-time setup above), update the CI
   secret **and** your vault copy.
2. Cut a release (new official binaries + Docker image carry the new key).
3. Re-seal the current provider keys under the new `kAppEncKey` (step 2 above) and
   publish (step 3), so already-updated installs keep rotating.

Old installs keep using their old `kAppEncKey` until they update — the intentional
blast-radius bound (ADR-0032). This is why routine provider-key rotation is kept
release-free and only a deeper compromise costs a release.

## Verifying a rotation took

```sh
# The Worker serves the new envelope:
curl -H 'User-Agent: juicebox/verify' https://<rotation-host>/v1/keys

# A server picks it up: watch its log for the rotation-source credential line on the
# next poll, or trigger one on demand in a test build via Server.RefreshRotationKeys().
```
