-- 0071_coverart_becomes_musicbrainz_url2: the Cover Art Archive stops being a
-- provider and becomes the MusicBrainz plugin's SECOND HOST (ADR-0059,
-- .scratch/bundled-plugins issue 06).
--
-- `coverart` was never a source. It had no client, nothing read its enable switch,
-- and its one live effect was the host MusicBrainz's album-cover URLs pointed at —
-- which the server resolved out of this row and into that provider's URL2 through a
-- special case in three different files. MusicBrainz is now a Bundled plugin whose
-- own manifest declares `settings.defaultUrl2: https://coverartarchive.org` and
-- whose allowlist carries that host, so the row has nothing left to do and the
-- special cases are gone.
--
-- What must NOT be lost is an operator's MIRROR. Somebody who pointed `coverart` at
-- https://mirror.example/caa said something, and a migration that dropped the row
-- would silently send every album cover back to the public host.

-- 1. CARRY THE OVERRIDE ACROSS, and only a real one.
--
-- The condition is "set, and not the default". A row holding the public URL is
-- carrying no decision — it is what the seed wrote — and copying it would turn an
-- absent override into a pinned one, which is the difference between "follow the
-- shipped default" and "always use exactly this", forever. An empty base_url is the
-- same non-decision spelled differently.
--
-- It is written only where MusicBrainz has no second host of its own yet, so an
-- operator who somehow already set one keeps it: the more specific setting wins.
UPDATE metadata_providers
   SET image_base_url = (SELECT ca.base_url
                           FROM metadata_providers AS ca
                          WHERE ca.slug = 'coverart'),
       updated_at = datetime('now')
 WHERE slug = 'musicbrainz'
   AND COALESCE(image_base_url, '') = ''
   AND EXISTS (SELECT 1
                 FROM metadata_providers AS ca
                WHERE ca.slug = 'coverart'
                  AND COALESCE(ca.base_url, '') NOT IN ('', 'https://coverartarchive.org'));

-- 2. THE ROW GOES.
--
-- A row whose slug no Plugin claims is never built and never shown, so leaving it
-- would be harmless and invisible — which is exactly why it has to go now rather
-- than at some later date when nobody remembers what it was: an operator reading
-- their own database should not find a provider this server has no code for.
DELETE FROM metadata_providers WHERE slug = 'coverart';

-- 3. AND THE PER-LIBRARY OVERRIDES THAT NAME IT.
--
-- library_provider_override is the per-Library Supplement tri-state (ADR-0027): a
-- row is a deliberate forced on/off, and its ABSENCE is "inherit". A forced on/off
-- for a source nobody can call is not an opinion any future Plugin claiming that
-- slug should inherit — which is the same rule store.DeletePlugin applies when an
-- Installed plugin is uninstalled (.scratch/plugin-system issue 17), and this is
-- that rule, run once, for a slug that is being retired rather than removed.
--
-- Every OTHER key in a Library's overrides is untouched: this is one DELETE with a
-- slug in its WHERE clause, not a rewrite of the set.
DELETE FROM library_provider_override WHERE provider = 'coverart';

-- There is deliberately NO clause for library_enrichment_policy.authoritative_provider.
-- `coverart` was registered artwork-only (ADR-0027 Class), so the settings API
-- could never have accepted it as a Library's Authoritative provider, and a row
-- naming it would be a row this schema has no way to have produced.
