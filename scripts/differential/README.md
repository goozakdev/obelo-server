# The differential run

    make differential

Boots **two** Obelo binaries — this tree's, and one built from a throwaway clone
checked out at the 2026-09-17 build (`cf5da34`) — against **one** set of local
stand-in metadata sources and **one** fixture library, and diffs what each of
them enriched and what each of them asked for.

It exists to close two success criteria of `.scratch/bundled-plugins/PRD.md`
that issue 08's walk could only *argue*:

| # | Criterion | What it rested on before |
| --- | --- | --- |
| A | "…enriches a movie and an album on the first pass exactly as the 2026-09-17 build does" | the ported unit suites, each asserting against its own canned JSON — never the two builds side by side |
| B | "…keeps every provider key, every per-Library override and every item pin; the Cover Art Archive row is gone; nothing re-enriches" | the Cover Art half is tested; the overrides/pins half was "by construction"; "nothing re-enriches" was unproven |

## What it does

1. **One stand-in, one port.** `standin/` serves TMDB (API + image host), OMDb,
   TheTVDB (login + search + series), MusicBrainz, the Cover Art Archive,
   fanart.tv and TheAudioDB from deterministic canned JSON lifted from the
   plugin unit tests, and counts every request by provider and by
   method+path+query (credentials redacted). AniDB is left unconfigured on both
   builds, because it ships with no seeded row.

   Every source is a **path prefix on a single loopback listener**, and that is
   load-bearing rather than tidy: the plugin host permits a guest's fetch when
   the target host is in the manifest allowlist *or* is the host of the URL the
   operator configured. MusicBrainz's second host (the Cover Art Archive, its
   `URL2` since issue 06) is not the configured target, so a stand-in for it on
   a port of its own would be refused by the allowlist *and* by the
   private-address rule. One host:port makes every mount the operator's own.

2. **One fixture library**, generated with ffmpeg: a movie, an episode under a
   show, and an album with an artist and three tagged tracks.

3. **Criterion A.** Each build gets a fresh data directory and the *same*
   environment (`OBELO_TMDB_*`, `OBELO_MUSICBRAINZ_*`, `OBELO_COVERART_BASE_URL`,
   `OBELO_FANART_TV_*`, `OBELO_THEAUDIODB_*`); OMDb and TheTVDB have no
   environment variables at all, so both are configured through
   `PUT /settings/metadata-providers`. Scan, settle, then every Title, Show,
   Season, Artist, Album and Track is read back through the API and diffed after
   normalizing row ids, clocks and artwork cache-bust tokens. The per-provider
   request paths are diffed as multisets.

4. **Criterion B.** Before the 2026-09-17 build is stopped it writes the things
   an upgrade must preserve: a non-default Cover Art base URL, a per-Library
   override naming `omdb`, a second Library's overrides, and a Show pinned to an
   external id. Then this tree's binary is booted on *that same data directory*,
   the counters are zeroed, and an Admin's ordinary rescan-and-enrich is run.

## Reading the result

Every assertion prints `PASS` / `FAIL` / `SKIP` with its criterion, and the
process exits non-zero on any `FAIL`. A **FINDING** is printed in full below the
table: a real difference, with the field, both values and the provider. The work
directory is deleted on success and kept on failure (and on
`DIFFERENTIAL_KEEP=1`), and the driver says where it is.

## What it is not

It is **not** part of `make check`. It needs a second checkout and two full Go
builds — minutes, not seconds — and `check` is a gate people have to be willing
to run. It needs `ffmpeg` on `PATH` and **no network**: every source it calls is
on loopback.

It never writes to `bin/`, never touches the developer's data directory, never
binds a fixed port and never modifies a tracked file. Its one prerequisite
inside the tree is `make plugins`, which writes gitignored build output that
`make check` writes anyway.

## Knobs

    DIFFERENTIAL_BASE=<sha>   compare against a different build (default cf5da34)
    DIFFERENTIAL_KEEP=1       keep the work directory on success

`driver/` can also be run by hand against binaries you already have:

    go build -o /tmp/standin ./scripts/differential/standin
    go build -o /tmp/driver  ./scripts/differential/driver
    /tmp/driver -old /tmp/obelo-old -new /tmp/obelo-new -standin /tmp/standin -keep

## One deliberate exception to "public surfaces only"

Everything the harness asserts is read through the HTTP API, with one exception
that the run itself discovered and reports as a finding: the 2026-09-17 build's
API **refuses** to create a per-Library override naming `coverart`
(`Catalog.SupplementProvidersForKind` offers only providers with `RequiresKey`,
and the Cover Art Archive is keyless), so no server driven through its API can
hold the row that issue 06's migration scrubs. To exercise the scrub anyway the
harness plants that one row directly, with the server stopped, through the
pure-Go SQLite driver the server itself uses — and says so, in the finding and
in `driver/coverartrow.go`. The scrub is then asserted against the
`library_provider_override` table, because the API view cannot tell "the row was
scrubbed" apart from "the provider left the catalog".
