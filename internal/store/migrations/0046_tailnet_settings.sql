-- 0046_tailnet_settings: DB-backed Tailnet remote-access settings (ADR-0043),
-- following the metadata/subtitle settings singletons shipped in 0018 and 0027.
--
-- This is the FIRST piece of this server's *network* configuration to live in the
-- database rather than in the environment, and that is deliberate (ADR-0043): TLS
-- is boot-time because a listener appearing mid-flight is a surprise, whereas a
-- Tailnet that cannot be turned on without shell access to the box is a feature
-- nobody can use. The OBELO_TAILSCALE_* env vars seed this row on first boot and
-- are ignored at runtime thereafter; the admin UI is the source of truth.
--
-- The table is named for the DOMAIN concept, not the vendor: CONTEXT.md's term is
-- Tailnet (the operator's network), while "Tailscale" is a company whose
-- coordination server an operator may replace with their own Headscale via the
-- control_url column below. The operator-facing spellings — the OBELO_TAILSCALE_*
-- env vars and the /settings/tailscale routes — keep the vendor name, because
-- that is the word the operator went looking for.
--
--   enabled       — the DESIRED state, and the whole state machine's single input:
--                   connect/disconnect write it, and an enabled node CONNECTS AT
--                   BOOT, because a restart that strands the operator outside a
--                   house they cannot reach is the failure this feature exists to
--                   prevent.
--   hostname      — the MagicDNS label this Server joins under, seeded once from
--                   the Server's display name sanitized to a legal DNS label
--                   (falling back to 'obelo'). NOT derived live from the display
--                   name: renaming a Server is cosmetic (ADR-0034) and must never
--                   silently change the address a roaming client has stored.
--   control_url   — the coordination server, '' for Tailscale's own. The escape
--                   hatch that keeps ADR-0001 defensible (point it at Headscale).
--   https_enabled — tailnet :443 with a real certificate for the MagicDNS name.
--                   OFF by default because it additionally requires MagicDNS and
--                   HTTPS certificates to be enabled in the Tailscale console — a
--                   prerequisite outside this web UI, so defaulting it on would
--                   produce a failure the operator cannot act on from here.
--
-- There is deliberately NO auth-key column. A pre-authorized join key is read from
-- OBELO_TAILSCALE_AUTHKEY at join time and dropped: no long-lived Tailnet
-- credential is ever persisted in this database (ADR-0043), so wiping the data
-- directory can cost a re-authorization but can never leak the operator's Tailnet.
--
-- A missing row means "never seeded" — the first-boot signal SeedIfEmpty reads.
-- Every reader treats absence as the all-off default, so nothing requires the row
-- to exist and a server with this feature untouched never writes one goroutine's
-- worth of state.
CREATE TABLE IF NOT EXISTS tailnet_settings (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    enabled       INTEGER NOT NULL DEFAULT 0,
    hostname      TEXT    NOT NULL DEFAULT '',
    control_url   TEXT,
    https_enabled INTEGER NOT NULL DEFAULT 0,
    updated_at    TEXT    NOT NULL DEFAULT (datetime('now'))
);
