import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, screen, waitFor, within } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import type { Link } from "../api/types";

// AdminLinkedServersScreen end-to-end through the faked API client (the one seam
// — the same shape AdminRemoteAccessScreen.test.tsx uses).
//
// The spine of this suite is the PRD's success criteria, which are a WALKTHROUGH
// and not a feature list: a friend sends a string, an Admin pastes it, the page
// names the sharer and the libraries that arrived and points at the grant dialog;
// later that friend's server goes dark and the page says so with nobody pressing
// reload; later still they delete the remote user and the page says "revoked" and
// asks for a fresh invite, which Re-key takes. Unlink is the only destructive
// path and its confirmation has to name what it deletes, watch state included.
//
// The paste-time refusals have their own unit suite (linkErrors.test.ts); what is
// asserted here is that the screen SHOWS them, on the right control.

const {
  listLinks,
  createLink,
  rekeyLink,
  syncLink,
  deleteLink,
  updateLibrary,
  subscribeEvents,
} = vi.hoisted(() => ({
  listLinks: vi.fn(),
  createLink: vi.fn(),
  rekeyLink: vi.fn(),
  syncLink: vi.fn(),
  deleteLink: vi.fn(),
  updateLibrary: vi.fn(),
  subscribeEvents: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listLinks: (...a: unknown[]) => listLinks(...a),
      createLink: (...a: unknown[]) => createLink(...a),
      rekeyLink: (...a: unknown[]) => rekeyLink(...a),
      syncLink: (...a: unknown[]) => syncLink(...a),
      deleteLink: (...a: unknown[]) => deleteLink(...a),
      updateLibrary: (...a: unknown[]) => updateLibrary(...a),
      subscribeEvents: (...a: unknown[]) => subscribeEvents(...a),
    },
  };
});

import AdminLinkedServersScreen, {
  lastSyncedLabel,
  linkStateNote,
} from "./AdminLinkedServersScreen";
import AdminScreen from "../screens/AdminScreen";

/** The live SSE listener the events hub registered, so a test can deliver a
 * `linkState` nudge the way the server would. */
let emit: ((type: string, data: unknown) => void) | null = null;

function link(over: Partial<Link> = {}): Link {
  return {
    id: "link1",
    serverId: "srv-sam",
    serverName: "Sam's server",
    state: "connected",
    activeOrigin: "https://sam.example.net",
    origins: ["https://sam.example.net", "http://obelo.tail1a2b.ts.net"],
    lastSyncedAt: new Date(Date.now() - 2 * 3600_000).toISOString(),
    lastError: "",
    libraries: [
      { id: "lib9", name: "Cartoons", kind: "movie" },
      { id: "lib8", name: "Sam's music", kind: "music" },
    ],
    ...over,
  };
}

const INVITE = "obelo-link:eyJ2IjoxfQ";

beforeEach(() => {
  listLinks.mockReset();
  createLink.mockReset();
  rekeyLink.mockReset();
  syncLink.mockReset();
  deleteLink.mockReset();
  updateLibrary.mockReset();
  subscribeEvents.mockReset();
  emit = null;
  subscribeEvents.mockImplementation((fn: (type: string, data: unknown) => void) => {
    emit = fn;
    return () => {
      emit = null;
    };
  });
});

// --- The pure helpers -------------------------------------------------------

describe("linkStateNote — three states, three next moves", () => {
  it("connected has nothing to say beyond the chip", () => {
    const { label, note } = linkStateNote(link());
    expect(label).toBe("Connected");
    expect(note).toBe("");
  });

  it("unreachable quotes the server's reason VERBATIM", () => {
    const { label, note } = linkStateNote(
      link({ state: "unreachable", lastError: "dial tcp 10.0.0.4:443: i/o timeout" }),
    );
    expect(label).toBe("Unreachable");
    expect(note).toContain("dial tcp 10.0.0.4:443: i/o timeout");
  });

  it("unreachable without a reason still says nothing is lost", () => {
    const { note } = linkStateNote(link({ state: "unreachable", lastError: "" }));
    expect(note).toMatch(/watch state are untouched/i);
  });

  it("revoked asks for a fresh invite and promises the libraries are kept", () => {
    const { label, note } = linkStateNote(link({ state: "revoked" }));
    expect(label).toBe("Revoked");
    expect(note).toMatch(/fresh invite/i);
    expect(note).toMatch(/re-key/i);
    expect(note).toMatch(/watch state are kept/i);
  });
});

describe("lastSyncedLabel", () => {
  it("says `never` for a Link the mirror has not pulled once", () => {
    expect(lastSyncedLabel(null)).toBe("never");
  });

  it("says how long ago otherwise", () => {
    const now = Date.parse("2026-09-02T12:00:00Z");
    expect(lastSyncedLabel("2026-09-02T10:00:00Z", now)).toBe("2h ago");
  });
});

// --- The walkthrough --------------------------------------------------------

describe("AdminLinkedServersScreen — the walkthrough", () => {
  it("pastes an invite and names the sharer, the libraries and where to grant them", async () => {
    listLinks.mockResolvedValue([]);
    createLink.mockResolvedValue(link());
    const user = userEvent.setup();
    renderWithAuth(<AdminLinkedServersScreen />, {
      initialEntries: ["/admin/linked-servers"],
      features: { linkedLibraries: true },
    });

    await screen.findByTestId("links-empty");
    // The paste box takes the WHOLE string and nothing else.
    await user.type(screen.getByTestId("link-invite-input"), INVITE);
    listLinks.mockResolvedValue([link()]);
    await user.click(screen.getByTestId("link-submit"));

    expect(createLink).toHaveBeenCalledWith(INVITE);

    const result = await screen.findByTestId("link-result");
    expect(within(result).getByTestId("link-result-server")).toHaveTextContent(
      "Sam's server",
    );
    // "Linked successfully" is not an answer to "did I get the cartoons?".
    const received = within(result).getByTestId("link-result-libraries");
    expect(received).toHaveTextContent("Cartoons");
    expect(received).toHaveTextContent("Sam's music");
    // Nobody can see them yet; the shortcut is into the EXISTING grant dialog.
    expect(within(result).getByTestId("link-result-grant")).toHaveAttribute(
      "href",
      "/admin/users",
    );

    // And the list refetched, so the new Link is a row.
    await waitFor(() => expect(screen.getByTestId("link-row")).toBeInTheDocument());
  });

  it("says so plainly when the sharer granted nothing", async () => {
    listLinks.mockResolvedValue([]);
    createLink.mockResolvedValue(link({ libraries: [] }));
    const user = userEvent.setup();
    renderWithAuth(<AdminLinkedServersScreen />);

    await screen.findByTestId("links-empty");
    await user.type(screen.getByTestId("link-invite-input"), INVITE);
    await user.click(screen.getByTestId("link-submit"));

    expect(await screen.findByTestId("link-result-no-libraries")).toHaveTextContent(
      /not shared any libraries/i,
    );
  });

  it("renders a connected Link's origin, its alternatives and its last sync", async () => {
    listLinks.mockResolvedValue([link()]);
    renderWithAuth(<AdminLinkedServersScreen />);

    const row = await screen.findByTestId("link-row");
    expect(within(row).getByTestId("link-state-chip")).toHaveTextContent("Connected");
    expect(within(row).getByTestId("link-active-origin")).toHaveTextContent(
      "https://sam.example.net",
    );
    // WHICH path is carrying the films is otherwise invisible and is the first
    // thing to look at when a Link is slow.
    expect(within(row).getByTestId("link-origins")).toHaveTextContent(
      "http://obelo.tail1a2b.ts.net",
    );
    expect(within(row).getByTestId("link-last-synced")).toHaveTextContent("2h ago");
    expect(within(row).getByTestId("link-libraries")).toHaveTextContent("Cartoons");
    expect(within(row).getAllByTestId("link-grant-users")[0]).toHaveAttribute(
      "href",
      "/admin/users",
    );
  });

  it("says `never` on a brand-new Link rather than showing an empty field", async () => {
    listLinks.mockResolvedValue([link({ lastSyncedAt: null })]);
    renderWithAuth(<AdminLinkedServersScreen />);

    const row = await screen.findByTestId("link-row");
    expect(within(row).getByTestId("link-last-synced")).toHaveTextContent("never");
  });

  it("shows the friend going dark on the `linkState` nudge, with NO reload", async () => {
    listLinks.mockResolvedValue([link()]);
    renderWithAuth(<AdminLinkedServersScreen />);

    const row = await screen.findByTestId("link-row");
    expect(within(row).getByTestId("link-state-chip")).toHaveTextContent("Connected");

    // The event is a nudge carrying a link id; the GET is the truth.
    listLinks.mockResolvedValue([
      link({ state: "unreachable", lastError: "connection refused" }),
    ]);
    await act(async () => {
      emit?.("linkState", { linkId: "link1" });
    });

    await waitFor(() =>
      expect(screen.getByTestId("link-state-chip")).toHaveTextContent("Unreachable"),
    );
    expect(screen.getByTestId("link-state-note")).toHaveTextContent("connection refused");
  });

  it("ignores an unrelated event rather than refetching on every nudge", async () => {
    listLinks.mockResolvedValue([link()]);
    renderWithAuth(<AdminLinkedServersScreen />);
    await screen.findByTestId("link-row");
    expect(listLinks).toHaveBeenCalledTimes(1);

    await act(async () => {
      emit?.("scanProgress", { libraryId: "lib1" });
    });
    expect(listLinks).toHaveBeenCalledTimes(1);
  });

  it("Sync now sweeps and refetches; a failed sweep reports the reason", async () => {
    listLinks.mockResolvedValue([link({ state: "unreachable", lastError: "timeout" })]);
    syncLink.mockRejectedValue(
      new ApiError(503, "LINK_UNREACHABLE", "connection refused"),
    );
    const user = userEvent.setup();
    renderWithAuth(<AdminLinkedServersScreen />);

    const row = await screen.findByTestId("link-row");
    await user.click(within(row).getByTestId("link-sync"));

    expect(syncLink).toHaveBeenCalledWith("link1");
    expect(await screen.findByTestId("link-action-error")).toHaveTextContent(
      /could not reach their server/i,
    );
    // The state was recorded server-side either way, so the row is refetched.
    await waitFor(() => expect(listLinks).toHaveBeenCalledTimes(2));
  });

  it("re-keys a revoked Link with a fresh invite, in the same kind of textarea", async () => {
    listLinks.mockResolvedValue([link({ state: "revoked" })]);
    rekeyLink.mockResolvedValue(link());
    const user = userEvent.setup();
    renderWithAuth(<AdminLinkedServersScreen />);

    const row = await screen.findByTestId("link-row");
    expect(within(row).getByTestId("link-state-chip")).toHaveTextContent("Revoked");
    expect(within(row).getByTestId("link-state-note")).toHaveTextContent(/fresh invite/i);

    await user.click(within(row).getByTestId("link-rekey-toggle"));
    await user.type(screen.getByTestId("link-rekey-input"), INVITE);
    listLinks.mockResolvedValue([link()]);
    await user.click(screen.getByTestId("link-rekey-submit"));

    expect(rekeyLink).toHaveBeenCalledWith("link1", INVITE);
    await waitFor(() =>
      expect(screen.getByTestId("link-state-chip")).toHaveTextContent("Connected"),
    );
    // The form closes on success rather than sitting there with a spent string.
    expect(screen.queryByTestId("link-rekey-input")).toBeNull();
  });

  it("refuses another household's invite on Re-key with the mismatch sentence", async () => {
    listLinks.mockResolvedValue([link({ state: "revoked" })]);
    rekeyLink.mockRejectedValue(new ApiError(409, "LINK_SERVER_MISMATCH", "nope"));
    const user = userEvent.setup();
    renderWithAuth(<AdminLinkedServersScreen />);

    const row = await screen.findByTestId("link-row");
    await user.click(within(row).getByTestId("link-rekey-toggle"));
    await user.type(screen.getByTestId("link-rekey-input"), INVITE);
    await user.click(screen.getByTestId("link-rekey-submit"));

    expect(await screen.findByTestId("link-rekey-error")).toHaveTextContent(
      /different server/i,
    );
    // The textarea stays, with what was typed still in it.
    expect(screen.getByTestId("link-rekey-input")).toHaveValue(INVITE);
  });

  it("unlinks behind a confirmation that NAMES the libraries and the watch state", async () => {
    listLinks.mockResolvedValue([link()]);
    deleteLink.mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderWithAuth(<AdminLinkedServersScreen />);

    const row = await screen.findByTestId("link-row");
    await user.click(within(row).getByTestId("link-unlink"));

    const message = screen.getByTestId("confirm-dialog-message");
    expect(message).toHaveTextContent("Cartoons");
    expect(message).toHaveTextContent("Sam's music");
    // The watch state is the one thing nothing else on this page can lose and no
    // re-link brings back, so the confirmation has to say it out loud.
    expect(message).toHaveTextContent(/watch state/i);
    expect(message).toHaveTextContent(/nothing on their server is affected/i);

    listLinks.mockResolvedValue([]);
    await user.click(screen.getByTestId("confirm-dialog-confirm"));

    expect(deleteLink).toHaveBeenCalledWith("link1");
    await waitFor(() => expect(screen.getByTestId("links-empty")).toBeInTheDocument());
  });

  it("shows the paste refusal on the paste box and keeps the string typed", async () => {
    listLinks.mockResolvedValue([]);
    createLink.mockRejectedValue(new ApiError(410, "INVITE_EXPIRED", "gone"));
    const user = userEvent.setup();
    renderWithAuth(<AdminLinkedServersScreen />);

    await screen.findByTestId("links-empty");
    await user.type(screen.getByTestId("link-invite-input"), INVITE);
    await user.click(screen.getByTestId("link-submit"));

    expect(await screen.findByTestId("link-error")).toHaveTextContent(/fresh one/i);
    expect(screen.getByTestId("link-invite-input")).toHaveValue(INVITE);
    expect(screen.queryByTestId("link-result")).toBeNull();
  });

  it("offers a retry when the list itself could not be read", async () => {
    listLinks.mockRejectedValueOnce(new ApiError(500, "INTERNAL", "database is on fire"));
    const user = userEvent.setup();
    renderWithAuth(<AdminLinkedServersScreen />);

    expect(await screen.findByTestId("links-load-error")).toHaveTextContent(
      "database is on fire",
    );
    listLinks.mockResolvedValue([link()]);
    await user.click(screen.getByTestId("links-retry"));
    await waitFor(() => expect(screen.getByTestId("link-row")).toBeInTheDocument());
  });
});

// --- Rename ------------------------------------------------------------------

// The one write the server allows on a mirror (ADR-0056 §1, api-contract §3.3),
// and — since issue 16 took the mirrors off the Libraries hub — this is the only
// place it is reachable from. What matters is that it PATCHes `name` and NOTHING
// ELSE: `addRootFolders` on a linked Library is 409 LINKED_LIBRARY, so a rename
// that carried one would fail every time.

describe("AdminLinkedServersScreen — renaming a linked library", () => {
  it("renames in place, sending only the name, and refetches the list", async () => {
    listLinks.mockResolvedValue([link()]);
    updateLibrary.mockResolvedValue({
      id: "lib9",
      name: "Saturday morning",
      kind: "movie",
      rootFolders: [],
      linked: true,
      available: true,
    });
    const user = userEvent.setup();
    renderWithAuth(<AdminLinkedServersScreen />);

    const row = await screen.findByTestId("link-row");
    const lib = within(row)
      .getAllByTestId("link-library-rename-toggle")
      .map((b) => b.closest("li") as HTMLElement)
      .find((li) => li.dataset.libraryId === "lib9")!;

    await user.click(within(lib).getByTestId("link-library-rename-toggle"));
    const input = within(lib).getByTestId("link-library-rename-input");
    expect(input).toHaveValue("Cartoons");

    await user.clear(input);
    await user.type(input, "Saturday morning");

    // The list the row will be redrawn from is the server's, as everywhere else
    // on this page: GET /links is the truth.
    listLinks.mockResolvedValue([
      link({
        libraries: [
          { id: "lib9", name: "Saturday morning", kind: "movie" },
          { id: "lib8", name: "Sam's music", kind: "music" },
        ],
      }),
    ]);
    await user.click(within(lib).getByTestId("link-library-rename-save"));

    await waitFor(() =>
      expect(updateLibrary).toHaveBeenCalledWith("lib9", { name: "Saturday morning" }),
    );
    // Only `name`. A stray addRootFolders here would be refused every time.
    expect(Object.keys(updateLibrary.mock.calls[0][1] as object)).toEqual(["name"]);

    // Back to a plain line, under its new name — and the shelf beside it is
    // untouched.
    await waitFor(() =>
      expect(screen.getByTestId("link-libraries")).toHaveTextContent(
        "Saturday morning",
      ),
    );
    expect(screen.getByTestId("link-libraries")).toHaveTextContent("Sam's music");
    expect(screen.queryByTestId("link-library-rename-input")).toBeNull();
  });

  it("keeps the grant shortcut on the line", async () => {
    listLinks.mockResolvedValue([link()]);
    renderWithAuth(<AdminLinkedServersScreen />);

    const row = await screen.findByTestId("link-row");
    expect(within(row).getAllByTestId("link-grant-users")[0]).toHaveAttribute(
      "href",
      "/admin/users",
    );
  });

  it("will not save an empty or unchanged name", async () => {
    listLinks.mockResolvedValue([link()]);
    const user = userEvent.setup();
    renderWithAuth(<AdminLinkedServersScreen />);

    await screen.findByTestId("link-row");
    await user.click(screen.getAllByTestId("link-library-rename-toggle")[0]);
    const input = screen.getByTestId("link-library-rename-input");

    // Unchanged: nothing to send.
    expect(screen.getByTestId("link-library-rename-save")).toBeDisabled();

    await user.clear(input);
    expect(screen.getByTestId("link-library-rename-save")).toBeDisabled();
    await user.keyboard("{Enter}");
    expect(updateLibrary).not.toHaveBeenCalled();
  });

  it("keeps what was typed when the server refuses, and reports why", async () => {
    listLinks.mockResolvedValue([link()]);
    updateLibrary.mockRejectedValue(
      new ApiError(409, "LINKED_LIBRARY", "that library is provided by another server"),
    );
    const user = userEvent.setup();
    renderWithAuth(<AdminLinkedServersScreen />);

    await screen.findByTestId("link-row");
    await user.click(screen.getAllByTestId("link-library-rename-toggle")[0]);
    const input = screen.getByTestId("link-library-rename-input");
    await user.clear(input);
    await user.type(input, "Saturday morning");
    await user.click(screen.getByTestId("link-library-rename-save"));

    expect(await screen.findByTestId("link-library-rename-error")).toHaveTextContent(
      /provided by another server/i,
    );
    // Still open, still holding the typed name: a refusal is worth another try.
    expect(screen.getByTestId("link-library-rename-input")).toHaveValue(
      "Saturday morning",
    );
  });

  it("Cancel puts the original name back and sends nothing", async () => {
    listLinks.mockResolvedValue([link()]);
    const user = userEvent.setup();
    renderWithAuth(<AdminLinkedServersScreen />);

    await screen.findByTestId("link-row");
    await user.click(screen.getAllByTestId("link-library-rename-toggle")[0]);
    const input = screen.getByTestId("link-library-rename-input");
    await user.clear(input);
    await user.type(input, "Nonsense");
    await user.click(screen.getByTestId("link-library-rename-cancel"));

    expect(screen.queryByTestId("link-library-rename-input")).toBeNull();
    expect(screen.getByTestId("link-libraries")).toHaveTextContent("Cartoons");
    expect(updateLibrary).not.toHaveBeenCalled();
  });
});

// --- The feature gate -------------------------------------------------------

describe("AdminLinkedServersScreen — the tab", () => {
  it("appears under /admin and mounts the screen when linkedLibraries is on", async () => {
    listLinks.mockResolvedValue([]);
    renderWithAuth(
      <Routes>
        <Route path="/admin/*" element={<AdminScreen />} />
      </Routes>,
      { initialEntries: ["/admin/linked-servers"], features: { linkedLibraries: true } },
    );

    expect(screen.getByTestId("admin-tab-linked-servers")).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByTestId("admin-linked-servers")).toBeInTheDocument(),
    );
    expect(listLinks).toHaveBeenCalled();
  });

  it("is absent — tab, route AND fetch — on a server without the feature", async () => {
    listLinks.mockResolvedValue([]);
    renderWithAuth(
      <Routes>
        <Route path="/admin/*" element={<AdminScreen />} />
      </Routes>,
      { initialEntries: ["/admin/linked-servers"], features: { linkedLibraries: false } },
    );

    await waitFor(() => expect(screen.getByTestId("admin-tabs")).toBeInTheDocument());
    expect(screen.queryByTestId("admin-tab-linked-servers")).toBeNull();
    expect(screen.queryByTestId("admin-linked-servers")).toBeNull();
    // A build without the feature should look like a build that never had it.
    expect(listLinks).not.toHaveBeenCalled();
  });

  it("an ABSENT linkedLibraries flag behaves exactly like false", async () => {
    listLinks.mockResolvedValue([]);
    renderWithAuth(
      <Routes>
        <Route path="/admin/*" element={<AdminScreen />} />
      </Routes>,
      { initialEntries: ["/admin/linked-servers"] }, // no linkedLibraries key at all
    );

    await waitFor(() => expect(screen.getByTestId("admin-tabs")).toBeInTheDocument());
    expect(screen.queryByTestId("admin-tab-linked-servers")).toBeNull();
    expect(listLinks).not.toHaveBeenCalled();
  });
});
