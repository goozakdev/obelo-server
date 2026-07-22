import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import type { TitleDetail } from "../api/types";
import { loadPreference } from "./playbackPreference";
import PlaybackOptionsSheet from "./PlaybackOptionsSheet";

// The Playback Options sheet (appletv-web-parity §1/§2). The whole sheet is built
// from the in-hand `title` — assert it opens with ZERO network calls, shows the
// Edition section (Auto + a row per Edition, active row marked), and that Play
// COMMITS the draft to the preference store AND starts playback while backing out
// DISCARDS it.

// Spy the whole api client so we can prove the sheet never touches the network on
// open (it builds purely from the `title` prop it's handed).
const apiCalls = vi.hoisted(() => ({ calls: 0 }));
vi.mock("../api/client", () => {
  const trap = new Proxy(
    {},
    {
      get: () => (..._a: unknown[]) => {
        apiCalls.calls += 1;
        return Promise.resolve({});
      },
    },
  );
  return { apiClient: trap };
});

function movieDetail(): TitleDetail {
  return {
    id: "t1",
    libraryId: "lib1",
    kind: "movie",
    title: "Blade Runner",
    year: 1982,
    needsReview: false,
    ambiguous: false,
    hidden: false,
    resumePositionMs: 0,
    watched: false,
    editions: [
      { id: "ed-tc", name: "Theatrical Cut", files: [] },
      { id: "ed-fc", name: "Final Cut", files: [] },
    ],
    artwork: [],
    subtitles: [],
    overview: "",
    tagline: "",
    contentRating: "",
    releaseDate: "",
    runtimeMinutes: 0,
    studio: "",
    genres: [],
    cast: [],
    enrichmentStatus: "",
    lockedFields: [],
    displayTitle: "",
  };
}

beforeEach(() => {
  window.localStorage.clear();
  apiCalls.calls = 0;
});

describe("PlaybackOptionsSheet — Edition axis", () => {
  it("builds from the in-hand payload with no network call, showing Auto + each Edition", () => {
    render(
      <PlaybackOptionsSheet
        title={movieDetail()}
        userId="u1"
        open
        onClose={() => {}}
        onPlay={() => {}}
      />,
    );
    expect(apiCalls.calls).toBe(0);
    expect(screen.getByTestId("edition-option-auto")).toBeInTheDocument();
    const rows = screen.getAllByTestId("edition-option");
    expect(rows.map((r) => r.textContent)).toEqual(
      expect.arrayContaining([expect.stringContaining("Theatrical Cut"), expect.stringContaining("Final Cut")]),
    );
    // Nothing stored → Auto is the active row.
    expect(screen.getByTestId("edition-option-auto")).toHaveAttribute("aria-checked", "true");
  });

  it("Play commits the Edition draft as the saved preference AND starts playback", () => {
    const onPlay = vi.fn();
    const onClose = vi.fn();
    render(
      <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={onClose} onPlay={onPlay} />,
    );
    // Pick "Final Cut" (a draft change only — nothing persisted yet).
    fireEvent.click(screen.getByRole("radio", { name: /Final Cut/ }));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).editionName).toBeNull();
    // Play commits + starts.
    fireEvent.click(screen.getByTestId("playback-options-play"));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).editionName).toBe("Final Cut");
    expect(onPlay).toHaveBeenCalledTimes(1);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("selecting Auto omits the Edition (stored as null)", () => {
    // Seed a prior pick, then commit Auto over it.
    window.localStorage.setItem(
      "juicebox.playback-pref.u1.title.t1",
      JSON.stringify({ editionName: "Final Cut" }),
    );
    render(
      <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    // Opens reflecting the saved pick.
    expect(screen.getByRole("radio", { name: /Final Cut/ })).toHaveAttribute("aria-checked", "true");
    fireEvent.click(screen.getByTestId("edition-option-auto"));
    fireEvent.click(screen.getByTestId("playback-options-play"));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).editionName).toBeNull();
  });

  it("backing out discards the draft (preference untouched, no playback)", () => {
    const onPlay = vi.fn();
    render(
      <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={onPlay} />,
    );
    fireEvent.click(screen.getByRole("radio", { name: /Final Cut/ }));
    fireEvent.click(screen.getByTestId("playback-options-cancel"));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).editionName).toBeNull();
    expect(onPlay).not.toHaveBeenCalled();
  });
});
