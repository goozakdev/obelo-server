import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, act } from "@testing-library/react";
import { renderWithAuth } from "../../test/renderWithAuth";
import type { ApiClient } from "../../api/client";
import type { TitleSummary } from "../../api/types";
import { buildShowQueue } from "./buildQueue";
import { entryFromTitle } from "./model";
import { useQueue, type QueueStore } from "./useQueue";

// R04-06: the lazy Show-tail walk appends into the REAL store. A new play context
// (or a cleared Queue) that happens before a later Season resolves must not get the
// old Show's episodes appended to it.

function ep(id: string) {
  return {
    id,
    kind: "episode",
    title: id,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
    episodeNumber: 1,
  };
}

let store: QueueStore;
function Probe() {
  store = useQueue();
  return <div data-testid="ids">{store.entries.map((e) => e.title.id).join(",")}</div>;
}

const album: TitleSummary = { ...(ep("track-1") as unknown as TitleSummary), kind: "track" };

let season2: (v: { episodes: unknown[] }) => void;
let client: ApiClient;

beforeEach(() => {
  client = {
    getSeasonEpisodes: vi.fn((id: string) =>
      id === "s1"
        ? Promise.resolve({ episodes: [ep("e1"), ep("e2")] })
        : new Promise((r) => (season2 = r)),
    ),
    getShowSeasons: vi.fn().mockResolvedValue({ seasons: [{ id: "s1" }, { id: "s2" }] }),
  } as unknown as ApiClient;
});

describe("buildShowQueue tail vs a newer play context", () => {
  it("appends the tail while its own context is still the Queue", async () => {
    renderWithAuth(<Probe />);
    let tail!: Promise<void>;
    await act(async () => {
      tail = (await buildShowQueue(client, { showId: "sh", seasonId: "s1" }, "e1", store)).tail;
    });
    await act(async () => {
      season2({ episodes: [ep("e3")] });
      await tail;
    });
    expect(screen.getByTestId("ids")).toHaveTextContent("e1,e2,e3");
  });

  it("drops the tail once another playNow replaced the Queue", async () => {
    renderWithAuth(<Probe />);
    let tail!: Promise<void>;
    await act(async () => {
      tail = (await buildShowQueue(client, { showId: "sh", seasonId: "s1" }, "e1", store)).tail;
    });
    act(() => store.playNow([entryFromTitle(album)]));
    await act(async () => {
      season2({ episodes: [ep("e3")] });
      await tail;
    });
    expect(screen.getByTestId("ids")).toHaveTextContent(/^track-1$/);
  });

  it("drops the tail once the Queue was cleared", async () => {
    renderWithAuth(<Probe />);
    let tail!: Promise<void>;
    await act(async () => {
      tail = (await buildShowQueue(client, { showId: "sh", seasonId: "s1" }, "e1", store)).tail;
    });
    act(() => store.clear());
    await act(async () => {
      season2({ episodes: [ep("e3")] });
      await tail;
    });
    expect(screen.getByTestId("ids")).toHaveTextContent(/^$/);
  });
});
