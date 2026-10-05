import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const { editEntityMetadata } = vi.hoisted(() => ({ editEntityMetadata: vi.fn() }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      editEntityMetadata: (...a: unknown[]) => editEntityMetadata(...a),
      releaseEntityLock: vi.fn(),
    },
  };
});

import EntityMetadataEditor from "./EntityMetadataEditor";

beforeEach(() => {
  editEntityMetadata.mockReset();
  editEntityMetadata.mockResolvedValue({});
});

function editor(over: { displayName?: string; genres?: string[] } = {}) {
  return (
    <EntityMetadataEditor
      entityType="shows"
      entityId="s1"
      displayName={over.displayName ?? "Hand Edited"}
      genres={over.genres ?? ["Drama"]}
      onChanged={() => {}}
    />
  );
}

describe("EntityMetadataEditor (R01-08)", () => {
  it("re-seeds its fields when the entity's props change, so a later save cannot re-lock a stale title", async () => {
    const { rerender } = render(editor());
    expect(screen.getByTestId("entity-edit-title")).toHaveValue("Hand Edited");

    // Release re-enriched the show: the server's own title is back.
    rerender(editor({ displayName: "Provider Title" }));
    expect(screen.getByTestId("entity-edit-title")).toHaveValue("Provider Title");

    // Editing only the genres must not carry the stale title along.
    const genres = screen.getByTestId("entity-edit-genres");
    await userEvent.clear(genres);
    await userEvent.type(genres, "Crime");
    await userEvent.click(screen.getByTestId("entity-save-metadata"));
    expect(editEntityMetadata).toHaveBeenCalledWith("shows", "s1", { genres: ["Crime"] });
  });

  it("sends nothing and does not claim 'Saved.' when nothing changed", async () => {
    render(editor());
    await userEvent.click(screen.getByTestId("entity-save-metadata"));
    expect(editEntityMetadata).not.toHaveBeenCalled();
    expect(screen.queryByTestId("entity-metadata-saved")).not.toBeInTheDocument();
  });
});
