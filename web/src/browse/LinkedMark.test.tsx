import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import LinkedMark, { isLinked, isUnavailable, linkedRowClass } from "./LinkedMark";

// The ONE mark a mirrored row wears (issue 15). Everything the browse surfaces
// do with `linked`/`available` goes through this file, so these are the rules the
// whole app inherits — and the rules the surface tests then assert are actually
// wired up.

describe("LinkedMark — the rules", () => {
  it("treats an ABSENT pair as local, which is what the server means by it", () => {
    // The pair is omitempty on the wire: a local row carries neither field, and
    // that must not read as "linked and unavailable" nor as "linked".
    expect(isLinked({})).toBe(false);
    expect(isLinked(undefined)).toBe(false);
    expect(isUnavailable({})).toBe(false);
    expect(linkedRowClass("poster-tile", {})).toBe("poster-tile");
  });

  it("never greys a LOCAL row, whatever `available` says", () => {
    // `available` is meaningful only while `linked` is true. A local row that
    // somehow carried available:false is still a local row — the bytes are on
    // this disk and no friend's Server is involved.
    expect(isUnavailable({ available: false })).toBe(false);
    expect(linkedRowClass("poster-tile", { available: false })).toBe("poster-tile");
  });

  it("badges a reachable mirror and leaves it ungreyed", () => {
    render(<LinkedMark entity={{ linked: true, available: true }} />);
    const badge = screen.getByTestId("linked-badge");
    expect(badge).toHaveTextContent("Linked");
    expect(badge).toHaveAttribute("data-available", "true");
    expect(linkedRowClass("poster-tile", { linked: true, available: true })).toBe(
      "poster-tile",
    );
  });

  it("greys an unreachable mirror, keeping the badge and the row's own classes", () => {
    render(<LinkedMark entity={{ linked: true, available: false }} />);
    expect(screen.getByTestId("linked-badge")).toHaveAttribute(
      "data-available",
      "false",
    );
    // The greying is APPENDED: the row keeps every layout class it had, so
    // nothing about where it sits or how it is selected changes.
    expect(
      linkedRowClass("poster-tile album-tile", { linked: true, available: false }),
    ).toBe("poster-tile album-tile is-unavailable");
  });

  it("reads a linked row with NO `available` as reachable, not as unknown", () => {
    // A sharer that is answering omits nothing this client needs: `linked` alone
    // means "mirrored, and nothing has said its Server is down".
    expect(isUnavailable({ linked: true })).toBe(false);
    render(<LinkedMark entity={{ linked: true }} />);
    expect(screen.getByTestId("linked-badge")).toHaveAttribute(
      "data-available",
      "true",
    );
  });

  it("renders NOTHING at all for a local row", () => {
    const { container } = render(<LinkedMark entity={{}} />);
    expect(container).toBeEmptyDOMElement();
    expect(screen.queryByTestId("linked-badge")).toBeNull();
  });

  it("names the providing Server when a screen has it, and stands alone when not", () => {
    const { rerender } = render(
      <LinkedMark entity={{ linked: true }} providedBy="Nadia's Server" />,
    );
    expect(screen.getByTestId("linked-badge-provided")).toHaveTextContent(
      "Provided by Nadia's Server",
    );

    // /links is Admin-only and can fail; the badge is the load-bearing half and
    // must survive a name this screen could not get.
    rerender(<LinkedMark entity={{ linked: true }} providedBy="" />);
    expect(screen.queryByTestId("linked-badge-provided")).toBeNull();
    expect(screen.getByTestId("linked-badge")).toBeInTheDocument();
  });

  it("lets a surface keep the testid its own spec already selects on", () => {
    render(<LinkedMark entity={{ linked: true }} testId="library-linked-badge" />);
    expect(screen.getByTestId("library-linked-badge")).toHaveTextContent("Linked");
  });

  it("names the sharing server INLINE when the row carries it (issue 18)", () => {
    // `linkedServer` now rides the row itself, so the badge reads the name off
    // the entity — no /links join, so a Member sees it too.
    render(
      <LinkedMark entity={{ linked: true, linkedServer: "Kate's Obelo" }} />,
    );
    const badge = screen.getByTestId("linked-badge");
    expect(badge).toHaveTextContent("Kate's Obelo");
    // The chain icon leads the name.
    expect(badge.querySelector("svg")).not.toBeNull();
  });

  it("falls back to 'Linked' when the row is a mirror but carries no name", () => {
    // An older server, or a document that marks the row without naming the
    // Server (the detail headers, which use `providedBy` instead).
    render(<LinkedMark entity={{ linked: true }} />);
    const badge = screen.getByTestId("linked-badge");
    expect(badge).toHaveTextContent("Linked");
    expect(badge.querySelector("svg")).not.toBeNull();
  });

  it("still greys an unreachable mirror that names its server", () => {
    render(
      <LinkedMark
        entity={{ linked: true, available: false, linkedServer: "Kate's Obelo" }}
      />,
    );
    const badge = screen.getByTestId("linked-badge");
    expect(badge).toHaveTextContent("Kate's Obelo");
    expect(badge).toHaveAttribute("data-available", "false");
  });
});
