-- 0032_relativize_artwork_cache_paths: make fetched/uploaded artwork paths
-- portable across a data-dir move or rename.
--
-- Fetched and uploaded artwork lives in the app's OWN cache dir
-- (config.ArtworkCacheDir = <data>/artwork). These rows historically stored an
-- ABSOLUTE path, so renaming/moving the data dir (e.g. renaming the project
-- directory) orphaned every one of them — the serve handler os.Stat'd a path that
-- no longer existed and every poster 404'd. Going forward the writer stores a
-- cache-RELATIVE name (just the filename) and catalog.Service.ResolveArtworkPath
-- re-roots it onto ArtworkCacheDir at serve time; this migration relativizes the
-- rows already on disk to the same convention.
--
-- Relativize = strip the directory prefix down to the basename. The SQLite idiom
-- rtrim(path, replace(path,'/','')) yields the dir prefix (everything up to and
-- including the last '/'); replacing it with '' leaves the basename. Scoped to
-- source IN ('fetched','uploaded'): 'local' artwork is media-adjacent (it lives
-- next to the media files, not in our cache) and MUST stay absolute. The
-- path LIKE '%/%' guard makes this idempotent — an already-relative basename has
-- no '/', so a re-run skips it.
UPDATE artwork
   SET path = replace(path, rtrim(path, replace(path, '/', '')), '')
 WHERE source IN ('fetched', 'uploaded') AND path LIKE '%/%';

UPDATE entity_artwork
   SET path = replace(path, rtrim(path, replace(path, '/', '')), '')
 WHERE source IN ('fetched', 'uploaded') AND path LIKE '%/%';
