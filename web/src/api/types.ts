// Wire types for the /api/v1 surface (docs/api-contract.md). camelCase,
// matching the server's JSON exactly. Only the shapes this slice needs are
// declared; later slices extend this file as endpoints come online.

/** The handshake payload from `GET /api/v1/server`. */
export interface ServerInfo {
  version: string;
  supportedVersions: number[];
  features: Record<string, boolean>;
  setupRequired: boolean;
}

/** A user's role. The backend is single-Admin today (Members deferred), but the
 * client models the role explicitly so the role gate works unchanged when
 * Member support lands (PRD "Role/user reality"). Unknown future roles are
 * tolerated as plain strings. */
export type Role = "admin" | "member" | (string & {});

/** A User as the auth + Admin user-management endpoints return it
 * (`{ id, username, role }`). */
export interface User {
  id: string;
  username: string;
  role: Role;
}

/** Response shape of `GET /api/v1/users` (Admin) — the list is wrapped. */
export interface UsersResponse {
  users: User[];
}

/** Raw `GET /api/v1/users/{id}` (Admin) — one User's full detail. The server
 * omits an empty `libraryIds` and an absent/`null` `ratingCeiling` (`omitempty`),
 * so both are optional on the wire; the client NORMALIZES the holes
 * ({@link UserDetail}). */
export interface UserDetailRaw {
  id: string;
  username: string;
  role: Role;
  /** The Libraries this Member is granted; absent/empty = no grants (sees no
   * catalog). Meaningless for an Admin (implicitly all-access). */
  libraryIds?: string[] | null;
  /** The Member's Rating ceiling label, or absent/`null` = uncapped. Carried here
   * (issue 02 fetches the detail for `libraryIds`); the ceiling control is issue
   * 03. */
  ratingCeiling?: string | null;
}

/** A User's full detail with its `omitempty` holes filled (`libraryIds` → [],
 * `ratingCeiling` → ""): the shape the per-User row consumes. `GET /users/{id}`
 * carries BOTH access dimensions — the granted Libraries (issue 02) and the
 * Rating ceiling (issue 03) — so the type holds both now even though this slice
 * only renders grants. An empty `libraryIds` means the Member sees no catalog; an
 * empty `ratingCeiling` means uncapped. */
export interface UserDetail {
  id: string;
  username: string;
  role: Role;
  libraryIds: string[];
  ratingCeiling: string;
}

/** Request body for `POST /api/v1/users` (Admin): create a User. `role` is
 * optional and defaults to "member" server-side; pass "admin" to deliberately
 * mint another Admin. A blank username/password is rejected (400 BAD_REQUEST);
 * a duplicate username is rejected (409 USERNAME_TAKEN). */
export interface CreateUserInput {
  username: string;
  password: string;
  role?: Role;
}

/** A Device as login returns it. The client persists a stable `clientId` (UUID)
 * so re-login reuses the same Device (docs/api-contract.md, ADR-0015). */
export interface Device {
  id: string;
  name: string;
  platform: string;
  clientId: string;
  createdAt?: string;
  lastSeenAt?: string;
}

/** The client-supplied Device descriptor on `POST /auth/login`. */
export interface DeviceInput {
  name: string;
  platform: string;
  clientId: string;
}

/** Request body for `POST /api/v1/setup` (first-Admin bootstrap). */
export interface SetupRequest {
  claimToken: string;
  username: string;
  password: string;
}

/** Response from `POST /api/v1/setup` — the created first Admin. */
export interface SetupResult {
  user: User;
}

/** Request body for `POST /api/v1/auth/login`. */
export interface LoginRequest {
  username: string;
  password: string;
  device: DeviceInput;
}

/** Response from `POST /api/v1/auth/login` — the bearer token + identity. */
export interface LoginResult {
  token: string;
  user: User;
  device: Device;
}

/** The standard error envelope: `{ "error": { code, message, details } }`. */
export interface ErrorEnvelope {
  error: {
    code: string;
    message: string;
    details?: Record<string, unknown>;
  };
}

/** Response shape of `GET /devices` (the caller's Devices; list wrapped). */
export interface DevicesResponse {
  devices: Device[];
}

// --- Admin: attention surfaces & devices (issue 07) ------------------------
//
// These mirror the server's JSON exactly (catalog_handlers.go Unmatched,
// match_handlers.go overrides/fix-match). The server omits empty strings and a
// false `orphaned` (`omitempty`), so the client NORMALIZES those holes: a string
// that may be absent becomes "" and `orphaned` becomes a present boolean (absent
// → false). The `Normalized*` shapes below are what components actually receive.

/** Raw `GET /libraries/{id}/unmatched` entry. `reason`/`addedAt` are absent when
 * empty (server `omitempty`). */
export interface UnmatchedFileRaw {
  id: string;
  path: string;
  reason?: string;
  addedAt?: string;
}

/** An Unmatched file with its `omitempty` holes filled (reason → ""). A
 * recognized media file the scanner could not turn into a Title — Admin-visible,
 * manually matchable, never a browsable Title (CONTEXT.md). */
export interface UnmatchedFile {
  id: string;
  path: string;
  reason: string;
  /** RFC3339, or undefined when the server omitted it. */
  addedAt?: string;
}

/** Raw `GET /libraries/{id}/overrides` entry / the `POST .../fix-match` result.
 * `year`/`tmdbId`/`imdbId`/`orphaned`/`createdAt` are absent when zero/empty/
 * false (server `omitempty`). */
export interface MatchOverrideRaw {
  id: string;
  folderPath: string;
  title: string;
  year?: number;
  tmdbId?: string;
  imdbId?: string;
  identityKey: string;
  orphaned?: boolean;
  createdAt?: string;
}

/** A Match override with its `omitempty` holes filled (year → 0, `orphaned` →
 * present boolean): a persisted Admin identity correction for a folder. An
 * `orphaned` override is one whose anchor folder was renamed/moved — surfaced,
 * never lost (ADR-0002/0014). The list view highlights these. */
export interface MatchOverride {
  id: string;
  folderPath: string;
  title: string;
  /** 0 when the override carries no year. */
  year: number;
  tmdbId?: string;
  imdbId?: string;
  identityKey: string;
  /** True when the anchor folder no longer exists on disk. */
  orphaned: boolean;
  /** RFC3339, or undefined when the server omitted it. */
  createdAt?: string;
}

/** Request body for `POST /api/v1/libraries/{id}/fix-match`: the folder the
 * override anchors to plus at least one corrected identity signal (title+year or
 * an embedded external id). */
export interface FixMatchInput {
  folderPath: string;
  title?: string;
  year?: number;
  tmdbId?: string;
  imdbId?: string;
}

/** Raw `GET /libraries/{id}/enrichment-attention` entry: one Title whose
 * Enrichment could not settle on a record (status unmatched/failed), awaiting a
 * hand-match. `year` is absent when 0 (server `omitempty`). */
export interface EnrichmentAttentionTitleRaw {
  id: string;
  kind: string;
  title: string;
  year?: number;
  enrichmentStatus: EnrichmentStatus;
}

/** An enrichment-attention Title with its `omitempty` holes filled (year → 0):
 * a browsable Title missing its descriptive metadata, listed for the Admin to
 * correct via {@link ApiClient.setEnrichmentMatch}. Distinct from the identity
 * Unmatched files and needs-review Titles. */
export interface EnrichmentAttentionTitle {
  id: string;
  kind: string;
  title: string;
  /** 0 when the Title has no year. */
  year: number;
  enrichmentStatus: EnrichmentStatus;
}

/** Raw entry from `GET /api/v1/libraries/{id}/needs-review`: a Movie / Episode /
 * Track / Show the scanner flagged as an uncertain identity parse (no year, or
 * non-SxxExx episode numbering). `folderPath` is present only for a Movie (so a
 * folder-keyed fix-match can be offered); `year` is absent when 0. */
export interface NeedsReviewItemRaw {
  id: string;
  kind: string;
  title: string;
  year?: number;
  folderPath?: string;
}

/** A needs-review item with its `omitempty` holes filled (year → 0, folderPath →
 * ""). The Admin resolves it by dismissing it (mark reviewed) or, for a Movie,
 * correcting the identity via a folder-keyed fix-match. Distinct from the
 * enrichment metadata-match list. */
export interface NeedsReviewItem {
  id: string;
  /** "movie" | "episode" | "track" | "show". */
  kind: string;
  title: string;
  /** 0 when the item has no year. */
  year: number;
  /** The Movie folder a fix-match targets, or "" when fix-match does not apply. */
  folderPath: string;
}

/** Body of `PUT /api/v1/titles/{id}/enrichmentMatch`: the external id an Admin
 * assigns to correct a wrong/missing metadata match. At least one id is required.
 * Setting it re-enriches just that Title and NEVER touches identity (distinct from
 * fix-match). */
export interface EnrichmentMatchInput {
  tmdbId?: string;
  imdbId?: string;
  musicbrainzId?: string;
}

/** One provider search result in the Edit-item "Fix info" picker (CONTEXT.md
 * "Enrichment override", ADR-0019): enough for an Admin to disambiguate two
 * same-named works before applying — the authoritative externalId to pin, the
 * source title + year, a thumbnail, and a disambiguation hint. Wire shape of
 * `GET /titles/{id}/enrichmentCandidates`. */
export interface EnrichmentCandidate {
  externalId: string;
  title: string;
  year?: number;
  thumbnailUrl?: string;
  disambiguation?: string;
  kind: string;
  /** A short record-type badge ("Album · Soundtrack", "Group") that disambiguates
   * same-titled hits (item-editing/search-improvements). Absent when none. */
  typeLabel?: string;
  /** An ALBUM candidate's ordered track preview (disc/position/title), so an Admin
   * can confirm the positional map before applying (ADR-0019). Absent otherwise. */
  tracklist?: CandidateTrack[];
}

/** One track in an album candidate's tracklist preview. */
export interface CandidateTrack {
  disc?: number;
  position: number;
  title: string;
}

/** Optional narrowing + paging for an Edit-item provider search (item-editing/
 * search-improvements): `artist` AND-narrows a music album/track search to a
 * specific artist (pre-filled from the item's parsed artist); `page` (0-based)
 * offsets a broad common-title query by whole pages so "show more" works. */
export interface EnrichmentSearchOptions {
  artist?: string;
  page?: number;
}

/** Response of `GET /api/v1/titles/{id}/enrichmentCandidates?q=…`. */
export interface EnrichmentCandidatesResult {
  candidates: EnrichmentCandidate[];
  /** Another page likely exists (a full page came back), so the picker can offer
   * "show more" for a broad common-title query (item-editing/search-improvements). */
  hasMore?: boolean;
}

/** One image the provider offers for a role in the Edit-item image picker (Fix
 * label, ADR-0019): the URL to preview + pick and the source dimensions (0 when
 * unreported). */
export interface ArtworkCandidate {
  url: string;
  width?: number;
  height?: number;
  source?: string;
}

/** Response of `GET /api/v1/{titles|shows|artists|albums}/{id}/artworkCandidates?role=…`. */
export interface ArtworkCandidatesResult {
  role: string;
  candidates: ArtworkCandidate[];
}

/** Body of `PUT /api/v1/{shows|artists|albums}/{id}/metadata` — a parent hand-edit.
 * Each present field is written AND Locked (a rename via `title` never touches
 * identity). Fields absent from the parent schema (tagline/cast/etc.) aren't here. */
export interface EntityMetadataEditInput {
  overview?: string;
  contentRating?: string;
  network?: string;
  genres?: string[];
  title?: string;
  lockArtwork?: string[];
}

/** The active durable Enrichment override on a browse parent (Show/Artist/Album),
 * so a client can show/undo it. Present only when an Admin has pinned a record. */
export interface EntityEnrichmentOverride {
  externalId: string;
  source?: string;
  status?: string;
}

/** The "also apply to children" cascade summary (item-editing/05): how many child
 * items received a durable Enrichment override, and how many were routed to the
 * Admin attention list. Present on a parent apply that ran the cascade. */
export interface CascadeSummary {
  updated: number;
  attention: number;
}

/** Compact parent-enrichment detail returned by the parent Edit-item endpoints
 * (`PUT /shows|artists|albums/{id}/enrichmentOverride` and `.../metadata`,
 * item-editing/02): the descriptive fields plus which fields are Locked and which
 * Enrichment override is in effect. A Fix-info / Wrong-item apply that cascaded also
 * carries the cascade summary (item-editing/05). */
export interface EntityEnrichmentDetail {
  entityType: string;
  entityId: string;
  overview?: string;
  genres?: string[];
  contentRating?: string;
  network?: string;
  enrichmentStatus?: EnrichmentStatus;
  lockedFields?: string[];
  enrichmentOverride?: EntityEnrichmentOverride;
  cascade?: CascadeSummary;
}

// --- Browse surface (issue 03) ---------------------------------------------
//
// These mirror the server's JSON exactly (catalog_handlers.go /
// library_handlers.go). The server omits false booleans and zero numbers
// (`omitempty`) and absent timestamps; the typed client NORMALIZES those holes
// so components see consistent shapes (see client.ts normalize* helpers). The
// `Normalized*` types below are what components actually receive — every
// boolean is present, every list is an array, timestamps stay as the RFC3339
// strings the server sent (parsed to local time at render, not here).

/** A Library's root folder on disk (`{ id, path }`). */
export interface LibraryRoot {
  id: string;
  path: string;
}

/** A Library as `GET /libraries` / `GET /libraries/{id}` return it. */
export interface Library {
  id: string;
  name: string;
  kind: string;
  /** RFC3339; may be absent (server `omitempty`). */
  createdAt?: string;
  rootFolders: LibraryRoot[];
}

/** Response shape of `GET /libraries` (the list is wrapped). */
export interface LibrariesResponse {
  libraries: Library[];
}

// --- Admin: libraries & scanning (issue 06) --------------------------------

/** Request body for `POST /api/v1/libraries` (Admin). `kind` is "movie" today
 * (the only media kind the server builds); `rootFolders` is one or more absolute
 * paths on the server's filesystem, each owned by exactly one Library — a root
 * overlapping another Library's root is rejected with a 409 FOLDER_OVERLAP. */
export interface CreateLibraryInput {
  name: string;
  kind: string;
  rootFolders: string[];
}

/** Request body for `PATCH /api/v1/libraries/{id}` (Admin) — a partial edit. An
 * absent `name` leaves the name unchanged; `addRootFolders` (absent/empty)
 * appends nothing. The kind is fixed at creation and cannot be changed. An added
 * folder overlapping any existing root is rejected with a 409 FOLDER_OVERLAP. */
export interface UpdateLibraryInput {
  name?: string;
  addRootFolders?: string[];
}

/** Options for {@link ApiClient.scanLibrary}: `mode` "full" forces a full
 * re-derivation; absent/"incremental" is the server default (ADR-0008). */
export type ScanMode = "incremental" | "full";

/** A library's scan state, from the pollable `GET /libraries/{id}/scan`. */
export type ScanState = "idle" | "running" | "error" | (string & {});

/** The raw `GET|POST /libraries/{id}/scan` response. The server omits the zero
 * counts and absent timestamps/error (`omitempty`); the client normalizes the
 * counts to 0 so the UI can render them unconditionally. */
export interface ScanStatusRaw {
  libraryId: string;
  state: ScanState;
  titlesFound?: number;
  filesFound?: number;
  errorMessage?: string;
  startedAt?: string;
  finishedAt?: string;
}

/** A scan status with the `omitempty` holes filled (counts → 0): the shape the
 * admin hub and its polling hook consume. */
export interface ScanStatus {
  libraryId: string;
  state: ScanState;
  titlesFound: number;
  filesFound: number;
  /** Present only when `state === "error"`. */
  errorMessage?: string;
  /** RFC3339 strings, or undefined when the server omitted them. */
  startedAt?: string;
  finishedAt?: string;
}

/** A Title summary as it appears in a library grid (`/libraries/{id}/titles`).
 * The RAW server shape: booleans/numbers/timestamps are `omitempty` and may be
 * absent. Components consume {@link TitleSummary} (normalized) instead. */
export interface TitleSummaryRaw {
  id: string;
  kind: string;
  title: string;
  year?: number;
  needsReview?: boolean;
  ambiguous?: boolean;
  tmdbId?: string;
  imdbId?: string;
  /** RFC3339, may be absent. */
  addedAt?: string;
  resumePositionMs?: number;
  watched?: boolean;
  // Enrichment (external-metadata-enrichment): a lean subset on the summary.
  genres?: string[];
  contentRating?: string;
  enrichmentStatus?: EnrichmentStatus;
  /** Opaque per-Title artwork cache-bust token (newest artwork timestamp);
   * absent when the Title has no artwork. Changes only when the served poster
   * could have (a re-fetched image / rescanned local file). */
  artworkVersion?: string;
  /** Present only for an Episode in a Home row (Continue Watching / Up Next /
   * Recently Added): the Show/Season/episode parent context so the card reads as
   * "The Bear · S01E03" rather than a bare episode title. Absent for a Movie. */
  episode?: EpisodeContext;
  /** Present only for a Track in a Home row: the Artist/Album parent context so
   * the card reads as "Radiohead · OK Computer". Absent for a Movie/Episode. */
  track?: TrackContext;
}

/** A Title summary with the `omitempty` holes filled (booleans → false, numbers
 * → 0): the shape components rely on. Note: the summary carries NO artwork flag
 * — whether a poster exists is discovered by the `<img>` load result, so the
 * grid uses an onError placeholder fallback (issue 03 poster strategy). */
export interface TitleSummary {
  id: string;
  kind: string;
  title: string;
  /** 0 when the Title has no year (a needs-review condition). */
  year: number;
  needsReview: boolean;
  ambiguous: boolean;
  tmdbId?: string;
  imdbId?: string;
  /** RFC3339 string, or undefined when the server omitted it. */
  addedAt?: string;
  resumePositionMs: number;
  watched: boolean;
  /** Enriched genres (empty when un-enriched) — drives browse/filter chips. */
  genres: string[];
  contentRating?: string;
  enrichmentStatus?: EnrichmentStatus;
  /** Opaque per-Title artwork cache-bust token (newest artwork timestamp), or
   * undefined when the Title has no artwork — the poster's `<img>` version, so a
   * re-fetched image reloads while a text-only edit leaves the poster untouched. */
  artworkVersion?: string;
  /** Show/Season/episode parent context for an Episode in a Home row (Continue
   * Watching / Up Next / Recently Added); undefined for a Movie or a plain grid
   * entry. Lets the card render "The Bear · S01E03". */
  episode?: EpisodeContext;
  /** Artist/Album parent context for a Track in a Home row; undefined otherwise.
   * Lets the card render "Radiohead · OK Computer". */
  track?: TrackContext;
}

/** Raw `GET /libraries/{id}/titles` response. `nextCursor` is absent on the
 * last page (server `omitempty`). */
export interface TitlesResponseRaw {
  titles: TitleSummaryRaw[];
  nextCursor?: string;
}

/** Normalized titles page: `titles` always an array, `nextCursor` null when
 * there are no more pages (so callers branch on `=== null`, not `undefined`). */
export interface TitlesPage {
  titles: TitleSummary[];
  nextCursor: string | null;
}

// --- Home surface (issue 05) -----------------------------------------------
//
// `GET /home` returns two per-User computed rows: Continue Watching (in-progress
// titles, most-recent first) and Recently Added (newest-added first). Each entry
// is a Title summary in the same shape the grid uses (the server's homeTitleJSON
// is a subset of the summary fields — it carries no watched flag, so Continue
// Watching entries normalize to watched:false and render their resume marker).

/** Raw `GET /home` response: each row is an array of Title summaries (the
 * server omits `watched`/zero `resumePositionMs`; the client normalizes).
 * `upNext` is the TV-only row (the next unwatched Episode in Show order for each
 * Show the User has started); it is empty when no Show is in progress. */
export interface HomeResponseRaw {
  continueWatching?: TitleSummaryRaw[];
  upNext?: TitleSummaryRaw[];
  recentlyAdded?: TitleSummaryRaw[];
}

/** Normalized Home rows: each always an array of normalized {@link TitleSummary}.
 * `upNext` is the TV-only computed row (next unwatched Episode per started Show). */
export interface HomeRows {
  continueWatching: TitleSummary[];
  upNext: TitleSummary[];
  recentlyAdded: TitleSummary[];
}

/** How a grid is ordered. Maps to the API's `sort=` param (catalog parseSort):
 * `title` → by title (default), `dateAdded` → newest-added first. */
export type TitleSort = "title" | "dateAdded";

/** Options for {@link ApiClient.listTitles}. */
export interface ListTitlesOptions {
  limit?: number;
  cursor?: string | null;
  sort?: TitleSort;
}

/** An elementary Stream inside a File (video/audio/subtitle; FFmpeg's sense). */
export interface Stream {
  index: number;
  kind: string;
  codec: string;
  language?: string;
  width: number;
  height: number;
  channels: number;
  isDefault: boolean;
}

/** One embedded audio Stream of a File, labeled for the player's Audio menu
 * (audio-streams/01). `id` is the stable selector later slices pass back as
 * `audioStreamId`; `language` is ISO-639-1 ("" = Unknown); `layout` is the
 * familiar surround label ("Stereo"/"5.1"/"7.1"); `label` is the ready menu
 * string (language + layout, or a title tag like "Director's Commentary"). Per
 * CONTEXT.md this is the audio Stream itself, not a coined "Audio track". */
export interface AudioStream {
  id: string;
  index: number;
  codec: string;
  language?: string;
  channels?: number;
  layout?: string;
  isDefault: boolean;
  commentary?: boolean;
  label: string;
}

/** One selectable video Stream of a File, labeled for the player's Video menu
 * (selectable-video/01, ADR-0025) — the video parallel of {@link AudioStream}.
 * `id` is the stable selector a switch passes back as `videoStreamId`; `label` is
 * the ready menu string (the embedded title tag like "Black & White"/"Colour",
 * else a resolution token like "1080p"/"4K"); `width`/`height` carry the
 * resolution. `isDefault` on the playback Decision is the capability-then-quality
 * pick the server resolved (not the container disposition). Per CONTEXT.md this is
 * the video Stream itself, not a coined "Video track"; cover-art Streams excluded. */
export interface VideoStream {
  id: string;
  index: number;
  codec: string;
  language?: string;
  width?: number;
  height?: number;
  isDefault: boolean;
  label: string;
}

/** One physical File on disk (raw server shape; numbers/`missing` omitempty). */
export interface FileRaw {
  id: string;
  path: string;
  container: string;
  videoCodec?: string;
  audioCodec?: string;
  width?: number;
  height?: number;
  bitrate?: number;
  durationMs?: number;
  sizeBytes?: number;
  missing?: boolean;
  streams?: Stream[];
  audioStreams?: AudioStream[];
  videoStreams?: VideoStream[];
}

/** A File with holes normalized (numbers → 0, `missing` → false, streams → []). */
export interface MediaFile {
  id: string;
  path: string;
  container: string;
  videoCodec?: string;
  audioCodec?: string;
  width: number;
  height: number;
  bitrate: number;
  durationMs: number;
  sizeBytes: number;
  missing: boolean;
  streams: Stream[];
  /** The File's embedded audio Streams, labeled for the Audio menu. */
  audioStreams: AudioStream[];
  /** The File's non-cover-art video Streams, labeled for the Video menu
   * (selectable-video/01). `isDefault` reflects the container disposition at
   * browse time (the catalog projection passes no capability-resolved pick). */
  videoStreams: VideoStream[];
}

/** An Edition (a quality/cut of a Title) with its Files. */
export interface EditionRaw {
  id: string;
  name: string;
  files?: FileRaw[];
}

export interface Edition {
  id: string;
  name: string;
  files: MediaFile[];
}

/** Where to fetch a Title's artwork bytes for a role (poster|background). The
 * `url` is same-origin and authenticated by the media cookie — used directly as
 * an `<img src>` (no JS header). `source` is "local" or "fetched"; local wins
 * over fetched at serve time (external-metadata-enrichment). */
export interface Artwork {
  role: string;
  url: string;
  path: string;
  source?: string;
}

/** One enriched cast/crew member on a Title detail. `kind` is "cast" | "crew";
 * `character` is the role played (cast only). `personId` is the provider-
 * namespaced person ref ("tmdb:<id>", absent when the provider supplied none) —
 * the client builds the headshot URL from it (a member with no ref/photo shows a
 * placeholder). `photoVersion` is an opaque cache-bust token for the headshot
 * (absent when there's no cached photo), analogous to a poster's version
 * (cast-photos/01). */
export interface Credit {
  person: string;
  role?: string;
  character?: string;
  kind?: string;
  personId?: string;
  photoVersion?: string;
}

/** A Title's enrichment status (external-metadata-enrichment): pending (scanned,
 * not yet enriched), matched, unmatched (no external record), failed (provider
 * errored), disabled (no provider configured). Absent on an un-enriched server. */
export type EnrichmentStatus =
  | "pending"
  | "matched"
  | "unmatched"
  | "failed"
  | "disabled"
  | (string & {});

/** Raw `GET /titles/{id}` response (nested Editions → Files → Streams). The
 * enrichment fields are absent (`omitempty`) on an un-enriched Title. */
export interface TitleDetailRaw {
  id: string;
  /** The Library this Title belongs to; drives the detail's parent "Back" link
   * for a Movie. Absent on an older server. */
  libraryId?: string;
  kind: string;
  title: string;
  year?: number;
  needsReview?: boolean;
  ambiguous?: boolean;
  hidden?: boolean;
  tmdbId?: string;
  imdbId?: string;
  resumePositionMs?: number;
  watched?: boolean;
  addedAt?: string;
  editions?: EditionRaw[];
  extras?: unknown[];
  artwork?: Artwork[];
  /** Every selectable Subtitle track the Title offers, from all sources (embedded
   * Streams + sidecar/fetched rows), deduped and labeled by the server (ADR-0020).
   * Absent on an older server; normalized to []. */
  subtitles?: SubtitleTrack[];
  // Enrichment (external-metadata-enrichment): descriptive decoration.
  overview?: string;
  tagline?: string;
  contentRating?: string;
  releaseDate?: string;
  runtimeMinutes?: number;
  studio?: string;
  genres?: string[];
  cast?: Credit[];
  enrichmentStatus?: EnrichmentStatus;
  /** Descriptive fields an Admin hand-edited and Locked (CONTEXT.md): re-enrich
   * skips them. Absent (server `omitempty`) when nothing is locked. */
  lockedFields?: string[];
  /** Canonical enriched display title (Episode/Track); display only — `title`
   * stays the parsed identity value. Absent when there is no enriched title. */
  displayTitle?: string;
  /** Present only for an Episode (kind "episode"): Show/Season/episode context. */
  episode?: EpisodeContext;
  /** Present only for a Track (kind "track"): Artist/Album/disc/track context. */
  track?: TrackContext;
}

/** Body of PUT /titles/{id}/metadata: a hand-edit that writes + Locks each
 * supplied field. Every field is optional — only the ones sent are written and
 * locked. `lockArtwork` pins artwork roles ("poster"/"background"). */
export interface MetadataEditInput {
  overview?: string;
  tagline?: string;
  title?: string;
  contentRating?: string;
  releaseDate?: string;
  runtimeMinutes?: number;
  studio?: string;
  genres?: string[];
  cast?: Credit[];
  lockArtwork?: string[];
}

// --- TV browse surface (issue tv-music/01) ---------------------------------
//
// A TV Library's GET /libraries/{id}/titles returns SHOWS (not Titles); the
// client branches on `library.kind` to render the Show grid → Seasons/Episodes.
// These mirror the server's JSON (tv_handlers.go); `omitempty` holes are filled
// by the normalize layer so components see consistent shapes.

/** Raw Show summary from a TV Library's `/libraries/{id}/titles`. */
export interface ShowSummaryRaw {
  id: string;
  /** The Library this Show belongs to; drives the Show detail's parent "Back"
   * link. Absent on an older server. */
  libraryId?: string;
  kind: string; // "show"
  title: string;
  year?: number;
  needsReview?: boolean;
  tmdbId?: string;
  imdbId?: string;
  addedAt?: string;
  /** Per-User count of unwatched Episodes; absent (server `omitempty`) when 0. */
  unwatchedEpisodeCount?: number;
  // Enrichment (issue 03): descriptive fields + fetched artwork URLs, all absent
  // (server `omitempty`) on an un-enriched Show.
  overview?: string;
  genres?: string[];
  contentRating?: string;
  network?: string;
  enrichmentStatus?: EnrichmentStatus;
  posterUrl?: string;
  backgroundUrl?: string;
  logoUrl?: string;
  // Edit-item surface (item-editing/02): on the Show DETAIL only — which fields are
  // Locked and which Enrichment override is in effect (absent on the lean grid).
  lockedFields?: string[];
  enrichmentOverride?: EntityEnrichmentOverride;
  /** The Show's series main cast (cast-photos/02) — same shape a Title's cast has,
   * so the Show detail renders the same headshot strip. Absent on the grid / when
   * no cast was captured. */
  cast?: Credit[];
}

/** A Show summary with holes filled — the TV grid entry. */
export interface ShowSummary {
  id: string;
  /** The Library this Show belongs to, "" when absent — the Show detail's parent
   * "Back" link returns to its owning Library. */
  libraryId: string;
  kind: string;
  title: string;
  year: number;
  needsReview: boolean;
  tmdbId?: string;
  imdbId?: string;
  addedAt?: string;
  /** Per-User unwatched-Episode count (0 when fully watched / none) — the
   * Show-poster watched affordance, analogous to a Movie's resume marker. */
  unwatchedEpisodeCount: number;
  /** Enriched synopsis (issue 03), "" when un-enriched. */
  overview: string;
  /** Enriched genres, [] when un-enriched. */
  genres: string[];
  contentRating?: string;
  network?: string;
  enrichmentStatus?: EnrichmentStatus;
  /** Fetched poster/backdrop/logo URLs (same-origin, media-cookie authed),
   * undefined when Enrichment fetched none. */
  posterUrl?: string;
  backgroundUrl?: string;
  logoUrl?: string;
  /** Locked fields + active Enrichment override (item-editing/02), on the detail. */
  lockedFields?: string[];
  enrichmentOverride?: EntityEnrichmentOverride;
  /** The Show's series main cast (cast-photos/02), [] when un-enriched / none. On
   * the Show detail only; drives the same cast strip the Movie detail uses. */
  cast: Credit[];
}

/** Raw `GET /libraries/{id}/titles` for a TV Library. */
export interface ShowsResponseRaw {
  shows?: ShowSummaryRaw[];
  nextCursor?: string;
}

/** Normalized Shows page: `shows` always an array; `nextCursor` null on the last
 * page (callers stop paginating on null). */
export interface ShowsPage {
  shows: ShowSummary[];
  nextCursor: string | null;
}

/** Raw Season from `GET /shows/{id}/seasons`. */
export interface SeasonRaw {
  id: string;
  showId: string;
  seasonNumber: number;
  specials?: boolean;
  episodeCount?: number;
  /** Fetched Season poster URL (issue 03), absent when none. */
  posterUrl?: string;
}

/** A Season with holes filled. seasonNumber 0 / `specials` true = Specials. */
export interface Season {
  id: string;
  showId: string;
  seasonNumber: number;
  specials: boolean;
  episodeCount: number;
  /** Fetched Season poster URL, undefined when Enrichment fetched none. */
  posterUrl?: string;
}

/** The resume-point mode (ADR-0028): `inProgress` = the anchor Episode is still
 * mid-play, so the detail page offers Continue + Restart; `next` = a fresh next
 * Episode, so a single Play (from 0). */
export type ResumePointMode = "inProgress" | "next";

/** Raw resume-point Episode on `GET /shows/{id}/seasons` — present only for a
 * STARTED, not-fully-watched Show. */
export interface ResumePointRaw {
  id: string;
  kind: string; // "episode"
  seasonId: string;
  seasonNumber: number;
  episodeNumber?: number;
  episodeLabel?: string;
  title: string;
  overview?: string;
  resumePositionMs?: number;
  durationMs?: number;
  mode: ResumePointMode;
  enrichmentStatus?: EnrichmentStatus;
  stillUrl?: string;
}

/** The Show's resume point (ADR-0028): the Episode the detail page surfaces as its
 * next-episode block, with the `mode` that selects the controls (Continue+Restart
 * vs. a single Play). `seasonId` is enough to build the cross-season show-from-here
 * Queue with this Episode as the head. Null for a not-started or fully-watched Show. */
export interface ResumePoint {
  id: string;
  kind: string;
  seasonId: string;
  seasonNumber: number;
  episodeNumber: number;
  /** A date / "Episode N" for a degraded-offline episode, else "". */
  episodeLabel: string;
  title: string;
  /** Episode synopsis, "" when un-enriched. */
  overview: string;
  /** Where Continue seeks (the in-progress anchor's stored resume); 0 for `next`. */
  resumePositionMs: number;
  /** The Episode's playable duration (ms); with resumePositionMs it drives the
   * in-progress Continue progress bar + minutes-remaining label. 0 when unknown. */
  durationMs: number;
  mode: ResumePointMode;
  enrichmentStatus?: EnrichmentStatus;
  stillUrl?: string;
}

/** Raw `GET /shows/{id}/seasons` response: the Show plus its Seasons. */
export interface ShowSeasonsResponseRaw {
  show: ShowSummaryRaw;
  seasons?: SeasonRaw[];
  resumePoint?: ResumePointRaw | null;
}

/** Normalized Show + Seasons. `resumePoint` is null for a not-started or
 * fully-watched Show (told apart by `show.unwatchedEpisodeCount`). */
export interface ShowSeasons {
  show: ShowSummary;
  seasons: Season[];
  resumePoint: ResumePoint | null;
}

/** Raw Episode summary from `GET /seasons/{id}/episodes`. */
export interface EpisodeSummaryRaw {
  id: string;
  kind: string; // "episode"
  title: string;
  seasonNumber: number;
  episodeNumber?: number;
  episodeLabel?: string;
  needsReview?: boolean;
  resumePositionMs?: number;
  watched?: boolean;
  addedAt?: string;
  // Enrichment (issue 03): title above already prefers the canonical name.
  overview?: string;
  enrichmentStatus?: EnrichmentStatus;
  stillUrl?: string;
}

/** An Episode summary with holes filled — one row in a Season's episode list.
 * `title` is the display title (the canonical enriched name when present). */
export interface EpisodeSummary {
  id: string;
  kind: string;
  title: string;
  seasonNumber: number;
  episodeNumber: number;
  /** A date / "Episode N" for a degraded-offline episode, else "". */
  episodeLabel: string;
  needsReview: boolean;
  resumePositionMs: number;
  watched: boolean;
  addedAt?: string;
  /** Enriched synopsis, "" when un-enriched. */
  overview: string;
  enrichmentStatus?: EnrichmentStatus;
  /** Episode still image URL, undefined when none was fetched. */
  stillUrl?: string;
}

/** Raw `GET /seasons/{id}/episodes` response: the Season plus its Episodes. */
export interface SeasonEpisodesResponseRaw {
  season: SeasonRaw;
  episodes?: EpisodeSummaryRaw[];
}

/** Normalized Season + Episodes. */
export interface SeasonEpisodes {
  season: Season;
  episodes: EpisodeSummary[];
}

/** An Episode's Show/Season/episode parent context, attached to its
 * {@link TitleDetail} (present only for an Episode; undefined for a Movie). */
export interface EpisodeContext {
  showId: string;
  showTitle: string;
  showYear?: number;
  seasonId: string;
  seasonNumber: number;
  episodeNumber?: number;
  episodeLabel?: string;
}

// --- Music browse surface (issue tv-music/03) ------------------------------
//
// A Music Library's GET /libraries/{id}/titles returns ARTISTS (not Titles); the
// client branches on `library.kind` to render the Artist list → Albums → Tracks.
// These mirror the server's JSON (music_handlers.go); `omitempty` holes are
// filled by the normalize layer so components see consistent shapes.

/** Raw Artist summary from a Music Library's `/libraries/{id}/titles`. */
export interface ArtistSummaryRaw {
  id: string;
  /** The Music Library this Artist belongs to; drives the Artist detail's parent
   * "Back" link. Absent on an older server. */
  libraryId?: string;
  kind: string; // "artist"
  name: string;
  // Enrichment (issue 03): bio (overview) + genres + a fetched image URL, all
  // absent on an un-enriched Artist. Set on the Artist detail, not the lean list.
  overview?: string;
  genres?: string[];
  enrichmentStatus?: EnrichmentStatus;
  artworkUrl?: string;
  // Edit-item surface (item-editing/02): on the Artist DETAIL only.
  lockedFields?: string[];
  enrichmentOverride?: EntityEnrichmentOverride;
}

/** An Artist summary — the Music list entry (and the Artist detail header). */
export interface ArtistSummary {
  id: string;
  /** The Music Library this Artist belongs to, "" when absent — the Artist
   * detail's parent "Back" link returns to its owning Library. */
  libraryId: string;
  kind: string;
  name: string;
  /** Enriched bio, "" when un-enriched. */
  overview: string;
  /** Enriched genres, [] when un-enriched. */
  genres: string[];
  enrichmentStatus?: EnrichmentStatus;
  /** Fetched artist image URL, undefined when none. */
  artworkUrl?: string;
  /** Locked fields + active Enrichment override (item-editing/02), on the detail. */
  lockedFields?: string[];
  enrichmentOverride?: EntityEnrichmentOverride;
}

/** Raw `GET /libraries/{id}/titles` for a Music Library. */
export interface ArtistsResponseRaw {
  artists?: ArtistSummaryRaw[];
  nextCursor?: string;
}

/** Normalized Artists page: `artists` always an array; `nextCursor` null on the
 * last page (callers stop paginating on null). */
export interface ArtistsPage {
  artists: ArtistSummary[];
  nextCursor: string | null;
}

/** Raw Album from `GET /artists/{id}/albums`. */
export interface AlbumRaw {
  id: string;
  artistId: string;
  /** Parent Artist's display name, for the album header's artist link. Absent
   * (omitempty) when the server couldn't resolve the Artist row. */
  artistName?: string;
  title: string;
  year?: number;
  hasArtwork?: boolean;
  /** Cover cache-bust token (newest fetched-cover timestamp); absent for a
   * local-only cover. Appended to the cover URL so a re-enriched cover reloads. */
  artworkVersion?: string;
  trackCount?: number;
  // Enrichment (issue 03): genres + status. hasArtwork above is true for a local
  // OR a fetched cover (local wins at serve time).
  genres?: string[];
  enrichmentStatus?: EnrichmentStatus;
  // Edit-item surface (item-editing/02): on the Album DETAIL only.
  lockedFields?: string[];
  enrichmentOverride?: EntityEnrichmentOverride;
}

/** An Album with holes filled. */
export interface Album {
  id: string;
  artistId: string;
  /** Parent Artist's display name, "" when the server couldn't resolve it. */
  artistName: string;
  title: string;
  year: number;
  hasArtwork: boolean;
  /** Cover cache-bust token (newest fetched-cover timestamp), or undefined for a
   * local-only cover — appended to the cover URL so a re-enriched cover reloads. */
  artworkVersion?: string;
  trackCount: number;
  /** Enriched genres, [] when un-enriched. */
  genres: string[];
  enrichmentStatus?: EnrichmentStatus;
  /** Locked fields + active Enrichment override (item-editing/02), on the detail. */
  lockedFields?: string[];
  enrichmentOverride?: EntityEnrichmentOverride;
}

/** Raw `GET /artists/{id}/albums` response: the Artist plus its Albums. */
export interface ArtistAlbumsResponseRaw {
  artist: ArtistSummaryRaw;
  albums?: AlbumRaw[];
}

/** Normalized Artist + Albums. */
export interface ArtistAlbums {
  artist: ArtistSummary;
  albums: Album[];
}

/** Raw Track summary from `GET /albums/{id}/tracks`. */
export interface TrackSummaryRaw {
  id: string;
  kind: string; // "track"
  title: string;
  discNumber?: number;
  trackNumber?: number;
  /** Playable length in ms (the Track's file duration); absent/0 when no file is
   * indexed yet. */
  durationMs?: number;
  needsReview?: boolean;
  resumePositionMs?: number;
  watched?: boolean;
  // Enrichment (issue 03): title above already prefers a canonical name where the
  // tag title was sparse; overview is the track synopsis.
  overview?: string;
  enrichmentStatus?: EnrichmentStatus;
}

/** A Track summary with holes filled — one row in an Album's track list. */
export interface TrackSummary {
  id: string;
  kind: string;
  title: string;
  discNumber: number;
  trackNumber: number;
  /** Playable length in ms (the Track's file duration), 0 when not yet indexed. */
  durationMs: number;
  needsReview: boolean;
  resumePositionMs: number;
  watched: boolean;
  /** Enriched synopsis, "" when un-enriched. */
  overview: string;
  enrichmentStatus?: EnrichmentStatus;
}

/** Raw `GET /albums/{id}/tracks` response: the Album plus its Tracks. */
export interface AlbumTracksResponseRaw {
  album: AlbumRaw;
  tracks?: TrackSummaryRaw[];
}

/** Normalized Album + Tracks (in disc/track order). */
export interface AlbumTracks {
  album: Album;
  tracks: TrackSummary[];
}

/** A Track's Artist/Album/disc/track parent context, attached to its
 * {@link TitleDetail} (present only for a Track; undefined otherwise). */
export interface TrackContext {
  artistId: string;
  artistName: string;
  albumId: string;
  albumTitle: string;
  albumYear?: number;
  discNumber?: number;
  trackNumber?: number;
}

// --- Collections surface (collections-playlists-ui issue 01) ----------------
//
// An Admin-curated, shared grouping of Titles, read-only here (curation is issue
// 02). Reads are any authenticated User and are access-filtered per viewer: a
// Collection with zero VISIBLE members is hidden from a non-Admin (absent from
// the list, 404 on detail). The detail's members come back in the EXACT same
// `titleSummaryJSON` shape a browse grid uses (the backend's decoration-parity
// guarantee), so the client REUSES {@link TitleSummary} and the existing title
// normalization — member cards drop into PosterTile unchanged. These mirror the
// server's JSON (collection_handlers.go); `omitempty` holes (description, the
// timestamps, posterUrl) are filled by the normalize layer.

/** Raw `GET /collections` card: the Collection plus its per-viewer list
 * metadata. `description`/`posterUrl`/timestamps are absent when empty (server
 * `omitempty`); `memberCount` is always present. */
export interface CollectionSummaryRaw {
  id: string;
  name: string;
  description?: string;
  createdAt?: string;
  updatedAt?: string;
  memberCount: number;
  /** The representative member's poster-artwork URL (same-origin, media-cookie
   * authed), absent when the Collection has no visible members. */
  posterUrl?: string;
}

/** A Collection card with its `omitempty` holes filled (description → ""): one
 * entry in the Collections list. `memberCount` and `posterUrl` are computed for
 * the calling viewer (a restricted member never contributes). */
export interface CollectionSummary {
  id: string;
  name: string;
  /** Optional blurb, "" when the Collection has none. */
  description: string;
  /** Number of members VISIBLE to the calling viewer. */
  memberCount: number;
  /** The representative member's poster URL, or undefined when the Collection
   * has no visible member to draw a poster from. */
  posterUrl?: string;
}

/** Raw `GET /collections` response (the list is wrapped). */
export interface CollectionsResponseRaw {
  collections?: CollectionSummaryRaw[];
}

/** Raw `GET /collections/{id}` response: the Collection plus its resolved member
 * Titles, each decorated identically to a browse-list summary. */
export interface CollectionDetailRaw {
  id: string;
  name: string;
  description?: string;
  createdAt?: string;
  updatedAt?: string;
  memberCount: number;
  members?: TitleSummaryRaw[];
}

/** A Collection's detail with holes filled (description → "", members → an array
 * of normalized {@link TitleSummary}). The members reuse the browse summary
 * shape, so the detail renders them with PosterTile/the grid unchanged. */
export interface CollectionDetail {
  id: string;
  name: string;
  /** Optional blurb, "" when the Collection has none. */
  description: string;
  memberCount: number;
  /** The visible member Titles (access/Missing-filtered server-side), in the
   * server's stable order. */
  members: TitleSummary[];
}

// --- Collection curation (collections-playlists-ui issue 02) -----------------
//
// The Admin-scope write surface for Collections (server-enforced; the UI gate on
// `useAuth().isAdmin` is convenience). The create/update endpoints return the bare
// Collection (collectionJSON) — id/name/description + timestamps, WITHOUT the
// per-viewer member count/poster the list and detail GETs carry — so they map to a
// lean {@link Collection}, distinct from {@link CollectionSummary}/{@link
// CollectionDetail}. add/remove-items and delete are 204 → void.

/** Request body for `POST /api/v1/collections` (Admin): create a Collection. A
 * blank `name` is rejected (400 BAD_REQUEST). `description` is optional. */
export interface CreateCollectionInput {
  name: string;
  description?: string;
}

/** Request body for `PUT /api/v1/collections/{id}` (Admin): rename and/or
 * re-describe a Collection. A blank `name` is rejected (400 BAD_REQUEST). */
export interface UpdateCollectionInput {
  name: string;
  description?: string;
}

/** Raw `POST`/`PUT /api/v1/collections[/{id}]` result — the bare Collection.
 * `description` and the timestamps are absent when empty (server `omitempty`).
 * Carries NO member count/poster (the list/detail GETs compute those per-viewer). */
export interface CollectionRaw {
  id: string;
  name: string;
  description?: string;
  createdAt?: string;
  updatedAt?: string;
}

/** A bare Collection (id/name/description) with its `omitempty` holes filled
 * (description → ""), as create/update return it. Distinct from {@link
 * CollectionSummary} (which adds the per-viewer member count + poster) and {@link
 * CollectionDetail} (which adds the member Titles). */
export interface Collection {
  id: string;
  name: string;
  /** Optional blurb, "" when the Collection has none. */
  description: string;
}

// --- Playlists surface (collections-playlists-ui issue 03) ------------------
//
// User-owned, PRIVATE, ORDERED, single-media-kind queues. Every /playlists route
// is owner == caller (no Admin override); a non-owner — including an Admin — is
// hidden a 404, exactly like another User's playback session. A Playlist member
// carries the EXACT same titleSummary fields a browse grid shows (decoration
// parity) PLUS an `itemId` (the playlist-item id) so duplicate entries are
// distinguishable and removable by position. A Playlist is created untyped; the
// FIRST appended Title fixes its `kind` (movie/tv/music) — until then `kind` is
// "" (the server omits it). These mirror the server's JSON
// (playlist_handlers.go); the normalize layer fills the `omitempty` holes.

/** Raw `GET /playlists` card: the Playlist plus its raw item count. `kind` is
 * absent while the Playlist is still untyped (server `omitempty`); the
 * timestamps are likewise absent when empty. */
export interface PlaylistSummaryRaw {
  id: string;
  kind?: string;
  name: string;
  createdAt?: string;
  updatedAt?: string;
  itemCount: number;
}

/** A Playlist card with its `omitempty` holes filled (kind → "" while untyped):
 * one entry in the caller's Playlists list. `itemCount` is the entry count. */
export interface PlaylistSummary {
  id: string;
  name: string;
  /** "movie" | "tv" | "music", or "" while the Playlist is still untyped (empty
   * until its first item fixes the kind). */
  kind: string;
  itemCount: number;
}

/** Raw `GET /playlists` response (the list is wrapped). */
export interface PlaylistsResponseRaw {
  playlists?: PlaylistSummaryRaw[];
}

/** Raw `POST`/`PUT /playlists[/{id}]` result — the bare Playlist (no item
 * count/members). `kind` is absent while untyped; timestamps absent when empty. */
export interface PlaylistRaw {
  id: string;
  kind?: string;
  name: string;
  createdAt?: string;
  updatedAt?: string;
}

/** A bare Playlist (id/name/kind), as create/rename return it. Distinct from
 * {@link PlaylistSummary} (which adds the item count) and {@link PlaylistDetail}
 * (which adds the ordered members). */
export interface Playlist {
  id: string;
  name: string;
  /** "movie" | "tv" | "music", or "" while the Playlist is still untyped. */
  kind: string;
}

/** Raw `GET /playlists/{id}` member: a browse-grid titleSummary PLUS the
 * playlist-item id (so duplicates are distinguishable / removable). */
export interface PlaylistMemberRaw extends TitleSummaryRaw {
  itemId: string;
}

/** A resolved, ordered Playlist member: the existing {@link TitleSummary} (so the
 * card drops into PosterTile unchanged) PLUS the `itemId` — the playlist-item id
 * by which a single entry is removed (so removing one duplicate leaves the
 * other). */
export type PlaylistMember = TitleSummary & { itemId: string };

/** Raw `GET /playlists/{id}` response: the Playlist plus its resolved member
 * Titles in POSITION order, each decorated identically to a browse-list summary
 * and tagged with its item id. */
export interface PlaylistDetailRaw {
  id: string;
  kind?: string;
  name: string;
  createdAt?: string;
  updatedAt?: string;
  memberCount: number;
  members?: PlaylistMemberRaw[];
}

/** A Playlist's detail with holes filled (kind → "" while untyped, members → an
 * array of normalized {@link PlaylistMember}). The members are in the server's
 * POSITION order; the screen renders them in that order with PosterTile/the grid
 * unchanged. */
export interface PlaylistDetail {
  id: string;
  name: string;
  kind: string;
  memberCount: number;
  /** The visible member Titles (access/Missing-filtered server-side) in position
   * order, each tagged with its playlist-item id. */
  members: PlaylistMember[];
}

// --- Playback surface (issue 04) -------------------------------------------
//
// The Capability profile is split into a static device profile (what the
// browser can demux/decode, derived from canPlayType/MediaSource at runtime)
// and per-request constraints (a bitrate/resolution cap). The server merges the
// two to pick a tier: `directPlay` (progressive <video>), `directStream`/
// `transcode` (HLS via hls.js / native HLS), a 503 SERVER_BUSY when the
// transcode cap is full (details.suggestedMaxBitrate), or a 501
// TRANSCODE_REQUIRED only for a genuinely unplayable Title.

/** A video codec the browser can decode, with its per-codec ceilings. Mirrors
 * the server's `videoCodecs[]` entry (docs/api-contract.md "Device profile"). */
export interface VideoCodecSupport {
  codec: string;
  maxLevel?: string;
  maxResolution?: string;
  hdr?: string[];
}

/** The static device profile sent on a playback request: the containers/codecs
 * the browser actually supports. Derived from the browser at call time
 * (see ../player/capabilities.ts), not hand-written. */
export interface DeviceProfile {
  containers: string[];
  videoCodecs: VideoCodecSupport[];
  audioCodecs: string[];
  maxAudioChannels: number;
  textSubtitleFormats: string[];
  /** This client plays a copied HEVC video inside MPEG-TS HLS segments. True on
   * the hls.js (MSE) path — hls.js ≥ 1.6 demuxes HEVC-in-TS itself, and the TS
   * pipeline's dictated cuts give the exact playlists strict MSE playback needs.
   * False/absent on the native-HLS path (Apple requires HEVC in fMP4). */
  hevcInMpegts?: boolean;
}

/** Per-request session constraints (network/quality caps). */
export interface PlaybackConstraints {
  maxBitrate?: number;
  maxResolution?: string;
  preferredAudioLang?: string;
  preferredSubtitleLang?: string;
}

/** Options for {@link ApiClient.startPlayback}: the capability profile plus an
 * optional resume start position (ms) and a forced editionId. */
export interface StartPlaybackOptions {
  deviceProfile: DeviceProfile;
  constraints: PlaybackConstraints;
  /** Resume offset in ms the server records on the session; the player still
   * seeks the <video> itself (the stream is a full progressive file). */
  startPosition?: number;
  editionId?: string;
  /** Selects an IMAGE Subtitle track to burn into the video frames (ADR-0020,
   * subtitles/04). Setting it escalates negotiation to the transcode tier (which
   * is governed — it can return SERVER_BUSY) with the sub burned in. The player
   * sends it when the viewer picks an image track from the captions menu (a fresh
   * negotiation that restarts the session). Omit for text/no subtitle. */
  burnSubtitleId?: string;
  /** Selects the audio Stream to deliver (audio-streams/02, ADR-0022), exactly
   * parallel to {@link burnSubtitleId}: a fresh negotiation, never a session-mutate.
   * A non-default pick escalates a direct-play File to the remux tier (direct play
   * carries only the default audio); a codec the browser can't decode escalates to a
   * governed transcode (SERVER_BUSY at the cap). The player sends it when the viewer
   * picks a non-default track from the Audio menu on a direct-play session (the one
   * escalating switch — on the HLS tiers switching is in-band, no re-negotiation).
   * Omit to take the server-resolved default audio. */
  audioStreamId?: string;
  /** Selects the video Stream to deliver (selectable-video/02, ADR-0025). Unlike
   * audio, there is no in-band video rendition in HLS, so EVERY non-default pick is
   * a fresh negotiation that restarts the session (the image-subtitle model): a
   * non-default pick escalates a direct-play File to HLS remux (mapped `-map 0:v:N`),
   * and a Stream the browser can't decode escalates to a governed transcode
   * (SERVER_BUSY at the cap). The player sends it when the viewer picks a track from
   * the Video menu. Omit to take the server-resolved capability-then-quality default. */
  videoStreamId?: string;
}

/** A selected elementary stream in a playback decision (raw server shape;
 * numbers are `omitempty`). */
export interface DecisionStream {
  index: number;
  codec: string;
  language?: string;
  width?: number;
  height?: number;
  channels?: number;
}

/** A playback decision from `POST /titles/{id}/playback`. `tier` selects how the
 * player consumes `streamUrl`:
 *   - `directPlay` → progressive byte-range URL → straight into `<video src>`;
 *   - `directStream` / `transcode` → HLS media-playlist URL → loaded via hls.js
 *     (MSE) on Chrome/Firefox/Edge, or set as `<video src>` on Safari (native
 *     HLS).
 * Either way `streamUrl` is same-origin and the media cookie authenticates it
 * (and, for HLS, its segments), so no JS Authorization header is needed. */
export type PlaybackTier = "directPlay" | "directStream" | "transcode" | (string & {});

/** One selectable Subtitle track on a playback decision (ADR-0020), the union of
 * every source a viewer can turn on for the played File. `id` selects the track;
 * `source` is embedded|sidecar|fetched; `kind` is text|image; `language` is
 * ISO-639-1 ("" = Unknown); `label` is the ready menu string. `url` is the
 * out-of-band WebVTT endpoint for a deliverable TEXT track — the player drops it
 * into a <track>; image tracks (burn-in, a later slice) and unconvertible text
 * carry no url. */
export interface SubtitleTrack {
  id: string;
  source: string;
  kind: "text" | "image" | (string & {});
  language?: string;
  forced: boolean;
  label: string;
  url?: string;
}

/** One external-provider subtitle candidate returned by "search online"
 * (ADR-0021). `id` is the opaque provider handle echoed back to fetch it; `label`
 * is the ready menu string (language + forced/SDH + release + an [exact] marker for
 * a release-exact moviehash match). The whole candidate is echoed back to
 * `fetchSubtitle` verbatim (the fetch is stateless). */
export interface SubtitleCandidate {
  id: string;
  language: string;
  format: string;
  release?: string;
  forced: boolean;
  hearingImpaired: boolean;
  matchedBy?: string;
  label: string;
}

export interface PlaybackDecision {
  sessionId: string;
  tier: PlaybackTier;
  streamUrl: string;
  edition: { id: string; name: string };
  videoStream: DecisionStream;
  /** Every selectable video Stream the played File offers, labeled for the Video
   * menu (selectable-video/01, ADR-0025) — the same projection the catalog exposes,
   * but with `isDefault` re-marked to the capability-then-quality pick the decision
   * resolved ({@link videoStream} above), not the container disposition. Unlike
   * audio there is no in-band video rendition, so a switch is always a fresh
   * negotiation via {@link StartPlaybackOptions.videoStreamId}. Non-nil; a single-
   * video File carries a one-element list, so the player shows the Video menu only
   * at ≥2. */
  videoStreams: VideoStream[];
  /** The RESOLVED audio Stream the delivery actually carries (audio-streams/02) —
   * what negotiation picked (Title/Show memory → preferredAudioLang → default
   * disposition → first). The player marks the matching {@link audioStreams} entry
   * active in the Audio menu. Absent only for a silent File. */
  audioStream?: DecisionStream;
  /** Every selectable audio Stream the played File offers, labeled for the Audio
   * menu (audio-streams/02, ADR-0022) — the same projection the catalog exposes per
   * File. On the HLS tiers a multi-audio File is demuxed, so each entry rides in-band
   * as an audio rendition (index-aligned with this list) and switching is instant;
   * on direct play a non-default pick escalates via {@link StartPlaybackOptions.audioStreamId}.
   * Non-nil; empty for a silent File. */
  audioStreams: AudioStream[];
  /** Every selectable Subtitle track for the played File (ADR-0020). Non-nil;
   * empty when the File offers none. Text-track selection is entirely client-side
   * (enable the <track>); no server round-trip. */
  subtitles: SubtitleTrack[];
  estimatedBitrate: number;
}

/** Play state reported with a progress ping (`POST /sessions/{id}/progress`). */
export type PlaybackState = "playing" | "paused" | "buffering";

/** The server-resolved watch state echoed by progress / watchState calls. The
 * server owns the Watched threshold; this is the authoritative result. */
export interface WatchStateResult {
  titleId: string;
  resumePositionMs: number;
  watched: boolean;
}

/** A Title's full detail with every `omitempty` hole filled. */
export interface TitleDetail {
  id: string;
  /** The Library this Title belongs to, "" when absent — the detail's parent
   * "Back" link returns a Movie to its owning Library. */
  libraryId: string;
  kind: string;
  title: string;
  year: number;
  needsReview: boolean;
  ambiguous: boolean;
  hidden: boolean;
  tmdbId?: string;
  imdbId?: string;
  resumePositionMs: number;
  watched: boolean;
  addedAt?: string;
  editions: Edition[];
  artwork: Artwork[];
  /** Every selectable Subtitle track the Title offers (embedded + sidecar +
   * fetched), deduped and labeled by the server (ADR-0020). Normalized to []. */
  subtitles: SubtitleTrack[];
  // Enrichment (external-metadata-enrichment): descriptive fields, holes filled
  // (strings → "", numbers → 0, lists → []). enrichmentStatus is "" when the
  // server sent none (un-enriched server).
  overview: string;
  tagline: string;
  contentRating: string;
  releaseDate: string;
  runtimeMinutes: number;
  studio: string;
  genres: string[];
  cast: Credit[];
  enrichmentStatus: EnrichmentStatus | "";
  /** Descriptive fields an Admin hand-edited and Locked (CONTEXT.md), [] when
   * none — re-enrichment skips these; the Admin can release a lock back to auto. */
  lockedFields: string[];
  /** Canonical enriched display title (Episode/Track), "" when none — display
   * only; `title` stays the parsed identity value. */
  displayTitle: string;
  /** Present only for an Episode: its Show/Season/episode parent context. */
  episode?: EpisodeContext;
  /** Present only for a Track: its Artist/Album/disc/track parent context. */
  track?: TrackContext;
}

// --- Metadata-provider settings (metadata-providers 02) --------------------

/** One external metadata source in the settings view (`GET/PUT
 * /settings/metadata-providers`): the static registry facts joined with the
 * current DB settings. `hasKey` reports whether a key is on file WITHOUT ever
 * exposing it; `baseURL` is the effective host (override or registry default). */
export interface MetadataProvider {
  slug: string;
  name: string;
  /** Coarse media-kind groups the source serves ("video" / "music"); the UI
   * groups providers by these. */
  kinds: string[];
  /** "authoritative" (drives the kind) vs. "supplement" (fill-only). */
  role: "authoritative" | "supplement" | (string & {});
  requiresKey: boolean;
  enabled: boolean;
  /** Whether a key is configured — the ONLY thing the UI knows about the secret
   * (the value is never sent to the client). */
  hasKey: boolean;
  baseURL: string;
  /** Effective artwork host for sources whose images come from a host distinct
   * from their API (today only TMDB). Absent for providers with no image host —
   * the UI shows the extra override only where it applies. */
  imageBaseURL?: string;
  description: string;
  docsURL: string;
}

/** The full settings view: the provider list, the server-wide language, and the
 * derived per-kind enablement summary (what the running server will enrich). */
export interface MetadataProvidersView {
  providers: MetadataProvider[];
  metadataLanguage: string;
  enablement: { video: boolean; music: boolean };
  /** Whether a completed scan enqueues a background Enrichment pass. */
  autoEnrichAfterScan: boolean;
  /** The scheduled safety-net enrich cadence, in seconds (0 disables the sweep).
   * The UI presents this in minutes. */
  enrichIntervalSeconds: number;
  /** The MusicBrainz throttle, in milliseconds (0 = no throttle). */
  musicBrainzRateLimitMs: number;
}

/** One provider's partial update in the PUT body. Every field is optional:
 * `apiKey` omitted = unchanged, "" = clear, non-empty = set; `enabled`/`baseURL`
 * omitted = unchanged (`baseURL` "" resets to the registry default). */
export interface ProviderUpdate {
  slug: string;
  enabled?: boolean;
  apiKey?: string;
  baseURL?: string;
  /** Image-host override (TMDB only); omitted = unchanged, "" = reset to default. */
  imageBaseURL?: string;
}

/** The PUT body: a set of per-provider partial updates plus an optional global
 * metadata language (omitted = unchanged; "" is rejected). */
export interface UpdateMetadataProvidersInput {
  providers?: ProviderUpdate[];
  metadataLanguage?: string;
  /** Behavior knobs (enrichment-runtime-settings): each optional (omitted =
   * unchanged). enrichIntervalSeconds / musicBrainzRateLimitMs must be >= 0. */
  autoEnrichAfterScan?: boolean;
  enrichIntervalSeconds?: number;
  musicBrainzRateLimitMs?: number;
}

/** The result of `POST /settings/metadata-providers/{slug}/test` — a best-effort
 * connectivity/credential probe. */
export interface TestProviderResult {
  ok: boolean;
  detail: string;
}

/** A Library's Enrichment policy view (ADR-0027): the SPARSE overrides plus the
 * derived enablement for display. This slice carries the enrich-on/off key.
 * `enrichEnabled` is the STORED override — `null` means inherit (the key tracks
 * the global config live), so `null`-vs-value reads as inherited-vs-overridden.
 * `inheritedEnrichEnabled` is what "inherit" currently resolves to (the server
 * enriches at least one kind), for labeling the inherit option. `effective` is the
 * per-kind enablement the Library will actually enrich under this policy. */
/** A provider's stable slug + display name (the authoritative dropdown entries). */
export interface ProviderRef {
  slug: string;
  name: string;
}

/** One per-Supplement tri-state control (issue 05): the provider, its STORED
 * override (`null` = inherit; `true`/`false` = forced on/off), and the global
 * enabled state inheriting resolves to (for the "Inherit (currently On/Off)" label). */
export interface SupplementControl {
  slug: string;
  name: string;
  override: boolean | null;
  inheritedEnabled: boolean;
}

export interface EnrichmentPolicy {
  enrichEnabled: boolean | null;
  inheritedEnrichEnabled: boolean;
  effective: { video: boolean; music: boolean };
  /** The STORED metadata-language override — `null` means inherit (the key tracks
   * the global language live), so `null`-vs-value reads as inherited-vs-overridden. */
  metadataLanguage: string | null;
  /** The global metadata language the unset key tracks live (for labeling the
   * inherit option and prefilling the field). "" when the server has none set. */
  inheritedMetadataLanguage: string;
  /** The STORED Authoritative-provider override slug — `null` means inherit the
   * kind's global default. */
  authoritativeProvider: string | null;
  /** The kind's global default authoritative (what "inherit" resolves to). */
  inheritedAuthoritative: ProviderRef;
  /** The provider actually LEADING under this policy (the stored override, or the
   * default when inheriting, or the fallback when the chosen one is unreachable). */
  effectiveAuthoritative: ProviderRef;
  /** Set (to the chosen-but-unreachable slug) when the pointer fell back to the
   * default because its provider lost its key / was disabled; `null` otherwise. */
  authoritativeUnreachable: string | null;
  /** The dropdown candidates: usable Full providers of the Library's kind (keyed). */
  authoritativeCandidates: ProviderRef[];
  /** The per-Supplement tri-state controls: one per togglable Supplement of the
   * Library's kind (the current authoritative excluded). */
  supplements: SupplementControl[];
}

/** The `PUT /libraries/{id}/enrichment-policy` body: a partial update. Each key is
 * OPTIONAL — omitted = unchanged. Sending a key as `null` clears it back to
 * inherit; a value sets a deliberate override. */
export interface UpdateEnrichmentPolicyInput {
  enrichEnabled?: boolean | null;
  metadataLanguage?: string | null;
  authoritativeProvider?: string | null;
  /** Per-Supplement tri-state partial update: a slug → true/false forces on/off,
   * → null clears to inherit; a slug ABSENT from the object is unchanged. */
  providerOverrides?: Record<string, boolean | null>;
}

/** One subtitle provider in the settings view (subtitles/05, ADR-0021): registry
 * facts joined with current settings. The key is never returned — only `hasKey`. */
export interface SubtitleProvider {
  slug: string;
  name: string;
  requiresKey: boolean;
  enabled: boolean;
  hasKey: boolean;
  baseURL: string;
  description: string;
  docsURL: string;
}

/** The `GET/PUT /settings/subtitle-providers` view: the provider list plus the
 * auto-fetch-after-scan language ("" = off). */
export interface SubtitleProvidersView {
  providers: SubtitleProvider[];
  autoFetchLang: string;
}

/** One subtitle provider's partial update (apiKey omitted = unchanged, "" = clear,
 * non-empty = set; enabled/baseURL follow the same omit=unchanged rule). */
export interface SubtitleProviderUpdate {
  slug: string;
  enabled?: boolean;
  apiKey?: string;
  baseURL?: string;
}

/** The `PUT /settings/subtitle-providers` body: per-provider partial updates plus
 * an optional auto-fetch language (omitted = unchanged; "" turns auto-fetch off). */
export interface UpdateSubtitleProvidersInput {
  providers?: SubtitleProviderUpdate[];
  autoFetchLang?: string;
}
