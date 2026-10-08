-- Per-User grants of an Online source (ADR-0068, Q3). A source is a Plugin's slug,
-- not a row anywhere, so source_id has no foreign key: a grant to a source that was
-- uninstalled simply names nothing until the Plugin is back. A User holding a Rating
-- ceiling holds no grant; the store keeps it so in one transaction (online_source_access.go).
CREATE TABLE user_online_source_access (
    user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_id TEXT NOT NULL,
    UNIQUE (user_id, source_id)
);
CREATE INDEX idx_user_online_source_access_user ON user_online_source_access(user_id);
