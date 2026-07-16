-- 0041_device_auth: the Device authorization grant — signing a TV in from a
-- phone (ADR-0036), so nobody types a password on a remote.
--
-- The flow has two secrets and they are not interchangeable:
--
--   device_code_hash — the POLL secret. 256 bits, generated and held by the TV,
--     never shown to a human. Stored only as a SHA-256 hash, exactly like
--     auth_tokens.token_hash (ADR-0015): a database leak must not yield a
--     pollable code. It is the PK because the poll looks up by it.
--
--   user_code — the HUMAN code. Four characters, read off the TV screen and
--     typed (or carried by the QR) into a phone. It is deliberately weak enough
--     to retype, which is only safe BECAUSE it is not the poll secret: guessing
--     one never yields a token, it only lets the guesser link a TV to their own
--     account. UNIQUE so a code names exactly one request.
--
-- The Device columns are the TV's self-description, captured when it starts the
-- flow rather than when it redeems, so the approving phone can name what it is
-- authorizing. They mirror devices(client_id, name, platform) but do NOT
-- reference it: no Device row exists yet, and may never (an unapproved request
-- must leave no trace in the Device list).

CREATE TABLE IF NOT EXISTS device_auth_requests (
    device_code_hash TEXT PRIMARY KEY,
    user_code        TEXT NOT NULL UNIQUE,

    -- The TV's Device descriptor, echoed back to the phone at approve time and
    -- used to mint the real Device row at redeem.
    client_id        TEXT NOT NULL,
    device_name      TEXT NOT NULL,
    device_platform  TEXT NOT NULL,

    -- pending -> approved -> redeemed. A redeemed row is KEPT until the sweeper
    -- reaps it, rather than deleted on collection: the state is what makes the
    -- code one-shot, and a deleted row would read as "never existed", which is
    -- the same answer a fresh guess gets. There is no 'denied' — approval is
    -- immediate on code entry, so there is no screen to refuse from (see
    -- auth/device_auth.go).
    state            TEXT NOT NULL DEFAULT 'pending'
                     CHECK (state IN ('pending', 'approved', 'redeemed')),

    -- Who approved it. NULL until then. This is the ONLY thing approval records:
    -- the token is minted at redeem, never here, so that a raw token never rests
    -- in a table waiting to be collected (ADR-0015 — only hashes are stored).
    approved_user_id TEXT REFERENCES users(id) ON DELETE CASCADE,

    -- Timestamps are RFC3339-UTC written by Go, NOT the datetime('now') default
    -- the neighbouring tables use. This is the first table whose rows expire, so
    -- it is the first to COMPARE a timestamp in SQL, and the two formats do not
    -- compare: 'T' (0x54) sorts after ' ' (0x20), so an RFC3339 expires_at is
    -- lexicographically greater than ANY same-day datetime('now') value and every
    -- row would read as unexpired. Both operands must be RFC3339, so both come
    -- from Go. api/time.go's formatTimestamp normalizes either shape on read,
    -- which is why the mixture is survivable everywhere that only displays them.
    created_at       TEXT NOT NULL,
    expires_at       TEXT NOT NULL,

    -- Last poll, for the RFC 8628 SLOW_DOWN rule. NULL until the first poll.
    last_polled_at   TEXT
);

-- The sweeper deletes by expiry; the approve path looks up by user_code (already
-- indexed by its UNIQUE constraint).
CREATE INDEX IF NOT EXISTS idx_device_auth_expires ON device_auth_requests(expires_at);
