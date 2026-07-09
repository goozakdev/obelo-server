import { describe, it, expect } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import CastStrip from "./CastStrip";
import { personPhotoUrl } from "./Poster";
import type { Credit } from "../api/types";

// Cast strip component tests (cast-photos/01), prior art: Poster.test.tsx (the
// img + onError placeholder) and the cast assertions in TitleDetailScreen.test.tsx.

const cast: Credit[] = [
  { person: "Timothée Chalamet", character: "Paul Atreides", kind: "cast", personId: "tmdb:12345", photoVersion: "v1" },
  { person: "Zendaya", character: "Chani", kind: "cast", personId: "tmdb:67890" },
  // A member with no ref/photo — must still appear, showing the placeholder.
  { person: "No Photo Actor", character: "Extra", kind: "cast" },
];

describe("CastStrip", () => {
  it("renders one card per cast member with a person <img>, name in bold, character below", () => {
    render(<CastStrip cast={cast} />);
    const cards = screen.getAllByTestId("cast-member");
    expect(cards).toHaveLength(3);

    // First card: headshot img built from personPhotoUrl (media-cookie auth, no header).
    const img = screen.getAllByTestId("cast-photo")[0] as HTMLImageElement;
    expect(img.getAttribute("src")).toBe(personPhotoUrl("tmdb:12345", "profile", "v1"));
    expect(img.getAttribute("alt")).toBe("Timothée Chalamet");

    // The actor name is rendered bold (.cast-person → font-weight 600), character below.
    const name = screen.getAllByTestId("cast-person")[0];
    expect(name).toHaveTextContent("Timothée Chalamet");
    expect(name).toHaveClass("cast-person");
    expect(screen.getAllByTestId("cast-character")[0]).toHaveTextContent("Paul Atreides");
  });

  it("shows a placeholder (not a broken image) for a member with no photo/ref", () => {
    render(<CastStrip cast={cast} />);
    // The third member has no personId → placeholder from the start.
    const placeholder = screen.getByTestId("cast-photo-placeholder");
    expect(placeholder).toBeInTheDocument();
    expect(placeholder).toHaveTextContent("NA"); // initials of "No Photo Actor"
    // The card still shows the name + character (no information lost).
    expect(screen.getByText("No Photo Actor")).toBeInTheDocument();
    expect(screen.getByText("Extra")).toBeInTheDocument();
  });

  it("falls back to the placeholder when a headshot load errors (404)", () => {
    render(<CastStrip cast={[cast[0]]} />);
    const img = screen.getByTestId("cast-photo");
    fireEvent.error(img);
    expect(screen.queryByTestId("cast-photo")).toBeNull();
    expect(screen.getByTestId("cast-photo-placeholder")).toHaveTextContent("TC");
  });

  it("is a horizontally-scrollable strip container", () => {
    render(<CastStrip cast={cast} />);
    const strip = screen.getByTestId("cast-strip");
    expect(strip).toHaveClass("cast-strip");
  });

  it("renders no strip when the cast is empty", () => {
    const { container } = render(<CastStrip cast={[]} />);
    expect(screen.queryByTestId("detail-cast")).toBeNull();
    expect(screen.queryByTestId("cast-strip")).toBeNull();
    expect(container).toBeEmptyDOMElement();
  });

  it("omits crew, showing only cast members", () => {
    render(
      <CastStrip
        cast={[
          { person: "Denis Villeneuve", role: "Director", kind: "crew" },
          { person: "Zendaya", character: "Chani", kind: "cast", personId: "tmdb:67890" },
        ]}
      />,
    );
    const cards = screen.getAllByTestId("cast-member");
    expect(cards).toHaveLength(1);
    expect(cards[0]).toHaveTextContent("Zendaya");
  });
});
