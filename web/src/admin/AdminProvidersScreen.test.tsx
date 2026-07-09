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
// exactly as AdminUsersScreen.test.tsx fakes apiClient): the screen lists
// providers grouped by kind; the masked key field never shows a stored key;
// toggles/keys/language submit the right PARTIAL payload; a validation refusal
// shows a readable inline error; the Test-connection button reflects ok/error;
// and a pending save disables the button. A separate block exercises the tab in
// the Admin hub and the RequireAdmin gate.

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
  it("renders providers grouped by kind", async () => {
    getMetadataProviders.mockResolvedValue(view());
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });

    await waitFor(() =>
      expect(screen.getByTestId("provider-group-video")).toBeInTheDocument(),
    );
    const videoGroup = screen.getByTestId("provider-group-video");
    const musicGroup = screen.getByTestId("provider-group-music");
    // Video group holds only TMDB; the four music sources are in the music group.
    expect(within(videoGroup).getAllByTestId("provider-row")).toHaveLength(1);
    expect(within(musicGroup).getAllByTestId("provider-row")).toHaveLength(4);
    expect(within(videoGroup).getByTestId("provider-name-tmdb")).toHaveTextContent(
      "The Movie Database (TMDB)",
    );
    expect(within(videoGroup).getByTestId("provider-role-tmdb")).toHaveTextContent(
      /authoritative/i,
    );
  });

  it("masks the key field and never shows a stored key", async () => {
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
    const user = userEvent.setup();
    await user.click(screen.getByTestId("provider-key-reveal-tmdb"));
    expect(keyInput).toHaveAttribute("type", "text");
  });

  it("submits changed toggles, keys, and language as a partial update", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view());
    updateMetadataProviders.mockResolvedValue(
      view({
        providers: [prov({ slug: "tmdb", hasKey: true })].concat(
          view().providers.slice(1).map((p) => (p.slug === "musicbrainz" ? { ...p, enabled: true } : p)),
        ),
        metadataLanguage: "fr-FR",
        enablement: { video: false, music: true },
      }),
    );

    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-group-video");

    // Type a TMDB key, enable MusicBrainz, change the language.
    await user.type(screen.getByTestId("provider-key-input-tmdb"), "new-key");
    await user.click(screen.getByTestId("provider-toggle-musicbrainz"));
    const lang = screen.getByTestId("metadata-language-input");
    await user.clear(lang);
    await user.type(lang, "fr-FR");
    await user.click(screen.getByTestId("save-providers-button"));

    await waitFor(() =>
      expect(updateMetadataProviders).toHaveBeenCalledWith({
        providers: [
          { slug: "tmdb", apiKey: "new-key" },
          { slug: "musicbrainz", enabled: true },
        ],
        metadataLanguage: "fr-FR",
      }),
    );
    // Success affordance shows.
    expect(await screen.findByTestId("save-providers-success")).toBeInTheDocument();
  });

  it("shows the image-host override only for a source that has one, and submits it", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view());
    updateMetadataProviders.mockResolvedValue(view());

    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-group-video");

    // TMDB has a distinct image host → the extra override renders; MusicBrainz
    // (no image host) does not.
    const imageInput = screen.getByTestId("provider-imagebaseurl-tmdb");
    expect(imageInput).toBeInTheDocument();
    expect(screen.queryByTestId("provider-imagebaseurl-musicbrainz")).toBeNull();

    await user.clear(imageInput);
    await user.type(imageInput, "https://img.mirror.test/p");
    await user.click(screen.getByTestId("save-providers-button"));

    await waitFor(() =>
      expect(updateMetadataProviders).toHaveBeenCalledWith({
        providers: [{ slug: "tmdb", imageBaseURL: "https://img.mirror.test/p" }],
      }),
    );
  });

  it("renders the enrichment-behavior controls reflecting loaded values", async () => {
    getMetadataProviders.mockResolvedValue(
      view({ autoEnrichAfterScan: false, enrichIntervalSeconds: 3600, musicBrainzRateLimitMs: 250 }),
    );
    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-behavior");

    const auto = screen.getByTestId("auto-enrich-after-scan-input") as HTMLInputElement;
    const interval = screen.getByTestId("enrich-interval-minutes-input") as HTMLInputElement;
    const rate = screen.getByTestId("musicbrainz-rate-limit-input") as HTMLInputElement;
    expect(auto.checked).toBe(false);
    expect(interval.value).toBe("60"); // 3600s → 60 minutes
    expect(rate.value).toBe("250");
  });

  it("submits changed enrichment-behavior knobs (minutes → seconds) as a partial update", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view()); // auto on, 360min, 1000ms
    updateMetadataProviders.mockResolvedValue(view());

    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-behavior");

    // Toggle auto off, set the sweep to 30 minutes, and clear the throttle to 0.
    await user.click(screen.getByTestId("auto-enrich-after-scan-input"));
    const interval = screen.getByTestId("enrich-interval-minutes-input");
    await user.clear(interval);
    await user.type(interval, "30");
    const rate = screen.getByTestId("musicbrainz-rate-limit-input");
    await user.clear(rate);
    await user.type(rate, "0");
    await user.click(screen.getByTestId("save-providers-button"));

    await waitFor(() =>
      expect(updateMetadataProviders).toHaveBeenCalledWith({
        autoEnrichAfterScan: false,
        enrichIntervalSeconds: 1800, // 30 minutes → 1800 seconds
        musicBrainzRateLimitMs: 0,
      }),
    );
  });

  it("shows a readable inline error when the save is rejected (validation)", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view());
    updateMetadataProviders.mockRejectedValue(
      new ApiError(422, "PROVIDER_KEY_REQUIRED", "TMDB requires an API key to be enabled"),
    );

    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-group-video");

    // Enable TMDB with no key → the server rejects it.
    await user.click(screen.getByTestId("provider-toggle-tmdb"));
    await user.click(screen.getByTestId("save-providers-button"));

    const err = await screen.findByTestId("save-providers-error");
    expect(err).toHaveTextContent(/requires an api key/i);
    // The form is still mounted (no crash) and the toggle state is preserved.
    expect(screen.getByTestId("provider-toggle-tmdb")).toBeChecked();
  });

  it("disables the save button while the save is in flight", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view());
    const pending = deferred<MetadataProvidersView>();
    updateMetadataProviders.mockReturnValue(pending.promise);

    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-group-video");

    await user.click(screen.getByTestId("provider-toggle-musicbrainz"));
    await user.click(screen.getByTestId("save-providers-button"));

    await waitFor(() =>
      expect(screen.getByTestId("save-providers-button")).toBeDisabled(),
    );
    expect(screen.getByTestId("save-providers-button")).toHaveTextContent(/saving/i);

    pending.resolve(view({ enablement: { video: false, music: true } }));
    await waitFor(() =>
      expect(screen.getByTestId("save-providers-button")).toBeEnabled(),
    );
  });

  it("reports a Test-connection result (ok and error)", async () => {
    const user = userEvent.setup();
    getMetadataProviders.mockResolvedValue(view());

    renderWithAuth(<AdminProvidersScreen />, { initialEntries: ["/admin/providers"] });
    await screen.findByTestId("provider-group-video");

    // A successful probe for MusicBrainz (keyless; sends no creds).
    testMetadataProvider.mockResolvedValueOnce({ ok: true, detail: "connection succeeded" });
    await user.click(screen.getByTestId("provider-test-musicbrainz"));
    const okStatus = await screen.findByTestId("provider-test-status-musicbrainz");
    expect(okStatus).toHaveAttribute("data-ok", "true");
    expect(okStatus).toHaveTextContent(/succeeded/i);
    expect(testMetadataProvider).toHaveBeenCalledWith("musicbrainz", {});

    // A failing probe for fanart.tv shows the error detail.
    testMetadataProvider.mockResolvedValueOnce({ ok: false, detail: "an API key is required" });
    await user.click(screen.getByTestId("provider-test-fanarttv"));
    const errStatus = await screen.findByTestId("provider-test-status-fanarttv");
    expect(errStatus).toHaveAttribute("data-ok", "false");
    expect(errStatus).toHaveTextContent(/api key is required/i);
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
