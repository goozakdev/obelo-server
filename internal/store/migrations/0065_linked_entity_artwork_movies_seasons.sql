-- Extend the linked_entity_artwork signal table (migration 0064) to mirrored
-- MOVIES and SEASONS (.scratch/linked-servers issue 20, ADR-0056 §5).
--
-- Issue 19 landed the table and cleanup triggers for Show/Artist/Album. Issue 20
-- folds Seasons in (entity_type 'season', keyed by the season's local id — same
-- shape as a Show) and adds Movies (entity_type 'title', keyed by the title's
-- local id; a local Title's artwork lives in the `artwork` table, so 'title' rows
-- appear only on the linked side). The mirror write, the roles/version read UNION
-- and the SIGNAL-ONLY discipline are all already generic over entity_type — only
-- these two cleanup triggers were missing, so a pruned/unlinked season or movie
-- would leave dead signal rows keyed to local ids this Server never mints again.
--
-- Same AFTER DELETE shape as 0064's shows/artists/albums triggers: clear an
-- entity's signal rows whenever the entity itself goes (a full-pull prune, the
-- unlink cascade, any direct delete).
CREATE TRIGGER IF NOT EXISTS seasons_linked_artwork_ad AFTER DELETE ON seasons
BEGIN
    DELETE FROM linked_entity_artwork WHERE entity_type = 'season' AND entity_id = OLD.id;
END;

CREATE TRIGGER IF NOT EXISTS titles_linked_artwork_ad AFTER DELETE ON titles
BEGIN
    DELETE FROM linked_entity_artwork WHERE entity_type = 'title' AND entity_id = OLD.id;
END;
