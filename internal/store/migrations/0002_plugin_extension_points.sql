-- The schema the plugin extension points need: Markers, Lyrics, Sign-in
-- providers and the Group mappings they answer to. 0001 is frozen; this file
-- takes a database built from it to the same schema a fresh one ends with.

-- ===========================================================================
-- users (ADR-0063)
-- ===========================================================================

-- Two columns and a wider CHECK: a User minted by a Sign-in provider may lack a
-- password, and so may one a Group mapping made an Admin. The columns are added
-- the ordinary way. The CHECK cannot be altered in place, and rebuilding users
-- would need foreign keys off — which cannot change inside the transaction a
-- migration runs in — or every session, device, watch state and per-User row
-- would cascade away with the old table. So the stored definition is rewritten
-- instead, SQLite's documented procedure for a change that leaves the stored
-- rows valid as they are: the new CHECK admits every row the old one did, and
-- the text written is exactly the CREATE TABLE a fresh database gets. ADD COLUMN
-- has already bumped the schema version, so every other connection re-reads the
-- schema; RESET makes this one re-read it too.
ALTER TABLE users ADD COLUMN external_origin INTEGER NOT NULL DEFAULT 0 CHECK (external_origin IN (0, 1));
ALTER TABLE users ADD COLUMN role_mapped INTEGER NOT NULL DEFAULT 0 CHECK (role_mapped IN (0, 1));

PRAGMA writable_schema = ON;
UPDATE sqlite_schema SET sql = 'CREATE TABLE users (
    id             TEXT PRIMARY KEY,
    username       TEXT NOT NULL UNIQUE,
    -- role is a vocabulary, not a flag, so every guard reads as one field
    -- ("role == remote", "role == admin") instead of a combination of flags.
    -- ''remote'' names a linked Server (ADR-0054): the counterpart Server''s own
    -- users are never rows here, only the Link itself is.
    role           TEXT NOT NULL DEFAULT ''admin'' CHECK (role IN (''admin'', ''member'', ''remote'')),
    password_hash  TEXT,
    -- Family/device controls: NULL means "no restriction for this User".
    rating_ceiling TEXT,
    max_resolution TEXT,
    max_bitrate    INTEGER,
    max_streams    INTEGER,
    created_at     TEXT NOT NULL DEFAULT (datetime(''now'')),
    -- external_origin is 1 for a User minted by a Sign-in provider the first
    -- time it vouched for an External identity (ADR-0063 decision 3). It is set
    -- once, at creation, and is the only reason a person may lack a password.
    external_origin INTEGER NOT NULL DEFAULT 0 CHECK (external_origin IN (0, 1)),
    -- role_mapped is 1 while the role was set by a Group mapping (ADR-0063
    -- decision 4) rather than by hand. It is the only reason a person without a
    -- password may be an Admin.
    role_mapped    INTEGER NOT NULL DEFAULT 0 CHECK (role_mapped IN (0, 1)),
    -- Only these kinds of User may lack a password: a ''remote'' User, which
    -- authenticates over the Link protocol and never by logging in locally
    -- (ADR-0054); a Member minted from a first-time External identity, which
    -- signs in through its Sign-in provider (ADR-0063); and such a User a Group
    -- mapping made an Admin. Every other person has a Local password, whoever
    -- inserted the row.
    CHECK (COALESCE(password_hash, '''') <> ''''
           OR role = ''remote''
           OR (role = ''member'' AND external_origin = 1)
           OR (role = ''admin'' AND external_origin = 1 AND role_mapped = 1))
)' WHERE type = 'table' AND name = 'users';
PRAGMA writable_schema = RESET;

-- ===========================================================================
-- markers (ADR-0065)
-- ===========================================================================

-- A Marker is a timed span of a File a player can offer to skip. It is keyed by
-- the File's PATH, not its id: a rescan deletes and re-inserts every files row
-- under a rebuilt Edition, so a file_id foreign key would cascade the Markers
-- away on every scan, and an unchanged File is not re-probed to put them back.
-- The path is what a Marker is measured against anyway — these exact bytes.
-- A path no files row holds any more (the File was renamed or deleted) has its
-- Markers removed at the end of every completed scan of a Library, incremental
-- or full.
--
-- from_edl is 1 on a Local Marker read from the File's `.edl` rather than its
-- chapters: an unchanged File is not re-probed, so once its `.edl` is gone the
-- Scanner must know to probe it again for the chapters that now apply.
CREATE TABLE markers (
    id        TEXT PRIMARY KEY,
    file_path TEXT NOT NULL CHECK (file_path <> ''),
    kind      TEXT NOT NULL CHECK (kind IN ('intro', 'recap', 'credits', 'preview')),
    source    TEXT NOT NULL CHECK (source IN ('local', 'detected', 'fetched')),
    start_ms  INTEGER NOT NULL CHECK (start_ms >= 0),
    end_ms    INTEGER NOT NULL,
    from_edl  INTEGER NOT NULL DEFAULT 0 CHECK (from_edl IN (0, 1) AND (from_edl = 0 OR source = 'local')),
    CHECK (end_ms > start_ms)
);
CREATE INDEX idx_markers_file_path ON markers(file_path, source);

-- ===========================================================================
-- lyrics
-- ===========================================================================

-- A Track's words. 'local' rows are the Scanner's — read from a sidecar .lrc or
-- the file's own tags — and a rescan rewrites them. 'fetched' rows are what the
-- Lyric providers answered, asked the first time someone opened the lyrics view;
-- they live only here, never in the library folder, and a scan never touches
-- them. kind says which shape body holds: 'synced' is a JSON array of
-- {"startMs","text"} lines, 'plain' the text, and 'none' — fetched only — a
-- remembered miss with an empty body. provider is the slug that answered ('' for
-- local rows and misses); question is what the providers were asked, so a miss
-- is asked again only once the question changes (ADR-0051).
CREATE TABLE lyrics (
    title_id   TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    source     TEXT NOT NULL CHECK (source IN ('local', 'fetched')),
    kind       TEXT NOT NULL CHECK (kind IN ('synced', 'plain', 'none')),
    body       TEXT NOT NULL,
    provider   TEXT NOT NULL DEFAULT '',
    question   TEXT NOT NULL DEFAULT '',
    CHECK (kind <> 'none' OR source = 'fetched'),
    PRIMARY KEY (title_id, source)
);

-- ===========================================================================
-- sign-in providers (ADR-0063)
-- ===========================================================================

-- An External identity: a Sign-in provider's own stable id for a person, keyed
-- by (plugin_id, subject) and never by username, held by exactly one User.
-- username and groups are what the provider said at the last sign-in — kept, never
-- resolved by. groups is a JSON array.
CREATE TABLE external_identities (
    plugin_id    TEXT NOT NULL,
    subject      TEXT NOT NULL,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    username     TEXT NOT NULL DEFAULT '',
    groups       TEXT NOT NULL DEFAULT '[]',
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    -- last_seen_at is when the provider last vouched for the identity: a
    -- sign-in, or a re-check that answered. The periodic re-check counts its
    -- interval from here.
    last_seen_at TEXT NOT NULL DEFAULT (datetime('now')),
    -- refresh_token is what a redirect provider handed back to re-check the
    -- identity without the person present (ADR-0063 decision 4). Never a
    -- password; '' when there is none.
    refresh_token TEXT NOT NULL DEFAULT '',
    -- retry_at is when a re-check that could not reach the provider is tried
    -- again ('' when none is pending), and check_failures counts the
    -- consecutive re-checks whose ID token failed verification.
    retry_at       TEXT NOT NULL DEFAULT '',
    check_failures INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plugin_id, subject)
);
CREATE INDEX idx_external_identities_user ON external_identities(user_id);

-- An Admin's Group mapping (ADR-0063 decision 4): per Sign-in provider, each of
-- its groups mapped to a role and the Libraries it grants. A User in several
-- mapped groups is an Admin if any of them says so, and holds every Library any
-- of them grants. A provider with no rows maps nothing, and its Users are left
-- as they are.
CREATE TABLE group_mappings (
    plugin_id  TEXT NOT NULL,
    group_name TEXT NOT NULL CHECK (group_name <> ''),
    role       TEXT NOT NULL CHECK (role IN ('admin', 'member')),
    PRIMARY KEY (plugin_id, group_name)
);

CREATE TABLE group_mapping_libraries (
    plugin_id  TEXT NOT NULL,
    group_name TEXT NOT NULL,
    library_id TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    PRIMARY KEY (plugin_id, group_name, library_id),
    FOREIGN KEY (plugin_id, group_name)
        REFERENCES group_mappings(plugin_id, group_name) ON DELETE CASCADE
);

-- The Admin's per-provider override of how often its identities are re-checked
-- between sign-ins. A provider with no row is re-checked every 24 hours.
CREATE TABLE sign_in_recheck_intervals (
    plugin_id TEXT PRIMARY KEY,
    seconds   INTEGER NOT NULL CHECK (seconds > 0)
);

-- The Admin's order for password-flow Sign-in providers: lower position is asked
-- first. A provider with no row is asked after every one that has one, in the
-- order the server registered it.
CREATE TABLE sign_in_provider_order (
    plugin_id TEXT PRIMARY KEY,
    position  INTEGER NOT NULL
);

-- The Sign-in providers uninstalled since they were last installed (ADR-0063
-- decision 10). A sign-in whose provider answered before the uninstall and
-- whose writes land after it finds its plugin here, and creates nothing.
-- Installing a plugin under the id clears its row.
CREATE TABLE uninstalled_sign_in_providers (
    plugin_id      TEXT PRIMARY KEY,
    uninstalled_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ===========================================================================
-- lyric providers
-- ===========================================================================

-- The Admin's order for the Lyric providers: the first acceptable Synced answer,
-- in this order, wins. A registered provider with no row is asked after every
-- one that has one, in registration order.
CREATE TABLE lyric_provider_order (
    slug     TEXT PRIMARY KEY,
    position INTEGER NOT NULL
);

-- ===========================================================================
-- marker detection (ADR-0065 §4)
-- ===========================================================================

-- The per-Library Marker detection toggle. Only a TV Library has one; a TV
-- Library with no row is ON (the default), so a row exists only once an Admin
-- has set it.
CREATE TABLE library_marker_detection (
    library_id TEXT PRIMARY KEY REFERENCES libraries(id) ON DELETE CASCADE,
    enabled    INTEGER NOT NULL
);

-- The Files Marker detection has already listened to, at the mtime it heard.
-- Keyed by path like markers, for the same reason: a rescan re-inserts files
-- rows, and an unchanged File must not be listened to again after every scan.
CREATE TABLE marker_detection_files (
    file_path TEXT PRIMARY KEY CHECK (file_path <> ''),
    mtime     TEXT NOT NULL
);

-- How many detection runs in a row failed to decode a File, at the mtime they
-- failed on. A File that keeps failing unchanged stops being retried after every
-- scan; a count at an older mtime reads as none, so a changed File starts over.
CREATE TABLE marker_detection_failures (
    file_path TEXT PRIMARY KEY CHECK (file_path <> ''),
    mtime     TEXT NOT NULL,
    failures  INTEGER NOT NULL CHECK (failures > 0)
);

-- ===========================================================================
-- wrong lyrics
-- ===========================================================================

-- The Lyric provider answers someone marked wrong for a Track. answer is the
-- answer as the host kept it, normalised and encoded (internal/lyricfetch),
-- whichever provider gave it, so none is kept for that Track again — nor a
-- Synced copy retimed by a few milliseconds. Shared: one row is every User's
-- view. A Track keeps its 20 most recent (by rowid); an older one is forgotten.
CREATE TABLE lyric_rejections (
    title_id    TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    answer      TEXT NOT NULL,
    rejected_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (title_id, answer)
);

-- ===========================================================================
-- fetched markers (ADR-0065)
-- ===========================================================================

-- The question the Marker providers were last asked about a File, as a digest
-- (the providers asked and everything the request carried), keyed by path like
-- markers. Its Fetched Markers, if any, are the 'fetched' rows in markers; a row
-- here with none is a remembered miss. The providers are asked again only once
-- the question changes. question is '' when a provider failed: what the others
-- answered is kept, but nothing is remembered as settled.
CREATE TABLE marker_fetches (
    file_path TEXT PRIMARY KEY CHECK (file_path <> ''),
    question  TEXT NOT NULL
);

-- ===========================================================================
-- marker auto-skip (ADR-0065 §6)
-- ===========================================================================

-- The Marker kinds a User has chosen to skip automatically instead of being
-- offered a Skip button. Per User, so it follows them from client to client; a
-- User with no rows skips nothing automatically.
CREATE TABLE user_marker_auto_skip (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind    TEXT NOT NULL CHECK (kind IN ('intro', 'recap', 'credits', 'preview')),
    PRIMARY KEY (user_id, kind)
);
