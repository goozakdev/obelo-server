-- 0062_links: this Server's standing relationships with other households'
-- Servers (ADR-0055, ADR-0056 §6, .scratch/linked-servers issue 06).
--
-- This is the HOME side of linking, and it is the mirror image of 0060: there,
-- a sharing Server stored a code it would hand out; here, a receiving Server
-- stores the token that code was spent for. A Link is one direction — the
-- Server holding this row browses and plays, the Server it points at shares —
-- so two households sharing both ways have two of these, one on each machine.
--
--   id            — this Server's own id for the Link, the one the /links routes
--                   address. NOT the other Server's id: an operator may unlink
--                   and re-link, and the row identity that the web UI holds
--                   should not be the peer's to change.
--
--   server_id     — the OTHER Server's identity (ADR-0034), carried in the
--                   invite. UNIQUE, and that uniqueness is the whole re-key
--                   story (ADR-0055 §2): a fresh invite from the same Server
--                   updates this row in place rather than creating a second
--                   Link, so a changed origin never orphans the mirror.
--
--   server_name   — the peer's display name, refreshed from its handshake on
--                   every (re-)link. Human-facing and freely changeable over
--                   there; nothing keys on it.
--
--   origins       — the addresses the sharing Admin typed, as a JSON array, IN
--                   ORDER. The order is meaningful (ADR-0055 §2) so it is stored
--                   as an array and not a set, and it is one column rather than a
--                   child table because nothing ever queries an origin — the only
--                   read is "all of them, in order, for this Link".
--
--   active_origin — the one that answered. Remembered so the next sync starts
--                   where the last one succeeded instead of walking the list
--                   again, and shown on the Linked servers page so an operator
--                   can see WHICH path is carrying their films.
--
--   token         — the ordinary Device-bound bearer the invite was redeemed for
--                   (ADR-0055 §1). It is an OUTBOUND credential: this Server
--                   presents it to somebody else, so unlike auth_tokens.token_hash
--                   it cannot be stored as a digest — a hash cannot be sent. It is
--                   therefore held exactly as the metadata- and subtitle-provider
--                   API keys are (0018, 0027): plaintext in this database, NEVER
--                   returned by any API, and protected by the same thing that
--                   protects those — the file permissions on the data directory.
--                   Inventing a second posture for it (an encryption key that
--                   would have to live beside the database it protects) would buy
--                   no real secrecy and would put this one credential outside the
--                   story the operator already has for the others.
--
--   device_id     — the id of the Device this Link IS over there (ADR-0055 §4:
--                   the redeeming Server presents itself as one). Recorded for
--                   the unlink: ADR-0055 requires that unlinking leave no ghost
--                   Device with a last-seen on the sharer, and POST /auth/logout
--                   deletes a TOKEN, not a Device — so the courtesy call is
--                   DELETE /devices/{this}, which cascades to the token and
--                   removes both. Empty on a Link made against a Server too old
--                   to report one, where the flow falls back to the logout.
--
--   link_protocol_version — what the two sides agreed on at link time
--                   (ADR-0055 §3), recorded so a later bump is a visible
--                   disagreement rather than a silent misread of the export.
--
--   state         — connected | unreachable | revoked (ADR-0056 §6). A CHECK
--                   constraint rather than a convention, because the admin page
--                   BRANCHES on it and a typo'd value is a dead branch, not an
--                   error. Note what is NOT here: "deleted". Only an explicit
--                   DELETE /links/{id} removes a Link, and the mirror survives
--                   both of the other two states.
--
--   last_synced_at / last_error — the mirror's bookkeeping, written by the sync
--                   (issues 07/08). Empty on a Link that has only just been made.
--
-- Timestamps are RFC3339-UTC strings written by Go, never datetime('now') — the
-- comparison bug 0041_device_auth.sql records.
--
-- There is deliberately no ON DELETE cascade FROM here to anything: the linked
-- Libraries this Link will own (issue 07) do not exist yet, and wiring a
-- constraint to a table that has not been designed would fix its shape now.

CREATE TABLE IF NOT EXISTS links (
    id                    TEXT PRIMARY KEY,
    server_id             TEXT NOT NULL UNIQUE,
    server_name           TEXT NOT NULL,
    origins               TEXT NOT NULL,
    active_origin         TEXT NOT NULL DEFAULT '',
    token                 TEXT NOT NULL,
    device_id             TEXT NOT NULL DEFAULT '',
    link_protocol_version INTEGER NOT NULL,
    state                 TEXT NOT NULL DEFAULT 'connected'
                          CHECK (state IN ('connected', 'unreachable', 'revoked')),
    last_synced_at        TEXT NOT NULL DEFAULT '',
    last_error            TEXT NOT NULL DEFAULT '',
    created_at            TEXT NOT NULL
);
