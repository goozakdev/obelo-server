# Metadata key-rotation Worker

The Cloudflare Worker that serves the sealed default-credential envelope for the
metadata key-rotation channel (ADR-0032). It is the **server half** of the same
contract `cmd/keytool` (the encrypt step) and `internal/rotation` (the client)
implement. Its only job is to hand the running server the latest ciphertext so a
leaked default TMDB/fanart.tv key can be **revoked and replaced without a release**.

Everything about this endpoint is optional and confidentiality-adding-nothing: the
value it serves is `nonce ‖ AES-256-GCM(kAppEncKey, {tmdb,fanart})`, useless without
the key baked into the official binary. See ADR-0032 for why a public URL serving
this is safe.

## What's in here

- `src/worker.js` — the Worker: `GET /v1/keys` → the KV-stored envelope. Bot-shed on
  the app `User-Agent`, per-IP rate limit, no request logging.
- `wrangler.toml` — deployment config (committed). The KV **value** it serves is
  **not** committed — it's published out-of-band by the runbook below.

## One-time setup

```sh
cd deploy/rotation-worker
wrangler kv namespace create KEYS_KV      # paste the returned id into wrangler.toml
wrangler deploy                           # publish the Worker
```

Point official builds at the deployed Worker by injecting the host at **build time**
(it is baked in via `-ldflags -X`, kept out of the repo — see
`internal/config/bootstrap.go`):

```sh
JUICEBOX_ROTATION_URL="https://<host>/v1/keys" make build   # or --build-arg for Docker
```

Operators can override the baked default at runtime with `JUICEBOX_KEY_ROTATION_URL`.

## Rotation runbook (revoke → encrypt → publish)

The full runbook, including the `kAppEncKey`-needs-a-release caveat, lives at
[`docs/runbooks/metadata-key-rotation.md`](../../docs/runbooks/metadata-key-rotation.md).
The publish step lands here:

```sh
# 2. seal the replacement keys (kAppEncKey from the maintainer's secret store)
JUICEBOX_APP_ENC_KEY=<base64-kAppEncKey> \
  go run ./cmd/keytool -tmdb <new-tmdb> -fanart <new-fanart> -o envelope.json

# 3. publish — installs pick it up on their next poll, no release
wrangler kv key put --binding=KEYS_KV envelope --path=envelope.json
rm envelope.json          # don't leave plaintext-adjacent material lying around
```

`envelope.json` is git-ignored here as a belt-and-suspenders guard.
