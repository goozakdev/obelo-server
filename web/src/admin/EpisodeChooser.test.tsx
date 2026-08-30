import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import EpisodeChooser, { type EpisodeChooserPage } from "./EpisodeChooser";

// EpisodeChooser is step two of the Episode pin: having picked a series, pick the
// episode within it whose record should decorate a Slot.
//
// Its caller is the file matcher's record picker (ShowMatcherScreen). The
// Needs-Fixing queue used to mount it one flagged Episode at a time, in a place
// where the Admin could not see the season they were fixing; that flow retired with
// file-matcher/07, when a Show's episode problems became one row whose action is
// the file matcher. The pin itself survives, narrowed to repointing a Slot's RECORD
// at another series (CONTEXT.md "Episode pin", ADR-0044), reached from a Slot inside
// the matcher.
//
// What this suite pins down is the seam that move required: the component no longer
// knows WHERE episodes come from (the queue anchored on a Title, the matcher anchors
// on a Show and a chosen series), so the fetch arrives as `load`.

function page(over: Partial<EpisodeChooserPage> = {}): EpisodeChooserPage {
  return {
    seasons: [
      { season: 3, episodeCount: 20 },
      { season: 4, episodeCount: 10 },
    ],
    season: 3,
    episodes: [
      { season: 3, episode: 11, name: "Sideshow", airDate: "1995-09-04" },
      { season: 3, episode: 12, name: "Avatar" },
    ],
    ...over,
  };
}

describe("EpisodeChooser", () => {
  it("opens on the caller's default season without asking for one", async () => {
    // Undefined means "the season this is already filed under", which is the list
    // the Admin most likely wants: right season, wrong episode.
    const load = vi.fn().mockResolvedValue(page());
    render(
      <EpisodeChooser
        seriesTitle="The New Batman Adventures"
        load={load}
        onPick={vi.fn()}
        onBack={vi.fn()}
      />,
    );

    await screen.findByTestId("episode-choice-list");
    expect(load).toHaveBeenCalledWith(undefined);
    expect(screen.getByTestId("episode-chooser-series")).toHaveTextContent(
      "The New Batman Adventures",
    );
    expect(screen.getAllByTestId("episode-choice")).toHaveLength(2);
  });

  it("lets the Admin switch seasons to find the episode", async () => {
    // The motivating case is a run of files the provider counts in ANOTHER season,
    // so the season the file is filed under is exactly the wrong list.
    const load = vi.fn().mockResolvedValue(page());
    render(
      <EpisodeChooser seriesTitle="Series" load={load} onPick={vi.fn()} onBack={vi.fn()} />,
    );
    await screen.findByTestId("episode-choice-list");

    await userEvent.selectOptions(screen.getByTestId("episode-chooser-season-select"), "4");
    await waitFor(() => expect(load).toHaveBeenCalledWith(4));
  });

  it("hands back the PROVIDER's numbers, not the file's", async () => {
    // The whole point of the second step: pinning must carry the record's own
    // season and episode, or nothing is fixed.
    const load = vi.fn().mockResolvedValue(page());
    const onPick = vi.fn().mockResolvedValue(undefined);
    render(
      <EpisodeChooser seriesTitle="Series" load={load} onPick={onPick} onBack={vi.fn()} />,
    );
    await screen.findByTestId("episode-choice-list");

    await userEvent.click(screen.getAllByTestId("episode-choice")[1]);
    await waitFor(() =>
      expect(onPick).toHaveBeenCalledWith(
        expect.objectContaining({ season: 3, episode: 12 }),
      ),
    );
  });

  it("keeps the season list after a second load, which does not resend it", async () => {
    const load = vi
      .fn()
      .mockResolvedValueOnce(page())
      .mockResolvedValueOnce({ season: 4, episodes: [] });
    render(
      <EpisodeChooser seriesTitle="Series" load={load} onPick={vi.fn()} onBack={vi.fn()} />,
    );
    await screen.findByTestId("episode-choice-list");

    await userEvent.selectOptions(screen.getByTestId("episode-chooser-season-select"), "4");
    await screen.findByTestId("episode-chooser-empty");
    // Still switchable: losing the season list would strand the Admin in an empty
    // season with no way back.
    expect(screen.getByTestId("episode-chooser-season-select")).toBeInTheDocument();
  });

  it("goes back to the series list rather than out of the flow", async () => {
    const onBack = vi.fn();
    render(
      <EpisodeChooser
        seriesTitle="Series"
        load={vi.fn().mockResolvedValue(page())}
        onPick={vi.fn()}
        onBack={onBack}
      />,
    );
    await screen.findByTestId("episode-choice-list");

    await userEvent.click(screen.getByTestId("episode-chooser-back"));
    expect(onBack).toHaveBeenCalled();
  });

  it("reports a failed load instead of showing an empty season", async () => {
    render(
      <EpisodeChooser
        seriesTitle="Series"
        load={vi.fn().mockRejectedValue(new Error("provider unreachable"))}
        onPick={vi.fn()}
        onBack={vi.fn()}
      />,
    );
    expect(await screen.findByTestId("episode-chooser-error")).toHaveTextContent(
      "provider unreachable",
    );
  });
});
