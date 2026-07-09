import { ApiError, NetworkError, parseErrorEnvelope } from "./errors";
import { localStorageTokenStore, type TokenStore } from "./token";
import {
  normalizeAlbumTracks,
  normalizeArtistAlbums,
  normalizeArtistsPage,
  normalizeCollection,
  normalizeCollectionDetail,
  normalizeCollectionSummary,
  normalizeEnrichmentAttentionTitle,
  normalizeNeedsReviewItem,
  normalizeHome,
  normalizeLibrary,
  normalizeMatchOverride,
  normalizePlaylist,
  normalizePlaylistDetail,
  normalizePlaylistSummary,
  normalizeScanStatus,
  normalizeSeasonEpisodes,
  normalizeShowSeasons,
  normalizeShowsPage,
  normalizeTitleDetail,
  normalizeTitlesPage,
  normalizeUnmatchedFile,
  normalizeUserDetail,
  normalizeWatchState,
} from "./normalize";
import type {
  AlbumTracks,
  AlbumTracksResponseRaw,
  ArtistAlbums,
  ArtistAlbumsResponseRaw,
  ArtistsPage,
  ArtistsResponseRaw,
  ArtworkCandidate,
  ArtworkCandidatesResult,
  EntityMetadataEditInput,
  Collection,
  CollectionDetail,
  CollectionDetailRaw,
  CollectionRaw,
  CollectionSummary,
  CollectionsResponseRaw,
  CreateCollectionInput,
  CreateLibraryInput,
  UpdateLibraryInput,
  UpdateCollectionInput,
  CreateUserInput,
  Device,
  DevicesResponse,
  EnrichmentAttentionTitle,
  EnrichmentAttentionTitleRaw,
  EnrichmentCandidate,
  EnrichmentCandidatesResult,
  EnrichmentSearchOptions,
  EntityEnrichmentDetail,
  EnrichmentMatchInput,
  FixMatchInput,
  NeedsReviewItem,
  NeedsReviewItemRaw,
  HomeResponseRaw,
  HomeRows,
  LibrariesResponse,
  Library,
  ListTitlesOptions,
  LoginRequest,
  LoginResult,
  MatchOverride,
  MatchOverrideRaw,
  MetadataEditInput,
  MetadataProvidersView,
  UpdateMetadataProvidersInput,
  UpdateSubtitleProvidersInput,
  TestProviderResult,
  Playlist,
  PlaylistDetail,
  PlaylistDetailRaw,
  PlaylistRaw,
  PlaylistSummary,
  PlaylistsResponseRaw,
  PlaybackDecision,
  PlaybackState,
  ScanMode,
  ScanStatus,
  ScanStatusRaw,
  SeasonEpisodes,
  SeasonEpisodesResponseRaw,
  ServerInfo,
  SetupRequest,
  SetupResult,
  ShowSeasons,
  ShowSeasonsResponseRaw,
  ShowsPage,
  ShowsResponseRaw,
  StartPlaybackOptions,
  SubtitleCandidate,
  SubtitleProvidersView,
  SubtitleTrack,
  TitleDetail,
  TitleDetailRaw,
  TitlesPage,
  TitlesResponseRaw,
  UnmatchedFile,
  UnmatchedFileRaw,
  User,
  UserDetail,
  UserDetailRaw,
  UsersResponse,
  WatchStateResult,
} from "./types";

// ApiClient is the SINGLE place in the app that talks HTTP to /api/v1
// (PRD: "the one seam"). Components and hooks call its typed methods; they must
// never call `fetch` directly. It:
//   - prefixes every path with /api/v1 (same-origin in production — the
//     monolith serves the SPA, so paths are relative; ADR-0006),
//   - attaches `Authorization: Bearer <token>` when the token store has one,
//   - parses the standard error envelope into a typed ApiError, and
//   - maps a failed fetch (unreachable server) into a typed NetworkError.
//
// New endpoints are added as methods here (getServerInfo today; login, browse,
// playback, … in later slices), keeping all request/response/error mapping in
// one tested module.

/** Version prefix every route lives under (docs/api-contract.md). */
export const API_PREFIX = "/api/v1";

/** The named SSE event types the server publishes over GET /events (ADR-0016,
 * docs/api-contract.md §"Real-time updates"). EventSource only delivers events
 * whose name has a registered listener, so a server event type only reaches the
 * app once it appears here. `enrichProgress` is broadcast; `scanProgress` /
 * `libraryUpdated` are library-scoped; the session events are Admin-only. */
export const EVENT_TYPES = [
  "enrichProgress",
  "scanProgress",
  "libraryUpdated",
  "sessionStarted",
  "nowPlaying",
  "sessionEnded",
] as const;

export interface ApiClientOptions {
  /** Override the base URL. Defaults to "" (same-origin). Tests may point this
   * at a mock server. */
  baseUrl?: string;
  /** Where the bearer token lives. Defaults to localStorage-backed storage. */
  tokenStore?: TokenStore;
  /** Injectable fetch for tests; defaults to the global fetch. */
  fetchImpl?: typeof fetch;
  /** Called once whenever any request comes back unauthorized (401). The auth
   * layer wires this to "clear the session and route to login" (PRD: a global
   * 401 path). Kept as a single callback so there is exactly one place that
   * reacts to a dead session, no matter which call surfaced it. */
  onUnauthorized?: () => void;
}

interface RequestOptions {
  method?: string;
  body?: unknown;
  signal?: AbortSignal;
  /** When true, a 401 from this request does NOT fire the global unauthorized
   * handler — the caller handles it. Used by login/setup, where a 401 means
   * "bad credentials" to show on the form, not "session expired, go to login"
   * (we are already there). */
  skipUnauthorizedHandler?: boolean;
}

/** enrichmentSearchParams builds the Edit-item search query string from the query
 * plus optional artist-narrowing + paging (item-editing/search-improvements). A blank
 * artist / page 0 are omitted so the URL stays clean for the common case. */
function enrichmentSearchParams(
  query: string,
  opts: EnrichmentSearchOptions,
): URLSearchParams {
  const params = new URLSearchParams({ q: query });
  const artist = opts.artist?.trim();
  if (artist) params.set("artist", artist);
  if (opts.page && opts.page > 0) params.set("page", String(opts.page));
  return params;
}

export class ApiClient {
  private readonly baseUrl: string;
  private readonly tokenStore: TokenStore;
  private readonly fetchImpl: typeof fetch;
  private onUnauthorized?: () => void;

  constructor(opts: ApiClientOptions = {}) {
    this.baseUrl = opts.baseUrl ?? "";
    this.tokenStore = opts.tokenStore ?? localStorageTokenStore;
    this.fetchImpl = opts.fetchImpl ?? globalThis.fetch.bind(globalThis);
    this.onUnauthorized = opts.onUnauthorized;
  }

  /** Register (or replace) the single 401 handler. The auth provider sets this
   * on mount so a stale/revoked token anywhere clears the session and routes to
   * login. Returning the prior handler is unnecessary; one owner at a time. */
  setUnauthorizedHandler(fn: (() => void) | undefined): void {
    this.onUnauthorized = fn;
  }

  /** The bearer token currently attached to requests, if any. */
  get token(): string | null {
    return this.tokenStore.get();
  }

  /** Replace (or clear, with null) the stored bearer token. Login sets it;
   * a 401 or logout clears it (issue 02 wires the call sites). */
  setToken(token: string | null): void {
    this.tokenStore.set(token);
  }

  // --- Endpoints ---------------------------------------------------------

  /** `GET /api/v1/server` — the handshake. Reports version, supported API
   * versions, the feature-flags map, and whether first-run setup is required. */
  getServerInfo(signal?: AbortSignal): Promise<ServerInfo> {
    return this.request<ServerInfo>("/server", { signal });
  }

  /** `POST /api/v1/setup` — create the first Admin from the claim token. A 401
   * is handled by the caller (invalid claim shows on the form), not the global
   * handler. Does NOT log in or store a token; the caller proceeds to login. */
  setup(req: SetupRequest, signal?: AbortSignal): Promise<SetupResult> {
    return this.request<SetupResult>("/setup", {
      method: "POST",
      body: req,
      signal,
      skipUnauthorizedHandler: true,
    });
  }

  /** `POST /api/v1/auth/login` — exchange credentials for a bearer token, then
   * STORE the token so every subsequent request is authenticated. A 401 here is
   * "invalid credentials" for the form, not a session-expiry redirect, so the
   * global handler is skipped. The server also sets the media cookie on this
   * call (for browser <video>/<img>); the client need not touch it. */
  async login(req: LoginRequest, signal?: AbortSignal): Promise<LoginResult> {
    const res = await this.request<LoginResult>("/auth/login", {
      method: "POST",
      body: req,
      signal,
      skipUnauthorizedHandler: true,
    });
    this.setToken(res.token);
    return res;
  }

  /** `POST /api/v1/auth/logout` — revoke the current token server-side (and
   * clear the media cookie), then drop the local token regardless of the
   * outcome (a network failure must not strand a "still logged in" UI). */
  async logout(signal?: AbortSignal): Promise<void> {
    try {
      await this.request<void>("/auth/logout", {
        method: "POST",
        signal,
        // A 401 here means the token was already dead; either way we are logging
        // out, so don't fire the global redirect — the caller clears the session.
        skipUnauthorizedHandler: true,
      });
    } finally {
      this.setToken(null);
    }
  }

  /** `GET /api/v1/devices` — the caller's registered Devices. Used here as a
   * lightweight authenticated probe to confirm a restored token is still valid:
   * a revoked/garbage token returns 401, which fires the global unauthorized
   * handler (clear session → route to login). Later issues add the devices UI. */
  verifySession(signal?: AbortSignal): Promise<unknown> {
    return this.request<unknown>("/devices", { signal });
  }

  // --- Browse surface (issue 03) -----------------------------------------

  /** `GET /api/v1/libraries` — the caller's Libraries. Today this is an
   * Admin-only endpoint (the backend is single-Admin); a non-Admin would get a
   * 403 the UI surfaces as a readable error. Returns the unwrapped array. */
  async listLibraries(signal?: AbortSignal): Promise<Library[]> {
    const res = await this.request<LibrariesResponse>("/libraries", { signal });
    return (res?.libraries ?? []).map(normalizeLibrary);
  }

  /** `GET /api/v1/libraries/{id}` — one Library (name/kind/roots). */
  async getLibrary(id: string, signal?: AbortSignal): Promise<Library> {
    const res = await this.request<Library>(
      `/libraries/${encodeURIComponent(id)}`,
      { signal },
    );
    return normalizeLibrary(res);
  }

  // --- Admin: libraries & scanning (issue 06) ----------------------------

  /** `POST /api/v1/libraries` (Admin) — create a Movie library at one or more
   * root folders. A root that overlaps another Library's root is rejected with
   * a 409 `FOLDER_OVERLAP` ApiError, which we deliberately do NOT swallow: the
   * create form branches on `code === "FOLDER_OVERLAP"` to render a clear inline
   * error (the only conflict a careful Admin still hits — see ADR-0002). Returns
   * the created Library (normalized). */
  async createLibrary(
    input: CreateLibraryInput,
    signal?: AbortSignal,
  ): Promise<Library> {
    const res = await this.request<Library>("/libraries", {
      method: "POST",
      body: input,
      signal,
    });
    return normalizeLibrary(res);
  }

  /** `PATCH /api/v1/libraries/{id}` (Admin) — a partial edit: rename the Library
   * and/or append root folders. An absent `name` leaves the name unchanged; an
   * added folder overlapping any existing root is rejected with a 409
   * `FOLDER_OVERLAP` ApiError (deliberately NOT swallowed, exactly as
   * createLibrary). Returns the updated Library (normalized, with its merged
   * roots). */
  async updateLibrary(
    id: string,
    input: UpdateLibraryInput,
    signal?: AbortSignal,
  ): Promise<Library> {
    const res = await this.request<Library>(
      `/libraries/${encodeURIComponent(id)}`,
      { method: "PATCH", body: input, signal },
    );
    return normalizeLibrary(res);
  }

  /** `DELETE /api/v1/libraries/{id}` (Admin) — remove a Library (and its catalog
   * rows). 204 No Content on success; a missing Library is 404. */
  deleteLibrary(id: string, signal?: AbortSignal): Promise<void> {
    return this.request<void>(`/libraries/${encodeURIComponent(id)}`, {
      method: "DELETE",
      signal,
    });
  }

  /** `POST /api/v1/libraries/{id}/scan` (Admin) — trigger a scan. Incremental by
   * default (ADR-0008: only new/changed/absent files); `mode: "full"` forces a
   * full re-derivation. The trigger is asynchronous: the server accepts it (202)
   * and runs the scan in the background, returning a `running` status right away.
   * The scan continues server-side regardless of this client, so navigating away
   * does not cancel it; track it to completion via {@link getScanStatus} polling
   * (or the scanProgress SSE stream). */
  async scanLibrary(
    id: string,
    opts: { mode?: ScanMode } = {},
    signal?: AbortSignal,
  ): Promise<ScanStatus> {
    const qs = opts.mode === "full" ? "?mode=full" : "";
    const res = await this.request<ScanStatusRaw>(
      `/libraries/${encodeURIComponent(id)}/scan${qs}`,
      { method: "POST", signal },
    );
    return normalizeScanStatus(res);
  }

  /** `GET /api/v1/libraries/{id}/scan` — the pollable scan status (state +
   * counts). The admin hub polls this on an interval while a scan is running and
   * stops when it reaches idle/error (no SSE in v1). */
  async getScanStatus(id: string, signal?: AbortSignal): Promise<ScanStatus> {
    const res = await this.request<ScanStatusRaw>(
      `/libraries/${encodeURIComponent(id)}/scan`,
      { signal },
    );
    return normalizeScanStatus(res);
  }

  /** `GET /api/v1/libraries/{id}/titles` — one cursor-paginated, sortable page
   * of a Library's Title summaries. Returns the normalized page: `titles` is
   * always an array (each summary's `omitempty` holes filled), and `nextCursor`
   * is null on the last page so callers stop paginating. `sort` maps to the
   * API's `sort=` param: "title" (default) or "dateAdded". */
  async listTitles(
    libraryId: string,
    opts: ListTitlesOptions = {},
    signal?: AbortSignal,
  ): Promise<TitlesPage> {
    const params = new URLSearchParams();
    if (opts.limit != null) params.set("limit", String(opts.limit));
    if (opts.cursor) params.set("cursor", opts.cursor);
    if (opts.sort) params.set("sort", opts.sort);
    const qs = params.toString();
    const path = `/libraries/${encodeURIComponent(libraryId)}/titles${
      qs ? `?${qs}` : ""
    }`;
    const res = await this.request<TitlesResponseRaw>(path, { signal });
    return normalizeTitlesPage(res);
  }

  /** `GET /api/v1/titles/{id}` — one Title with its nested Editions → Files →
   * Streams, artwork links, and the calling User's watch state. Normalized so
   * `omitempty` booleans/numbers are present and lists are arrays. */
  async getTitle(id: string, signal?: AbortSignal): Promise<TitleDetail> {
    const res = await this.request<TitleDetailRaw>(
      `/titles/${encodeURIComponent(id)}`,
      { signal },
    );
    return normalizeTitleDetail(res);
  }

  // --- TV browse surface (issue tv-music/01) -----------------------------

  /** `GET /api/v1/libraries/{id}/titles` for a TV Library — one cursor-paginated
   * page of SHOWS (the top-level grid). The server returns `{ shows, nextCursor }`
   * for a TV Library (vs `{ titles, ... }` for a Movie Library); callers pick this
   * after branching on `library.kind`. Normalized like the Movie titles page. */
  async listShows(
    libraryId: string,
    opts: ListTitlesOptions = {},
    signal?: AbortSignal,
  ): Promise<ShowsPage> {
    const params = new URLSearchParams();
    if (opts.limit != null) params.set("limit", String(opts.limit));
    if (opts.cursor) params.set("cursor", opts.cursor);
    const qs = params.toString();
    const path = `/libraries/${encodeURIComponent(libraryId)}/titles${
      qs ? `?${qs}` : ""
    }`;
    const res = await this.request<ShowsResponseRaw>(path, { signal });
    return normalizeShowsPage(res);
  }

  /** `GET /api/v1/shows/{id}/seasons` — a Show plus its Seasons (ordered, hidden
   * excluded), each with its visible-Episode count. An unknown/inaccessible Show
   * is 404 (hide-existence). */
  async getShowSeasons(
    showId: string,
    signal?: AbortSignal,
  ): Promise<ShowSeasons> {
    const res = await this.request<ShowSeasonsResponseRaw>(
      `/shows/${encodeURIComponent(showId)}/seasons`,
      { signal },
    );
    return normalizeShowSeasons(res);
  }

  /** `GET /api/v1/seasons/{id}/episodes` — a Season plus its Episodes (Titles) in
   * order, each decorated with the calling User's watch state (resume/watched).
   * An unknown/inaccessible Season is 404. */
  async getSeasonEpisodes(
    seasonId: string,
    signal?: AbortSignal,
  ): Promise<SeasonEpisodes> {
    const res = await this.request<SeasonEpisodesResponseRaw>(
      `/seasons/${encodeURIComponent(seasonId)}/episodes`,
      { signal },
    );
    return normalizeSeasonEpisodes(res);
  }

  // --- Music browse surface (issue tv-music/03) --------------------------

  /** `GET /api/v1/libraries/{id}/titles` for a Music Library — one cursor-
   * paginated page of ARTISTS (the top-level list). The server returns
   * `{ artists, nextCursor }` for a Music Library (vs `{ titles }` / `{ shows }`);
   * callers pick this after branching on `library.kind`. */
  async listArtists(
    libraryId: string,
    opts: ListTitlesOptions = {},
    signal?: AbortSignal,
  ): Promise<ArtistsPage> {
    const params = new URLSearchParams();
    if (opts.limit != null) params.set("limit", String(opts.limit));
    if (opts.cursor) params.set("cursor", opts.cursor);
    const qs = params.toString();
    const path = `/libraries/${encodeURIComponent(libraryId)}/titles${
      qs ? `?${qs}` : ""
    }`;
    const res = await this.request<ArtistsResponseRaw>(path, { signal });
    return normalizeArtistsPage(res);
  }

  /** `GET /api/v1/artists/{id}/albums` — an Artist plus its Albums (ordered,
   * hidden excluded), each with its visible-Track count. An unknown/inaccessible
   * Artist is 404 (hide-existence). */
  async getArtistAlbums(
    artistId: string,
    signal?: AbortSignal,
  ): Promise<ArtistAlbums> {
    const res = await this.request<ArtistAlbumsResponseRaw>(
      `/artists/${encodeURIComponent(artistId)}/albums`,
      { signal },
    );
    return normalizeArtistAlbums(res);
  }

  /** `GET /api/v1/albums/{id}/tracks` — an Album plus its Tracks (Titles) in
   * disc/track order, each decorated with the calling User's watch state. An
   * unknown/inaccessible Album is 404. */
  async getAlbumTracks(
    albumId: string,
    signal?: AbortSignal,
  ): Promise<AlbumTracks> {
    const res = await this.request<AlbumTracksResponseRaw>(
      `/albums/${encodeURIComponent(albumId)}/tracks`,
      { signal },
    );
    return normalizeAlbumTracks(res);
  }

  /** `GET /api/v1/home` — the calling User's two computed rows: Continue
   * Watching (in-progress Titles, 2%–90% band, most-recent first) and Recently
   * Added (newest-added first). The server computes both (never stored); the
   * client just renders them. Each entry is normalized to the same summary shape
   * the grid uses, so Home cards reuse the grid's PosterTile unchanged. A
   * brand-new/empty server yields two empty arrays. */
  async getHome(signal?: AbortSignal): Promise<HomeRows> {
    const res = await this.request<HomeResponseRaw>("/home", { signal });
    return normalizeHome(res);
  }

  // --- Collections surface (collections-playlists-ui issue 01) -----------

  /** `GET /api/v1/collections` (any authenticated User) — the Collections the
   * caller can see, each a card with a per-viewer member count and a
   * representative poster. The list is ALREADY access-filtered server-side (a
   * Member's restricted Collections — including any with zero visible members —
   * simply aren't returned), so the screen renders exactly what comes back.
   * Returns the unwrapped, normalized array. */
  async listCollections(signal?: AbortSignal): Promise<CollectionSummary[]> {
    const res = await this.request<CollectionsResponseRaw>("/collections", {
      signal,
    });
    return (res?.collections ?? []).map(normalizeCollectionSummary);
  }

  /** `GET /api/v1/collections/{id}` (any authenticated User) — one Collection
   * plus its visible member Titles. Members come back in the SAME summary shape
   * a browse grid uses (decoration parity), so they reuse PosterTile/the grid
   * unchanged. A Member with zero visible members gets a 404 (hide-existence)
   * the caller surfaces as a readable "not found" state. Returns the normalized
   * detail. */
  async getCollection(
    id: string,
    signal?: AbortSignal,
  ): Promise<CollectionDetail> {
    const res = await this.request<CollectionDetailRaw>(
      `/collections/${encodeURIComponent(id)}`,
      { signal },
    );
    return normalizeCollectionDetail(res);
  }

  // --- Collection curation (collections-playlists-ui issue 02) -----------
  //
  // The Admin-scope write surface (server-enforced; the UI gate on isAdmin is
  // convenience). A blank name is a 400 BAD_REQUEST and an unknown title id on
  // add is a 422 UNKNOWN_TITLE; neither is swallowed, so the curation screens
  // branch on the readable ApiError (exactly as createLibrary lets FOLDER_OVERLAP
  // through). add-items is IDEMPOTENT — re-adding an existing member is a no-op.

  /** `POST /api/v1/collections` (Admin) — create a Collection. A blank `name` is
   * a 400 `BAD_REQUEST` (surfaced as a readable ApiError). Returns the created,
   * normalized Collection (id/name/description; no member count yet). */
  async createCollection(
    input: CreateCollectionInput,
    signal?: AbortSignal,
  ): Promise<Collection> {
    const body: Record<string, unknown> = { name: input.name };
    if (input.description !== undefined) body.description = input.description;
    const res = await this.request<CollectionRaw>("/collections", {
      method: "POST",
      body,
      signal,
    });
    return normalizeCollection(res);
  }

  /** `PUT /api/v1/collections/{id}` (Admin) — rename and/or re-describe a
   * Collection. A blank `name` is a 400 `BAD_REQUEST`; an unknown id is 404.
   * Returns the updated, normalized Collection. */
  async updateCollection(
    id: string,
    input: UpdateCollectionInput,
    signal?: AbortSignal,
  ): Promise<Collection> {
    const body: Record<string, unknown> = { name: input.name };
    if (input.description !== undefined) body.description = input.description;
    const res = await this.request<CollectionRaw>(
      `/collections/${encodeURIComponent(id)}`,
      { method: "PUT", body, signal },
    );
    return normalizeCollection(res);
  }

  /** `DELETE /api/v1/collections/{id}` (Admin) — remove a Collection. 204 No
   * Content on success; an unknown id is 404. */
  deleteCollection(id: string, signal?: AbortSignal): Promise<void> {
    return this.request<void>(`/collections/${encodeURIComponent(id)}`, {
      method: "DELETE",
      signal,
    });
  }

  /** `POST /api/v1/collections/{id}/items` (Admin) — add one or more Titles to a
   * Collection. IDEMPOTENT: re-adding an existing member is a harmless no-op
   * (204). A title id that doesn't exist is a 422 `UNKNOWN_TITLE`, left to
   * surface as a readable ApiError; an unknown Collection is 404. */
  addCollectionItems(
    id: string,
    titleIds: string[],
    signal?: AbortSignal,
  ): Promise<void> {
    return this.request<void>(
      `/collections/${encodeURIComponent(id)}/items`,
      { method: "POST", body: { titleIds }, signal },
    );
  }

  /** `DELETE /api/v1/collections/{id}/items/{titleId}` (Admin) — remove one
   * member Title from a Collection. 204 No Content on success; an unknown
   * Collection is 404. */
  removeCollectionItem(
    id: string,
    titleId: string,
    signal?: AbortSignal,
  ): Promise<void> {
    return this.request<void>(
      `/collections/${encodeURIComponent(id)}/items/${encodeURIComponent(titleId)}`,
      { method: "DELETE", signal },
    );
  }

  // --- Playlists surface (collections-playlists-ui issue 03) -------------
  //
  // User-owned, PRIVATE, ordered, single-media-kind queues. Every route is
  // owner == caller — there is NO Admin override; a non-owner (Member OR Admin)
  // gets a 404 (hide-existence), which the screens surface as a readable "not
  // found" state. Members come back in the SAME titleSummary shape a browse grid
  // uses PLUS an `itemId`. The append refusals (422 KIND_MISMATCH — the Title's
  // kind doesn't fit a typed Playlist; 422 UNKNOWN_TITLE) are NOT swallowed, so
  // the "Add to playlist" affordance branches on the readable ApiError (exactly
  // as createLibrary lets FOLDER_OVERLAP through).

  /** `GET /api/v1/playlists` — the caller's OWN playlists only (each a card with
   * its name, kind, and item count). Another User's playlists never appear.
   * Returns the unwrapped, normalized array. */
  async listPlaylists(signal?: AbortSignal): Promise<PlaylistSummary[]> {
    const res = await this.request<PlaylistsResponseRaw>("/playlists", {
      signal,
    });
    return (res?.playlists ?? []).map(normalizePlaylistSummary);
  }

  /** `GET /api/v1/playlists/{id}` — one playlist the caller OWNS plus its members
   * in POSITION order (and its kind). Members reuse the browse summary shape PLUS
   * an `itemId`, so they drop into PosterTile/the grid unchanged. A non-owner
   * (incl. an Admin) gets a 404 (hide-existence) the caller surfaces as "not
   * found". Returns the normalized detail. */
  async getPlaylist(id: string, signal?: AbortSignal): Promise<PlaylistDetail> {
    const res = await this.request<PlaylistDetailRaw>(
      `/playlists/${encodeURIComponent(id)}`,
      { signal },
    );
    return normalizePlaylistDetail(res);
  }

  /** `POST /api/v1/playlists` `{ name }` — create an empty, untyped playlist owned
   * by the caller. A blank `name` is a 400 `BAD_REQUEST` (surfaced as a readable
   * ApiError). Returns the created, normalized Playlist (kind "" until its first
   * item fixes it). */
  async createPlaylist(name: string, signal?: AbortSignal): Promise<Playlist> {
    const res = await this.request<PlaylistRaw>("/playlists", {
      method: "POST",
      body: { name },
      signal,
    });
    return normalizePlaylist(res);
  }

  /** `PUT /api/v1/playlists/{id}` `{ name }` — rename a playlist the caller owns. A
   * blank `name` is a 400 `BAD_REQUEST`; a non-owned/unknown id is 404. Returns
   * the updated, normalized Playlist. */
  async renamePlaylist(
    id: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<Playlist> {
    const res = await this.request<PlaylistRaw>(
      `/playlists/${encodeURIComponent(id)}`,
      { method: "PUT", body: { name }, signal },
    );
    return normalizePlaylist(res);
  }

  /** `DELETE /api/v1/playlists/{id}` — delete a playlist the caller owns. 204 No
   * Content on success; a non-owned/unknown id is 404. */
  deletePlaylist(id: string, signal?: AbortSignal): Promise<void> {
    return this.request<void>(`/playlists/${encodeURIComponent(id)}`, {
      method: "DELETE",
      signal,
    });
  }

  /** `POST /api/v1/playlists/{id}/items` `{ titleId }` — append a Title at the END
   * of a playlist the caller owns. The first append fixes the playlist's kind; a
   * later append whose kind doesn't fit is a 422 `KIND_MISMATCH` (left to surface
   * as a readable ApiError), and an unknown title id is a 422 `UNKNOWN_TITLE`.
   * Adding the same Title twice is allowed (two distinct entries). 204 No Content
   * on success; a non-owned playlist is 404. */
  appendPlaylistItem(
    id: string,
    titleId: string,
    signal?: AbortSignal,
  ): Promise<void> {
    return this.request<void>(`/playlists/${encodeURIComponent(id)}/items`, {
      method: "POST",
      body: { titleId },
      signal,
    });
  }

  /** `PUT /api/v1/playlists/{id}/items` `{ itemIds }` — REPLACE the playlist's
   * whole order with `itemIds`: the FULL desired permutation of the playlist's
   * current item ids (a replace, not a delta). The client computes `itemIds` from
   * the live member list, so it shouldn't produce a mismatched set — but a body
   * that isn't EXACTLY the current item ids is a 422 `ITEM_SET_MISMATCH` no-op,
   * left to surface as a readable ApiError (exactly as createLibrary lets
   * FOLDER_OVERLAP through); a non-owned playlist is 404. 204 No Content on
   * success. */
  reorderPlaylistItems(
    id: string,
    itemIds: string[],
    signal?: AbortSignal,
  ): Promise<void> {
    return this.request<void>(`/playlists/${encodeURIComponent(id)}/items`, {
      method: "PUT",
      body: { itemIds },
      signal,
    });
  }

  /** `DELETE /api/v1/playlists/{id}/items/{itemId}` — remove exactly ONE entry by
   * its playlist-item id (so removing one duplicate leaves the other). 204 No
   * Content on success; an itemId that isn't a row of this playlist (or a
   * non-owned playlist) is 404. */
  removePlaylistItem(
    id: string,
    itemId: string,
    signal?: AbortSignal,
  ): Promise<void> {
    return this.request<void>(
      `/playlists/${encodeURIComponent(id)}/items/${encodeURIComponent(itemId)}`,
      { method: "DELETE", signal },
    );
  }

  // --- Watchlist (watchlist 01) ------------------------------------------
  //
  // The per-User system Playlist, addressed by NAME (not id): the server resolves
  // — and lazily seeds — "the caller's Watchlist" on every call, so it is always
  // present. It is an ordinary owner-private Playlist in every other respect (it
  // also shows up in listPlaylists), with the single difference that it can't be
  // renamed or deleted. An append reuses the single-kind Playlist rule (the first
  // movie fixes it to "movie"; a later cross-kind add is a 422 KIND_MISMATCH).

  /** `GET /api/v1/watchlist` — the caller's Watchlist (created on first access)
   * plus its members in position order, decorated exactly like a browse grid.
   * Returns the normalized {@link PlaylistDetail}. */
  async getWatchlist(signal?: AbortSignal): Promise<PlaylistDetail> {
    const res = await this.request<PlaylistDetailRaw>("/watchlist", { signal });
    return normalizePlaylistDetail(res);
  }

  /** `POST /api/v1/watchlist/items` `{ titleId }` — add a Title to the END of the
   * caller's Watchlist (ensuring the Watchlist exists first). A cross-kind add is a
   * 422 `KIND_MISMATCH` and an unknown id a 422 `UNKNOWN_TITLE` (both left to
   * surface as a readable ApiError); adding the same Title twice is allowed. 204 No
   * Content on success. */
  addToWatchlist(titleId: string, signal?: AbortSignal): Promise<void> {
    return this.request<void>("/watchlist/items", {
      method: "POST",
      body: { titleId },
      signal,
    });
  }

  /** `DELETE /api/v1/watchlist/items/{itemId}` — remove one entry from the caller's
   * Watchlist by its playlist-item id. 204 on success; an unknown item id is 404. */
  removeFromWatchlist(itemId: string, signal?: AbortSignal): Promise<void> {
    return this.request<void>(
      `/watchlist/items/${encodeURIComponent(itemId)}`,
      { method: "DELETE", signal },
    );
  }

  // --- Admin: attention surfaces & devices (issue 07) --------------------

  /** `GET /api/v1/libraries/{id}/unmatched` (Admin) — the Library's Unmatched
   * files: recognized media the scanner could not turn into a Title (no
   * extractable identity). Admin-visible, manually matchable, never browsable.
   * Returns the unwrapped, normalized array (each file's `reason` filled). */
  async listUnmatched(
    libraryId: string,
    signal?: AbortSignal,
  ): Promise<UnmatchedFile[]> {
    const res = await this.request<{ files?: UnmatchedFileRaw[] }>(
      `/libraries/${encodeURIComponent(libraryId)}/unmatched`,
      { signal },
    );
    return (res?.files ?? []).map(normalizeUnmatchedFile);
  }

  /** `GET /api/v1/libraries/{id}/overrides` (Admin) — the Library's Match
   * overrides, INCLUDING orphaned ones (the anchor folder was renamed/moved).
   * Returns the unwrapped, normalized array; each override's `orphaned` is a
   * present boolean so the list can highlight orphans unconditionally. */
  async listOverrides(
    libraryId: string,
    signal?: AbortSignal,
  ): Promise<MatchOverride[]> {
    const res = await this.request<{ overrides?: MatchOverrideRaw[] }>(
      `/libraries/${encodeURIComponent(libraryId)}/overrides`,
      { signal },
    );
    return (res?.overrides ?? []).map(normalizeMatchOverride);
  }

  /** `POST /api/v1/libraries/{id}/fix-match` (Admin) — record an identity
   * correction for a folder (the override persists across rescans and, once its
   * folder is renamed, is surfaced as orphaned). The body anchors to a
   * `folderPath` (the directory of the Unmatched file / the needs-review Title's
   * folder) and supplies at least one identity signal: title (+year) or an
   * embedded tmdb/imdb id. A 400 `BAD_REQUEST` (missing identity, etc.) is left
   * to surface as a readable ApiError on the form. Returns the created override
   * (normalized). */
  async fixMatch(
    libraryId: string,
    input: FixMatchInput,
    signal?: AbortSignal,
  ): Promise<MatchOverride> {
    const res = await this.request<MatchOverrideRaw>(
      `/libraries/${encodeURIComponent(libraryId)}/fix-match`,
      { method: "POST", body: input, signal },
    );
    return normalizeMatchOverride(res);
  }

  /** `GET /api/v1/libraries/{id}/enrichment-attention` (Admin) — the Library's
   * Titles whose Enrichment could not settle on a record (status unmatched/
   * failed), awaiting a hand-match. A NEW attention dimension, distinct from the
   * identity Unmatched files and needs-review Titles. Returns the unwrapped,
   * normalized array (each entry's `year` filled). */
  async listEnrichmentAttention(
    libraryId: string,
    signal?: AbortSignal,
  ): Promise<EnrichmentAttentionTitle[]> {
    const res = await this.request<{ titles?: EnrichmentAttentionTitleRaw[] }>(
      `/libraries/${encodeURIComponent(libraryId)}/enrichment-attention`,
      { signal },
    );
    return (res?.titles ?? []).map(normalizeEnrichmentAttentionTitle);
  }

  /** `GET /api/v1/libraries/{id}/needs-review` (Admin) — the Library's Movies,
   * Episodes, Tracks, and Shows the scanner flagged as an uncertain identity parse
   * (no year, or non-SxxExx episode numbering), still awaiting resolution. Works
   * for every Library kind (the server descends into TV/Music for us), replacing
   * the old client-side page-walk. Returns the unwrapped, normalized array. */
  async listNeedsReview(
    libraryId: string,
    signal?: AbortSignal,
  ): Promise<NeedsReviewItem[]> {
    const res = await this.request<{ items?: NeedsReviewItemRaw[] }>(
      `/libraries/${encodeURIComponent(libraryId)}/needs-review`,
      { signal },
    );
    return (res?.items ?? []).map(normalizeNeedsReviewItem);
  }

  /** `POST /api/v1/titles/{id}/review` (Admin) — dismiss a Movie / Episode /
   * Track's needs_review flag (the Admin confirms the uncertain parse is fine).
   * The dismissal sticks across rescans. 204 on success; unknown Title is 404. */
  reviewTitle(titleId: string, signal?: AbortSignal): Promise<void> {
    return this.request<void>(
      `/titles/${encodeURIComponent(titleId)}/review`,
      { method: "POST", signal },
    );
  }

  /** `POST /api/v1/shows/{id}/review` (Admin) — dismiss a Show's needs_review
   * flag. 204 on success; unknown Show is 404. */
  reviewShow(showId: string, signal?: AbortSignal): Promise<void> {
    return this.request<void>(
      `/shows/${encodeURIComponent(showId)}/review`,
      { method: "POST", signal },
    );
  }

  /** `PUT /api/v1/titles/{id}/enrichmentMatch` (Admin) — set the external id used
   * for a Title's Enrichment lookup and re-enrich JUST that Title immediately. It
   * refreshes the descriptive fields/artwork but NEVER touches identity/watch
   * state (ADR-0014) — deliberately distinct from identity fix-match. At least one
   * id (tmdbId/imdbId/musicbrainzId) is required (400 otherwise). Returns the
   * updated, normalized Title detail (now `enrichmentStatus: "matched"`). */
  async setEnrichmentMatch(
    titleId: string,
    input: EnrichmentMatchInput,
    signal?: AbortSignal,
  ): Promise<TitleDetail> {
    const res = await this.request<TitleDetailRaw>(
      `/titles/${encodeURIComponent(titleId)}/enrichmentMatch`,
      { method: "PUT", body: input, signal },
    );
    return normalizeTitleDetail(res);
  }

  /** `GET /api/v1/titles/{id}/enrichmentCandidates?q=…` (Admin) — search the
   * authoritative metadata provider for the records that could decorate this leaf
   * Title, so the Admin can pick the right one and apply it as an Enrichment
   * override (Edit-item "Fix info", ADR-0019). The searched kind is the Title's
   * own kind (server-derived). A blank query returns an empty list; an
   * unconfigured/unreachable provider throws an ApiError (SEARCH_UNAVAILABLE) the
   * box reports. Read-only — identity/watch state untouched. */
  async searchEnrichmentCandidates(
    titleId: string,
    query: string,
    opts: EnrichmentSearchOptions = {},
    signal?: AbortSignal,
  ): Promise<EnrichmentCandidatesResult> {
    const params = enrichmentSearchParams(query, opts);
    const res = await this.request<EnrichmentCandidatesResult>(
      `/titles/${encodeURIComponent(titleId)}/enrichmentCandidates?${params.toString()}`,
      { signal },
    );
    return { candidates: res.candidates ?? [], hasMore: res.hasMore ?? false };
  }

  /** `POST /api/v1/titles/{id}/subtitles/search` — "search online" for a subtitle
   * in a language the Title lacks (ADR-0021). Available to any User (Members
   * included). Returns the provider candidates (empty when the provider is
   * disabled/offline — a normal, non-error outcome). */
  async searchSubtitles(
    titleId: string,
    language: string,
    signal?: AbortSignal,
  ): Promise<SubtitleCandidate[]> {
    const res = await this.request<{ candidates: SubtitleCandidate[] }>(
      `/titles/${encodeURIComponent(titleId)}/subtitles/search`,
      { method: "POST", body: { language }, signal },
    );
    return res.candidates ?? [];
  }

  /** `POST /api/v1/titles/{id}/subtitles/fetch` — download + persist a chosen
   * candidate as a fetched track (ADR-0021). The candidate is echoed verbatim from
   * searchSubtitles. Returns the new track as a decision-style SubtitleTrack (with
   * its .vtt url for a text track) so the caller can add it to the captions menu and
   * enable it immediately. */
  async fetchSubtitle(
    titleId: string,
    language: string,
    candidate: SubtitleCandidate,
    signal?: AbortSignal,
  ): Promise<SubtitleTrack> {
    const res = await this.request<{ subtitle: SubtitleTrack }>(
      `/titles/${encodeURIComponent(titleId)}/subtitles/fetch`,
      { method: "POST", body: { language, candidate }, signal },
    );
    return res.subtitle;
  }

  /** `GET /api/v1/titles/{id}/externalPreview?ref=…` (Admin) — resolve a pasted
   * MusicBrainz/TMDB id-or-URL to a single preview candidate WITHOUT searching (the
   * "paste an id when search isn't enough" escape hatch, item-editing/search-
   * improvements). A typo'd/stale id throws a NOT_FOUND ApiError; a wrong-kind URL a
   * BAD_REQUEST. The returned candidate is applied via applyEnrichmentOverride. */
  async previewExternalCandidate(
    titleId: string,
    ref: string,
    signal?: AbortSignal,
  ): Promise<EnrichmentCandidate> {
    const params = new URLSearchParams({ ref });
    return this.request<EnrichmentCandidate>(
      `/titles/${encodeURIComponent(titleId)}/externalPreview?${params.toString()}`,
      { signal },
    );
  }

  /** `PUT /api/v1/titles/{id}/enrichmentOverride` (Admin) — apply a picked
   * candidate as a durable Enrichment override on this leaf and re-enrich just it
   * (Edit-item "Fix info", ADR-0019). The server pins the authoritative externalId
   * (mapping it to the right id column for the Title's kind) and refreshes the
   * unlocked fields/artwork from that record; identity_key and watch state are
   * NEVER touched, Locked fields are honored. Returns the updated Title detail. */
  async applyEnrichmentOverride(
    titleId: string,
    externalId: string,
    signal?: AbortSignal,
  ): Promise<TitleDetail> {
    const res = await this.request<TitleDetailRaw>(
      `/titles/${encodeURIComponent(titleId)}/enrichmentOverride`,
      { method: "PUT", body: { externalId }, signal },
    );
    return normalizeTitleDetail(res);
  }

  /** `GET /api/v1/{shows|artists|albums}/{id}/enrichmentCandidates?q=…` (Admin) —
   * search the authoritative provider for the records that could decorate a browse
   * PARENT (Show → TMDB tv, Artist/Album → MusicBrainz), so the Admin can Fix-info
   * it (item-editing/02, ADR-0019). An album candidate carries its tracklist. Same
   * SEARCH_UNAVAILABLE semantics as the leaf search. `entityType` is the URL
   * segment ("shows" | "artists" | "albums"). */
  async searchEntityEnrichmentCandidates(
    entityType: "shows" | "artists" | "albums",
    entityId: string,
    query: string,
    opts: EnrichmentSearchOptions = {},
    signal?: AbortSignal,
  ): Promise<EnrichmentCandidatesResult> {
    const params = enrichmentSearchParams(query, opts);
    const res = await this.request<EnrichmentCandidatesResult>(
      `/${entityType}/${encodeURIComponent(entityId)}/enrichmentCandidates?${params.toString()}`,
      { signal },
    );
    return { candidates: res.candidates ?? [], hasMore: res.hasMore ?? false };
  }

  /** `GET /api/v1/{shows|artists|albums}/{id}/externalPreview?ref=…` (Admin) — the
   * parent analogue of previewExternalCandidate: resolve a pasted id-or-URL to a
   * single preview candidate for a Show/Artist/Album (item-editing/search-
   * improvements). Applied via applyEntityEnrichmentOverride. */
  async previewEntityExternalCandidate(
    entityType: "shows" | "artists" | "albums",
    entityId: string,
    ref: string,
    signal?: AbortSignal,
  ): Promise<EnrichmentCandidate> {
    const params = new URLSearchParams({ ref });
    return this.request<EnrichmentCandidate>(
      `/${entityType}/${encodeURIComponent(entityId)}/externalPreview?${params.toString()}`,
      { signal },
    );
  }

  /** `PUT /api/v1/{shows|artists|albums}/{id}/enrichmentOverride` (Admin) — apply a
   * picked candidate as a durable Enrichment override on a browse PARENT and
   * re-enrich just it (item-editing/02, ADR-0019). Identity/watch state untouched;
   * Locked fields honored. Returns the updated parent enrichment detail. */
  async applyEntityEnrichmentOverride(
    entityType: "shows" | "artists" | "albums",
    entityId: string,
    externalId: string,
    cascade = false,
    signal?: AbortSignal,
  ): Promise<EntityEnrichmentDetail> {
    return this.request<EntityEnrichmentDetail>(
      `/${entityType}/${encodeURIComponent(entityId)}/enrichmentOverride`,
      { method: "PUT", body: { externalId, cascade }, signal },
    );
  }

  /** `PUT /api/v1/titles/{id}/identityCorrection` (Admin) — the "Wrong item"
   * DESTRUCTIVE correction (item-editing/04, ADR-0019/0014): the file is genuinely a
   * different work. The server writes a folder-keyed Match override (re-keying
   * identity so it survives a rescan), RESETS this Title's watch state, CLEARS its
   * Locked fields, and re-enriches to the picked record. Movie only — an Episode /
   * Track is rejected (WRONG_KIND). Returns the updated (re-identified) Title detail. */
  async applyTitleIdentityCorrection(
    titleId: string,
    candidate: { externalId: string; title?: string; year?: number },
    signal?: AbortSignal,
  ): Promise<TitleDetail> {
    const res = await this.request<TitleDetailRaw>(
      `/titles/${encodeURIComponent(titleId)}/identityCorrection`,
      { method: "PUT", body: candidate, signal },
    );
    return normalizeTitleDetail(res);
  }

  /** `PUT /api/v1/shows/{id}/identityCorrection` (Admin) — the Show "Wrong item"
   * correction (item-editing/04): re-key the Show, reset every Episode's watch
   * state, clear the Show's Locked fields, and re-enrich to the picked record.
   * Returns the updated parent enrichment detail. */
  async applyShowIdentityCorrection(
    showId: string,
    candidate: { externalId: string; title?: string; year?: number; cascade?: boolean },
    signal?: AbortSignal,
  ): Promise<EntityEnrichmentDetail> {
    return this.request<EntityEnrichmentDetail>(
      `/shows/${encodeURIComponent(showId)}/identityCorrection`,
      { method: "PUT", body: candidate, signal },
    );
  }

  // --- Edit-item "Fix label": image picker + parent hand-edit (item-editing/03)

  /** `GET /api/v1/titles/{id}/artworkCandidates?role=…` (Admin) — list the provider
   * images offered for a leaf Title's role (poster/background), so the Admin can
   * pick a specific one (Fix label, ADR-0019). Same SEARCH_UNAVAILABLE semantics as
   * the record search. */
  async searchTitleArtworkCandidates(
    titleId: string,
    role: string,
    signal?: AbortSignal,
  ): Promise<ArtworkCandidate[]> {
    const params = new URLSearchParams({ role });
    const res = await this.request<ArtworkCandidatesResult>(
      `/titles/${encodeURIComponent(titleId)}/artworkCandidates?${params.toString()}`,
      { signal },
    );
    return res.candidates ?? [];
  }

  /** `PUT /api/v1/titles/{id}/artwork` `{ role, url }` (Admin) — apply a picked
   * provider image to a leaf Title's role and Lock the role (Fix label). Local
   * artwork still wins. Returns the updated Title detail. */
  async pickTitleArtwork(
    titleId: string,
    role: string,
    url: string,
    signal?: AbortSignal,
  ): Promise<TitleDetail> {
    const res = await this.request<TitleDetailRaw>(
      `/titles/${encodeURIComponent(titleId)}/artwork`,
      { method: "PUT", body: { role, url }, signal },
    );
    return normalizeTitleDetail(res);
  }

  /** `POST /api/v1/titles/{id}/artworkUpload?role=…` (Admin, multipart) — upload
   * your own image for a leaf Title's role; uploading IS selecting: the bytes fill
   * the role and Lock it, and an Uploaded image outranks Local + Fetched at serve
   * time (ADR-0026). Server-validated JPEG/PNG/WebP ≤ 16 MiB. Returns the updated
   * Title detail (its poster/background URL version changes so the `<img>` reloads). */
  async uploadTitleArtwork(
    titleId: string,
    role: string,
    file: File | Blob,
    signal?: AbortSignal,
  ): Promise<TitleDetail> {
    const form = new FormData();
    form.append("image", file);
    const params = new URLSearchParams({ role });
    const res = await this.request<TitleDetailRaw>(
      `/titles/${encodeURIComponent(titleId)}/artworkUpload?${params.toString()}`,
      { method: "POST", body: form, signal },
    );
    return normalizeTitleDetail(res);
  }

  /** `PUT /api/v1/{shows|artists|albums}/{id}/metadata` (Admin) — hand-edit a browse
   * parent's descriptive fields; each present field is written AND Locked (a rename
   * via `title` never touches identity/watch state, ADR-0002). Returns the updated
   * parent enrichment detail. */
  editEntityMetadata(
    entityType: "shows" | "artists" | "albums",
    entityId: string,
    edit: EntityMetadataEditInput,
    signal?: AbortSignal,
  ): Promise<EntityEnrichmentDetail> {
    return this.request<EntityEnrichmentDetail>(
      `/${entityType}/${encodeURIComponent(entityId)}/metadata`,
      { method: "PUT", body: edit, signal },
    );
  }

  /** `DELETE /api/v1/{shows|artists|albums}/{id}/metadata/locks/{field}` (Admin) —
   * release a browse parent's Locked field back to auto. Returns the updated parent
   * detail. */
  releaseEntityLock(
    entityType: "shows" | "artists" | "albums",
    entityId: string,
    field: string,
    signal?: AbortSignal,
  ): Promise<EntityEnrichmentDetail> {
    return this.request<EntityEnrichmentDetail>(
      `/${entityType}/${encodeURIComponent(entityId)}/metadata/locks/${encodeURIComponent(field)}`,
      { method: "DELETE", signal },
    );
  }

  /** `GET /api/v1/{shows|artists|albums}/{id}/artworkCandidates?role=…` (Admin) —
   * list the provider images offered for a browse parent's role (Fix label). */
  async searchEntityArtworkCandidates(
    entityType: "shows" | "artists" | "albums",
    entityId: string,
    role: string,
    signal?: AbortSignal,
  ): Promise<ArtworkCandidate[]> {
    const params = new URLSearchParams({ role });
    const res = await this.request<ArtworkCandidatesResult>(
      `/${entityType}/${encodeURIComponent(entityId)}/artworkCandidates?${params.toString()}`,
      { signal },
    );
    return res.candidates ?? [];
  }

  /** `PUT /api/v1/{shows|artists|albums}/{id}/artwork` `{ role, url }` (Admin) —
   * apply a picked provider image to a browse parent's role and Lock the role.
   * Local artwork still wins. Returns the updated parent detail. */
  pickEntityArtwork(
    entityType: "shows" | "artists" | "albums",
    entityId: string,
    role: string,
    url: string,
    signal?: AbortSignal,
  ): Promise<EntityEnrichmentDetail> {
    return this.request<EntityEnrichmentDetail>(
      `/${entityType}/${encodeURIComponent(entityId)}/artwork`,
      { method: "PUT", body: { role, url }, signal },
    );
  }

  /** `POST /api/v1/{shows|artists|albums}/{id}/artworkUpload?role=…` (Admin,
   * multipart) — upload your own image for a browse parent's role; uploading IS
   * selecting (sets + Locks the role) and an Uploaded image outranks Local +
   * Fetched (ADR-0026). Returns the updated parent detail. */
  uploadEntityArtwork(
    entityType: "shows" | "artists" | "albums",
    entityId: string,
    role: string,
    file: File | Blob,
    signal?: AbortSignal,
  ): Promise<EntityEnrichmentDetail> {
    const form = new FormData();
    form.append("image", file);
    const params = new URLSearchParams({ role });
    return this.request<EntityEnrichmentDetail>(
      `/${entityType}/${encodeURIComponent(entityId)}/artworkUpload?${params.toString()}`,
      { method: "POST", body: form, signal },
    );
  }

  /** `GET /api/v1/devices` (authenticated) — the signed-in User's registered
   * Devices (name / platform / last-seen). Returns the unwrapped array; the
   * Device shape's timestamps stay as RFC3339 strings (formatted at render). */
  async listDevices(signal?: AbortSignal): Promise<Device[]> {
    const res = await this.request<DevicesResponse>("/devices", { signal });
    return res?.devices ?? [];
  }

  /** `DELETE /api/v1/devices/{id}` (self or Admin) — revoke a Device, which
   * invalidates its token immediately (a subsequent call with it is rejected).
   * 204 No Content on success; a missing Device is 404. */
  deleteDevice(id: string, signal?: AbortSignal): Promise<void> {
    return this.request<void>(`/devices/${encodeURIComponent(id)}`, {
      method: "DELETE",
      signal,
    });
  }

  // --- Admin: users (access-control-admin-ui issue 01) -------------------

  /** `GET /api/v1/users` (Admin) — every User on the server (`{ id, username,
   * role }` each). Returns the unwrapped array. Admin scope: a Member gets a 403
   * the UI surfaces as a readable error (the tab is also role-gated). */
  async listUsers(signal?: AbortSignal): Promise<User[]> {
    const res = await this.request<UsersResponse>("/users", { signal });
    return res?.users ?? [];
  }

  /** `POST /api/v1/users` (Admin) — create a User. `role` defaults to "member"
   * server-side when omitted. A blank username/password is a 400 `BAD_REQUEST`;
   * a duplicate username is a 409 `USERNAME_TAKEN` — neither is swallowed, so the
   * create form branches on `code === "USERNAME_TAKEN"` for a readable inline
   * error (exactly as createLibrary lets FOLDER_OVERLAP through). Returns the
   * created User. */
  createUser(input: CreateUserInput, signal?: AbortSignal): Promise<User> {
    return this.request<User>("/users", {
      method: "POST",
      body: input,
      signal,
    });
  }

  /** `GET /api/v1/users/{id}` (Admin) — one User's full detail, carrying BOTH
   * access dimensions: the granted `libraryIds` (issue 02) and the `ratingCeiling`
   * (issue 03). Read to show a Member's current grants (and, later, their cap).
   * Normalized so a missing/`null` `libraryIds`/`ratingCeiling` becomes `[]`/`""`.
   * An unknown id is 404 (surfaced as a readable ApiError). */
  async getUser(id: string, signal?: AbortSignal): Promise<UserDetail> {
    const res = await this.request<UserDetailRaw>(
      `/users/${encodeURIComponent(id)}`,
      { signal },
    );
    return normalizeUserDetail(res);
  }

  /** `PUT /api/v1/users/{id}/libraryAccess` (Admin) — REPLACE a Member's whole
   * Library grant set with `libraryIds` (the full desired set, not a delta). An
   * empty array clears every grant (the Member sees no catalog). 204 No Content on
   * success. The refusals are NOT swallowed, so the row branches on them for a
   * readable inline message: 422 `ADMIN_GRANT` (the target is an Admin — implicitly
   * all-access, so grants are meaningless) and 422 `UNKNOWN_LIBRARY` (an id doesn't
   * exist); an unknown user is 404. */
  setLibraryAccess(
    id: string,
    libraryIds: string[],
    signal?: AbortSignal,
  ): Promise<void> {
    return this.request<void>(
      `/users/${encodeURIComponent(id)}/libraryAccess`,
      { method: "PUT", body: { libraryIds }, signal },
    );
  }

  /** `DELETE /api/v1/users/{id}` (Admin) — remove a User (and their access,
   * Devices, and watch history). 204 No Content on success; deleting the final
   * Admin is a 409 `LAST_ADMIN` (left to surface as a readable ApiError so the
   * row's delete control shows it and the User stays); an unknown id is 404. */
  deleteUser(id: string, signal?: AbortSignal): Promise<void> {
    return this.request<void>(`/users/${encodeURIComponent(id)}`, {
      method: "DELETE",
      signal,
    });
  }

  /** `PUT /api/v1/users/{id}/ratingCeiling` (Admin) — set or clear a Member's
   * Rating ceiling (access-control-admin-ui issue 03). A maturity-rung label
   * (e.g. "PG-13") caps the Member at or below it (across both movie and TV
   * ratings, by the server's shared rank); `null` clears the cap (uncapped). 204
   * No Content on success. The refusals are NOT swallowed, so the row branches on
   * them for a readable inline message: 422 `ADMIN_CEILING` (the target is an
   * Admin — implicitly all-access, so a cap is meaningless) and 422
   * `UNKNOWN_RATING` (the label isn't a known rung); an unknown user is 404. */
  setRatingCeiling(
    id: string,
    rating: string | null,
    signal?: AbortSignal,
  ): Promise<void> {
    return this.request<void>(
      `/users/${encodeURIComponent(id)}/ratingCeiling`,
      { method: "PUT", body: { rating }, signal },
    );
  }

  /** `PUT /api/v1/users/{id}/password` (Admin) — set a new password for ANY User
   * (account recovery without deleting it; not access-restricted, so it applies
   * to an Admin too). 204 No Content on success; an unknown id is 404. */
  setPassword(
    id: string,
    password: string,
    signal?: AbortSignal,
  ): Promise<void> {
    return this.request<void>(`/users/${encodeURIComponent(id)}/password`, {
      method: "PUT",
      body: { password },
      signal,
    });
  }

  // --- Admin: metadata-provider settings (metadata-providers 02) ---------
  //
  // The Admin-scope Enrichment-provider surface (server-enforced; the UI gate on
  // isAdmin is convenience). Secrets are NEVER returned — a provider reports only
  // `hasKey`. A save takes effect at runtime with no restart (the server rebuilds
  // + hot-swaps the active provider). The validation refusals are NOT swallowed,
  // so the screen branches on the readable ApiError code: 422 PROVIDER_KEY_REQUIRED
  // / PROVIDER_INVALID_BASE_URL / PROVIDER_INVALID_LANGUAGE / PROVIDER_UNKNOWN.

  /** `GET /api/v1/settings/metadata-providers` (Admin) — the provider registry
   * joined with current settings: per provider `{ enabled, hasKey, baseURL, … }`
   * (never the key), the global `metadataLanguage`, and the derived per-kind
   * enablement summary. A Member gets a 403. */
  getMetadataProviders(signal?: AbortSignal): Promise<MetadataProvidersView> {
    return this.request<MetadataProvidersView>("/settings/metadata-providers", {
      signal,
    });
  }

  /** `PUT /api/v1/settings/metadata-providers` (Admin) — a PARTIAL update. Per
   * provider `apiKey` omitted = unchanged, "" = clear, non-empty = set;
   * `enabled`/`baseURL` follow the same omit=unchanged rule. On success the server
   * persists, rebuilds + hot-swaps the running provider, and returns the same
   * masked view GET returns. */
  updateMetadataProviders(
    input: UpdateMetadataProvidersInput,
    signal?: AbortSignal,
  ): Promise<MetadataProvidersView> {
    return this.request<MetadataProvidersView>("/settings/metadata-providers", {
      method: "PUT",
      body: input,
      signal,
    });
  }

  /** `POST /api/v1/settings/metadata-providers/{slug}/test` (Admin) — a
   * best-effort connectivity/credential probe for one provider using the supplied
   * (edited-but-unsaved) credentials or, when omitted, what is on file. This is
   * the one endpoint that makes a real outbound call, and only on explicit Admin
   * action. Returns `{ ok, detail }` (never throws for a failed probe). */
  testMetadataProvider(
    slug: string,
    creds: { apiKey?: string; baseURL?: string } = {},
    signal?: AbortSignal,
  ): Promise<TestProviderResult> {
    return this.request<TestProviderResult>(
      `/settings/metadata-providers/${encodeURIComponent(slug)}/test`,
      { method: "POST", body: creds, signal },
    );
  }

  // --- Admin: subtitle-provider settings (subtitles/05, ADR-0021) --------
  //
  // The exact shape of the metadata-provider surface above: Admin-scope, the key
  // is never returned (only `hasKey`), and a save rebuilds + hot-swaps the running
  // provider with no restart.

  /** `GET /api/v1/settings/subtitle-providers` (Admin) — the subtitle-provider
   * registry joined with current settings plus the `autoFetchLang` knob. A Member
   * gets a 403. */
  getSubtitleProviders(signal?: AbortSignal): Promise<SubtitleProvidersView> {
    return this.request<SubtitleProvidersView>("/settings/subtitle-providers", {
      signal,
    });
  }

  /** `PUT /api/v1/settings/subtitle-providers` (Admin) — a PARTIAL update (apiKey
   * omitted = unchanged, "" = clear, non-empty = set; enabled/baseURL follow the
   * same rule; autoFetchLang "" turns auto-fetch off). Returns the masked view. */
  updateSubtitleProviders(
    input: UpdateSubtitleProvidersInput,
    signal?: AbortSignal,
  ): Promise<SubtitleProvidersView> {
    return this.request<SubtitleProvidersView>("/settings/subtitle-providers", {
      method: "PUT",
      body: input,
      signal,
    });
  }

  /** `POST /api/v1/settings/subtitle-providers/{slug}/test` (Admin) — a
   * best-effort connectivity/credential probe. Returns `{ ok, detail }`. */
  testSubtitleProvider(
    slug: string,
    creds: { apiKey?: string; baseURL?: string } = {},
    signal?: AbortSignal,
  ): Promise<TestProviderResult> {
    return this.request<TestProviderResult>(
      `/settings/subtitle-providers/${encodeURIComponent(slug)}/test`,
      { method: "POST", body: creds, signal },
    );
  }

  // --- Playback surface (issue 04) ---------------------------------------

  /** `POST /api/v1/titles/{id}/playback` — negotiate playback. The body carries
   * a capability profile derived from the browser (containers/codecs it can
   * actually play) plus per-request constraints; the server merges them and
   * returns a decision whose `tier` is one of:
   *   - `directPlay` → a same-origin progressive byte-range `streamUrl`;
   *   - `directStream` / `transcode` → a same-origin HLS media-playlist
   *     `streamUrl` the player loads via hls.js (native HLS on Safari).
   * The media cookie authenticates `streamUrl` (and, for HLS, its segments), so
   * it drops straight into the player with no JS header. Errors are surfaced (not
   * swallowed) as a typed ApiError the player branches on:
   *   - 503 `SERVER_BUSY` (`details: { retryable, suggestedMaxBitrate }`) → the
   *     transcode cap is full; the player shows a busy state and retries at the
   *     suggested lower bitrate;
   *   - 501 `TRANSCODE_REQUIRED` → a genuinely unplayable Title (the server can
   *     neither remux nor transcode it). */
  startPlayback(
    titleId: string,
    opts: StartPlaybackOptions,
    signal?: AbortSignal,
  ): Promise<PlaybackDecision> {
    const body: Record<string, unknown> = {
      deviceProfile: opts.deviceProfile,
      constraints: opts.constraints,
    };
    if (opts.startPosition != null) body.startPosition = opts.startPosition;
    if (opts.editionId) body.editionId = opts.editionId;
    if (opts.burnSubtitleId) body.burnSubtitleId = opts.burnSubtitleId;
    if (opts.audioStreamId) body.audioStreamId = opts.audioStreamId;
    if (opts.videoStreamId) body.videoStreamId = opts.videoStreamId;
    return this.request<PlaybackDecision>(
      `/titles/${encodeURIComponent(titleId)}/playback`,
      { method: "POST", body, signal },
    );
  }

  /** Absolute URL for the sessionless direct-file download
   * (`GET /api/v1/files/{id}/download`) behind the "Open in VLC" affordance.
   *
   * It must be ABSOLUTE (scheme + host) and self-authenticating, because the URL
   * is handed to an EXTERNAL player (VLC) that shares neither this page's origin
   * resolution nor its credentials: the bearer token rides as `?token=` (the one
   * endpoint that accepts it) and the host comes from `window.location.origin`
   * (same-origin deployment, ADR-0010). Returns null when there is no token to
   * embed (logged out) — the caller hides the affordance. */
  directFileDownloadUrl(fileId: string): string | null {
    const token = this.tokenStore.get();
    if (!token) return null;
    const origin =
      typeof window !== "undefined" && window.location
        ? window.location.origin
        : "";
    return (
      `${origin}${this.baseUrl}${API_PREFIX}` +
      `/files/${encodeURIComponent(fileId)}/download` +
      `?token=${encodeURIComponent(token)}`
    );
  }

  /** `POST /api/v1/sessions/{id}/progress` — report a raw position + play state.
   * Doubles as the session keepalive. The client reports position only; the
   * SERVER applies the Watched threshold and returns the resolved watch state,
   * so the caller can update its UI without inventing "watched" semantics.
   *
   * `audioStreamId`, when set, records an in-band audio pick as Remembered audio
   * (audio-streams/05, ADR-0023) — an in-band HLS audio switch never re-negotiates,
   * so the player reports it here, on the same watch-state surface as progress. It
   * does not affect the Watched threshold; the server resolves it to the picked
   * Stream's meaning and remembers it for the next play. */
  async reportProgress(
    sessionId: string,
    report: { positionMs: number; state: PlaybackState; audioStreamId?: string },
    signal?: AbortSignal,
  ): Promise<WatchStateResult> {
    const res = await this.request<WatchStateResult>(
      `/sessions/${encodeURIComponent(sessionId)}/progress`,
      { method: "POST", body: report, signal },
    );
    return normalizeWatchState(res);
  }

  /** `DELETE /api/v1/sessions/{id}` — the clean stop. Ends the session
   * server-side (frees its slot / stops keepalive). 204 No Content. */
  endSession(sessionId: string, signal?: AbortSignal): Promise<void> {
    return this.request<void>(`/sessions/${encodeURIComponent(sessionId)}`, {
      method: "DELETE",
      signal,
    });
  }

  /** `PUT /api/v1/titles/{id}/watchState` — manually mark watched/unwatched,
   * bypassing the threshold. Returns the resolved watch state for the UI. */
  async setWatchState(
    titleId: string,
    watched: boolean,
    signal?: AbortSignal,
  ): Promise<WatchStateResult> {
    const res = await this.request<WatchStateResult>(
      `/titles/${encodeURIComponent(titleId)}/watchState`,
      { method: "PUT", body: { watched }, signal },
    );
    return normalizeWatchState(res);
  }

  /** `PUT /api/v1/titles/{id}/metadata` — Admin hand-edit: write + Lock each
   * supplied descriptive field (CONTEXT.md "Locked field"), so re-enrichment
   * never overwrites it. Returns the updated, normalized Title detail (now
   * listing the edits in `lockedFields`). Admin-only (403 for a Member). */
  async editMetadata(
    titleId: string,
    edit: MetadataEditInput,
    signal?: AbortSignal,
  ): Promise<TitleDetail> {
    const res = await this.request<TitleDetailRaw>(
      `/titles/${encodeURIComponent(titleId)}/metadata`,
      { method: "PUT", body: edit, signal },
    );
    return normalizeTitleDetail(res);
  }

  /** `DELETE /api/v1/titles/{id}/metadata/locks/{field}` — release a Locked
   * field back to auto so the next enrich pass refreshes it. Returns the updated
   * Title detail. Admin-only. */
  async releaseLock(
    titleId: string,
    field: string,
    signal?: AbortSignal,
  ): Promise<TitleDetail> {
    const res = await this.request<TitleDetailRaw>(
      `/titles/${encodeURIComponent(titleId)}/metadata/locks/${encodeURIComponent(field)}`,
      { method: "DELETE", signal },
    );
    return normalizeTitleDetail(res);
  }

  // --- Realtime (SSE, ADR-0016) ------------------------------------------

  /** Subscribe to the server→client SSE stream (`GET /api/v1/events`). Returns
   * an unsubscribe fn that closes the connection. The browser EventSource
   * carries the media cookie (set at login) automatically — no Authorization
   * header is possible on an EventSource, which is exactly why the endpoint is
   * cookie-capable. Same-origin only. In a non-browser/test environment without
   * EventSource this is a no-op (returns a no-op unsubscribe), so callers can
   * mount it unconditionally. onEvent receives the event name and parsed data.
   *
   * Every named event type the server publishes (ADR-0016) must be registered
   * here — EventSource only delivers events whose name has a listener — so a new
   * server event type is opt-in on the client by adding it to EVENT_TYPES. */
  subscribeEvents(onEvent: (type: string, data: unknown) => void): () => void {
    if (typeof EventSource === "undefined") return () => {};
    const es = new EventSource(`${this.baseUrl}${API_PREFIX}/events`, {
      withCredentials: true,
    });
    const handle = (type: string) => (ev: MessageEvent) => {
      let data: unknown;
      try {
        data = ev.data ? JSON.parse(ev.data) : undefined;
      } catch {
        data = undefined;
      }
      onEvent(type, data);
    };
    for (const type of EVENT_TYPES) {
      es.addEventListener(type, handle(type));
    }
    return () => es.close();
  }

  // --- Core request --------------------------------------------------------

  private async request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
    const headers = new Headers();
    const token = this.tokenStore.get();
    if (token) {
      headers.set("Authorization", `Bearer ${token}`);
    }

    let body: BodyInit | undefined;
    if (opts.body instanceof FormData) {
      // A multipart upload (artwork upload, ADR-0026): pass the FormData through
      // untouched — the browser sets Content-Type with the multipart boundary, and
      // JSON-stringifying it would corrupt the body.
      body = opts.body;
    } else if (opts.body !== undefined) {
      headers.set("Content-Type", "application/json");
      body = JSON.stringify(opts.body);
    }

    let res: Response;
    try {
      res = await this.fetchImpl(`${this.baseUrl}${API_PREFIX}${path}`, {
        method: opts.method ?? "GET",
        headers,
        body,
        signal: opts.signal,
      });
    } catch (cause) {
      // fetch only rejects on network-level failures (server down, DNS,
      // aborted). HTTP error statuses still resolve and are handled below.
      if (cause instanceof DOMException && cause.name === "AbortError") {
        throw cause;
      }
      throw new NetworkError("could not reach the server", { cause });
    }

    if (!res.ok) {
      const err = await parseErrorEnvelope(res);
      // Global 401 path (PRD auth model): any unauthorized response clears the
      // session and routes to login. The login/setup callers opt out so their
      // own error (bad credentials / bad claim token) surfaces on the form.
      if (err.isUnauthorized && !opts.skipUnauthorizedHandler) {
        this.onUnauthorized?.();
      }
      throw err;
    }

    // 204 No Content (and other empty bodies) → undefined. Callers expecting a
    // body type for such endpoints simply won't read it.
    if (res.status === 204) {
      return undefined as T;
    }
    return (await res.json()) as T;
  }
}

export { ApiError, NetworkError };
export type {
  Album,
  AlbumTracks,
  ArtistAlbums,
  ArtistsPage,
  ArtistSummary,
  Artwork,
  Collection,
  CollectionDetail,
  CollectionSummary,
  CreateCollectionInput,
  CreateLibraryInput,
  UpdateLibraryInput,
  CreateUserInput,
  UpdateCollectionInput,
  DecisionStream,
  Device,
  DeviceProfile,
  Edition,
  EnrichmentAttentionTitle,
  EnrichmentMatchInput,
  EpisodeContext,
  EpisodeSummary,
  FixMatchInput,
  HomeRows,
  Library,
  LibraryRoot,
  ListTitlesOptions,
  LoginRequest,
  LoginResult,
  MatchOverride,
  MediaFile,
  MetadataProvider,
  MetadataProvidersView,
  ProviderUpdate,
  UpdateMetadataProvidersInput,
  TestProviderResult,
  NeedsReviewItem,
  Playlist,
  PlaylistDetail,
  PlaylistMember,
  PlaylistSummary,
  PlaybackConstraints,
  PlaybackDecision,
  PlaybackState,
  PlaybackTier,
  Role,
  ScanMode,
  ScanState,
  ScanStatus,
  Season,
  SeasonEpisodes,
  ServerInfo,
  SetupRequest,
  SetupResult,
  ShowSeasons,
  ShowSummary,
  ShowsPage,
  StartPlaybackOptions,
  Stream,
  TitleDetail,
  TitleSort,
  TitleSummary,
  TitlesPage,
  TrackContext,
  TrackSummary,
  UnmatchedFile,
  User,
  UserDetail,
  VideoCodecSupport,
  WatchStateResult,
} from "./types";

/** The app-wide singleton. Components import this; tests construct their own
 * ApiClient with a fake fetch/token store. */
export const apiClient = new ApiClient();
