// Cloudflare Worker — the SERVER half of the metadata key-rotation channel
// (ADR-0032). It serves one thing: the sealed rotation envelope that the offline
// `cmd/keytool` produced and the maintainer published into KV. The value is
// ciphertext (nonce ‖ AES-256-GCM under kAppEncKey), so this endpoint's public URL
// being known adds no exposure — a scraper that fetches it gets a payload useless
// without the key baked into the official binary.
//
// It is deliberately tiny and stateless. Hardening (ADR-0032) is bot-shedding and
// abuse-throttling, NOT a security control: the confidentiality lives in the
// ciphertext, not here.
//   - App User-Agent filter: sheds dumb crawlers. Trivially spoofed — politeness,
//     not security.
//   - Per-IP rate limit (Cloudflare native binding): throttles abuse.
//   - No request logging: `[observability] enabled = false` in wrangler.toml keeps
//     us from accumulating operator IPs (respect for a self-hosting audience).

const ROUTE = "/v1/keys";
const KV_KEY = "envelope"; // the single KV entry the runbook publishes via `wrangler kv key put`
const UA_PREFIX = "juicebox/"; // official builds send `User-Agent: juicebox/<version>`

export default {
  async fetch(request, env) {
    const url = new URL(request.url);

    // Only GET /v1/keys exists. Everything else is a flat 404 — no hints, no logs.
    if (request.method !== "GET") {
      return new Response("method not allowed\n", { status: 405 });
    }
    if (url.pathname !== ROUTE) {
      return new Response("not found\n", { status: 404 });
    }

    // Bot-shedding on the app User-Agent. A crawler without it gets a 404 (not a
    // 403 — we don't confirm the route exists). Trivially spoofed on purpose: this
    // is not the security boundary, the ciphertext is.
    const ua = request.headers.get("User-Agent") || "";
    if (!ua.startsWith(UA_PREFIX)) {
      return new Response("not found\n", { status: 404 });
    }

    // Per-IP rate limit via the native Ratelimit binding (configured in
    // wrangler.toml). Absent in `wrangler dev` without the binding, so guard it.
    if (env.RATE_LIMITER) {
      const ip = request.headers.get("CF-Connecting-IP") || "unknown";
      const { success } = await env.RATE_LIMITER.limit({ key: ip });
      if (!success) {
        return new Response("rate limited\n", { status: 429 });
      }
    }

    // Serve the published envelope verbatim. A missing value (never published, or
    // mid-rotation) is a 503 the client treats like any unreachable endpoint: it
    // falls through to the bootstrap key and logs once. Never a crash on either end.
    const envelope = await env.KEYS_KV.get(KV_KEY);
    if (!envelope) {
      return new Response("no envelope published\n", { status: 503 });
    }

    return new Response(envelope, {
      status: 200,
      headers: {
        "Content-Type": "application/json",
        // Let Cloudflare's edge absorb polling load; the client polls every N hours
        // regardless, so a few minutes of edge caching is free abuse-dampening.
        "Cache-Control": "public, max-age=300",
      },
    });
  },
};
