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

## First-time setup (Cloudflare from scratch)

Start-to-finish for someone who has only created a Cloudflare account. The **free
plan covers all of it.** The Worker treats the rate limiter as optional
(`if (env.RATE_LIMITER)`), so you can deploy without it first and harden after.

Cloudflare dashboard labels and `wrangler` flags drift over time — if a command's
output differs from what's below, check the current Cloudflare docs. (Newer wrangler
uses `wrangler kv namespace create`; older versions used a colon:
`wrangler kv:namespace create`.)

### 1. Install the CLI and log in

```sh
npm install -g wrangler        # or prefix every command with `npx`
wrangler login                 # opens a browser to authorize your account
```

### 2. Create the KV namespace

KV holds the one sealed envelope. Run from this directory so it reads `wrangler.toml`:

```sh
cd deploy/rotation-worker
wrangler kv namespace create KEYS_KV
```

Copy the printed `id = "…"` into `wrangler.toml`, replacing
`REPLACE_WITH_KV_NAMESPACE_ID`.

### 3. First deploy (skip rate limiting for now)

Temporarily comment out the `[[unsafe.bindings]]` rate-limit block in `wrangler.toml`
to avoid a first-timer snag, then:

```sh
wrangler deploy
```

It prints your live URL, e.g. `https://juicebox-key-rotation.<subdomain>.workers.dev`.
Your rotation endpoint is that URL **+ `/v1/keys`**. It returns `503 no envelope
published` until step 5 — that's correct.

### 4. Generate `kAppEncKey` (once)

```sh
openssl rand -base64 32        # store in your vault AND as the CI secret JUICEBOX_APP_ENC_KEY
```

This is the symmetric key `keytool` encrypts with and the binary decrypts with — see
[the runbook](../../docs/runbooks/metadata-key-rotation.md#generating-kappenckey-one-time-setup).

### 5. Seal and publish the first envelope

Use the TMDB **v3 API Key** (the `?api_key=` string, not the v4 Read Access Token):

```sh
# from the repo root
JUICEBOX_APP_ENC_KEY="<base64 key from step 4>" \
  go run ./cmd/keytool -tmdb "<tmdb-v3-key>" -fanart "<fanart-key>" -o envelope.json
cd deploy/rotation-worker
wrangler kv key put --binding=KEYS_KV envelope --path=../../envelope.json
rm ../../envelope.json
```

### 6. Verify

```sh
curl -H 'User-Agent: juicebox/verify' https://juicebox-key-rotation.<subdomain>.workers.dev/v1/keys
```

Returns the `{"v":1,…,"payload":"…"}` JSON. Without the `juicebox/` User-Agent you get
a 404 — the bot-shed working.

### 7. Point official builds at it

Inject the host at **build time** so it stays out of the repo (baked via `-ldflags -X`,
see `internal/config/bootstrap.go`); in CI these become secrets:

```sh
JUICEBOX_ROTATION_URL="https://juicebox-key-rotation.<subdomain>.workers.dev/v1/keys" \
JUICEBOX_APP_ENC_KEY="<same base64 key>" \
JUICEBOX_BOOTSTRAP_TMDB_KEY="<tmdb key>" \
JUICEBOX_BOOTSTRAP_FANART_KEY="<fanart key>" \
  make build                   # or --build-arg for Docker
```

Operators can override the baked default at runtime with `JUICEBOX_KEY_ROTATION_URL`.

### 8. Add rate limiting back (optional)

Un-comment `[[unsafe.bindings]]` and `wrangler deploy` again. If it errors on your
plan/wrangler version, leave it off — the Worker runs fine without it, and you can add
a rule in the dashboard instead (Security → WAF → Rate limiting rules).

### Custom domain (optional)

The `*.workers.dev` URL works immediately. To serve from `keys.yourdomain.com`, add the
domain to Cloudflare first (Websites → Add site), then attach a route (uncomment
`routes` in `wrangler.toml` or set it in the dashboard).

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
