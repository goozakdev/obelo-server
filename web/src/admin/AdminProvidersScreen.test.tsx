import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import { AuthProvider } from "../auth/session";
import { RequireAdmin } from "../auth/guards";
import { ApiError } from "../api/errors";
import type { ApiClient } from "../api/client";
import type { MetadataProvider, MetadataProvidersView } from "../api/types";

// AdminProvidersScreen end-to-end through the faked API client (the one seam —
// exactly as AdminUsersScreen.test.tsx fakes apiClient). The redesigned screen
// lists providers grouped by kind with the authoritative one labeled; each row's
// Enabled checkbox persists IMMEDIATELY (save-per-action); an Edit icon opens a
// per-provider config dialog whose masked key field never shows a stored key and
// whose Save submits a single-provider partial payload; a refused enable shows a
// readable inline row error; the Test-connection button (in the dialog) reflects
// ok/error; and the server-wide language/behavior knobs keep their own Save. A
// separate block exercises the tab in the Admin hub and the RequireAdmin gate.

const { getMetadataProviders, updateMetadataProviders, testMetadataProvider } =
  vi.hoisted(() => ({
    getMetadataProviders: vi.fn(),
    updateMetadataProviders: vi.fn(),
    testMetadataProvider: vi.fn(),
  }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getMetadataProviders: (...a: unknown[]) => getMetadataProviders(...a),
      updateMetadataProviders: (...a: unknown[]) => updateMetadataProviders(...a),
      testMetadataProvider: (...a: unknown[]) => testMetadataProvider(...a),
      // The embedded EnrichmentConsentControl (ADR-0032) fetches on mount; stub it
      // to a settled state so this screen's tests don't exercise the consent flow.
      getEnrichmentConsent: () => Promise.resolve({ state: "granted" }),
      setEnrichmentConsent: () => Promise.resolve({ state: "granted" }),
    },
  };
});

import AdminProvidersScreen from "./AdminProvidersScreen";
import AdminScreen from "../screens/AdminScreen";

function prov(over: Partial<MetadataProvider>): MetadataProvider {
  return {
    slug: "tmdb",
    name: "The Movie Database (TMDB)",
    kinds: ["video"],
    role: "authoritative",
    requiresKey: true,
    enabled: false,
    hasKey: false,
    baseURL: "https://api.themoviedb.org/3",
    description: "Movies and TV.",
    docsURL: "https://example.test/key",
    ...over,
  };
}

function view(over: Partial<MetadataProvidersView> = {}): MetadataProvidersView {
  return {
    providers: [
      prov({ slug: "tmdb", imageBaseURL: "https://image.tmdb.org/t/p/original" }),
      prov({
        slug: "musicbrainz",
        name: "MusicBrainz",
        kinds: ["music"],
        requiresKey: false,
        baseURL: "https://musicbrainz.org/ws/2",
      }),
      prov({
        slug: "coverart",
        name: "Cover Art Archive",
        kinds: ["music"],
        role: "supplement",
        requiresKey: false,
        baseURL: "https://coverartarchive.org",
      }),
      prov({
        slug: "fanarttv",
        name: "fanart.tv",
        kinds: ["music"],
        role: "supplement",
        baseURL: "https://webservice.fanart.tv/v3",
      }),
      prov({
        slug: "theaudiodb",
        name: "TheAudioDB",
        kinds: ["music"],
        role: "supplement",
        baseURL: "https://www.theaudiodb.com/api/v1/json",
      }),
    ],
    metadataLanguage: "en-US",
    enablement: { video: false, music: false },
    autoEnrichAfterScan: true,
    enrichIntervalSeconds: 21600, // 360 minutes
    musicBrainzRateLimitMs: 1000,
    ...over,
  };
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

beforeEach(() => {
  getMetadataProviders.mockReset();
  updateMetadataProviders.mockReset();
  testMetadataProvider.mockReset();
});

describe("AdminProvidersScreen", () => {
  it("renders providers grouped by kind with the authoritative one labeled and on top", async () => {
    getMetadataProviders.mockResolvedValue(view());
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });

    await waitFor(() =>
      expect(screen.getByTestId("provider-group-video")).toBeInTheDocument(),
    );
    const videoGroup = screen.getByTestId("provider-group-video");
    const musicGroup = screen.getByTestId("provider-group-music");
    // Video group holds only TMDB; the four music sources are in the music group.
    expect(within(videoGroup).getAllByTestId("provider-row")).toHaveLength(1);
    const musicRows = within(musicGroup).getAllByTestId("provider-row");
    expect(musicRows).toHaveLength(4);
    // The authoritative source (MusicBrainz) is the first music row and is labeled;
    // the supplements carry no role badge.
    expect(musicRows[0]).toHaveAttribute("data-slug", "musicbrainz");
    expect(within(musicRows[0]).getByTestId("provider-role-musicbrainz")).toHaveTextContent(
      /authoritative/i,
    );
    expect(within(videoGroup).getByTestId("provider-role-tmdb")).toHaveTextContent(
      /authoritative/i,
    );
    expect(screen.queryByTestId("provider-role-coverart")).toBeNull();
  });

  it("enabling a provider's checkbox persists immediately (save-per-action)", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view());
    updateMetadataProviders.mockResolvedValue(
      view({ enablement: { video: false, music: true } }),
    );
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-group-video");

    await user.click(screen.getByTestId("provider-toggle-musicbrainz"));

    await waitFor(() =>
      expect(updateMetadataProviders).toHaveBeenCalledWith({
        providers: [{ slug: "musicbrainz", enabled: true }],
      }),
    );
  });

  it("shows an inline row error and leaves the box unchecked when an enable is refused", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view());
    updateMetadataProviders.mockRejectedValue(
      new ApiError(422, "PROVIDER_KEY_REQUIRED", "TMDB requires an API key to be enabled"),
    );
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-group-video");

    // Enable TMDB with no key → the server rejects it.
    await user.click(screen.getByTestId("provider-toggle-tmdb"));

    const err = await screen.findByTestId("provider-row-error-tmdb");
    expect(err).toHaveTextContent(/requires an api key/i);
    // The view never changed, so the checkbox reflects the still-disabled state.
    expect(screen.getByTestId("provider-toggle-tmdb")).not.toBeChecked();
  });

  it("opens the config dialog and never shows a stored key", async () => {
    const user = userEvent.setup();
    // TMDB already has a key on file (hasKey), but the value is never sent to the
    // client — the field starts empty and masked, with a "configured" indicator.
    getMetadataProviders.mockResolvedValue(
      view({
        providers: [prov({ slug: "tmdb", enabled: true, hasKey: true })].concat(
          view().providers.slice(1),
        ),
        enablement: { video: true, music: false },
      }),
    );
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-group-video");

    await user.click(screen.getByTestId("provider-edit-tmdb"));

    const keyInput = (await screen.findByTestId(
      "provider-key-input-tmdb",
    )) as HTMLInputElement;
    expect(keyInput).toHaveAttribute("type", "password");
    expect(keyInput.value).toBe(""); // the stored key is never populated
    expect(screen.getByTestId("provider-key-status-tmdb")).toHaveAttribute(
      "data-configured",
      "true",
    );

    // The reveal toggle flips the field to text (for shoulder-surf-safe pasting).
    await user.click(screen.getByTestId("provider-key-reveal-tmdb"));
    expect(keyInput).toHaveAttribute("type", "text");
  });

  it("saves a typed key from the dialog as a single-provider partial update and closes", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view());
    updateMetadataProviders.mockResolvedValue(
      view({
        providers: [prov({ slug: "tmdb", hasKey: true })].concat(view().providers.slice(1)),
      }),
    );
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-group-video");

    await user.click(screen.getByTestId("provider-edit-tmdb"));
    await user.type(await screen.findByTestId("provider-key-input-tmdb"), "new-key");
    await user.click(screen.getByTestId("provider-config-save-tmdb"));

    await waitFor(() =>
      expect(updateMetadataProviders).toHaveBeenCalledWith({
        providers: [{ slug: "tmdb", apiKey: "new-key" }],
      }),
    );
    // The dialog closes after a successful save.
    await waitFor(() =>
      expect(screen.queryByTestId("provider-config-dialog-tmdb")).toBeNull(),
    );
  });

  it("shows the image-host override only for a source that has one, and submits it", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view());
    updateMetadataProviders.mockResolvedValue(view());
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-group-video");

    // TMDB has a distinct image host → the extra override renders in its dialog.
    await user.click(screen.getByTestId("provider-edit-tmdb"));
    const imageInput = await screen.findByTestId("provider-imagebaseurl-tmdb");
    await user.clear(imageInput);
    await user.type(imageInput, "https://img.mirror.test/p");
    await user.click(screen.getByTestId("provider-config-save-tmdb"));

    await waitFor(() =>
      expect(updateMetadataProviders).toHaveBeenCalledWith({
        providers: [{ slug: "tmdb", imageBaseURL: "https://img.mirror.test/p" }],
      }),
    );
  });

  it("omits the image-host override for a source that has none", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view());
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-group-music");

    // MusicBrainz has no distinct image host → no override field in its dialog.
    await user.click(screen.getByTestId("provider-edit-musicbrainz"));
    await screen.findByTestId("provider-baseurl-musicbrainz");
    expect(screen.queryByTestId("provider-imagebaseurl-musicbrainz")).toBeNull();
  });

  it("reports a Test-connection result from the dialog (ok and error)", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view());
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-group-music");

    // A successful probe for MusicBrainz (keyless; sends no creds).
    await user.click(screen.getByTestId("provider-edit-musicbrainz"));
    testMetadataProvider.mockResolvedValueOnce({ ok: true, detail: "connection succeeded" });
    await user.click(await screen.findByTestId("provider-test-musicbrainz"));
    const okStatus = await screen.findByTestId("provider-test-status-musicbrainz");
    expect(okStatus).toHaveAttribute("data-ok", "true");
    expect(okStatus).toHaveTextContent(/succeeded/i);
    expect(testMetadataProvider).toHaveBeenCalledWith("musicbrainz", {});
    await user.click(screen.getByTestId("provider-config-cancel-musicbrainz"));

    // A failing probe for fanart.tv shows the error detail.
    await user.click(screen.getByTestId("provider-edit-fanarttv"));
    testMetadataProvider.mockResolvedValueOnce({ ok: false, detail: "an API key is required" });
    await user.click(await screen.findByTestId("provider-test-fanarttv"));
    const errStatus = await screen.findByTestId("provider-test-status-fanarttv");
    expect(errStatus).toHaveAttribute("data-ok", "false");
    expect(errStatus).toHaveTextContent(/api key is required/i);
  });

  it("renders the enrichment-behavior controls reflecting loaded values", async () => {
    getMetadataProviders.mockResolvedValue(
      view({ autoEnrichAfterScan: false, enrichIntervalSeconds: 3600, musicBrainzRateLimitMs: 250 }),
    );
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-behavior");

    const auto = screen.getByTestId("auto-enrich-after-scan-input") as HTMLInputElement;
    const interval = screen.getByTestId("enrich-interval-minutes-input") as HTMLInputElement;
    expect(auto.checked).toBe(false);
    expect(interval.value).toBe("60"); // 3600s → 60 minutes
    // The MusicBrainz throttle no longer lives on the behavior card — it moved to
    // the MusicBrainz provider dialog.
    expect(screen.queryByTestId("musicbrainz-rate-limit-input")).toBeNull();
  });

  it("submits changed language + behavior knobs (minutes → seconds) via the settings Save", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view()); // en-US, auto on, 360min
    updateMetadataProviders.mockResolvedValue(view());
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-behavior");

    // Change the language, toggle auto off, set the sweep to 30 minutes.
    const lang = screen.getByTestId("metadata-language-input");
    await user.clear(lang);
    await user.type(lang, "fr-FR");
    await user.click(screen.getByTestId("auto-enrich-after-scan-input"));
    const interval = screen.getByTestId("enrich-interval-minutes-input");
    await user.clear(interval);
    await user.type(interval, "30");
    await user.click(screen.getByTestId("save-providers-button"));

    await waitFor(() =>
      expect(updateMetadataProviders).toHaveBeenCalledWith({
        metadataLanguage: "fr-FR",
        autoEnrichAfterScan: false,
        enrichIntervalSeconds: 1800, // 30 minutes → 1800 seconds
      }),
    );
    // Success affordance shows.
    expect(await screen.findByTestId("save-providers-success")).toBeInTheDocument();
  });

  it("edits the MusicBrainz throttle from its dialog and saves it (server-wide knob)", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view()); // musicBrainzRateLimitMs: 1000
    updateMetadataProviders.mockResolvedValue(view({ musicBrainzRateLimitMs: 0 }));
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-group-music");

    await user.click(screen.getByTestId("provider-edit-musicbrainz"));
    const rate = (await screen.findByTestId(
      "musicbrainz-rate-limit-input",
    )) as HTMLInputElement;
    expect(rate.value).toBe("1000"); // seeded from the server-wide value
    await user.clear(rate);
    await user.type(rate, "0");
    await user.click(screen.getByTestId("provider-config-save-musicbrainz"));

    // Only the throttle changed → the payload carries just that server-wide knob.
    await waitFor(() =>
      expect(updateMetadataProviders).toHaveBeenCalledWith({ musicBrainzRateLimitMs: 0 }),
    );
    await waitFor(() =>
      expect(screen.queryByTestId("provider-config-dialog-musicbrainz")).toBeNull(),
    );
  });

  it("shows the throttle field only in the MusicBrainz dialog", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view());
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-group-video");

    // TMDB's dialog has no MusicBrainz throttle.
    await user.click(screen.getByTestId("provider-edit-tmdb"));
    await screen.findByTestId("provider-baseurl-tmdb");
    expect(screen.queryByTestId("musicbrainz-rate-limit-input")).toBeNull();
  });

  it("disables the settings Save button while the save is in flight", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view());
    const pending = deferred<MetadataProvidersView>();
    updateMetadataProviders.mockReturnValue(pending.promise);
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-behavior");

    const lang = screen.getByTestId("metadata-language-input");
    await user.clear(lang);
    await user.type(lang, "de-DE");
    await user.click(screen.getByTestId("save-providers-button"));

    await waitFor(() =>
      expect(screen.getByTestId("save-providers-button")).toBeDisabled(),
    );
    expect(screen.getByTestId("save-providers-button")).toHaveTextContent(/saving/i);

    pending.resolve(view({ metadataLanguage: "de-DE" }));
    await waitFor(() =>
      expect(screen.getByTestId("save-providers-button")).toBeEnabled(),
    );
  });
});

describe("Metadata Providers tab in the Admin hub", () => {
  it("renders the tab beside the existing tabs and mounts the screen", async () => {
    getMetadataProviders.mockResolvedValue(view());
    renderWithAuth(
      <Routes>
        <Route path="/admin/*" element={<AdminScreen />} />
      </Routes>,
      { initialEntries: ["/admin/providers"] },
    );

    expect(screen.getByTestId("admin-tab-providers")).toBeInTheDocument();
    expect(screen.getByTestId("admin-tab-users")).toBeInTheDocument();
    expect(screen.getByTestId("admin-tab-libraries")).toBeInTheDocument();

    await waitFor(() =>
      expect(screen.getByTestId("admin-providers")).toBeInTheDocument(),
    );
    expect(getMetadataProviders).toHaveBeenCalled();
  });

  it("redirects a Member away from /admin/providers (RequireAdmin gate)", async () => {
    window.localStorage.setItem("juicebox.token", "fake-token");
    window.localStorage.setItem(
      "juicebox.user",
      JSON.stringify({ id: "m1", username: "kid", role: "member" }),
    );
    const stub = {
      token: "fake-token",
      setToken: () => {},
      setUnauthorizedHandler: () => {},
      verifySession: () => Promise.resolve({}),
    } as unknown as ApiClient;

    render(
      <MemoryRouter initialEntries={["/admin/providers"]}>
        <AuthProvider client={stub}>
          <Routes>
            <Route path="/" element={<div data-testid="landing" />} />
            <Route
              path="/admin/*"
              element={
                <RequireAdmin>
                  <AdminScreen />
                </RequireAdmin>
              }
            />
          </Routes>
        </AuthProvider>
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByTestId("landing")).toBeInTheDocument());
    expect(screen.queryByTestId("admin-providers")).not.toBeInTheDocument();
    expect(getMetadataProviders).not.toHaveBeenCalled();
  });
});
