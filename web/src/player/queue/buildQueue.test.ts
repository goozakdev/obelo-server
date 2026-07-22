import { describe, it, expect, vi } from "vitest";
import type { ApiClient } from "../../api/client";
import type {
  AlbumTracks,
  EpisodeSummary,
  PlaylistDetail,
  PlaylistMember,
  SeasonEpisodes,
  ShowSeasons,
  TitleSummary,
  TrackSummary,
} from "../../api/types";
import {
  buildAlbumQueue,
  buildFullShowEntries,
  buildPlaylistQueue,
  buildShowQueue,
  buildSingleQueue,
  type QueueSink,
} from "./buildQueue";
import type { QueueEntry } from "./model";

// The build helpers turn a play context into ordered entries via a faked
// ApiClient (the one I/O seam). We assert the Playlist build maps the ordered
// members → entries in position order, that duplicate members get distinct entry
// ids, and that the single-Title build is a one-entry Queue.

function member(itemId: string, id: string, name: string): PlaylistMember {
  return {
    itemId,
    id,
    kind: "movie",
    title: name,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

function title(id: string): TitleSummary {
  return {
    id,
    kind: "movie",
    title: id,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

const playlist: PlaylistDetail = {
  id: "p1",
  name: "Watch later",
  kind: "movie",
  memberCount: 3,
  members: [
    member("i1", "t1", "Dune"),
    member("i2", "t2", "Arrival"),
    member("i3", "t1", "Dune"), // a deliberate duplicate of t1 (distinct itemId)
  ],
};

function fakeClient(getPlaylist: ReturnType<typeof vi.fn>): ApiClient {
  return { getPlaylist } as unknown as ApiClient;
}

describe("buildPlaylistQueue", () => {
  it("maps the ordered members to entries in position order", async () => {
    const getPlaylist = vi.fn().mockResolvedValue(playlist);
    const entries = await buildPlaylistQueue(fakeClient(getPlaylist), "p1");

    expect(getPlaylist).toHaveBeenCalledWith("p1", undefined);
    expect(entries.map((e) => e.title.id)).toEqual(["t1", "t2", "t1"]);
    expect(entries.map((e) => e.title.title)).toEqual(["Dune", "Arrival", "Dune"]);
  });

  it("gives the duplicated member distinct, addressable entry ids", async () => {
    const getPlaylist = vi.fn().mockResolvedValue(playlist);
    const entries = await buildPlaylistQueue(fakeClient(getPlaylist), "p1");
    const entryIds = entries.map((e) => e.entryId);
    expect(new Set(entryIds).size).toBe(3); // the two t1 occurrences are distinct
  });

  it("propagates a fetch failure (the caller falls back to a single Title)", async () => {
    const getPlaylist = vi.fn().mockRejectedValue(new Error("boom"));
    await expect(buildPlaylistQueue(fakeClient(getPlaylist), "p1")).rejects.toThrow(
      "boom",
    );
  });

  it("passes the abort signal through", async () => {
    const getPlaylist = vi.fn().mockResolvedValue(playlist);
    const ctrl = new AbortController();
    await buildPlaylistQueue(fakeClient(getPlaylist), "p1", ctrl.signal);
    expect(getPlaylist).toHaveBeenCalledWith("p1", ctrl.signal);
  });
});

describe("buildSingleQueue", () => {
  it("builds a one-entry Queue from a single Title", () => {
    const entries = buildSingleQueue(title("t9"));
    expect(entries).toHaveLength(1);
    expect(entries[0].title.id).toBe("t9");
    expect(typeof entries[0].entryId).toBe("string");
  });
});

// --- buildAlbumQueue (Track → album-from-here) ------------------------------

function track(id: string, trackNumber: number): TrackSummary {
  return {
    id,
    kind: "track",
    title: id.toUpperCase(),
    discNumber: 1,
    trackNumber,
    needsReview: false,
    resumePositionMs: 0,
    watched: false,
    overview: "",
  };
}

function albumTracks(ids: number[]): AlbumTracks {
  return {
    album: {
      id: "al1",
      artistId: "ar1",
      title: "OK Computer",
      year: 1997,
      hasArtwork: true,
      trackCount: ids.length,
      genres: [],
    },
    tracks: ids.map((n) => track(`tr${n}`, n)),
  };
}

function albumClient(getAlbumTracks: ReturnType<typeof vi.fn>): ApiClient {
  return { getAlbumTracks } as unknown as ApiClient;
}

describe("buildAlbumQueue", () => {
  it("slices the album tracks from the chosen track forward (Track 3 of 10 → 3–10)", async () => {
    const getAlbumTracks = vi
      .fn()
      .mockResolvedValue(albumTracks([1, 2, 3, 4, 5, 6, 7, 8, 9, 10]));
    const entries = await buildAlbumQueue(albumClient(getAlbumTracks), "al1", "tr3");

    expect(getAlbumTracks).toHaveBeenCalledWith("al1", undefined);
    // Tracks 3–10 (8 entries), in disc/track order — NOT 1–10.
    expect(entries.map((e) => e.title.id)).toEqual([
      "tr3", "tr4", "tr5", "tr6", "tr7", "tr8", "tr9", "tr10",
    ]);
    // The chosen Track is the first (now-playing) entry.
    expect(entries[0].title.id).toBe("tr3");
  });

  it("maps the Track summary onto a Title summary entry (kind/watch state carried)", async () => {
    const t = track("tr1", 1);
    t.watched = true;
    t.resumePositionMs = 4200;
    const getAlbumTracks = vi.fn().mockResolvedValue({
      album: albumTracks([1]).album,
      tracks: [t],
    });
    const [entry] = await buildAlbumQueue(albumClient(getAlbumTracks), "al1", "tr1");

    expect(entry.title.kind).toBe("track");
    expect(entry.title.watched).toBe(true);
    expect(entry.title.resumePositionMs).toBe(4200);
  });

  it("propagates a fetch failure (the caller falls back to a single Title)", async () => {
    const getAlbumTracks = vi.fn().mockRejectedValue(new Error("boom"));
    await expect(
      buildAlbumQueue(albumClient(getAlbumTracks), "al1", "tr3"),
    ).rejects.toThrow("boom");
  });
});

// --- buildShowQueue (Episode → show-from-here, cross-season) ----------------

function episode(id: string, seasonNumber: number, episodeNumber: number): EpisodeSummary {
  return {
    id,
    kind: "episode",
    title: id.toUpperCase(),
    seasonNumber,
    episodeNumber,
    episodeLabel: "",
    needsReview: false,
    resumePositionMs: 0,
    watched: false,
    overview: "",
  };
}

function seasonEpisodes(
  seasonId: string,
  seasonNumber: number,
  eps: EpisodeSummary[],
): SeasonEpisodes {
  return {
    season: { id: seasonId, showId: "sh1", seasonNumber, specials: false, episodeCount: eps.length },
    episodes: eps,
  };
}

function showSeasons(seasonIds: { id: string; n: number }[]): ShowSeasons {
  return {
    show: {
      id: "sh1",
      kind: "show",
      title: "The Bear",
      year: 2022,
      needsReview: false,
      unwatchedEpisodeCount: 0,
      overview: "",
      genres: [],
    },
    seasons: seasonIds.map((s) => ({
      id: s.id,
      showId: "sh1",
      seasonNumber: s.n,
      specials: false,
      episodeCount: 0,
    })),
  };
}

function recordingSink() {
  const playNow = vi.fn();
  const enqueue = vi.fn();
  const sink: QueueSink = { playNow, enqueue };
  const ids = (calls: ReturnType<typeof vi.fn>) =>
    calls.mock.calls.map((c) => (c[0] as QueueEntry[]).map((e) => e.title.id));
  return { sink, playNow, enqueue, playedIds: () => ids(playNow), enqueuedIds: () => ids(enqueue) };
}

describe("buildShowQueue", () => {
  it("plays the current season from the chosen episode forward immediately", async () => {
    const s1 = seasonEpisodes("s1", 1, [
      episode("e1", 1, 1),
      episode("e2", 1, 2),
      episode("e3", 1, 3),
    ]);
    const getSeasonEpisodes = vi.fn().mockResolvedValue(s1);
    const getShowSeasons = vi.fn().mockResolvedValue(showSeasons([{ id: "s1", n: 1 }]));
    const client = { getSeasonEpisodes, getShowSeasons } as unknown as ApiClient;
    const { sink, playedIds } = recordingSink();

    const { tail } = await buildShowQueue(client, { showId: "sh1", seasonId: "s1" }, "e2", sink);
    await tail;

    // The now-playing batch is the current Season from the chosen Episode forward.
    expect(playedIds()).toEqual([["e2", "e3"]]);
  });

  it("threads the Show id onto every Episode entry (per-Show preference keying)", async () => {
    const s1 = seasonEpisodes("s1", 1, [episode("e1", 1, 1), episode("e2", 1, 2)]);
    const s2 = seasonEpisodes("s2", 2, [episode("e3", 2, 1)]);
    const byId: Record<string, SeasonEpisodes> = { s1, s2 };
    const getSeasonEpisodes = vi.fn((id: string) => Promise.resolve(byId[id]));
    const getShowSeasons = vi
      .fn()
      .mockResolvedValue(showSeasons([{ id: "s1", n: 1 }, { id: "s2", n: 2 }]));
    const client = { getSeasonEpisodes, getShowSeasons } as unknown as ApiClient;
    const { sink, playNow, enqueue } = recordingSink();

    const { tail } = await buildShowQueue(client, { showId: "sh1", seasonId: "s1" }, "e1", sink);
    await tail;

    // Both the now-playing batch and the appended tail carry showId "sh1", so the
    // player can key the Show's Playback preference synchronously (no detail wait).
    const played = playNow.mock.calls[0][0] as QueueEntry[];
    const appended = enqueue.mock.calls[0][0] as QueueEntry[];
    expect(played.every((e) => e.showId === "sh1")).toBe(true);
    expect(appended.every((e) => e.showId === "sh1")).toBe(true);
  });

  it("walks the following seasons in order, across the season boundary", async () => {
    const s1 = seasonEpisodes("s1", 1, [episode("e1", 1, 1), episode("e2", 1, 2)]);
    const s2 = seasonEpisodes("s2", 2, [episode("e3", 2, 1), episode("e4", 2, 2)]);
    const s3 = seasonEpisodes("s3", 3, [episode("e5", 3, 1)]);
    const byId: Record<string, SeasonEpisodes> = { s1, s2, s3 };
    const getSeasonEpisodes = vi.fn((id: string) => Promise.resolve(byId[id]));
    const getShowSeasons = vi.fn().mockResolvedValue(
      showSeasons([{ id: "s1", n: 1 }, { id: "s2", n: 2 }, { id: "s3", n: 3 }]),
    );
    const client = { getSeasonEpisodes, getShowSeasons } as unknown as ApiClient;
    const { sink, playedIds, enqueuedIds } = recordingSink();

    // Start mid-season-1 at e2: now-playing is [e2], then seasons 2 and 3 append.
    const { tail } = await buildShowQueue(client, { showId: "sh1", seasonId: "s1" }, "e2", sink);
    await tail;

    expect(playedIds()).toEqual([["e2"]]);
    // Following seasons, in order, each its full episode list.
    expect(enqueuedIds()).toEqual([["e3", "e4"], ["e5"]]);
  });

  it("makes the now-playing batch available BEFORE the cross-season walk finishes", async () => {
    const s1 = seasonEpisodes("s1", 1, [episode("e1", 1, 1), episode("e2", 1, 2)]);
    // getShowSeasons stays pending until we release it — the tail can't progress.
    let releaseSeasons!: (v: ShowSeasons) => void;
    const seasonsPromise = new Promise<ShowSeasons>((res) => {
      releaseSeasons = res;
    });
    const getSeasonEpisodes = vi.fn().mockResolvedValue(s1);
    const getShowSeasons = vi.fn().mockReturnValue(seasonsPromise);
    const client = { getSeasonEpisodes, getShowSeasons } as unknown as ApiClient;
    const { sink, playNow, enqueue } = recordingSink();

    const { tail } = await buildShowQueue(client, { showId: "sh1", seasonId: "s1" }, "e1", sink);

    // playNow already happened (playback can start); the tail hasn't appended yet.
    expect(playNow).toHaveBeenCalledTimes(1);
    expect(enqueue).not.toHaveBeenCalled();

    releaseSeasons(showSeasons([{ id: "s1", n: 1 }]));
    await tail;
  });

  it("propagates a first-season fetch failure (the caller falls back to a single Title)", async () => {
    const getSeasonEpisodes = vi.fn().mockRejectedValue(new Error("boom"));
    const getShowSeasons = vi.fn();
    const client = { getSeasonEpisodes, getShowSeasons } as unknown as ApiClient;
    const { sink } = recordingSink();

    await expect(
      buildShowQueue(client, { showId: "sh1", seasonId: "s1" }, "e1", sink),
    ).rejects.toThrow("boom");
  });

  it("a failing tail fetch never disturbs the already-playing Queue", async () => {
    const s1 = seasonEpisodes("s1", 1, [episode("e1", 1, 1)]);
    const getSeasonEpisodes = vi.fn().mockResolvedValue(s1);
    const getShowSeasons = vi.fn().mockRejectedValue(new Error("tail boom"));
    const client = { getSeasonEpisodes, getShowSeasons } as unknown as ApiClient;
    const { sink, playNow, enqueue } = recordingSink();

    // The now-playing batch resolved; the tail swallows its failure (no reject).
    const { tail } = await buildShowQueue(client, { showId: "sh1", seasonId: "s1" }, "e1", sink);
    await expect(tail).resolves.toBeUndefined();
    expect(playNow).toHaveBeenCalledTimes(1);
    expect(enqueue).not.toHaveBeenCalled();
  });
});

// --- buildFullShowEntries (whole series → ordered entries) ------------------

describe("buildFullShowEntries", () => {
  it("returns every Season's Episodes in the server's Season order", async () => {
    const byId: Record<string, SeasonEpisodes> = {
      s1: seasonEpisodes("s1", 1, [episode("e1", 1, 1), episode("e2", 1, 2)]),
      s2: seasonEpisodes("s2", 2, [episode("e3", 2, 1)]),
    };
    const getSeasonEpisodes = vi.fn((id: string) => Promise.resolve(byId[id]));
    const getShowSeasons = vi.fn().mockResolvedValue(
      showSeasons([{ id: "s1", n: 1 }, { id: "s2", n: 2 }]),
    );
    const client = { getSeasonEpisodes, getShowSeasons } as unknown as ApiClient;

    const entries = await buildFullShowEntries(client, "sh1");
    expect(entries.map((e) => e.title.id)).toEqual(["e1", "e2", "e3"]);
  });

  it("propagates a fetch failure to the caller", async () => {
    const getShowSeasons = vi.fn().mockRejectedValue(new Error("boom"));
    const getSeasonEpisodes = vi.fn();
    const client = { getSeasonEpisodes, getShowSeasons } as unknown as ApiClient;

    await expect(buildFullShowEntries(client, "sh1")).rejects.toThrow("boom");
  });
});
