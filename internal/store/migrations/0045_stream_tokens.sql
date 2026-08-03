-- 0045_stream_tokens: the session-scoped, expiring media credential for players
-- that hand a URL to somebody else (.scratch/session-stream-tokens).
--
-- The motivating case is AirPlay video: AVFoundation does not send pixels to the
-- receiver, it sends the URL, and the receiver — somebody else's television —
-- fetches the media itself. It cannot be made to set an Authorization header, and
-- whether it forwards the ms_media cookie is a property of firmware we do not
-- own. So the credential has to travel IN the URL, which means it must be a
-- credential we are willing to see written to an access log on a device we do not
-- control.
--
-- That is what this table is for, and it is why a stream token is emphatically
-- NOT an auth_tokens row:
--
--   auth_tokens  — the account credential. Never expires (no expires_at column,
--                  nothing reads created_at), authorises every mutation in the
--                  API. Handing one to a television would make
--                  requireAuthAllowQueryToken's "scoped to a single read-only
--                  GET" comment a lie by extension.
--
--   stream_tokens — one Playback session's media artifacts, read-only, expiring,
--                  and revoked when the session ends. Its loss is the disclosure
--                  of one film for the life of one session, to someone already on
--                  the LAN. The two namespaces never overlap: a stream token is
--                  looked up only here and an auth token only there, so neither
--                  authenticates on the other's surface.
--
-- Shaped after device_auth_requests (0041, ADR-0036), this server's other
-- expiring row, down to the details that matter:
--
--   token_hash — the secret is 256 bits of CSPRNG in URL-safe base64 (auth's
--                newToken, the same generator as deviceCode), stored ONLY as a
--                SHA-256 hash exactly like auth_tokens.token_hash (ADR-0015). A
--                database leak must not yield a replayable URL. It is the PK
--                because every lookup is by it.
--
--   session_id — the ONE session this authorises. Deliberately NOT a foreign key:
--                Playback sessions live in memory in the playback Manager and are
--                never persisted (they do not survive a restart), so there is no
--                table to reference. The cascade is therefore explicit — see
--                DeleteStreamTokensForSession, driven by the session-ended
--                observer that both DELETE /sessions/{id} and the idle reaper
--                fire.
--
--   user_id    — for access filtering on the token path (the bytes served must be
--                the bytes that User may see) and for auditability. It DOES
--                reference users, cascading, so deleting a User revokes their
--                outstanding stream tokens with everything else of theirs.
--
-- Timestamps are RFC3339-UTC written by Go, never SQLite's datetime('now'), for
-- the reason 0041 spells out at length: expiry is compared IN SQL and the two
-- formats do not compare ('T' sorts after ' ', so every row would read as
-- unexpired). Both operands come from Go.

CREATE TABLE IF NOT EXISTS stream_tokens (
    token_hash TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

-- The session cascade deletes by session_id; the sweeper deletes by expiry.
CREATE INDEX IF NOT EXISTS idx_stream_tokens_session ON stream_tokens(session_id);
CREATE INDEX IF NOT EXISTS idx_stream_tokens_expires ON stream_tokens(expires_at);
