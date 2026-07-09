-- 0025_uninvert_display_titles: some sources store a title with its leading
-- article trailing behind a comma ("Island, The") so it files under its real
-- word. After 0024 those sort correctly but now DISPLAY inconsistently — "Island,
-- The" next to "The Lord of the Rings". The scanner's ParseIdentity now rewrites
-- the display title to natural order ("The Island") at scan time; this backfills
-- rows already in the catalog without a re-scan.
--
-- Applies to movies/episodes/tracks (titles), shows, albums, and artists.
--
-- identity_key is deliberately NOT touched: it is the rescan dedup key, and the
-- scanner keys on the parsed (still-inverted) title / tags, so leaving it as-is
-- keeps existing rows matched instead of orphaning them into duplicates. Only the
-- display column (`title`/`name`) and its article-stripped sort key change.
--
-- Each row is rewritten once by a CASE evaluated against the original value (no
-- cascade): drop the trailing ", The"/", An"/", A" (5/4/3 chars incl. the comma
-- and space) and re-emit the article, canonically cased, at the front. The new
-- sort_title is the lower-cased remainder — which equals what the Go sortTitle()
-- produces for the natural-order title, since it strips that same leading
-- article. LIKE is ASCII-case-insensitive in SQLite, so any casing is caught and
-- normalized. Titles without the comma-article suffix are left untouched.
UPDATE titles SET
    title = CASE
        WHEN title LIKE '%, the' THEN 'The ' || rtrim(substr(title, 1, length(title) - 5))
        WHEN title LIKE '%, an'  THEN 'An '  || rtrim(substr(title, 1, length(title) - 4))
        WHEN title LIKE '%, a'   THEN 'A '   || rtrim(substr(title, 1, length(title) - 3))
        ELSE title
    END,
    sort_title = CASE
        WHEN title LIKE '%, the' THEN lower(rtrim(substr(title, 1, length(title) - 5)))
        WHEN title LIKE '%, an'  THEN lower(rtrim(substr(title, 1, length(title) - 4)))
        WHEN title LIKE '%, a'   THEN lower(rtrim(substr(title, 1, length(title) - 3)))
        ELSE sort_title
    END
WHERE title LIKE '%, the' OR title LIKE '%, an' OR title LIKE '%, a';

UPDATE shows SET
    title = CASE
        WHEN title LIKE '%, the' THEN 'The ' || rtrim(substr(title, 1, length(title) - 5))
        WHEN title LIKE '%, an'  THEN 'An '  || rtrim(substr(title, 1, length(title) - 4))
        WHEN title LIKE '%, a'   THEN 'A '   || rtrim(substr(title, 1, length(title) - 3))
        ELSE title
    END,
    sort_title = CASE
        WHEN title LIKE '%, the' THEN lower(rtrim(substr(title, 1, length(title) - 5)))
        WHEN title LIKE '%, an'  THEN lower(rtrim(substr(title, 1, length(title) - 4)))
        WHEN title LIKE '%, a'   THEN lower(rtrim(substr(title, 1, length(title) - 3)))
        ELSE sort_title
    END
WHERE title LIKE '%, the' OR title LIKE '%, an' OR title LIKE '%, a';

-- Albums mirror titles/shows (title + sort_title).
UPDATE albums SET
    title = CASE
        WHEN title LIKE '%, the' THEN 'The ' || rtrim(substr(title, 1, length(title) - 5))
        WHEN title LIKE '%, an'  THEN 'An '  || rtrim(substr(title, 1, length(title) - 4))
        WHEN title LIKE '%, a'   THEN 'A '   || rtrim(substr(title, 1, length(title) - 3))
        ELSE title
    END,
    sort_title = CASE
        WHEN title LIKE '%, the' THEN lower(rtrim(substr(title, 1, length(title) - 5)))
        WHEN title LIKE '%, an'  THEN lower(rtrim(substr(title, 1, length(title) - 4)))
        WHEN title LIKE '%, a'   THEN lower(rtrim(substr(title, 1, length(title) - 3)))
        ELSE sort_title
    END
WHERE title LIKE '%, the' OR title LIKE '%, an' OR title LIKE '%, a';

-- Artists carry the display string in `name` and the sort key in `sort_name`.
UPDATE artists SET
    name = CASE
        WHEN name LIKE '%, the' THEN 'The ' || rtrim(substr(name, 1, length(name) - 5))
        WHEN name LIKE '%, an'  THEN 'An '  || rtrim(substr(name, 1, length(name) - 4))
        WHEN name LIKE '%, a'   THEN 'A '   || rtrim(substr(name, 1, length(name) - 3))
        ELSE name
    END,
    sort_name = CASE
        WHEN name LIKE '%, the' THEN lower(rtrim(substr(name, 1, length(name) - 5)))
        WHEN name LIKE '%, an'  THEN lower(rtrim(substr(name, 1, length(name) - 4)))
        WHEN name LIKE '%, a'   THEN lower(rtrim(substr(name, 1, length(name) - 3)))
        ELSE sort_name
    END
WHERE name LIKE '%, the' OR name LIKE '%, an' OR name LIKE '%, a';
