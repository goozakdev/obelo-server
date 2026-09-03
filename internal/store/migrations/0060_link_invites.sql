-- 0060_link_invites: the one-time credential a linked Server redeems
-- (ADR-0055 §1, .scratch/linked-servers issue 03).
--
-- This is 0041_device_auth with the roles reversed and one secret instead of
-- two. There, a TV minted the code and a person approved it; here the SHARING
-- Admin mints it and another SERVER redeems it, so there is nobody to show a
-- short human code to and nothing to approve — the redeeming machine is the only
-- party that ever reads the string. That is why there is no `user_code` twin:
-- the invite carries a single 256-bit secret, and a secret nobody retypes has no
-- reason to be typable.
--
--   code_hash  — the invite's secret, SHA-256 like auth_tokens.token_hash and
--                device_auth_requests.device_code_hash (ADR-0015). The RAW code
--                exists only in the mint response and in the string the Admin
--                sends; a database leak yields nothing redeemable. PRIMARY KEY
--                because the redeem looks up by it and nothing else ever does.
--
--   user_id    — the `remote` User (ADR-0054) whose credential this becomes. ON
--                DELETE CASCADE, so deleting the linked Server — the sharer's
--                kill switch — takes its unspent invites with it rather than
--                leaving a code that would redeem into nothing.
--
--   redeemed_at — NULL until spent, then the moment it was. Single use is a
--                compare-and-swap on this column (store.RedeemLinkInvite), the
--                same shape device_auth_requests uses its `state` for: two
--                redemptions racing both run the UPDATE, exactly one affects a
--                row, and only that one mints a token.
--
-- A SPENT ROW IS KEPT until the sweeper reaps it, exactly as a redeemed device
-- code is. Deleting it on collection would make a second attempt read as "never
-- existed" — which is the same INVALID_INVITE answer a fresh guess gets, so
-- nothing leaks either way, but keeping it is what lets the row explain itself
-- to an operator reading the table.
--
-- There is no `origins` column and that is deliberate (ADR-0055 §2). The origins
-- are typed by the Admin at mint time and travel INSIDE the invite string; they
-- are the redeeming Server's business, not this one's, and this Server
-- deliberately never learns or stores its own public address (ADR-0005, the
-- retired External URL).
--
-- Timestamps are RFC3339-UTC written by Go, never SQLite's datetime('now') —
-- see 0041_device_auth.sql for why the two formats cannot be compared and why
-- expiry, which IS compared in SQL, forces the choice.

CREATE TABLE IF NOT EXISTS link_invites (
    code_hash   TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    redeemed_at TEXT
);

-- The sweeper deletes by expiry; minting a fresh invite invalidates the User's
-- unspent ones, which is the only lookup that is not by primary key.
CREATE INDEX IF NOT EXISTS idx_link_invites_expires ON link_invites(expires_at);
CREATE INDEX IF NOT EXISTS idx_link_invites_user ON link_invites(user_id);
