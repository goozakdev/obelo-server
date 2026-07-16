# Naming Convention

The on-disk naming convention is the authority for media identity ([ADR-0002](./adr/0002-naming-convention-is-identity-authority.md)). The Scanner derives Titles, Editions, Files, and Streams from it deterministically and offline. This document is the precise grammar.

## Principle: folder is the primary identity source

Identity is carried by **folder structure**, not by filenames alone. Each Title (or Show/Album) lives in its own folder; the folder name carries the primary identity (title + year), and filenames carry only detail (Edition, part, stream). Co-locating posters/subtitles, holding multiple Editions together, and anchoring a Match override all depend on this.

- **Movie:** one folder per movie — `Dune (2021)/Dune (2021).mkv`. The folder name is the identity source; the main video file ideally repeats it.
- **TV:** `Show (Year)/Season NN/…episodes…`.
- **Music:** `Artist/Album (Year)/…tracks…`.

A bare file at a library root (`Dune (2021).mkv`, no folder) is accepted as a single-file Movie fallback, but folder-based layout is required to use Editions, extras, or co-located subtitles.

## Identity key & disambiguation

- **Movie identity key = normalized title + `(YYYY)` year.** Normalization is case-insensitive with punctuation/whitespace folded. The year is part of identity (`Dune (2021)` ≠ `Dune (1984)`), not decoration.
- **Optional embedded external id** in the folder name: `Dune (2021) {tmdb-438631}` or `{imdb-tt1160419}`. When present it *is* the identity (overrides title+year parsing) and is also the Enrichment key. This is the escape hatch for same-title-same-year collisions and for pinning the exact external record.
- **Embedded id vs Match override:** the embedded id is operator-authored *in the filename*. A Match override ([ADR-0002](./adr/0002-naming-convention-is-identity-authority.md)) is app-managed *in the DB, keyed to the folder path*, letting a user fix a match in the web app **without renaming files**. Both express "this is really X"; they are different mechanisms for different users.

## Editions, parts, and extras within a Movie folder

All main video files in a movie folder belong to the one Title. They are sorted into Editions as follows:

- **Named cut** — explicit tag `Dune (2021) {edition-Director's Cut}.mkv` → an Edition named "Director's Cut".
- **Quality-distinguished Edition** — with no edition tag, the Edition is inferred from parsed tokens: `Dune (2021) - 2160p.mkv` and `Dune (2021) - 1080p.mkv` → two Editions auto-labeled by resolution/source. Distinct resolution/source automatically creates a distinct Edition; no explicit tag required.
- **Multi-part** — a part suffix joins multiple Files into *one* Edition that plays back-to-back: `… - part1` / `… - part2` (aliases `cd1`/`pt1`).
- **Collision rule** — two files that parse to the same Edition identity and are not parts are flagged ambiguous in the web app, never silently guessed.

## Extras and ignored files

- **Extras** are recognized by a named subfolder — `Extras/`, `Trailers/`, `Behind The Scenes/`, `Featurettes/`, `Deleted Scenes/`, `Interviews/`, `Shorts/`, `Other/` — or by a filename suffix (`-trailer`, `-behindthescenes`, `-deleted`, `-featurette`, …). An extra attaches to the parent Title with an extra-type, is **not** a Title itself, and never appears in browse/search/Continue Watching/Up Next.
- **Sample/junk** — files matching `-sample` / `sample.*`, and files below a size floor — are ignored entirely (not even extras).
- **Recognized media extensions are an allowlist** (e.g. video `.mkv .mp4 .m4v .avi .mov .ts .webm`; audio `.flac .mp3 .m4a .ogg .opus .wav`). Anything not on the allowlist is ignored.

## TV: seasons and episodes

- **Canonical layout:** `Show (Year)/Season NN/Show (Year) - S01E05 - Title.ext`. The `SxxExx` token (case-insensitive) is the authoritative Episode identity; the folder confirms the season.
- **Season folders:** `Season NN`; **Specials = `Season 00`** (a `Specials/` folder is an accepted alias).
- **Multi-episode file:** `S01E05-E06` maps one File to both Episodes — watching it marks both; it plays once.
- **Date-based episodes** (daily/talk shows): a `YYYY-MM-DD` token is accepted as episode identity by air date.
- **Absolute numbering** (anime): a bare absolute episode token is accepted.
- **Degradeable mapping:** date-based and absolute-numbered episodes are filed offline using their raw token as identity and remain playable; mapping them onto canonical season/episode numbers requires Enrichment. Offline they show labeled by date / absolute number.

## Music: embedded tags are authority

For music, **embedded tags are the identity authority**, not the path (see amended [ADR-0002](./adr/0002-naming-convention-is-identity-authority.md)). Tags are local and offline — just a different local source than the filename.

- **Identity from tags:** Artist, Album, Album Artist, Disc #, Track #, Track title come from ID3v2 / Vorbis comments / MP4 atoms.
- **Grouping:** **Album Artist** (falling back to Artist) groups Albums, so compilations and "Various Artists" albums file correctly under one Album rather than fragmenting per track artist.
- **Path is fallback only** — when tags are missing, the scanner falls back to `Artist/Album (Year)/NN - Title.ext` folder/filename parsing.

## Co-located assets: subtitles and artwork

Local on-disk assets are honored before, and instead of, external Enrichment. **Local always wins; Enrichment only fills what is absent** ([ADR-0001](./adr/0001-fully-self-hosted-no-vendor-dependency.md)).

### Sidecar subtitles
`<media-basename>.<lang>[.<flag>].<ext>` — e.g. `Dune (2021).en.srt`, `Dune (2021).en.forced.srt`, `Dune (2021).es.sdh.ass`.
- `lang` = ISO-639 code (`en`/`eng`).
- `flag` ∈ `forced`, `sdh`, `cc` (optional).
- `ext` ∈ text `srt .ass .ssa .vtt` (delivered as selectable tracks) and image-based `.sup .sub/.idx` (burn-in path, Q13).
- Matched to the media file sharing the basename, or to the sole video in the folder.

### Local artwork
- **Movie:** `poster.jpg`/`cover.jpg` → poster; `fanart.jpg`/`backdrop.jpg` → background; also `<basename>-poster.jpg`.
- **Music:** `cover.jpg`/`folder.jpg` in the album folder, with embedded cover art in the audio tags as fallback.
- **TV:** in the **Show** folder — `poster.jpg`/`cover.jpg` → the Show's poster; `fanart.jpg`/
  `backdrop.jpg` → its background; `Season NN.jpg` → that season's poster. The season image sits
  beside the season folder, not inside it, and is named by the same grammar as the folder — so
  `Season 1.png` and `Specials.jpg` (season 0) work too. A `Season NN.jpg` naming a season no
  media backs is ignored rather than conjuring an empty Season.

## Unmatched and needs-review files

A recognized-extension file that doesn't cleanly match is never silently dropped. Two tiers:

- **Filed + needs-review** — a best-effort parse extracts enough for an identity (a title without a year; partial music tags). Filed as a Title, browsable, and marked **needs-review** in an Admin attention list. A yearless movie is filed title-only and flagged (Enrichment may mismatch without a year).
- **Unmatched** — no minimal identity can be extracted. The File goes to an Unmatched list: visible to the Admin, manually matchable (writing a Match override), but not a browsable Title and never auto-guessed into one.

Determinism is preserved: best-effort parsing only ever yields a local flagged result or the Unmatched bucket — a low-confidence *external* guess is never committed as identity.

## Identity stability across renames and re-encodes

Watch state is keyed to the **parsed Title identity** (per-Title, not per-Edition or per-File) — see [ADR-0014](./adr/0014-watch-state-keyed-to-parsed-identity.md). Therefore:

- Renaming a file to fix it (scanner sees missing + new) re-resolves to the same Title → history survives.
- Swapping a 1080p rip for a 4K one is a new Edition under the same Title → history survives.
- Moving the whole library changes only paths, not identities → history survives.

Match overrides stay keyed to the folder path; renaming/moving a folder drops its override (the user is re-asserting identity), and orphaned overrides are surfaced in the Admin attention list.
