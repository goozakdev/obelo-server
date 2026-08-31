// The one place that turns the server's raw, `omitempty`-pruned JSON into the
// consistent shapes the rest of the app consumes (PRD "Known API ergonomics":
// boolean flags use omitempty — absent means false — and timestamps are
// RFC3339, parsed to local at render). Doing it here, behind the single API
// client, means components never have to write `?? false` / `?? 0` / `?? []`
// and never see two shapes for the same field.
//
// Timestamps are intentionally left as the RFC3339 strings the server sent;
// formatting to local time is a render concern (see ../time.ts), kept out of
// the wire layer so the raw value is still available if a component needs it.

import type {
  Album,
  AlbumRaw,
  MatcherApplied,
  MatcherDocument,
  MatcherDocumentRaw,
  MatcherFile,
  MatcherFileRaw,
  MatcherFileState,
  MatcherGroup,
  MatcherGroupRaw,
  MatcherPlacement,
  MatcherSlot,
  MatcherSlotRaw,
  SeriesSlots,
  SeriesSlotsRaw,
  SlotPosition,
  SlotsUnavailableReason,
  AlbumTracks,
  AlbumTracksResponseRaw,
  ArtistAlbums,
  ArtistAlbumsResponseRaw,
  ArtistSummary,
  ArtistSummaryRaw,
  ArtistsPage,
  ArtistsResponseRaw,
  Collection,
  CollectionDetail,
  CollectionDetailRaw,
  CollectionRaw,
  CollectionSummary,
  CollectionSummaryRaw,
  Edition,
  EditionRaw,
  EnrichmentAttentionTitle,
  EnrichmentAttentionTitleRaw,
  FixContext,
  FixContextRaw,
  NeedsReviewItem,
  NeedsReviewItemRaw,
  NeedsReviewReason,
  EpisodeSummary,
  EpisodeSummaryRaw,
  HomeResponseRaw,
  HomeRows,
  Library,
  MatchOverride,
  MatchOverrideRaw,
  MediaFile,
  FileRaw,
  Playlist,
  PlaylistDetail,
  PlaylistDetailRaw,
  PlaylistMember,
  PlaylistMemberRaw,
  PlaylistRaw,
  PlaylistSummary,
  PlaylistSummaryRaw,
  ScanStatus,
  ScanStatusRaw,
  ResumePoint,
  ResumePointRaw,
  Season,
  SeasonEpisodes,
  SeasonEpisodesResponseRaw,
  SeasonRaw,
  ShowProblems,
  ShowProblemsRaw,
  ShowSeasons,
  ShowSeasonsResponseRaw,
  ShowSummary,
  ShowSummaryRaw,
  ShowsPage,
  ShowsResponseRaw,
  Stream,
  TitleDetail,
  TitleDetailRaw,
  TitleSummary,
  TitleSummaryRaw,
  TitlesPage,
  TitlesResponseRaw,
  TrackSummary,
  TrackSummaryRaw,
  UnmatchedFile,
  UnmatchedFileRaw,
  UserDetail,
  UserDetailRaw,
  WatchStateResult,
} from "./types";

/** Fill a Library's holes (rootFolders may be absent on a malformed payload). */
export function normalizeLibrary(raw: Library): Library {
  return {
    id: raw.id,
    name: raw.name,
    kind: raw.kind,
    createdAt: raw.createdAt,
    rootFolders: raw.rootFolders ?? [],
  };
}

/** Fill a scan status's `omitempty` holes (counts → 0): the admin hub renders
 * the counts unconditionally, so `titlesFound`/`filesFound` must always be a
 * number even on a fresh, never-scanned Library. */
export function normalizeScanStatus(raw: ScanStatusRaw): ScanStatus {
  return {
    libraryId: raw.libraryId,
    state: raw.state,
    titleCount: raw.titleCount ?? 0,
    titlesFound: raw.titlesFound ?? 0,
    filesFound: raw.filesFound ?? 0,
    errorMessage: raw.errorMessage,
    startedAt: raw.startedAt,
    finishedAt: raw.finishedAt,
    scope: raw.scope,
  };
}

/** Fill a Title summary's `omitempty` holes (booleans → false, numbers → 0,
 * genres → []). */
export function normalizeTitleSummary(raw: TitleSummaryRaw): TitleSummary {
  return {
    id: raw.id,
    kind: raw.kind,
    title: raw.title,
    year: raw.year ?? 0,
    needsReview: raw.needsReview ?? false,
    ambiguous: raw.ambiguous ?? false,
    tmdbId: raw.tmdbId,
    imdbId: raw.imdbId,
    addedAt: raw.addedAt,
    resumePositionMs: raw.resumePositionMs ?? 0,
    watched: raw.watched ?? false,
    genres: raw.genres ?? [],
    contentRating: raw.contentRating,
    enrichmentStatus: raw.enrichmentStatus,
    artworkVersion: raw.artworkVersion,
    episode: raw.episode,
    track: raw.track,
  };
}

/** Normalize a titles page: titles → array (each normalized); nextCursor → null
 * on the last page (the server omits it), so callers stop paginating on null. */
export function normalizeTitlesPage(raw: TitlesResponseRaw): TitlesPage {
  return {
    titles: (raw?.titles ?? []).map(normalizeTitleSummary),
    nextCursor: raw?.nextCursor ? raw.nextCursor : null,
  };
}

/** Normalize the Home rows: each row → an array of normalized Title summaries
 * (the same shape the grid consumes), so Home cards reuse the grid's PosterTile
 * unchanged. A row the server omitted (or sent empty) becomes []. */
export function normalizeHome(raw: HomeResponseRaw): HomeRows {
  return {
    continueWatching: (raw?.continueWatching ?? []).map(normalizeTitleSummary),
    upNext: (raw?.upNext ?? []).map(normalizeTitleSummary),
    recentlyAdded: (raw?.recentlyAdded ?? []).map(normalizeTitleSummary),
  };
}

/** Fill a watch-state response's `omitempty` holes (resume → 0, watched →
 * false): the server omits a zero resume and a false `watched`, but callers
 * branch on present values to update the resume marker / watched badge. */
export function normalizeWatchState(raw: WatchStateResult): WatchStateResult {
  return {
    titleId: raw.titleId,
    resumePositionMs: raw.resumePositionMs ?? 0,
    watched: raw.watched ?? false,
  };
}

/** Fill an Unmatched file's `omitempty` holes (reason → ""): the attention list
 * renders the reason unconditionally, so it must always be a string. */
/** Fill a Show-problems entry's holes (numbers → 0, strings → "", lists → []), so
 * a row can add its counts without guarding every field. An absent count means
 * zero, which is the meaning rather than a fallback. */
export function normalizeShowProblems(raw: ShowProblemsRaw): ShowProblems {
  return {
    showId: raw.showId,
    title: raw.title,
    year: raw.year ?? 0,
    path: raw.path ?? "",
    unassigned: raw.unassigned ?? 0,
    unidentified: raw.unidentified ?? 0,
    unmatchedPaths: raw.unmatchedPaths ?? [],
    orphaned: raw.orphaned ?? 0,
    orphanedPath: raw.orphanedPath ?? "",
    unreadablePaths: raw.unreadablePaths ?? [],
  };
}

export function normalizeUnmatchedFile(raw: UnmatchedFileRaw): UnmatchedFile {
  return {
    id: raw.id,
    path: raw.path,
    folderPath: raw.folderPath ?? "",
    kind: raw.kind === "unreadable" ? "unreadable" : "unidentified",
    reason: raw.reason ?? "",
    addedAt: raw.addedAt,
  };
}

/** Fill a Match override's `omitempty` holes (year → 0, `orphaned` → present
 * boolean): the overrides list branches on `orphaned` to highlight folder-rename
 * orphans, so it must always be a present boolean (absent → false). */
export function normalizeMatchOverride(raw: MatchOverrideRaw): MatchOverride {
  return {
    id: raw.id,
    folderPath: raw.folderPath,
    title: raw.title,
    year: raw.year ?? 0,
    tmdbId: raw.tmdbId,
    imdbId: raw.imdbId,
    identityKey: raw.identityKey,
    orphaned: raw.orphaned ?? false,
    createdAt: raw.createdAt,
  };
}

/** Fill an enrichment-attention Title's `omitempty` holes (year → 0): the list
 * renders the year conditionally, but a present number keeps the shape uniform. */
export function normalizeEnrichmentAttentionTitle(
  raw: EnrichmentAttentionTitleRaw,
): EnrichmentAttentionTitle {
  return {
    id: raw.id,
    kind: raw.kind,
    title: raw.title,
    year: raw.year ?? 0,
    enrichmentStatus: raw.enrichmentStatus,
    ...normalizeFixContext(raw),
  };
}

/** Fill the shared "which item / which file" context both attention lists carry
 * (strings → "", numbers → 0). A row renders its breadcrumb unconditionally, so
 * these must be present values rather than optional holes. */
export function normalizeFixContext(raw: FixContextRaw): FixContext {
  return {
    path: raw.path ?? "",
    showTitle: raw.showTitle ?? "",
    seasonNumber: raw.seasonNumber ?? 0,
    episodeNumber: raw.episodeNumber ?? 0,
    episodeLabel: raw.episodeLabel ?? "",
    artistName: raw.artistName ?? "",
    albumTitle: raw.albumTitle ?? "",
    discNumber: raw.discNumber ?? 0,
    trackNumber: raw.trackNumber ?? 0,
    showId: raw.showId ?? "",
    albumId: raw.albumId ?? "",
    enrichedTitle: raw.enrichedTitle ?? "",
    releaseDate: raw.releaseDate ?? "",
  };
}

/** The reason the scanner flags an item is decided by its kind (see the server's
 * needsReviewReason), so an older server that omits the field still yields the
 * right sentence rather than a blank one. */
function reasonForKind(kind: string): NeedsReviewReason {
  if (kind === "episode") return "episode-numbering";
  if (kind === "track") return "untagged";
  return "no-year";
}

/** Fill an identity-attention item's holes (year → 0, folderPath → "").
 *
 * `needsReview` defaults to TRUE rather than false: the list carried nothing but
 * needs-review items before the flag was sent, so an absent field means "an older
 * server", not "not a needs-review row". Defaulting it the other way would silently
 * stop every row on such a server from stating its problem. */
export function normalizeNeedsReviewItem(
  raw: NeedsReviewItemRaw,
): NeedsReviewItem {
  return {
    id: raw.id,
    kind: raw.kind,
    title: raw.title,
    year: raw.year ?? 0,
    folderPath: raw.folderPath ?? "",
    reason: raw.reason ?? reasonForKind(raw.kind),
    needsReview: raw.needsReview !== false,
    ambiguous: raw.ambiguous ?? false,
    collidingPaths: raw.collidingPaths ?? [],
    enrichmentStatus: raw.enrichmentStatus ?? "pending",
    ...normalizeFixContext(raw),
  };
}

/** Fill a User detail's `omitempty` holes (`libraryIds` → [], `ratingCeiling` →
 * ""): the server omits an empty grant set and a null ceiling, but the row maps
 * `libraryIds` to names unconditionally (and issue 03 branches on the ceiling),
 * so both must always be present. An empty `libraryIds` means "sees no catalog";
 * an empty `ratingCeiling` means "uncapped". */
export function normalizeUserDetail(raw: UserDetailRaw): UserDetail {
  return {
    id: raw.id,
    username: raw.username,
    role: raw.role,
    libraryIds: raw.libraryIds ?? [],
    ratingCeiling: raw.ratingCeiling ?? "",
  };
}

function normalizeStream(s: Stream): Stream {
  return {
    index: s.index,
    kind: s.kind,
    codec: s.codec,
    language: s.language,
    width: s.width ?? 0,
    height: s.height ?? 0,
    channels: s.channels ?? 0,
    isDefault: s.isDefault ?? false,
  };
}

function normalizeFile(raw: FileRaw): MediaFile {
  return {
    id: raw.id,
    path: raw.path,
    container: raw.container,
    videoCodec: raw.videoCodec,
    audioCodec: raw.audioCodec,
    width: raw.width ?? 0,
    height: raw.height ?? 0,
    bitrate: raw.bitrate ?? 0,
    durationMs: raw.durationMs ?? 0,
    sizeBytes: raw.sizeBytes ?? 0,
    missing: raw.missing ?? false,
    streams: (raw.streams ?? []).map(normalizeStream),
    audioStreams: raw.audioStreams ?? [],
    videoStreams: raw.videoStreams ?? [],
  };
}

function normalizeEdition(raw: EditionRaw): Edition {
  return {
    id: raw.id,
    name: raw.name,
    files: (raw.files ?? []).map(normalizeFile),
  };
}

/** Fill a Title detail's holes: booleans → false, numbers → 0, nested lists →
 * arrays (each element normalized). The Episode parent context (TV) is passed
 * through as-is when present (undefined for a Movie). */
export function normalizeTitleDetail(raw: TitleDetailRaw): TitleDetail {
  return {
    id: raw.id,
    libraryId: raw.libraryId ?? "",
    kind: raw.kind,
    title: raw.title,
    year: raw.year ?? 0,
    needsReview: raw.needsReview ?? false,
    ambiguous: raw.ambiguous ?? false,
    hidden: raw.hidden ?? false,
    tmdbId: raw.tmdbId,
    imdbId: raw.imdbId,
    resumePositionMs: raw.resumePositionMs ?? 0,
    watched: raw.watched ?? false,
    addedAt: raw.addedAt,
    editions: (raw.editions ?? []).map(normalizeEdition),
    artwork: raw.artwork ?? [],
    artworkVersion: raw.artworkVersion,
    subtitles: raw.subtitles ?? [],
    // Enrichment holes filled: strings → "", numbers → 0, lists → [].
    overview: raw.overview ?? "",
    tagline: raw.tagline ?? "",
    contentRating: raw.contentRating ?? "",
    releaseDate: raw.releaseDate ?? "",
    runtimeMinutes: raw.runtimeMinutes ?? 0,
    studio: raw.studio ?? "",
    genres: raw.genres ?? [],
    cast: raw.cast ?? [],
    enrichmentStatus: raw.enrichmentStatus ?? "",
    lockedFields: raw.lockedFields ?? [],
    displayTitle: raw.displayTitle ?? "",
    episode: raw.episode,
    track: raw.track,
  };
}

// --- TV browse normalizers (issue tv-music/01) ------------------------------

/** Fill a Show summary's holes (year → 0, needsReview → false). */
export function normalizeShowSummary(raw: ShowSummaryRaw): ShowSummary {
  return {
    id: raw.id,
    libraryId: raw.libraryId ?? "",
    kind: raw.kind,
    title: raw.title,
    year: raw.year ?? 0,
    needsReview: raw.needsReview ?? false,
    tmdbId: raw.tmdbId,
    imdbId: raw.imdbId,
    addedAt: raw.addedAt,
    unwatchedEpisodeCount: raw.unwatchedEpisodeCount ?? 0,
    overview: raw.overview ?? "",
    genres: raw.genres ?? [],
    contentRating: raw.contentRating,
    network: raw.network,
    enrichmentStatus: raw.enrichmentStatus,
    posterUrl: raw.posterUrl,
    backgroundUrl: raw.backgroundUrl,
    logoUrl: raw.logoUrl,
    lockedFields: raw.lockedFields,
    enrichmentOverride: raw.enrichmentOverride,
    cast: raw.cast ?? [],
  };
}

/** Normalize a Shows page (TV grid): shows → array; nextCursor → null on last page. */
export function normalizeShowsPage(raw: ShowsResponseRaw): ShowsPage {
  return {
    shows: (raw?.shows ?? []).map(normalizeShowSummary),
    nextCursor: raw?.nextCursor ? raw.nextCursor : null,
  };
}

function normalizeSeason(raw: SeasonRaw): Season {
  return {
    id: raw.id,
    showId: raw.showId,
    seasonNumber: raw.seasonNumber,
    specials: raw.specials ?? raw.seasonNumber === 0,
    episodeCount: raw.episodeCount ?? 0,
    posterUrl: raw.posterUrl,
  };
}

/** Normalize a Show's resume point (ADR-0028), or null when the Show is not
 * started / fully watched (the server omits it). */
function normalizeResumePoint(
  raw: ResumePointRaw | null | undefined,
): ResumePoint | null {
  if (!raw) return null;
  return {
    id: raw.id,
    kind: raw.kind,
    seasonId: raw.seasonId,
    seasonNumber: raw.seasonNumber,
    episodeNumber: raw.episodeNumber ?? 0,
    episodeLabel: raw.episodeLabel ?? "",
    title: raw.title,
    overview: raw.overview ?? "",
    resumePositionMs: raw.resumePositionMs ?? 0,
    durationMs: raw.durationMs ?? 0,
    mode: raw.mode,
    enrichmentStatus: raw.enrichmentStatus,
    stillUrl: raw.stillUrl,
  };
}

/** Normalize a Show's Seasons response. */
export function normalizeShowSeasons(raw: ShowSeasonsResponseRaw): ShowSeasons {
  return {
    show: normalizeShowSummary(raw.show),
    seasons: (raw?.seasons ?? []).map(normalizeSeason),
    resumePoint: normalizeResumePoint(raw?.resumePoint),
  };
}

function normalizeEpisodeSummary(raw: EpisodeSummaryRaw): EpisodeSummary {
  return {
    id: raw.id,
    kind: raw.kind,
    title: raw.title,
    seasonNumber: raw.seasonNumber,
    episodeNumber: raw.episodeNumber ?? 0,
    episodeLabel: raw.episodeLabel ?? "",
    needsReview: raw.needsReview ?? false,
    resumePositionMs: raw.resumePositionMs ?? 0,
    watched: raw.watched ?? false,
    addedAt: raw.addedAt,
    overview: raw.overview ?? "",
    enrichmentStatus: raw.enrichmentStatus,
    stillUrl: raw.stillUrl,
  };
}

/** Normalize a Season's Episodes response. */
export function normalizeSeasonEpisodes(
  raw: SeasonEpisodesResponseRaw,
): SeasonEpisodes {
  return {
    season: normalizeSeason(raw.season),
    episodes: (raw?.episodes ?? []).map(normalizeEpisodeSummary),
  };
}

// --- Music browse normalizers (issue tv-music/03) ---------------------------

/** Fill an Artist summary's holes (nothing optional today, but normalized for
 * consistency with the other list shapes). */
export function normalizeArtistSummary(raw: ArtistSummaryRaw): ArtistSummary {
  return {
    id: raw.id,
    libraryId: raw.libraryId ?? "",
    kind: raw.kind,
    name: raw.name,
    overview: raw.overview ?? "",
    genres: raw.genres ?? [],
    enrichmentStatus: raw.enrichmentStatus,
    artworkUrl: raw.artworkUrl,
    backgroundUrl: raw.backgroundUrl,
    logoUrl: raw.logoUrl,
    lockedFields: raw.lockedFields,
    enrichmentOverride: raw.enrichmentOverride,
  };
}

/** Normalize an Artists page (Music list): artists → array; nextCursor → null
 * on the last page. */
export function normalizeArtistsPage(raw: ArtistsResponseRaw): ArtistsPage {
  return {
    artists: (raw?.artists ?? []).map(normalizeArtistSummary),
    nextCursor: raw?.nextCursor ? raw.nextCursor : null,
  };
}

function normalizeAlbum(raw: AlbumRaw): Album {
  return {
    id: raw.id,
    artistId: raw.artistId,
    artistName: raw.artistName ?? "",
    title: raw.title,
    year: raw.year ?? 0,
    hasArtwork: raw.hasArtwork ?? false,
    artworkVersion: raw.artworkVersion,
    releaseType: raw.releaseType ?? "",
    trackCount: raw.trackCount ?? 0,
    genres: raw.genres ?? [],
    enrichmentStatus: raw.enrichmentStatus,
    lockedFields: raw.lockedFields,
    enrichmentOverride: raw.enrichmentOverride,
  };
}

/** Normalize an Artist's Albums response. */
export function normalizeArtistAlbums(raw: ArtistAlbumsResponseRaw): ArtistAlbums {
  return {
    artist: normalizeArtistSummary(raw.artist),
    albums: (raw?.albums ?? []).map(normalizeAlbum),
  };
}

function normalizeTrackSummary(raw: TrackSummaryRaw): TrackSummary {
  return {
    id: raw.id,
    kind: raw.kind,
    title: raw.title,
    discNumber: raw.discNumber ?? 0,
    trackNumber: raw.trackNumber ?? 0,
    durationMs: raw.durationMs ?? 0,
    needsReview: raw.needsReview ?? false,
    resumePositionMs: raw.resumePositionMs ?? 0,
    watched: raw.watched ?? false,
    overview: raw.overview ?? "",
    enrichmentStatus: raw.enrichmentStatus,
  };
}

/** Normalize an Album's Tracks response (tracks already in disc/track order). */
export function normalizeAlbumTracks(raw: AlbumTracksResponseRaw): AlbumTracks {
  return {
    album: normalizeAlbum(raw.album),
    tracks: (raw?.tracks ?? []).map(normalizeTrackSummary),
  };
}

// --- Collections normalizers (collections-playlists-ui issue 01) ------------

/** Fill a Collection card's `omitempty` holes (description → ""): the list
 * renders the name + member count + a poster, with the description shown only
 * when present. `posterUrl` stays undefined when the server omitted it (no
 * visible member to draw from), so the card falls back to a placeholder. */
export function normalizeCollectionSummary(
  raw: CollectionSummaryRaw,
): CollectionSummary {
  return {
    id: raw.id,
    name: raw.name,
    description: raw.description ?? "",
    memberCount: raw.memberCount ?? 0,
    posterUrl: raw.posterUrl,
  };
}

/** Normalize a Collection's detail: description → "", members → an array of
 * normalized {@link TitleSummary} (the SAME shape a browse grid consumes), so
 * the detail renders members with PosterTile/the grid unchanged. */
export function normalizeCollectionDetail(
  raw: CollectionDetailRaw,
): CollectionDetail {
  return {
    id: raw.id,
    name: raw.name,
    description: raw.description ?? "",
    memberCount: raw.memberCount ?? 0,
    members: (raw?.members ?? []).map(normalizeTitleSummary),
  };
}

/** Fill a bare Collection's `omitempty` hole (description → ""): the shape
 * create/update return (collections-playlists-ui issue 02). The member
 * count/poster are NOT carried here — the list/detail GETs compute those
 * per-viewer — so the curation screens refetch the list/detail after a write. */
export function normalizeCollection(raw: CollectionRaw): Collection {
  return {
    id: raw.id,
    name: raw.name,
    description: raw.description ?? "",
  };
}

// --- Playlists normalizers (collections-playlists-ui issue 03) --------------

/** Fill a Playlist card's `omitempty` holes (kind → "" while untyped, itemCount
 * → 0): one entry in the caller's Playlists list. */
export function normalizePlaylistSummary(
  raw: PlaylistSummaryRaw,
): PlaylistSummary {
  return {
    id: raw.id,
    name: raw.name,
    kind: raw.kind ?? "",
    itemCount: raw.itemCount ?? 0,
  };
}

/** Fill a bare Playlist's hole (kind → "" while untyped): the shape create/rename
 * return. The item count/members are NOT carried here — the list/detail GETs do —
 * so the screens refetch after a write. */
export function normalizePlaylist(raw: PlaylistRaw): Playlist {
  return { id: raw.id, name: raw.name, kind: raw.kind ?? "" };
}

/** Fill a Playlist member: the existing title-summary normalization PLUS its
 * playlist-item id (preserved so duplicates stay distinguishable/removable). */
export function normalizePlaylistMember(
  raw: PlaylistMemberRaw,
): PlaylistMember {
  return { ...normalizeTitleSummary(raw), itemId: raw.itemId };
}

/** Normalize a Playlist's detail: kind → "" while untyped, members → an array of
 * normalized {@link PlaylistMember} in the server's POSITION order. */
export function normalizePlaylistDetail(
  raw: PlaylistDetailRaw,
): PlaylistDetail {
  return {
    id: raw.id,
    name: raw.name,
    kind: raw.kind ?? "",
    memberCount: raw.memberCount ?? 0,
    members: (raw?.members ?? []).map(normalizePlaylistMember),
  };
}

// --- The file matcher (ADR-0044) -----------------------------------------
//
// The matcher document is pruned hard on the wire (every count and every array is
// `omitempty`), and the screen's whole job is comparing two lists — so an absent
// `parsed` reaching a component as `undefined` would be a bug factory. Everything
// arrives here as a present array with present numbers.

const SLOTS_UNAVAILABLE_REASONS: SlotsUnavailableReason[] = [
  "no-series-match",
  "enrichment-disabled",
  "provider-cannot-list",
  "provider-unreachable",
];

/** Keep only a reason this client knows how to explain. A reason it does not is
 * dropped rather than printed raw, because the point of the field is a SENTENCE
 * naming what to go fix; an unknown token names nothing. */
function normalizeSlotsUnavailable(raw?: string): SlotsUnavailableReason | undefined {
  const found = SLOTS_UNAVAILABLE_REASONS.find((r) => r === raw);
  return found;
}

function normalizeFileState(raw?: string): MatcherFileState {
  return raw === "placed" || raw === "ignored" ? raw : "unassigned";
}

function normalizeSlotPosition(raw?: { group?: number; slot?: number }): SlotPosition {
  return { group: raw?.group ?? 0, slot: raw?.slot ?? 0 };
}

export function normalizeMatcherSlot(raw: MatcherSlotRaw): MatcherSlot {
  const slot: MatcherSlot = {
    group: raw.group ?? 0,
    slot: raw.slot ?? 0,
    titleId: raw.titleId,
    name: raw.name,
    overview: raw.overview,
    airDate: raw.airDate,
    stillUrl: raw.stillUrl,
  };
  if (raw.record) {
    slot.record = {
      externalId: raw.record.externalId,
      group: raw.record.group ?? 0,
      slot: raw.record.slot ?? 0,
      name: raw.record.name,
      overview: raw.record.overview,
      airDate: raw.record.airDate,
      stillUrl: raw.record.stillUrl,
    };
  }
  return slot;
}

export function normalizeMatcherGroup(raw: MatcherGroupRaw): MatcherGroup {
  return {
    number: raw.number ?? 0,
    source: raw.source ?? "local",
    slotCount: raw.slotCount ?? 0,
    slotsLoaded: raw.slotsLoaded ?? false,
    slotsUnavailable: normalizeSlotsUnavailable(raw.slotsUnavailable),
    fileCount: raw.fileCount ?? 0,
    placedCount: raw.placedCount ?? 0,
    unassignedCount: raw.unassignedCount ?? 0,
    ignoredCount: raw.ignoredCount ?? 0,
    slots: (raw.slots ?? []).map(normalizeMatcherSlot),
  };
}

export function normalizeMatcherFile(raw: MatcherFileRaw): MatcherFile {
  // `ordinal` is omitempty, so the single-File case arrives with no ordinal at
  // all; 1 is what "the only part" means and what a later merge counts from.
  const placements: MatcherPlacement[] = (raw.placements ?? []).map((p) => ({
    group: p.group ?? 0,
    slot: p.slot ?? 0,
    ordinal: p.ordinal ?? 1,
  }));
  return {
    path: raw.path ?? "",
    state: normalizeFileState(raw.state),
    titleId: raw.titleId,
    parsed: (raw.parsed ?? []).map(normalizeSlotPosition),
    placements,
    decided: raw.decided ?? false,
    orphaned: raw.orphaned ?? false,
    unreadable: raw.unreadable ?? false,
    reason: raw.reason ?? "",
  };
}

export function normalizeMatcher(raw: MatcherDocumentRaw): MatcherDocument {
  const applied: MatcherApplied | undefined = raw.applied
    ? {
        rearranged: raw.applied.rearranged ?? 0,
        displaced: raw.applied.displaced ?? [],
        deferred: raw.applied.deferred ?? [],
      }
    : undefined;
  return {
    containerId: raw.containerId ?? "",
    containerType: raw.containerType ?? "",
    libraryId: raw.libraryId ?? "",
    title: raw.title ?? "",
    year: raw.year,
    seriesExternalId: raw.seriesExternalId,
    slotsUnavailable: normalizeSlotsUnavailable(raw.slotsUnavailable),
    groups: (raw.groups ?? []).map(normalizeMatcherGroup),
    files: (raw.files ?? []).map(normalizeMatcherFile),
    applied,
  };
}

export function normalizeSeriesSlots(raw: SeriesSlotsRaw): SeriesSlots {
  return {
    externalId: raw.externalId ?? "",
    groups: (raw.groups ?? []).map((g) => ({
      number: g.number ?? 0,
      slotCount: g.slotCount ?? 0,
    })),
    group: raw.group ? { number: raw.group.number ?? 0 } : undefined,
    slots: (raw.slots ?? []).map(normalizeMatcherSlot),
  };
}
