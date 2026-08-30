import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { API_PREFIX, apiClient } from "../api/client";
import type {
  MatcherDocument,
  MatcherGroup,
  MatcherSlot,
  SlotsUnavailableReason,
} from "../api/types";
import Poster from "../browse/Poster";
import { useAsync } from "../browse/useAsync";
import EpisodeChooser, { type EpisodeChooserLoad } from "./EpisodeChooser";
import FileMatcher, { type RepointRequest } from "./FileMatcher";
import FixItemPicker from "./FixItemPicker";
import type { MatcherLabels } from "./matcherLabels";

// The TV adapter for the file matcher: the route, the fetches, and the WORDS.
//
// `FileMatcher` knows only groups, slots, files and ordinals — the same vocabulary
// the API puts on the wire. Everything on this screen that says "season" or
// "episode" is here, which is what makes the Album matcher an adapter
// (`albumMatcherLabels` + `/admin/albums/:id/matcher`) rather than a second screen
// (ADR-0044, PRD non-goals).
//
// It also supersedes the per-episode pin picker's ROUTE — fixing a misnumbered run
// used to mean visiting five queue rows, each offering to fix the SERIES, which was
// never the part that was wrong — while reusing the picker itself. Repointing a
// Slot's record is search-a-series-then-pick-an-episode exactly as FixItemPicker
// and EpisodeChooser already do it; all this screen supplies is where the episodes
// come from, which is why EpisodeChooser takes `load` rather than a Title.

const pad = (n: number) => String(Math.abs(n)).padStart(2, "0");

/** The TV wording. The one place this whole feature says "season" or "episode". */
export const tvMatcherLabels: MatcherLabels = {
  groupName: (group) =>
    group === 0 ? "Specials" : group < 0 ? "Unsorted" : `Season ${group}`,
  slotCode: (group, slot) => `S${pad(group)}E${pad(slot)}`,
  slotNoun: "episode",
  slotNounPlural: "episodes",
  groupNoun: "season",
  seriesNoun: "series",
  unsortedName: "Unsorted",
  unsortedHint:
    "Files in no season folder, and files the scanner could not number. They stay pinned here so you can drag one into whichever season you have open.",
  unavailableSentence: (reason) => UNAVAILABLE_SENTENCES[reason],
};

// Four reasons, four different things to go and fix. "No titles available" would
// name none of them, which is exactly why the field is an enum and not a boolean.
const UNAVAILABLE_SENTENCES: Record<SlotsUnavailableReason, string> = {
  "no-series-match":
    "This show has never matched a provider record, so there are no episode titles to compare against. Fix the show's match first — renumbering works without it.",
  "enrichment-disabled":
    "Enrichment is switched off for this library, so no episode titles were fetched. Renumbering works without them; turn Enrichment on in the library's settings to see titles here.",
  "provider-cannot-list":
    "This library's metadata provider cannot list episodes — only TMDB can — so the slots below are bare numbers. Renumbering still works.",
  "provider-unreachable":
    "The metadata provider could not be reached, so episode titles are missing. Everything below is complete and still applies; reload later to see the titles.",
};

export default function ShowMatcherScreen() {
  const { showId = "" } = useParams();
  const navigate = useNavigate();

  // The cheap first load: complete for everything LOCAL, at most one provider
  // call. A season's records arrive when the Admin expands it.
  const initial = useAsync<MatcherDocument>(
    (signal) => apiClient.getShowMatcher(showId, undefined, signal),
    [showId],
  );
  const [doc, setDoc] = useState<MatcherDocument | null>(null);
  useEffect(() => {
    if (initial.status === "ready") setDoc(initial.data);
  }, [initial]);

  const loadGroup = useCallback(
    async (group: number): Promise<MatcherGroup | null> => {
      const next = await apiClient.getShowMatcher(showId, group);
      return next.groups.find((g) => g.number === group) ?? null;
    },
    [showId],
  );

  const apply = useCallback(
    (input: Parameters<typeof apiClient.applyShowMatcher>[1]) =>
      apiClient.applyShowMatcher(showId, input),
    [showId],
  );

  if (initial.status === "loading" && !doc) {
    return (
      <p className="status status-loading" data-testid="show-matcher-loading">
        Loading this show&rsquo;s files&hellip;
      </p>
    );
  }
  if (initial.status === "error" && !doc) {
    return (
      <p className="status status-error" data-testid="show-matcher-error" role="alert">
        <span className="dot dot-error" aria-hidden="true" />
        {initial.message}
      </p>
    );
  }
  if (!doc) return null;

  return (
    <FileMatcher
      matcher={doc}
      labels={tvMatcherLabels}
      loadGroup={loadGroup}
      apply={apply}
      repointRecord={(request) => (
        <SeriesRecordPicker showId={showId} seed={doc.title} request={request} />
      )}
      onApplied={setDoc}
      onClose={() => navigate(`/shows/${encodeURIComponent(showId)}`)}
      header={
        <header className="matcher-header" data-testid="show-matcher-header">
          <Poster
            titleId={doc.containerId}
            title={doc.title}
            src={`${API_PREFIX}/shows/${encodeURIComponent(doc.containerId)}/artwork/poster`}
          />
          <div className="matcher-header-body">
            <h2 className="matcher-title" data-testid="show-matcher-title">
              {doc.title}
              {doc.year ? ` (${doc.year})` : ""}
            </h2>
            <p className="matcher-header-series" data-testid="show-matcher-series">
              {doc.seriesExternalId
                ? `Matched to series ${doc.seriesExternalId}`
                : "Not matched to any series"}
            </p>
            {/* The escape hatch: if the SHOW is wrong, no amount of sorting its
                files will help. The existing picker lives on the show page. */}
            <Link
              className="nav-link"
              to={`/shows/${encodeURIComponent(doc.containerId)}`}
              data-testid="show-matcher-wrong-series"
            >
              Wrong series? Fix the show&rsquo;s match
            </Link>
            <p className="matcher-hint">
              Nothing on disk is renamed, moved or deleted — this only changes how
              Obelo files what is already there.
            </p>
          </div>
        </header>
      }
    />
  );
}

// --- The record picker ------------------------------------------------------

/** Which season the chooser opens on: the group being repointed when the series
 * has it, otherwise its first real season (a numbered one over Specials, which is
 * rarely what anyone is looking for).
 *
 * The fallback is the whole point rather than a nicety. An Admin reaches this
 * because the disk and the provider disagree — the records they want live in a
 * re-numbered continuation with no such season at all, which is exactly the Batman
 * → New Batman Adventures shape. Opening on an empty list would read as "this
 * series has no episodes". It mirrors the server's own defaultSeason. */
export function defaultGroup(groups: readonly { number: number }[], want: number): number {
  let first = -1;
  for (const g of groups) {
    if (g.number === want) return want;
    if (g.number > 0 && (first < 0 || g.number < first)) first = g.number;
  }
  if (first >= 0) return first;
  return groups.length > 0 ? groups[0].number : want;
}

/** Repoint a Slot's record: search a series, then pick the record within it.
 *
 * The two steps are the components that already exist. What is new is only the
 * ANCHOR: the queue used to hang the episode list off a Title, and this hangs it
 * off a Show plus a chosen series, which is what `listSeriesSlots` answers and what
 * `EpisodeChooser`'s `load` seam was left open for.
 *
 * Picking a record hands back the run STARTING there, so one gesture fills a whole
 * group: the Admin picks The New Batman Adventures season 1 episode 1 once and the
 * five Slots take records 1..5 in order. Their own numbering is untouched — the
 * records travel with their own positions and stay in them. */
function SeriesRecordPicker({
  showId,
  seed,
  request,
}: {
  showId: string;
  /** What to search on open — the Show's own name, because the record living in
   * the SAME series one season along is the common case, not the exotic one. */
  seed: string;
  request: RepointRequest;
}) {
  // The season list last loaded, kept so picking a record can hand back the run
  // that FOLLOWS it. EpisodeChooser hands over one candidate; the bulk gesture
  // needs the rest of the season behind it.
  const page = useRef<MatcherSlot[]>([]);
  // One stable loader per series. EpisodeChooser refetches whenever `load` changes
  // identity, so a fresh closure per render would loop.
  const loaders = useRef(new Map<string, EpisodeChooserLoad>());

  const loaderFor = useCallback(
    (externalId: string): EpisodeChooserLoad => {
      const cached = loaders.current.get(externalId);
      if (cached) return cached;
      const load: EpisodeChooserLoad = async (which) => {
        const listed = await apiClient.listSeriesSlots(showId, externalId, which);
        const seasons = listed.groups.map((g) => ({
          season: g.number,
          episodeCount: g.slotCount,
        }));
        let group = which;
        let slots = listed.slots;
        if (group === undefined) {
          group = defaultGroup(listed.groups, request.group);
          slots = (await apiClient.listSeriesSlots(showId, externalId, group)).slots;
        }
        page.current = slots;
        return {
          seasons,
          season: group,
          episodes: slots.map((s) => ({
            season: s.group,
            episode: s.slot,
            name: s.name ?? "",
            overview: s.overview,
            airDate: s.airDate,
            stillUrl: s.stillUrl,
          })),
        };
      };
      loaders.current.set(externalId, load);
      return load;
    },
    [showId, request.group],
  );

  // Opening on the series a Slot already borrows from turns "use a different
  // record" into a one-step correction; otherwise the Show's own name, because the
  // record living one season along in the SAME series is the common case.
  const openOn = request.current?.externalId ?? seed;

  return (
    <FixItemPicker
      seed={openOn}
      provider="tmdb"
      applyLabel="Use this series"
      applyHint="Then choose which episode's details these slots should show."
      search={(query, pageNumber) =>
        apiClient.searchEntityEnrichmentCandidates("shows", showId, query, { page: pageNumber })
      }
      preview={(ref) => apiClient.previewEntityExternalCandidate("shows", showId, ref)}
      onApply={async () => {
        /* Unreachable: chooseEpisode is always supplied, so the series is never
           applied on its own — picking it only advances to the record list. */
      }}
      onCancel={request.onCancel}
      chooseEpisode={(series, back) => (
        <EpisodeChooser
          seriesTitle={series.title}
          load={loaderFor(series.externalId)}
          onBack={back}
          onPick={async (candidate) => {
            const from = page.current.findIndex(
              (s) => s.group === candidate.season && s.slot === candidate.episode,
            );
            const run =
              from >= 0
                ? page.current.slice(from)
                : [
                    {
                      group: candidate.season,
                      slot: candidate.episode,
                      name: candidate.name,
                      overview: candidate.overview,
                      airDate: candidate.airDate,
                      stillUrl: candidate.stillUrl,
                    },
                  ];
            request.onPicked(series.externalId, run);
          }}
        />
      )}
    />
  );
}
