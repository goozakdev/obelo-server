-- 0069_plugin_signing_and_catalog: the two OPTIONAL, OFF-BY-DEFAULT trust and
-- discovery features of .scratch/plugin-system issue 15 — pinned publisher keys,
-- and a catalog URL the operator chose.
--
-- Both exist because ADR-0001 says this project runs no catalog and vouches for
-- no publisher. That posture does not mean an operator may not have either; it
-- means the SERVER ships with neither, and every row this migration creates is
-- one an Admin typed. A fresh install has no publishers pinned and no catalog
-- URL, and behaves exactly as it did before this migration existed.

-- The publisher keys an Admin has PINNED, by hand, on this server.
--
-- THE TABLE BEING EMPTY IS A POLICY, not an absence of one, and it is the default
-- policy: with no rows here nothing is verified and an install is refused for none
-- of the reasons signing can give. With one or more rows, every install must carry
-- a signature naming a publisher in this table and verifying under that
-- publisher's key. There is no middle setting and deliberately no "warn only" —
-- an operator who pinned a key did so to stop something, and a warning does not.
--
-- `publisher` is the PRIMARY KEY and it is the lookup: a signature names a
-- publisher, the host finds the row, and the row's key is the only key that
-- document may be checked under. Matching is case-insensitive, which is why the
-- column is COLLATE NOCASE — an operator who pinned "Example Publisher" and a
-- document that says "example publisher" mean the same publisher, and the
-- alternative is a refusal nobody can see the cause of.
--
-- `public_key` is a base64 ed25519 public key — 32 bytes, 44 characters. It is
-- PUBLIC: unlike every other credential-shaped column in this database it is
-- returned by the API in full, because an operator has to be able to read back
-- what they pinned and compare it against what a publisher advertises. A masked
-- public key would be a masked fact.
--
-- `key_id` is the short fingerprint (the first eight bytes of the key's SHA-256,
-- in hex) that a signature document also carries. It is DERIVED from public_key
-- and stored only so a screen need not recompute it; nothing verifies against it.
CREATE TABLE IF NOT EXISTS plugin_publishers (
    publisher  TEXT PRIMARY KEY COLLATE NOCASE,
    public_key TEXT NOT NULL,
    key_id     TEXT NOT NULL DEFAULT '',
    added_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Who signed an Installed plugin, recorded at install time — and ONLY when the
-- signature was verified against a pinned key.
--
-- An unverified claim is never written here, which is the whole point of the
-- columns: the Plugins screen reads them to say "signed by X", and a screen that
-- said so on the strength of a string in a file an author shipped would be worse
-- than one that said nothing. A plugin installed with no keys pinned therefore
-- has both columns empty even if a signature travelled with it — the signature
-- file is still stored beside the manifest, as provenance, and can be re-checked
-- by hand once a key is pinned.
--
-- It is a RECORD OF WHAT HAPPENED AT INSTALL and is never re-derived. Enabling,
-- disabling or re-enabling an already-installed plugin does not re-verify
-- anything (the files on disk are the ones this server already accepted), so
-- these columns keep saying what was true when the code arrived.
ALTER TABLE plugins ADD COLUMN publisher TEXT NOT NULL DEFAULT '';
ALTER TABLE plugins ADD COLUMN key_id TEXT NOT NULL DEFAULT '';

-- The catalog the operator chose, in the singleton-row shape ADR-0043's tailnet
-- settings established (one row, id = 1, guarded by the PRIMARY KEY, read
-- TOTALLY so a missing row is the shipped default rather than an error).
--
-- EMPTY IS THE DEFAULT AND EMPTY MEANS OFF. There is no bundled index, no
-- fallback address and no "official" catalog to fall back to — the URL in this
-- row is the whole of the trust decision, and it is one an Admin typed. With it
-- empty the Plugins screen has no Browse tab at all, and the server makes no
-- outbound request on its account.
--
-- The index this URL points at is DATA, not code, so it is fetched under the
-- ordinary safefetch policy rather than the stricter first-hop address check the
-- install path uses. That asymmetry is deliberate: an operator serving their own
-- index from a box on their LAN is a case worth supporting, and choosing a plugin
-- from a list is not the same act as executing it — installing the entry still
-- goes through the address-checked install path and is still refused if the
-- manifest URL resolves inside this server's own network.
CREATE TABLE IF NOT EXISTS plugin_catalog_settings (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    url        TEXT,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
