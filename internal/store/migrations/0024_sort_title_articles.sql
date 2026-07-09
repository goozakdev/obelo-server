-- 0024_sort_title_articles: re-derive the persisted ordering keys so a leading
-- English article ("The ", "An ", "A ") no longer bunches titles under T/A. The
-- scanner's sortTitle() now strips those articles when computing sort_title/
-- sort_name (so future scans are correct); this backfills the rows already in the
-- catalog without forcing a full re-scan. Only the ordering key changes — the
-- display `title`/`name` columns are untouched.
--
-- Stored keys are already lower-cased and trimmed, so a case-insensitive match is
-- unnecessary. Exactly ONE article is stripped per row — the CASE picks the first
-- matching prefix and evaluates against the original value, mirroring the Go
-- sortTitle() (which returns after the first match). A single pass is essential:
-- stripping "the " from "the a list" would otherwise re-expose "a list" to a
-- later "a " pass and double-strip to "list", diverging from the scanner. "the "
-- is tested before "an " before "a " (longest first) and the substr offsets drop
-- the article + its trailing space; ltrim() folds any residual space. A bare
-- article is stored without a trailing space, so it never matches 'article %'.
UPDATE titles SET sort_title = CASE
    WHEN sort_title LIKE 'the %' THEN ltrim(substr(sort_title, 5))
    WHEN sort_title LIKE 'an %'  THEN ltrim(substr(sort_title, 4))
    WHEN sort_title LIKE 'a %'   THEN ltrim(substr(sort_title, 3))
END
WHERE sort_title LIKE 'the %' OR sort_title LIKE 'an %' OR sort_title LIKE 'a %';

UPDATE shows SET sort_title = CASE
    WHEN sort_title LIKE 'the %' THEN ltrim(substr(sort_title, 5))
    WHEN sort_title LIKE 'an %'  THEN ltrim(substr(sort_title, 4))
    WHEN sort_title LIKE 'a %'   THEN ltrim(substr(sort_title, 3))
END
WHERE sort_title LIKE 'the %' OR sort_title LIKE 'an %' OR sort_title LIKE 'a %';

UPDATE albums SET sort_title = CASE
    WHEN sort_title LIKE 'the %' THEN ltrim(substr(sort_title, 5))
    WHEN sort_title LIKE 'an %'  THEN ltrim(substr(sort_title, 4))
    WHEN sort_title LIKE 'a %'   THEN ltrim(substr(sort_title, 3))
END
WHERE sort_title LIKE 'the %' OR sort_title LIKE 'an %' OR sort_title LIKE 'a %';

UPDATE artists SET sort_name = CASE
    WHEN sort_name LIKE 'the %' THEN ltrim(substr(sort_name, 5))
    WHEN sort_name LIKE 'an %'  THEN ltrim(substr(sort_name, 4))
    WHEN sort_name LIKE 'a %'   THEN ltrim(substr(sort_name, 3))
END
WHERE sort_name LIKE 'the %' OR sort_name LIKE 'an %' OR sort_name LIKE 'a %';
