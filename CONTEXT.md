# Juice Box

A fully self-hosted media server that organizes a household's video and music collection, presents it through a management web app, and streams it to connecting client apps. See [ADR-0001](./docs/adr/0001-fully-self-hosted-no-vendor-dependency.md) for the self-hosting posture.

## Language

**Library**:
A top-level collection of media of a single kind (e.g. a Movie library, a TV library, a Music library). Backed by one or more root folders on disk (multiple roots merge into one logical library); each folder is owned by exactly one Library, and a Library holds exactly one media kind. A server has many libraries.
_Avoid_: Collection (means something else — see Collection), Section.

**Media kind**:
The category a library holds: Movie, TV, or Music in v1. Photos is a planned future kind.
_Avoid_: Type, Format.

### Movie kind
**Movie**: A single film. The playable unit.

### TV kind
**Show** → **Season** → **Episode**. The Episode is the playable unit.
_Avoid_: Series (use Show).

### Music kind
**Artist** → **Album** → **Track**. The Track is the playable unit.
_Avoid_: Song (use Track).

## Organization

**Collection**:
An Admin-curated, unordered grouping of Titles for browsing/discovery (e.g. "A24 Films"), shared across Users subject to their library access and rating ceiling. Manual membership in v1.
_Avoid_: Playlist (that's ordered + personal), Group, Tag.

**Playlist**:
A User-owned, ordered list of Titles of a single media kind, for sequential playback. Manual in v1. A Playlist can be loaded *into* the Queue (the ephemeral, in-session playback list) — but the two are distinct: a Playlist is the saved, owned, persisted list a Queue can be built from.
_Avoid_: Collection.

## Physical / logical split

**Title**:
The logical media entity a user browses and selects to play. A Movie, an Episode, and a Track are each a Title. Carries descriptive metadata (name, artwork, cast).
_Avoid_: Item, Media (too vague).

**Edition**:
A specific version of a Title at a given quality or cut (e.g. "1080p", "4K", "Director's Cut"). A Title has one or more Editions; the player picks one to play.
_Avoid_: Version, Media (Plex's term — we don't use it).

**File**:
One physical file on disk. An Edition has one or more Files (more than one only for multi-part media, e.g. CD1/CD2). Carries technical attributes: container, codecs, resolution, bitrate, duration.
_Avoid_: Part, Asset.

**Stream**:
An elementary media stream inside a File's container — a video, audio, or subtitle Stream (FFmpeg's sense). A File has one or more. The audio Stream is itself the client-facing selectable unit a viewer picks in the player's Audio menu — deliberately *not* a coined compound like the Subtitle track, because embedded Streams are its only source; coin "Audio track" only if a second source (external dubs, commentary files) ever appears. Selecting a non-default audio Stream, or an image-based subtitle Stream, may escalate the playback tier. Symmetrically, when a File carries **more than one video Stream** (e.g. a black-and-white and a colour cut, or the same picture at two bitrates, sharing one set of audio Streams), the video Stream is likewise a client-facing selectable unit — picked in the player's Video menu, labelled by its embedded track title and falling back to resolution/bitrate. Same rule as audio: no coined "Video track" while embedded Streams are its only source (that noun is reserved for if a second source — e.g. a sibling video File — ever arrives). Unlike audio, a video Stream has no in-band switch: selecting a non-default one restarts the playback session (there is no user-selectable video rendition in HLS). Distinct from the music Track.
_Avoid_: Track (reserved for music), Audio track (no such concept — it's just the audio Stream), Video track (likewise — it's just the video Stream), Channel.

**Sidecar subtitle**:
A subtitle stored as a separate file next to the media, discovered by naming convention (`Movie.en.srt`). One of the three sources of a Subtitle track. Text-based sidecars are delivered as selectable tracks (SRT→WebVTT); image-based subtitles (whether sidecar or embedded Stream) are burned in during transcode.
_Avoid_: External sub.

**Subtitle track**:
The client-facing, selectable subtitle a viewer turns on for a Title — the union of three **sources**: an embedded subtitle Stream, a Sidecar subtitle, and a Fetched subtitle (downloaded from a provider). Always source-tagged, because the source decides delivery ([ADR-0020](./docs/adr/0020-subtitle-delivery-in-band-hls-out-of-band-track-image-burn-in.md)): text tracks (embedded text, text sidecars, fetched, and ASS/SSA downgraded to plain cues) are selectable and switch instantly as WebVTT; image tracks (embedded PGS/VOBSUB) burn in on explicit selection and escalate the playback tier. Distinct from the embedded Stream (one possible source) — a Subtitle track may have no Stream at all.
_Avoid_: Track (bare — reserved for the music Track; this concept is always the two-word compound), Caption, Sub.

**Fetched subtitle**:
A Subtitle track sourced by downloading from an external provider (OpenSubtitles in v1) for a Title that lacks one in the wanted language. Matched to the file by content hash (moviehash), then `imdb_id`, then filename; cached identity-keyed on the filesystem like artwork and never written into library folders ([ADR-0021](./docs/adr/0021-external-subtitle-fetching-mirrors-enrichment.md)). The third source of a Subtitle track, alongside the embedded Stream and the Sidecar subtitle; a local source outranks it at serve time.
_Avoid_: Downloaded sub, OpenSubtitles sub (provider-specific).

## Ingestion

**Scanner**:
The process that walks a library's folders and derives Titles, Editions, Files, and Streams from local on-disk information — the path for video, embedded tags for music — which is the authority for identity (see [ADR-0002](./docs/adr/0002-naming-convention-is-identity-authority.md)). Runs incrementally; triggered manually, on a schedule, or by best-effort filesystem watching ([ADR-0008](./docs/adr/0008-incremental-scan-soft-delete-missing-files.md)).
_Avoid_: Indexer, Crawler.

**Missing**:
The state of a File (or a Title with all Files absent) that is no longer on disk. Soft state, not deletion — it can return, and watch state survives it.
_Avoid_: Deleted, Removed, Orphaned.

**Enrichment**:
The separate, optional step that fetches descriptive metadata (artwork, descriptions, cast) from external Metadata providers to decorate a Title. Never affects identity; degrades gracefully when offline. Provider selection is server-wide by default but overridable per Library through its Enrichment policy ([ADR-0027](./docs/adr/0027-per-library-enrichment-policy-sparse-override.md)).
_Avoid_: Matching (that's identity), Agent.

**Needs-review**:
State of a Title or Show filed from a partial best-effort parse (e.g. a movie with no year, or a TV episode with non-SxxExx numbering). Browsable, but surfaced in an Admin attention list for confirmation. The Admin resolves an entry by dismissing it (mark reviewed — confirms the parse is fine; sticky across rescans) or, for a Movie, correcting its identity via a folder-keyed fix-match.
_Avoid_: Pending, Unconfirmed.

**Unmatched**:
A recognized media File from which no minimal identity could be extracted. Listed for the Admin to match manually; not a browsable Title and never auto-guessed. Distinct from Missing (which is about absence from disk).
_Avoid_: Unidentified, Unknown.

**Locked field**:
A descriptive field an Admin has edited by hand. Re-enrichment refreshes only unlocked fields and never overwrites a locked one; a lock is releasable back to auto.
_Avoid_: Pinned, Override (override means identity, below).

**Match override**:
An Admin's identity correction — fix-match, merge, or split — that overrules the convention-derived guess and persists across rescans (so the next scan doesn't undo it).
_Avoid_: Remap, Manual match.

**Enrichment override**:
An Admin's correction of *which external provider record* an entity is enriched from, overruling enrichment's automatic match — without changing identity or watch state. The common "edit item" correction: the item is filed correctly but was decorated from the wrong record (wrong poster, wrong overview), so the Admin re-points it at the right one and it re-enriches. The identity sibling of Match override; the two are distinct operations with different blast radii (see [ADR-0002](./docs/adr/0002-naming-convention-is-identity-authority.md), [ADR-0014](./docs/adr/0014-watch-state-keyed-to-parsed-identity.md)). Operates on one entity's matched record; contrast the per-Library Enrichment policy, which selects *providers*, not records.
_Avoid_: Match (reserved for identity), enrichment match (the old code name), Enrichment policy (that's the per-Library provider layer).

**Edit item**:
The Admin affordance for correcting a browsable item, exposing three separated actions ([ADR-0019](./docs/adr/0019-item-editing-preserves-local-identity.md)): **Fix info** (search a provider for the right record → Enrichment override; the common case), **Wrong item** (the file is a genuinely different work → Match override, Movie/Show only, resets watch state and clears Locked fields), and **Fix label** (hand-edit fields or pick an image → Locked field, per-item). The admin's choice of action, not inference, decides whether identity changes.
_Avoid_: Match editor, Metadata editor (too narrow — it also corrects identity).

**Cascade**:
Applying a Fix-info or Wrong-item correction to a parent's children as well ("apply to children" — opt-in). Album→tracks and Show→episodes map positionally; Artist→albums map by title, then recurse into tracks. Best-effort: a child's own Enrichment override or Locked field wins, and children that don't line up are surfaced in the Admin attention list, never silently changed.
_Avoid_: Propagate, Recurse.

## Metadata providers

**Metadata provider**:
An external public source of descriptive metadata (artwork, descriptions, cast) used by Enrichment — never identity. Each is registered for one or more media kinds and is either a Full provider or an Artwork-only provider. Enabled and credentialed server-wide.
_Avoid_: Agent (Plex/Kodi's term), Source (reserved for Artwork source), Scraper.

**Full provider**:
A Metadata provider that supplies complete descriptive records — titles, overviews, cast, dates, artwork — and is therefore eligible to lead a Library's Enrichment as its Authoritative provider (TMDB, OMDb, TheTVDB, MusicBrainz, AniDB).
_Avoid_: Full source (collides with Artwork source).

**Artwork-only provider**:
A Metadata provider that supplies only images, so it can never lead — only ever act as a Supplement (fanart.tv, Cover Art Archive, TheAudioDB).
_Avoid_: Art source, Image provider.

**Authoritative provider**:
The single Full provider that leads a Library's Enrichment, supplying the canonical record the Supplements fill around. A per-Library choice that inherits a global default per media kind (TMDB for video, MusicBrainz for music) and can be repointed through the Library's Enrichment policy. When enrichment is on it always runs — even if that provider is disabled for general use — as long as it is usable (credentialed). Constrained to Full providers of the Library's kind ([ADR-0027](./docs/adr/0027-per-library-enrichment-policy-sparse-override.md)).
_Avoid_: Primary, Master, Agent.

**Supplement**:
A Metadata provider that only fills descriptive fields the Authoritative provider left empty, never overriding it — every enabled provider that isn't the Authoritative provider, Artwork-only providers included. Runs fill-only.
_Avoid_: Fallback, Secondary, Scraper.

**Enrichment policy**:
A Library's sparse set of overrides to the server-wide enrichment configuration — whether to enrich at all, the metadata language, the Library's Authoritative provider, and per-provider enable/disable. Empty by default: unset keys inherit the global configuration live, and each override is an independent, surviving delta ([ADR-0027](./docs/adr/0027-per-library-enrichment-policy-sparse-override.md)). Distinct from the per-item Enrichment override, which repoints a single entity's matched record.
_Avoid_: Provider override (too narrow — it also carries language and on/off), Provider profile, Enrichment override (that's the per-item record correction).

## Artwork

**Artwork role**:
The slot an image fills for an entity: **Poster** and **Background** for a Movie or Show, **Artist photo** for an Artist, **Album cover** for an Album. An entity has one image per role at a time (the one shown). The role is the axis the Edit-item artwork tabs and the provider candidate lists are keyed on.
_Avoid_: Backdrop (use Background), Fanart, Thumb.

**Artwork source**:
The provenance of the image filling a role — one of three: **Local** (a file in the library folder, discovered by the Scanner), **Fetched** (auto-downloaded during Enrichment, including a provider image an Admin picks — stored Fetched-and-Locked), or **Uploaded** (see below). When a role has images from more than one source, serve precedence is **Uploaded > Local > Fetched** ([ADR-0026](./docs/adr/0026-user-uploaded-artwork-upload-is-select-top-precedence.md)): an Admin's Upload is the deliberate "show exactly this" choice and outranks even Local folder art, while a picked provider image is Fetched and so still loses to Local like any fetched image.
_Avoid_: Origin, Provider (a provider is one supplier of Fetched candidates, not the source axis).

**Artwork candidate**:
One selectable image offered for a role, shown as a thumbnail in an artwork tab. Provider candidates are queried **live** from the metadata providers each time the tab opens (TMDB posters/backdrops, Cover Art Archive covers, fanart.tv/TheAudioDB artist photos) and are never persisted; only the image an Admin selects is stored. Distinct from the resolved Artwork (the single image actually serving the role).
_Avoid_: Option, Variant.

**Uploaded artwork**:
An image an Admin supplies through an artwork tab (drag-drop or Browse) instead of choosing a provider candidate ([ADR-0026](./docs/adr/0026-user-uploaded-artwork-upload-is-select-top-precedence.md)). Uploading **is** selecting — it applies immediately: the image is stored in the identity-keyed artwork cache (never in the library folder), fills the role, and Locks it, winning over every other Artwork source. Releasing the artwork Lock deletes the Uploaded image and reverts the role to auto (Fetched/Local). Admin-only.
_Avoid_: Custom art, Manual image (a picked provider image is also manual).

## Users & access

**User**:
A person with credentials on this Server (stored locally per [ADR-0001](./docs/adr/0001-fully-self-hosted-no-vendor-dependency.md)). Has one role and their own private watch state.
_Avoid_: Account, Profile.

**Admin**:
A User role that can manage libraries, trigger scans, change settings, and create/remove Users.

**Member**:
A User role that can only browse and play, limited to the libraries granted to them and within their content-rating ceiling. May additionally trigger an on-demand subtitle fetch, which spends the shared provider quota ([ADR-0021](./docs/adr/0021-external-subtitle-fetching-mirrors-enrichment.md)) — the one outward, cost-bearing action a Member has.

**Watch state**:
Per-(User, Title) playback data: resume position, watched/unwatched, personal rating, the Remembered audio, and the Remembered video. Belongs to the User, never to the Title.
_Avoid_: Progress, History (history is a future, separate concept).

**Remembered audio**:
A User's explicit audio Stream pick for a Title, stored in Watch state and reapplied on the next play. Keyed to what the pick *means* (language plus distinguishing traits like a commentary label), not to a stream index, so it survives file replacement and Edition switches and degrades gracefully to the default when no match exists ([ADR-0023](./docs/adr/0023-per-user-audio-memory-two-level-language-keyed.md)). Two-level: an Episode's pick also bubbles up as the Show's remembered audio for Episodes without their own pick. Outranks the client-sent preferred audio language. Subtitle choice, by contrast, has no memory in v1.
_Avoid_: Audio preference (that's the client-sent language hint), Last audio.

**Remembered video**:
A User's explicit video Stream pick for a Title, stored in Watch state and reapplied on the next play — the direct mirror of the Remembered audio for the video Stream, applying only to Titles whose File carries more than one selectable video Stream (e.g. Spider Noir's black-and-white vs colour cut). Keyed to what the pick *means* — the video Stream's title tag, falling back to its resolution/bitrate — never to a stream index, so it survives re-rips and remuxes. Two-level like the Remembered audio: an Episode's pick bubbles up as the Show's remembered video for Episodes without their own pick, so a per-Show style choice sticks across a season. It selects *which* video content plays; the Capability profile still governs how it is delivered (a remembered pick never forces the wrong content, only the chosen one). Resolution order: Title's remembered video → Show's remembered video → the default video Stream (capability-then-quality heuristic, as for Editions) → default disposition → first. Unlike the Remembered audio it has no client-sent hint to outrank (there is no preferred-video signal). Subtitle choice still has no memory in v1.
_Avoid_: Video preference (no such client hint exists), Last video, Quality preference.

**Watched threshold**:
Core server constant: crossing ~90% played marks a Title watched (removing it from Continue Watching and advancing TV Up Next); below a ~2% floor it counts as not started. Not per-User configurable in v1.
_Avoid_: Completed.

**Continue Watching / Up Next / Recently Added**:
Per-User computed views (not stored entities), filtered by the User's library access and rating ceiling. Continue Watching = Titles between the 2% floor and 90% ceiling; Up Next = next unwatched Episode in Show order; Recently Added = Titles with Files added recently.
_Avoid_: On Deck (Plex's term), Home row.

**Content rating**:
The age/maturity classification of a Title (e.g. PG-13, TV-MA), sourced from metadata.

**Rating ceiling**:
A per-User cap on Content rating; Titles above the ceiling are hidden from that User.
_Avoid_: Parental control.

## Playback

**Capability profile**:
The declaration a client sends when requesting playback: supported containers, codecs, max resolution/bitrate, and current bandwidth. The server uses it to choose a playback tier (see [ADR-0003](./docs/adr/0003-three-tier-playback-with-capability-negotiation.md)).
_Avoid_: Client info, Device profile.

**Direct play**:
Streaming a File's bytes unchanged because the client can play it as-is.

**Direct stream**:
Repackaging a File's container (or swapping a Stream) on the fly without re-encoding.
_Avoid_: Remux (use as a parenthetical only).

**Transcode**:
Re-encoding video and/or audio in real time via FFmpeg so a client can play it.

**Playback session**:
A single active stream from server to one client, including the chosen tier and (if transcoding) the running FFmpeg job and its resource cost.
_Avoid_: Stream, Transcode job (those are parts of it).

**Queue**:
The ordered, in-session list of Titles the player walks; the entry at the current position is playing now and the rest are up next. Built on the fly from a play context (an Album from a Track forward, a Show from an Episode forward, a Playlist, or a single Title) and mutable in real time (reorder/append/remove) without interrupting the current Title; emptying it stops playback. By default the walk stops cleanly at the last entry (no loop); Shuffle mode and Repeat mode modify it. Walked by the persistent Now Playing player, which survives navigation ([ADR-0018](./docs/adr/0018-persistent-shell-owned-player.md)). Client-side and per playback session — not persisted server-side, not shared across Devices.
_Avoid_: Playlist (the saved, owned, persisted list a Queue can be built from), Up Next (the computed next-unwatched-Episode view).

**Shuffle mode**:
A Queue walk-order modifier, music only. Non-destructive: it randomizes the up-next entries while the now-playing entry keeps playing and remembers the authored order, so turning it off restores that order from the current position. Distinct from a one-time reorder (which mutates the Queue and can't be undone).
_Avoid_: Randomize, Shuffle-play (as a separate mode).

**Repeat mode**:
A Queue walk-order modifier, music only, with three states: off (stop at the end), repeat-all (the last entry's natural end wraps to the first), and repeat-one (the current Title replays on its natural end). The sanctioned exceptions to the Queue's default stop-at-end. Manual next/prev always move normally — repeat-one never traps the skip controls.
_Avoid_: Loop, Repeat-track (use repeat-one).

## Connection

**Device**:
A first-class, named client installation belonging to a User (e.g. "Brandon's iPhone"). Holds a long-lived bearer token issued at login; listed per-User and individually revocable. Revoking a Device invalidates its token.
_Avoid_: Client (the app generically), Session (that's playback).
