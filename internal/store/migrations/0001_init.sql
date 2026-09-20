-- Obelo's initial schema. A later change is added as its own numbered file
-- applied in order by internal/store/migrate.go, which also owns the
-- schema_migrations bookkeeping table (created at runtime, not here).

-- ===========================================================================
-- users / auth / devices
-- ===========================================================================

CREATE TABLE users (
    id             TEXT PRIMARY KEY,
    username       TEXT NOT NULL UNIQUE,
    -- role is a vocabulary, not a flag, so every guard reads as one field
    -- ("role == remote", "role == admin") instead of a combination of flags.
    -- 'remote' names a linked Server (ADR-0054): the counterpart Server's own
    -- users are never rows here, only the Link itself is.
    role           TEXT NOT NULL DEFAULT 'admin' CHECK (role IN ('admin', 'member', 'remote')),
    password_hash  TEXT,
    -- Family/device controls: NULL means "no restriction for this User".
    rating_ceiling TEXT,
    max_resolution TEXT,
    max_bitrate    INTEGER,
    max_streams    INTEGER,
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    -- Only a 'remote' User may lack a password: it authenticates over the Link
    -- protocol, never by logging in locally (ADR-0058).
    CHECK (COALESCE(password_hash, '') <> '' OR role = 'remote')
);

CREATE TABLE devices (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- client_id is the stable per-installation UUID the client persists.
    client_id    TEXT NOT NULL,
    name         TEXT NOT NULL,
    platform     TEXT NOT NULL,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    last_seen_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (user_id, client_id)
);
CREATE INDEX idx_devices_user ON devices(user_id);

CREATE TABLE auth_tokens (
    token_hash TEXT PRIMARY KEY,
    device_id  TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_auth_tokens_device ON auth_tokens(device_id);

CREATE TABLE stream_tokens (
    token_hash TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE INDEX idx_stream_tokens_session ON stream_tokens(session_id);
CREATE INDEX idx_stream_tokens_expires ON stream_tokens(expires_at);

-- device_auth_requests implements RFC 8628 device authorization (a TV enters a
-- short code, a phone approves it) for the initial pairing of a Device that
-- has no browser to log in with.
CREATE TABLE device_auth_requests (
    device_code_hash TEXT PRIMARY KEY,
    user_code        TEXT NOT NULL UNIQUE,
    -- The TV's Device descriptor, echoed back to the phone at approve time and
    -- minted into the real Device row at redeem.
    client_id        TEXT NOT NULL,
    device_name      TEXT NOT NULL,
    device_platform  TEXT NOT NULL,
    -- pending -> approved -> redeemed. A redeemed row is KEPT until the
    -- sweeper reaps it, rather than deleted on collection: the state is what
    -- makes the code one-shot, and a deleted row would read as "never
    -- existed", the same answer a fresh guess gets. There is no 'denied' —
    -- approval is immediate on code entry, so there is no screen to refuse
    -- from (see auth/device_auth.go).
    state            TEXT NOT NULL DEFAULT 'pending'
                     CHECK (state IN ('pending', 'approved', 'redeemed')),
    -- Who approved it, NULL until then. This is the ONLY thing approval
    -- records: the token is minted at redeem, never here, so a raw token
    -- never rests in a table waiting to be collected (ADR-0015 — only hashes
    -- are stored).
    approved_user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
    -- Timestamps are RFC3339-UTC written by Go, NOT the datetime('now')
    -- default the neighbouring tables use. This is the first table whose rows
    -- expire, so it is the first to COMPARE a timestamp in SQL, and the two
    -- formats do not compare: 'T' (0x54) sorts after ' ' (0x20), so an
    -- RFC3339 expires_at is lexicographically greater than any same-day
    -- datetime('now') value and every row would read as unexpired. Both
    -- operands must be RFC3339, so both come from Go. api/time.go's
    -- formatTimestamp normalizes either shape on read, which is why the mix
    -- is survivable everywhere that only displays them.
    created_at       TEXT NOT NULL,
    expires_at       TEXT NOT NULL,
    -- Last poll, for the RFC 8628 SLOW_DOWN rule. NULL until the first poll.
    last_polled_at   TEXT
);
CREATE INDEX idx_device_auth_expires ON device_auth_requests(expires_at);

-- link_invites is the one-shot code an admin hands a remote server's admin to
-- establish a Link (ADR-0054): redeeming it creates the 'remote' User on this
-- side.
CREATE TABLE link_invites (
    code_hash   TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    redeemed_at TEXT
);
CREATE INDEX idx_link_invites_expires ON link_invites(expires_at);
CREATE INDEX idx_link_invites_user ON link_invites(user_id);

-- ===========================================================================
-- libraries / access
-- ===========================================================================

CREATE TABLE libraries (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL,
    kind              TEXT NOT NULL CHECK (kind IN ('movie', 'tv', 'music')),
    -- source distinguishes a Library scanned from local disk from one
    -- mirrored over a Link (ADR-0059); 'linked' Libraries carry the three
    -- columns below, 'local' ones leave them at their zero value.
    source            TEXT NOT NULL DEFAULT 'local' CHECK (source IN ('local', 'linked')),
    link_id           TEXT,
    remote_library_id TEXT NOT NULL DEFAULT '',
    -- remote_checkpoint is the sharer's sync cursor last applied to this
    -- mirror, so a reconnect resumes rather than re-mirrors from scratch.
    remote_checkpoint TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_libraries_source ON libraries(source);
CREATE UNIQUE INDEX idx_libraries_link_remote
    ON libraries(link_id, remote_library_id) WHERE link_id IS NOT NULL;

CREATE TABLE library_roots (
    id         TEXT PRIMARY KEY,
    library_id TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_library_roots_library ON library_roots(library_id);

CREATE TABLE user_library_access (
    user_id    TEXT NOT NULL REFERENCES users(id)     ON DELETE CASCADE,
    library_id TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    UNIQUE (user_id, library_id)
);
CREATE INDEX idx_user_library_access_user ON user_library_access(user_id);

CREATE TABLE library_enrichment_policy (
    library_id             TEXT PRIMARY KEY REFERENCES libraries(id) ON DELETE CASCADE,
    enrich_enabled         INTEGER,  -- NULL = inherit; 0/1 = deliberate override
    metadata_language      TEXT,
    authoritative_provider TEXT,
    updated_at             TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE library_provider_override (
    library_id TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    provider   TEXT NOT NULL,             -- registry slug
    enabled    INTEGER NOT NULL,          -- 0/1 forced state (row present = override)
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (library_id, provider)
);

-- scan_status / unmatched_files / match_overrides / file_decisions record the
-- scanner's ongoing work and the admin corrections that steer it.

CREATE TABLE scan_status (
    library_id    TEXT PRIMARY KEY REFERENCES libraries(id) ON DELETE CASCADE,
    state         TEXT NOT NULL DEFAULT 'idle' CHECK (state IN ('idle', 'running', 'error')),
    scope         TEXT NOT NULL DEFAULT '',
    titles_found  INTEGER NOT NULL DEFAULT 0,
    files_found   INTEGER NOT NULL DEFAULT 0,
    error_message TEXT NOT NULL DEFAULT '',
    started_at    TEXT,
    finished_at   TEXT
);

CREATE TABLE unmatched_files (
    id         TEXT PRIMARY KEY,
    library_id TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL UNIQUE,
    kind       TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    added_at   TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_unmatched_library ON unmatched_files(library_id);

CREATE TABLE match_overrides (
    id           TEXT PRIMARY KEY,
    library_id   TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    -- folder_path is the absolute on-disk folder the override anchors to. For
    -- a bare-file Title the anchor is the file's own path.
    folder_path  TEXT NOT NULL,
    -- The corrected identity the scanner must use instead of its parse.
    title        TEXT NOT NULL,
    year         INTEGER,
    tmdb_id      TEXT NOT NULL DEFAULT '',
    imdb_id      TEXT NOT NULL DEFAULT '',
    identity_key TEXT NOT NULL,
    -- orphaned=1 once a scan finds no folder at folder_path (the user
    -- renamed/moved it). Surfaced in the Admin attention list.
    orphaned     INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (library_id, folder_path)
);
CREATE INDEX idx_match_overrides_library ON match_overrides(library_id);

-- File decision storage (ADR-0044): the file-anchored Match override the
-- scanner replays at resolve time.
CREATE TABLE file_decisions (
    id           TEXT PRIMARY KEY,
    library_id   TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    -- path is the absolute on-disk file the decision anchors to. The anchor
    -- is the FILE, not the folder match_overrides keys on: the work is
    -- already identified and it is the arrangement inside it being corrected.
    path         TEXT NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('placed', 'unassigned', 'ignored')),
    -- The assigned Slot's POSITION, always in the local library's own
    -- numbering (a borrowed provider record's numbering would collide with
    -- the library's own — that is the Episode pin's job on titles, kept
    -- deliberately separate). group_number is the season / disc; slot_number
    -- is the episode / track. NULL for the two settled states, which name no
    -- Slot; NULL rather than a sentinel because season 0 is a real value
    -- (Specials), so 0 cannot mean "no Slot".
    group_number INTEGER,
    slot_number  INTEGER,
    -- Part order within a shared Slot, 1-based. Meaningful only when several
    -- Files are placed on one Slot, where it decides Edition.Files order and
    -- so the joint playback timeline; 1 for the ordinary one-File-per-Slot
    -- case and ignored for the settled states.
    ordinal      INTEGER NOT NULL DEFAULT 1,
    -- orphaned=1 once a scan finds no file at path (renamed, moved or
    -- deleted). A Placement pointing at nothing is broken rather than done,
    -- so it is surfaced in the needs-fixing queue, never silently dropped —
    -- the same posture match_overrides.orphaned already has for folder
    -- anchors. Only 'placed' rows are orphaned: a settled decision about a
    -- File that has gone is not a broken correction, and re-surfacing it
    -- would be pure noise (and would un-settle an ignore that correctly
    -- re-applies if the File ever comes back).
    orphaned     INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    -- A Slot belongs to the placed state and only to it, so the two settled
    -- states cannot smuggle in half a Placement that later code would read
    -- as real.
    CHECK ((state =  'placed' AND group_number IS NOT NULL AND slot_number IS NOT NULL)
        OR (state <> 'placed' AND group_number IS     NULL AND slot_number IS     NULL)),
    -- One decision per (file, Slot). Re-placing the same File on the same
    -- Slot updates that row rather than adding a second; placing it on a
    -- second Slot, or a second File on the same Slot, is a genuinely
    -- different row and is allowed.
    UNIQUE (library_id, path, group_number, slot_number)
);
CREATE UNIQUE INDEX idx_file_decisions_settled
    ON file_decisions(library_id, path) WHERE state <> 'placed';
CREATE TRIGGER file_decisions_one_kind_per_path_insert
BEFORE INSERT ON file_decisions
WHEN EXISTS (
    SELECT 1 FROM file_decisions d
     WHERE d.library_id = NEW.library_id AND d.path = NEW.path
       AND (d.state = 'placed') <> (NEW.state = 'placed')
)
BEGIN
    SELECT RAISE(ABORT, 'file_decisions: a File is either placed or settled, never both');
END;
CREATE TRIGGER file_decisions_one_kind_per_path_update
BEFORE UPDATE OF state, path, library_id ON file_decisions
WHEN EXISTS (
    SELECT 1 FROM file_decisions d
     WHERE d.library_id = NEW.library_id AND d.path = NEW.path AND d.id <> NEW.id
       AND (d.state = 'placed') <> (NEW.state = 'placed')
)
BEGIN
    SELECT RAISE(ABORT, 'file_decisions: a File is either placed or settled, never both');
END;

-- ===========================================================================
-- catalog: video (shows / seasons)
-- ===========================================================================

CREATE TABLE shows (
    id           TEXT PRIMARY KEY,
    library_id   TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    title        TEXT NOT NULL,
    year         INTEGER,
    identity_key TEXT NOT NULL,
    sort_title   TEXT NOT NULL,
    tmdb_id      TEXT NOT NULL DEFAULT '',
    imdb_id      TEXT NOT NULL DEFAULT '',
    -- needs_review flags a Show filed from a partial parse (e.g. a yearless
    -- Show); reviewed=1 once an Admin has cleared that flag from the list.
    needs_review INTEGER NOT NULL DEFAULT 0,
    reviewed     INTEGER NOT NULL DEFAULT 0,
    -- hidden mirrors the titles convention: a Show with no visible Episodes
    -- is hidden from the grid but stays fetchable so state recovers
    -- (ADR-0008).
    hidden       INTEGER NOT NULL DEFAULT 0,
    -- remote_id is this row's id on the sharer's server, set only for a Show
    -- mirrored into a linked Library (ADR-0059); NULL for a local Show.
    remote_id    TEXT,
    added_at     TEXT NOT NULL DEFAULT (datetime('now')),
    -- updated_at is maintained by the touch triggers below, bumped whenever
    -- this row or a dependent (Season, Title, Enrichment, genres, credits)
    -- changes, so a linked Library's sync can ask "what changed since X".
    updated_at   TEXT NOT NULL DEFAULT '',
    UNIQUE (library_id, identity_key)
);
CREATE INDEX idx_shows_library    ON shows(library_id);
CREATE INDEX idx_shows_sort_title ON shows(library_id, sort_title, id);
CREATE INDEX idx_shows_updated    ON shows(library_id, updated_at, id);
CREATE UNIQUE INDEX idx_shows_remote
    ON shows(library_id, remote_id) WHERE remote_id IS NOT NULL;
CREATE TRIGGER shows_touch_ai AFTER INSERT ON shows
BEGIN
    UPDATE shows SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;
CREATE TRIGGER shows_touch_au AFTER UPDATE ON shows
BEGIN
    UPDATE shows SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

CREATE TABLE seasons (
    id            TEXT PRIMARY KEY,
    show_id       TEXT NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    -- season_number is the parsed number; 0 = Specials (Season 00 / Specials/).
    season_number INTEGER NOT NULL,
    -- identity_key is "<show identity>|s<NN>" so a rescan re-resolves the
    -- Season.
    identity_key  TEXT NOT NULL,
    hidden        INTEGER NOT NULL DEFAULT 0,
    remote_id     TEXT,
    added_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT '',
    UNIQUE (show_id, season_number)
);
CREATE INDEX idx_seasons_show    ON seasons(show_id, season_number);
CREATE INDEX idx_seasons_updated ON seasons(updated_at, id);
CREATE UNIQUE INDEX idx_seasons_remote
    ON seasons(remote_id) WHERE remote_id IS NOT NULL;
CREATE TRIGGER seasons_touch_ai AFTER INSERT ON seasons
BEGIN
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;
CREATE TRIGGER seasons_touch_au AFTER UPDATE ON seasons
BEGIN
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

-- ===========================================================================
-- catalog: music (artists / albums)
-- ===========================================================================

CREATE TABLE artists (
    id             TEXT PRIMARY KEY,
    library_id     TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    -- name is the display Album Artist (falling back to Artist) for this
    -- grouping.
    name           TEXT NOT NULL,
    -- identity_key is the normalized Album-Artist (fallback Artist) name,
    -- scoped to the Library, so two spellings of one artist collapse and a
    -- rescan re-resolves.
    identity_key   TEXT NOT NULL,
    sort_name      TEXT NOT NULL,
    musicbrainz_id TEXT NOT NULL DEFAULT '',
    -- hidden mirrors the titles/shows convention: an Artist with no visible
    -- Tracks is hidden from the list but stays fetchable so state recovers
    -- (ADR-0008).
    hidden         INTEGER NOT NULL DEFAULT 0,
    remote_id      TEXT,
    added_at       TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at     TEXT NOT NULL DEFAULT '',
    UNIQUE (library_id, identity_key)
);
CREATE INDEX idx_artists_library   ON artists(library_id);
CREATE INDEX idx_artists_sort_name ON artists(library_id, sort_name, id);
CREATE INDEX idx_artists_updated   ON artists(library_id, updated_at, id);
CREATE UNIQUE INDEX idx_artists_remote
    ON artists(library_id, remote_id) WHERE remote_id IS NOT NULL;
CREATE TRIGGER artists_touch_ai AFTER INSERT ON artists
BEGIN
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;
CREATE TRIGGER artists_touch_au AFTER UPDATE ON artists
BEGIN
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

CREATE TABLE albums (
    id                     TEXT PRIMARY KEY,
    artist_id              TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    title                  TEXT NOT NULL,
    year                   INTEGER,
    -- identity_key is "<artist identity>|<normalized album title>" so the
    -- same Album re-resolves on rescan and two artists may share an album
    -- title.
    identity_key           TEXT NOT NULL,
    sort_title             TEXT NOT NULL,
    release_type           TEXT NOT NULL DEFAULT '',
    musicbrainz_id         TEXT NOT NULL DEFAULT '',
    musicbrainz_release_id TEXT NOT NULL DEFAULT '',
    -- artwork_path is the local album cover (cover.jpg/folder.jpg) when
    -- present, empty otherwise. Local always wins; embedded cover art is the
    -- fallback the scanner records here too (ADR-0001, naming-convention.md
    -- "Local artwork").
    artwork_path           TEXT NOT NULL DEFAULT '',
    hidden                 INTEGER NOT NULL DEFAULT 0,
    remote_id              TEXT,
    added_at               TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at             TEXT NOT NULL DEFAULT '',
    UNIQUE (artist_id, identity_key)
);
CREATE INDEX idx_albums_artist  ON albums(artist_id, sort_title, id);
CREATE INDEX idx_albums_updated ON albums(updated_at, id);
CREATE UNIQUE INDEX idx_albums_remote
    ON albums(remote_id) WHERE remote_id IS NOT NULL;
CREATE TRIGGER albums_touch_ai AFTER INSERT ON albums
BEGIN
    UPDATE albums SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;
CREATE TRIGGER albums_touch_au AFTER UPDATE ON albums
BEGIN
    UPDATE albums SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

-- ===========================================================================
-- catalog: titles (the movie / episode / track leaf, shared by every kind)
-- ===========================================================================

CREATE TABLE titles (
    id                       TEXT PRIMARY KEY,
    library_id               TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    -- kind is the leaf discriminator: 'movie' | 'episode' | 'track'.
    kind                     TEXT NOT NULL CHECK (kind IN ('movie', 'episode', 'track')),
    title                    TEXT NOT NULL,
    year                     INTEGER,
    identity_key             TEXT NOT NULL,
    sort_title               TEXT NOT NULL,
    tmdb_id                  TEXT NOT NULL DEFAULT '',
    imdb_id                  TEXT NOT NULL DEFAULT '',
    needs_review             INTEGER NOT NULL DEFAULT 0,
    ambiguous                INTEGER NOT NULL DEFAULT 0,
    reviewed                 INTEGER NOT NULL DEFAULT 0,
    hidden                   INTEGER NOT NULL DEFAULT 0,
    -- TV linkage, NULL for a Movie/Track.
    season_id                TEXT REFERENCES seasons(id) ON DELETE CASCADE,
    season_number            INTEGER NOT NULL DEFAULT 0,
    episode_number           INTEGER NOT NULL DEFAULT 0,
    episode_label            TEXT NOT NULL DEFAULT '',
    -- Music linkage, NULL for a Movie/Episode. A Track references its Album;
    -- the Artist is reachable via the Album. ON DELETE CASCADE so dropping an
    -- Album/Artist drops its Tracks.
    album_id                 TEXT REFERENCES albums(id) ON DELETE CASCADE,
    -- disc_number / track_number are the parsed Music ordering for a Track
    -- (from tags, path fallback). A Track lists in disc-then-track order.
    -- Both default 0 for a Movie/Episode and for a Track with no disc/track
    -- tag.
    disc_number              INTEGER NOT NULL DEFAULT 0,
    track_number             INTEGER NOT NULL DEFAULT 0,
    -- Descriptive fields, shared by every kind (empty/zero when a provider
    -- has not supplied them).
    overview                 TEXT NOT NULL DEFAULT '',
    tagline                  TEXT NOT NULL DEFAULT '',
    content_rating           TEXT NOT NULL DEFAULT '',
    release_date             TEXT NOT NULL DEFAULT '',
    runtime_minutes          INTEGER NOT NULL DEFAULT 0,
    studio                   TEXT NOT NULL DEFAULT '',
    musicbrainz_recording_id TEXT NOT NULL DEFAULT '',
    -- Enrichment state for this leaf (parents track their own in
    -- entity_enrichment below).
    enrichment_status        TEXT NOT NULL DEFAULT 'pending'
        CHECK (enrichment_status IN ('pending', 'matched', 'unmatched', 'failed', 'disabled')),
    enriched_at              TEXT NOT NULL DEFAULT '',
    enriched_title           TEXT NOT NULL DEFAULT '',
    enrichment_source        TEXT NOT NULL DEFAULT '',
    -- enrichment_id_namespace/enrichment_id_origin record which provider (and
    -- via which chain of trust) the enrichment ids on this row came from, so
    -- a later pass knows whether to trust or replace them.
    enrichment_id_namespace  TEXT NOT NULL DEFAULT '',
    enrichment_id_origin     TEXT NOT NULL DEFAULT '',
    enrichment_attempts      INTEGER NOT NULL DEFAULT 0,
    enrichment_retry_at      TEXT NOT NULL DEFAULT '',
    enrichment_reason        TEXT NOT NULL DEFAULT '',
    -- enrichment_season / enrichment_episode PIN which provider episode
    -- record decorates this Slot, independent of the Slot's own position
    -- (ADR-0044). Enrichment normally resolves an Episode using the Slot's
    -- own season_number/episode_number (ADR-0002: local naming is the
    -- identity authority); a provider that numbers a series differently
    -- needs a pin to reach the right record without touching identity_key,
    -- season_id, season_number, episode_number or any User's watch state
    -- (ADR-0014), so the Slot's position and watch history stay put while
    -- only the fetched record changes. Moving a file to a different position
    -- is a Placement decision (file_decisions), never this pin. NULL means
    -- "use the Slot's own numbers", the default.
    enrichment_season        INTEGER,
    enrichment_episode       INTEGER,
    -- remote_id is this row's id on the sharer's server, set only for a Title
    -- mirrored into a linked Library (ADR-0059); NULL for a local Title.
    remote_id                TEXT,
    added_at                 TEXT NOT NULL DEFAULT (datetime('now')),
    -- updated_at is maintained by the touch triggers below, bumped whenever
    -- this row or a dependent (Edition, File, Stream, genres, credits,
    -- external ids) changes, so a linked Library's sync can ask "what
    -- changed since X".
    updated_at               TEXT NOT NULL DEFAULT '',
    UNIQUE (library_id, identity_key)
);
CREATE INDEX idx_titles_library      ON titles(library_id);
CREATE INDEX idx_titles_sort_title   ON titles(library_id, sort_title, id);
CREATE INDEX idx_titles_added        ON titles(library_id, added_at, id);
CREATE INDEX idx_titles_updated      ON titles(library_id, updated_at, id);
CREATE INDEX idx_titles_needs_review ON titles(library_id, needs_review, ambiguous);
CREATE INDEX idx_titles_hidden       ON titles(library_id, hidden);
CREATE INDEX idx_titles_season       ON titles(season_id, season_number, episode_number);
CREATE INDEX idx_titles_album        ON titles(album_id, disc_number, track_number);
CREATE INDEX idx_titles_enrichment   ON titles(library_id, enrichment_status);
CREATE UNIQUE INDEX idx_titles_remote
    ON titles(library_id, remote_id) WHERE remote_id IS NOT NULL;
CREATE TRIGGER titles_touch_ai AFTER INSERT ON titles
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;
CREATE TRIGGER titles_touch_au AFTER UPDATE ON titles
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

CREATE TABLE title_genres (
    title_id TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    genre    TEXT NOT NULL,
    ord      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_title_genres_title ON title_genres(title_id);
CREATE INDEX idx_title_genres_genre ON title_genres(genre);
CREATE TRIGGER title_genres_touch_ai AFTER INSERT ON title_genres
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;
CREATE TRIGGER title_genres_touch_au AFTER UPDATE ON title_genres
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;
CREATE TRIGGER title_genres_touch_ad AFTER DELETE ON title_genres
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = OLD.title_id;
END;

CREATE TABLE title_credits (
    title_id   TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    person     TEXT NOT NULL,
    -- person_ref is the provider's stable person id ('' when unknown); person
    -- is the display name.
    person_ref TEXT NOT NULL DEFAULT '',
    -- role is the job ("Actor", "Director"); character is the role played
    -- (cast only). kind groups them: 'cast' | 'crew'. ord is billing order.
    role       TEXT NOT NULL DEFAULT '',
    character  TEXT NOT NULL DEFAULT '',
    kind       TEXT NOT NULL DEFAULT 'cast',
    ord        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_title_credits_title ON title_credits(title_id);
CREATE TRIGGER title_credits_touch_ai AFTER INSERT ON title_credits
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;
CREATE TRIGGER title_credits_touch_au AFTER UPDATE ON title_credits
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;
CREATE TRIGGER title_credits_touch_ad AFTER DELETE ON title_credits
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = OLD.title_id;
END;

CREATE TABLE title_field_locks (
    title_id TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    field    TEXT NOT NULL,
    PRIMARY KEY (title_id, field)
);

-- title_external_ids holds every namespaced provider id a Title carries
-- (ADR-0060/ADR-0061), beyond the tmdb_id/imdb_id columns kept on titles for
-- the two identity providers every Library type needs.
CREATE TABLE title_external_ids (
    title_id    TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    namespace   TEXT NOT NULL,
    external_id TEXT NOT NULL,
    PRIMARY KEY (title_id, namespace)
);
CREATE TRIGGER title_external_ids_touch_ai AFTER INSERT ON title_external_ids
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;
CREATE TRIGGER title_external_ids_touch_au AFTER UPDATE ON title_external_ids
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;
CREATE TRIGGER title_external_ids_touch_ad AFTER DELETE ON title_external_ids
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = OLD.title_id;
END;

-- editions / extras: a Title's alternate cuts and its bonus material.

CREATE TABLE editions (
    id         TEXT PRIMARY KEY,
    title_id   TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    -- name is a human label ("1080p", "Director's Cut"), possibly inferred
    -- from the folder/file name.
    name       TEXT NOT NULL DEFAULT '',
    remote_id  TEXT,
    added_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_editions_title   ON editions(title_id);
CREATE INDEX idx_editions_updated ON editions(updated_at, id);
CREATE UNIQUE INDEX idx_editions_remote
    ON editions(remote_id) WHERE remote_id IS NOT NULL;
CREATE TRIGGER editions_touch_ai AFTER INSERT ON editions
BEGIN
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;
CREATE TRIGGER editions_touch_au AFTER UPDATE ON editions
BEGIN
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;
CREATE TRIGGER editions_touch_ad AFTER DELETE ON editions
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = OLD.title_id;
END;

CREATE TABLE extras (
    id          TEXT PRIMARY KEY,
    title_id    TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    -- extra_type ∈ trailer | behindthescenes | deleted | featurette |
    -- interview | short | scene | clip | other (naming-convention.md).
    extra_type  TEXT NOT NULL DEFAULT 'other',
    path        TEXT NOT NULL UNIQUE,
    container   TEXT NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    added_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_extras_title ON extras(title_id);

-- ===========================================================================
-- files / streams / subtitles
-- ===========================================================================

CREATE TABLE files (
    id           TEXT PRIMARY KEY,
    edition_id   TEXT NOT NULL REFERENCES editions(id) ON DELETE CASCADE,
    -- path is the absolute on-disk path for a locally-scanned File and '' for
    -- a mirrored one. NOT NULL: "no path" is a fact about linked Libraries,
    -- not a missing value.
    path         TEXT NOT NULL,
    container    TEXT NOT NULL DEFAULT '',
    video_codec  TEXT NOT NULL DEFAULT '',
    audio_codec  TEXT NOT NULL DEFAULT '',
    width        INTEGER NOT NULL DEFAULT 0,
    height       INTEGER NOT NULL DEFAULT 0,
    bitrate      INTEGER NOT NULL DEFAULT 0,
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    size_bytes   INTEGER NOT NULL DEFAULT 0,
    mtime        TEXT NOT NULL DEFAULT '',
    -- present=0 marks a File a rescan cannot find on disk, kept rather than
    -- deleted so watch state and decisions survive a transient disconnect.
    present      INTEGER NOT NULL DEFAULT 1,
    -- part_ordinal is this File's 1-based position within its Edition when
    -- several Files share one Slot (file_decisions.ordinal drives it).
    part_ordinal INTEGER NOT NULL DEFAULT 0,
    remote_id    TEXT,
    added_at     TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_files_edition_path
    ON files(edition_id, path) WHERE path <> '';
CREATE INDEX idx_files_edition ON files(edition_id);
CREATE INDEX idx_files_path    ON files(path);
CREATE INDEX idx_files_updated ON files(updated_at, id);
CREATE UNIQUE INDEX idx_files_remote
    ON files(remote_id) WHERE remote_id IS NOT NULL;
CREATE TRIGGER files_touch_ai AFTER INSERT ON files
BEGIN
    UPDATE files    SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.edition_id;
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT title_id FROM editions WHERE id = NEW.edition_id);
END;
CREATE TRIGGER files_touch_au AFTER UPDATE ON files
BEGIN
    UPDATE files    SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.edition_id;
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT title_id FROM editions WHERE id = NEW.edition_id);
END;
CREATE TRIGGER files_touch_ad AFTER DELETE ON files
BEGIN
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = OLD.edition_id;
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT title_id FROM editions WHERE id = OLD.edition_id);
END;

CREATE TABLE streams (
    id               TEXT PRIMARY KEY,
    file_id          TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    stream_index     INTEGER NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('video', 'audio', 'subtitle')),
    codec            TEXT NOT NULL DEFAULT '',
    language         TEXT NOT NULL DEFAULT '',
    title            TEXT NOT NULL DEFAULT '',
    -- video geometry (0 for non-video streams).
    width            INTEGER NOT NULL DEFAULT 0,
    height           INTEGER NOT NULL DEFAULT 0,
    -- audio channel count (0 for non-audio).
    channels         INTEGER NOT NULL DEFAULT 0,
    is_default       INTEGER NOT NULL DEFAULT 0,
    forced           INTEGER NOT NULL DEFAULT 0,
    commentary       INTEGER NOT NULL DEFAULT 0,
    hearing_impaired INTEGER NOT NULL DEFAULT 0,
    remote_id        TEXT,
    updated_at       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_streams_file    ON streams(file_id);
CREATE INDEX idx_streams_updated ON streams(updated_at, id);
CREATE UNIQUE INDEX idx_streams_remote
    ON streams(remote_id) WHERE remote_id IS NOT NULL;
CREATE TRIGGER streams_touch_ai AFTER INSERT ON streams
BEGIN
    UPDATE streams  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
    UPDATE files    SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.file_id;
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT edition_id FROM files WHERE id = NEW.file_id);
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT e.title_id FROM editions e JOIN files f ON f.edition_id = e.id
                  WHERE f.id = NEW.file_id);
END;
CREATE TRIGGER streams_touch_au AFTER UPDATE ON streams
BEGIN
    UPDATE streams  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
    UPDATE files    SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.file_id;
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT edition_id FROM files WHERE id = NEW.file_id);
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT e.title_id FROM editions e JOIN files f ON f.edition_id = e.id
                  WHERE f.id = NEW.file_id);
END;
CREATE TRIGGER streams_touch_ad AFTER DELETE ON streams
BEGIN
    UPDATE files    SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = OLD.file_id;
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT edition_id FROM files WHERE id = OLD.file_id);
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT e.title_id FROM editions e JOIN files f ON f.edition_id = e.id
                  WHERE f.id = OLD.file_id);
END;

CREATE TABLE subtitles (
    id          TEXT PRIMARY KEY,
    title_id    TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    -- 'sidecar' = a subtitle file discovered next to the media; 'fetched' =
    -- an externally downloaded subtitle cached under the data dir.
    source      TEXT NOT NULL CHECK (source IN ('sidecar', 'fetched')),
    -- 'text' subs are selectable/WebVTT-convertible; 'image' subs
    -- (PGS/VOBSUB) burn in on transcode (ADR-0020).
    kind        TEXT NOT NULL CHECK (kind IN ('text', 'image')),
    -- language is normalized to ISO-639-1 by the scanner ('' = Unknown).
    language    TEXT NOT NULL DEFAULT '',
    forced      INTEGER NOT NULL DEFAULT 0,
    is_default  INTEGER NOT NULL DEFAULT 0,
    -- codec/format token (srt/ass/vtt for text; vobsub/sup for image) —
    -- drives the WebVTT conversion and burn-in paths.
    codec       TEXT NOT NULL DEFAULT '',
    -- on-disk path: the sidecar file, or the cached fetched file. Never a
    -- library write for fetched (ADR-0021).
    path        TEXT NOT NULL DEFAULT '',
    -- provider candidate id for a fetched pick-lock; '' otherwise.
    provider_id TEXT NOT NULL DEFAULT '',
    added_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_subtitles_title ON subtitles(title_id);

CREATE TABLE subtitle_providers (
    slug       TEXT PRIMARY KEY,
    enabled    INTEGER NOT NULL DEFAULT 0,
    api_key    TEXT,
    base_url   TEXT,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE subtitle_settings (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    auto_fetch_lang TEXT NOT NULL DEFAULT '',
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ===========================================================================
-- artwork
-- ===========================================================================

CREATE TABLE artwork (
    id       TEXT PRIMARY KEY,
    title_id TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    role     TEXT NOT NULL CHECK (role IN ('poster', 'background', 'logo')),
    path     TEXT NOT NULL,
    source   TEXT NOT NULL CHECK (source IN ('local', 'fetched', 'uploaded')),
    added_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (title_id, role, source)
);
CREATE INDEX idx_artwork_title ON artwork(title_id);

-- entity_artwork holds artwork for a parent entity (show/season/artist/
-- album), keyed generically since those parents share no base table.
CREATE TABLE entity_artwork (
    id          TEXT PRIMARY KEY,
    entity_type TEXT NOT NULL,
    entity_id   TEXT NOT NULL,
    role        TEXT NOT NULL,
    path        TEXT NOT NULL,
    source      TEXT NOT NULL DEFAULT 'fetched' CHECK (source IN ('local', 'fetched', 'uploaded')),
    added_at    TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (entity_type, entity_id, role, source)
);
CREATE INDEX idx_entity_artwork ON entity_artwork(entity_type, entity_id);

-- linked_entity_artwork tracks the sharer's cache-bust token for artwork
-- mirrored from a linked Library (ADR-0059), so a re-sync knows whether the
-- artwork already fetched is still current without re-downloading it.
CREATE TABLE linked_entity_artwork (
    entity_type TEXT NOT NULL,             -- 'show' | 'artist' | 'album' | 'season' | 'title'
    entity_id   TEXT NOT NULL,             -- this Server's local id for the mirrored entity
    role        TEXT NOT NULL,             -- 'poster' | 'background' | 'logo' | 'cover'
    version     TEXT NOT NULL DEFAULT '',  -- the sharer's entity-level cache-bust token
    PRIMARY KEY (entity_type, entity_id, role)
);
CREATE TRIGGER shows_linked_artwork_ad AFTER DELETE ON shows
BEGIN
    DELETE FROM linked_entity_artwork WHERE entity_type = 'show' AND entity_id = OLD.id;
END;
CREATE TRIGGER artists_linked_artwork_ad AFTER DELETE ON artists
BEGIN
    DELETE FROM linked_entity_artwork WHERE entity_type = 'artist' AND entity_id = OLD.id;
END;
CREATE TRIGGER albums_linked_artwork_ad AFTER DELETE ON albums
BEGIN
    DELETE FROM linked_entity_artwork WHERE entity_type = 'album' AND entity_id = OLD.id;
END;
CREATE TRIGGER seasons_linked_artwork_ad AFTER DELETE ON seasons
BEGIN
    DELETE FROM linked_entity_artwork WHERE entity_type = 'season' AND entity_id = OLD.id;
END;
CREATE TRIGGER titles_linked_artwork_ad AFTER DELETE ON titles
BEGIN
    DELETE FROM linked_entity_artwork WHERE entity_type = 'title' AND entity_id = OLD.id;
END;

-- ===========================================================================
-- enrichment (parent entities: show / season / artist / album)
-- ===========================================================================

-- entity_enrichment is entity_type's counterpart to titles' own enrichment
-- columns, for the parents (show/season/artist/album) that have no base
-- table of their own to carry enrichment state on.
CREATE TABLE entity_enrichment (
    entity_type            TEXT NOT NULL,
    entity_id              TEXT NOT NULL,
    overview               TEXT NOT NULL DEFAULT '',   -- show synopsis / artist bio / album notes
    content_rating         TEXT NOT NULL DEFAULT '',   -- show maturity rating (TV-MA, …)
    network                TEXT NOT NULL DEFAULT '',   -- show network / album label
    -- external_id is the resolved provider id of this parent (e.g. the
    -- show's TMDB id), kept so a child Season/Episode lookup can resolve
    -- under it on a later only-new pass without re-fetching the parent.
    external_id            TEXT NOT NULL DEFAULT '',
    external_id_namespace  TEXT NOT NULL DEFAULT '',
    external_id_origin     TEXT NOT NULL DEFAULT '',
    external_release_id    TEXT NOT NULL DEFAULT '',
    enrichment_status      TEXT NOT NULL DEFAULT 'pending'
        CHECK (enrichment_status IN ('pending', 'matched', 'unmatched', 'failed', 'disabled')),
    enriched_at            TEXT NOT NULL DEFAULT '',
    enrichment_source      TEXT NOT NULL DEFAULT '',
    enrichment_attempts    INTEGER NOT NULL DEFAULT 0,
    enrichment_retry_at    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (entity_type, entity_id)
);
CREATE TRIGGER entity_enrichment_touch_ai AFTER INSERT ON entity_enrichment
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'show'   AND id = NEW.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'season' AND id = NEW.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'artist' AND id = NEW.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'album'  AND id = NEW.entity_id;
END;
CREATE TRIGGER entity_enrichment_touch_au AFTER UPDATE ON entity_enrichment
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'show'   AND id = NEW.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'season' AND id = NEW.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'artist' AND id = NEW.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'album'  AND id = NEW.entity_id;
END;
CREATE TRIGGER entity_enrichment_touch_ad AFTER DELETE ON entity_enrichment
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'show'   AND id = OLD.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'season' AND id = OLD.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'artist' AND id = OLD.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'album'  AND id = OLD.entity_id;
END;

CREATE TABLE entity_genres (
    entity_type TEXT NOT NULL,
    entity_id   TEXT NOT NULL,
    genre       TEXT NOT NULL,
    ord         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_entity_genres       ON entity_genres(entity_type, entity_id);
CREATE INDEX idx_entity_genres_genre ON entity_genres(genre);
CREATE TRIGGER entity_genres_touch_ai AFTER INSERT ON entity_genres
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'show'   AND id = NEW.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'season' AND id = NEW.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'artist' AND id = NEW.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'album'  AND id = NEW.entity_id;
END;
CREATE TRIGGER entity_genres_touch_au AFTER UPDATE ON entity_genres
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'show'   AND id = NEW.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'season' AND id = NEW.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'artist' AND id = NEW.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'album'  AND id = NEW.entity_id;
END;
CREATE TRIGGER entity_genres_touch_ad AFTER DELETE ON entity_genres
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'show'   AND id = OLD.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'season' AND id = OLD.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'artist' AND id = OLD.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'album'  AND id = OLD.entity_id;
END;

CREATE TABLE entity_credits (
    id          TEXT PRIMARY KEY,
    entity_type TEXT NOT NULL,
    entity_id   TEXT NOT NULL,
    person_ref  TEXT NOT NULL DEFAULT '',
    person      TEXT NOT NULL,
    character   TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL DEFAULT 'cast',
    ord         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_entity_credits ON entity_credits(entity_type, entity_id);
CREATE TRIGGER entity_credits_touch_ai AFTER INSERT ON entity_credits
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'show'   AND id = NEW.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'season' AND id = NEW.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'artist' AND id = NEW.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'album'  AND id = NEW.entity_id;
END;
CREATE TRIGGER entity_credits_touch_au AFTER UPDATE ON entity_credits
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'show'   AND id = NEW.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'season' AND id = NEW.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'artist' AND id = NEW.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'album'  AND id = NEW.entity_id;
END;
CREATE TRIGGER entity_credits_touch_ad AFTER DELETE ON entity_credits
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'show'   AND id = OLD.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'season' AND id = OLD.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'artist' AND id = OLD.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'album'  AND id = OLD.entity_id;
END;

CREATE TABLE entity_field_locks (
    entity_type TEXT NOT NULL,
    entity_id   TEXT NOT NULL,
    field       TEXT NOT NULL,
    PRIMARY KEY (entity_type, entity_id, field)
);

CREATE TABLE metadata_providers (
    slug           TEXT PRIMARY KEY,
    enabled        INTEGER NOT NULL DEFAULT 0,
    api_key        TEXT,
    base_url       TEXT,
    image_base_url TEXT,
    updated_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE metadata_settings (
    id                         INTEGER PRIMARY KEY CHECK (id = 1),
    metadata_language          TEXT NOT NULL DEFAULT '',
    auto_enrich_after_scan     INTEGER,
    enrich_interval_seconds    INTEGER,
    musicbrainz_rate_limit_ms  INTEGER,
    enrichment_consent_granted INTEGER,
    enrichment_consent_at      TEXT,
    updated_at                 TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ===========================================================================
-- playback state (per-User watch progress and remembered stream picks)
-- ===========================================================================

CREATE TABLE watch_state (
    id                 TEXT PRIMARY KEY,
    user_id            TEXT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    title_id           TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    -- resume_position_ms is where the User left off, in milliseconds. 0
    -- means no resume offset (either never started, finished/watched, or a
    -- stop below the ~2% floor that we deliberately do not record). A row
    -- with watched=1 always has resume_position_ms=0 (crossing the ceiling
    -- clears the resume).
    resume_position_ms INTEGER NOT NULL DEFAULT 0,
    -- watched is the server-applied Watched threshold outcome (~90%), or a
    -- manual override via PUT /titles/{id}/watchState. Clients never compute
    -- it.
    watched            INTEGER NOT NULL DEFAULT 0,
    played_at          TEXT,
    updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (user_id, title_id)
);
CREATE INDEX idx_watch_state_user ON watch_state(user_id, updated_at);

-- title_audio_memory / show_audio_memory / title_video_memory /
-- show_video_memory remember a User's last deliberate stream pick so a
-- rewatch (or the next Episode of a Show) reapplies it. The remembered
-- pick's MEANING, not a raw stream index, is stored (ADR-0023/ADR-0025):
-- language is the ISO-639-1 code ('' = Unknown); label is the embedded
-- title/video tag ('' when untagged); channels/codec/width/height are the
-- audio/video traits. Re-resolution matches these against the current
-- File's Streams (exact-trait -> label/language -> default fallback).

CREATE TABLE title_audio_memory (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    title_id   TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    language   TEXT    NOT NULL DEFAULT '',
    label      TEXT    NOT NULL DEFAULT '',
    channels   INTEGER NOT NULL DEFAULT 0,
    commentary INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (user_id, title_id)
);

CREATE TABLE show_audio_memory (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    show_id    TEXT NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    language   TEXT    NOT NULL DEFAULT '',
    label      TEXT    NOT NULL DEFAULT '',
    channels   INTEGER NOT NULL DEFAULT 0,
    commentary INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (user_id, show_id)
);

CREATE TABLE title_video_memory (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    title_id   TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    label      TEXT    NOT NULL DEFAULT '',
    codec      TEXT    NOT NULL DEFAULT '',
    width      INTEGER NOT NULL DEFAULT 0,
    height     INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (user_id, title_id)
);

CREATE TABLE show_video_memory (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    show_id    TEXT NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    label      TEXT    NOT NULL DEFAULT '',
    codec      TEXT    NOT NULL DEFAULT '',
    width      INTEGER NOT NULL DEFAULT 0,
    height     INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (user_id, show_id)
);

-- ===========================================================================
-- collections / playlists / watchlist
-- ===========================================================================

CREATE TABLE collections (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    -- description carries an optional sentence of context for the row;
    -- stored as '' (never NULL) when absent so reads need no COALESCE.
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE collection_items (
    collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    title_id      TEXT NOT NULL REFERENCES titles(id)      ON DELETE CASCADE,
    added_at      TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (collection_id, title_id)
);
CREATE INDEX idx_collection_items_collection ON collection_items(collection_id);

CREATE TABLE playlists (
    id            TEXT PRIMARY KEY,
    -- The owning User. The cascade gives "delete a User → their Playlists
    -- (and, via the items cascade below, their items) go with them" for free.
    owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    -- The single media kind the Playlist holds: one of 'movie' | 'tv' |
    -- 'music' (NOT the raw Title kind — an Episode maps to 'tv', a Track to
    -- 'music'). It is NULL until the FIRST item fixes it (the Playlist is
    -- created empty/untyped); thereafter every appended Title must map to
    -- the same kind (single-kind rule, enforced in the organize service).
    kind          TEXT,
    -- system names a server-managed Playlist (e.g. a Watchlist) that the
    -- owner did not create by hand; NULL for an ordinary user Playlist. At
    -- most one system Playlist of a given name per owner.
    system        TEXT,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_playlists_owner ON playlists(owner_user_id);
CREATE UNIQUE INDEX idx_playlists_owner_system
    ON playlists(owner_user_id, system) WHERE system IS NOT NULL;

CREATE TABLE playlist_items (
    id          TEXT PRIMARY KEY,
    playlist_id TEXT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    title_id    TEXT NOT NULL REFERENCES titles(id)    ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    added_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_playlist_items_playlist ON playlist_items(playlist_id);

-- ===========================================================================
-- plugins
-- ===========================================================================

CREATE TABLE plugins (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL DEFAULT '',
    version      TEXT NOT NULL DEFAULT '',
    api_version  INTEGER NOT NULL DEFAULT 0,
    provides     TEXT NOT NULL DEFAULT '[]',
    -- publisher/key_id name the signer that vouches for this plugin;
    -- '' for one installed without a signature check (a Bundled plugin, or a
    -- side-loaded dev build). origin records how it got here: e.g. 'admin'
    -- for one an Admin installed by hand, distinct from a Bundled plugin.
    publisher    TEXT NOT NULL DEFAULT '',
    key_id       TEXT NOT NULL DEFAULT '',
    origin       TEXT NOT NULL CHECK (origin IN ('admin', 'bundled')),
    source       TEXT NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    last_error   TEXT,
    installed_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE plugin_settings (
    plugin_id  TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT,
    secret     INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (plugin_id, key)
);

CREATE TABLE plugin_kv (
    plugin_id  TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      BLOB NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (plugin_id, key)
);

CREATE TABLE plugin_publishers (
    publisher  TEXT PRIMARY KEY COLLATE NOCASE,
    public_key TEXT NOT NULL,
    key_id     TEXT NOT NULL DEFAULT '',
    added_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE plugin_catalog_settings (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    url        TEXT,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- declined_plugins remembers a Bundled plugin an Admin chose not to install,
-- so the catalog does not keep re-offering it every time it is listed.
CREATE TABLE declined_plugins (
    id          TEXT PRIMARY KEY,
    declined_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ===========================================================================
-- links / mirroring (server-to-server Library sharing, ADR-0054/ADR-0059)
-- ===========================================================================

CREATE TABLE links (
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

-- ===========================================================================
-- events
-- ===========================================================================

CREATE TABLE event_sinks (
    slug       TEXT PRIMARY KEY,
    enabled    INTEGER NOT NULL DEFAULT 0,
    secret     TEXT,
    url        TEXT,
    events     TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ===========================================================================
-- settings
-- ===========================================================================

CREATE TABLE tailnet_settings (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    enabled       INTEGER NOT NULL DEFAULT 0,
    hostname      TEXT    NOT NULL DEFAULT '',
    control_url   TEXT,
    https_enabled INTEGER NOT NULL DEFAULT 0,
    updated_at    TEXT    NOT NULL DEFAULT (datetime('now'))
);
