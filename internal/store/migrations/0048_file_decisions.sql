-- What the Admin has SAID about a File inside an already-identified work: where it
-- sits (its Placement), that they deliberately took it off its Slot, or that it is
-- not part of the work at all (ADR-0044).
--
-- Placement — which Slot(s) a File fills — is derived from local information
-- (ADR-0002: the `SxxExx` token, the embedded disc/track tags) and until now had
-- nowhere to be corrected. titles.season_id / season_number / episode_number are
-- rewritten from the parse by writeTitleRow on EVERY upsert, and titles.hidden is
-- reset to 0 there too, so nothing on the Title row can carry a decision across a
-- scan. The real failure: a provider counts the last five episodes of a Show's
-- season 3 as season 4, or as season 1 of a re-numbered continuation series. Each
-- of those five files becomes its own queue row offering to fix the SERIES, which
-- was never the part that was wrong. There is also no way at all to say "these two
-- files are one episode", "this one file is two episodes", or "this file is not an
-- episode" unless the filenames already say so.
--
-- So the decision is stored as a file-anchored Match override — (library, absolute
-- path) → a decision — that the Scanner replays at resolve time, exactly as it
-- already replays the folder-anchored match_overrides in resolveShowFolder. It has
-- to be an INPUT TO RESOLUTION rather than a decoration read at render time,
-- because all three cardinalities the Admin needs to express decide how many Title
-- rows exist, and only the resolve step creates, merges and splits Title rows. An
-- override applied only to live rows would be silently undone by the next
-- scheduled scan.
--
-- WHY SPARSE. A row exists only where the decision DIFFERS from what the parse
-- would produce, following ADR-0027's precedent for the per-Library Enrichment
-- policy. Correcting five files in a 65-file Show writes five rows, not
-- sixty-five, and that is not merely a size argument: a filename the Admin later
-- fixes on disk then takes effect instead of being overruled by a stale record, a
-- future improvement to the Scanner's parsing reaches every File the Admin never
-- touched, and only the five paths actually corrected can ever orphan.
--
-- WHY THREE STATES AND NOT TWO — the load-bearing point, and the one a future
-- reader will otherwise try to "simplify" away by deleting the 'unassigned' state.
-- Sparse storage means the ABSENCE of a row is itself an answer: "derive the
-- Placement from the filename". So absence cannot also mean "the Admin took this
-- File off its Slot" — those are opposite instructions. A File dragged off `S03E64`
-- and left in the unassigned column, recorded by having no row, would be re-placed
-- on S03E64 by the very next scan, from the same filename the Admin was overruling.
-- Hence one record with three states:
--
--   * 'placed'     — the Admin said which Slot(s) this File fills. One row per
--                    Slot (see below). Overrules the parse.
--   * 'unassigned' — the Admin deliberately took it off its Slot. UNDECIDED: the
--                    File is listed in the matcher, belongs to no Title, is
--                    invisible in browse, and keeps its Show in the Needs-Fixing
--                    queue until it is placed or ignored.
--   * 'ignored'    — a sample, a stray rip, anything that is no Slot's File.
--                    SETTLED: skipped by every future scan and absent from the
--                    queue, which is how an unassigned File stops being work.
--
--   * (no row)     — neither of those. Follow the parse. This is the common case
--                    and the reason the table stays small.
--
-- Ignoring is emphatically NOT titles.hidden, which is the derived "every File is
-- Missing" cache and is recomputed (and reset) on every scan. And nothing here
-- touches disk: no File is ever renamed, moved or deleted, by any state.
--
-- WHY group_number / slot_number RATHER THAN season / episode. The same gesture is
-- the same gesture in a Music library: TV supplies season+episode, Music will
-- supply disc+track, and the shape is identical because a Slot is just "one
-- numbered position within a browsable parent". Naming these columns after TV
-- would force a migration (and a second table, and a second read path) the day the
-- Music adapter lands, for no gain today. Kind-neutral storage is what makes that
-- adapter an adapter rather than a rewrite (ADR-0044); the kind-specific parts —
-- fetching Slots from a provider, replaying an override during resolve — stay in
-- the TV and Music resolve paths, which share little code anyway.
--
-- WHY ONE ROW PER (FILE, SLOT) for the placed state, rather than per file. The
-- cardinalities are the whole point: a File spanning two Slots is two rows sharing
-- a path (co-File sibling Titles), and two Files sharing one Slot are two rows
-- sharing a (group, slot) with distinct ordinals (a multi-part Edition, ordinal
-- deciding which half plays first). Anything narrower — a slot column on the file
-- row — can express neither. The two settled states have no Slot, so they are
-- exactly one row per File.
CREATE TABLE IF NOT EXISTS file_decisions (
    id            TEXT PRIMARY KEY,
    library_id    TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    -- path is the absolute on-disk file the decision anchors to. The anchor is the
    -- FILE, not the folder match_overrides keys on: the work is already identified
    -- and it is the arrangement inside it that is being corrected.
    path          TEXT NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('placed', 'unassigned', 'ignored')),
    -- The assigned Slot's POSITION, always in the local library's own numbering (a
    -- borrowed provider record's numbering would collide with the library's own —
    -- that is the Episode pin's job, deliberately separate, see 0047).
    -- group_number is the season / disc; slot_number is the episode / track. NULL
    -- for the two settled states, which name no Slot; NULL rather than a sentinel
    -- because season 0 is a real value (Specials), so 0 cannot mean "no Slot".
    group_number  INTEGER,
    slot_number   INTEGER,
    -- Part order within a shared Slot, 1-based. Meaningful only when several Files
    -- are placed on one Slot, where it decides Edition.Files order and so the joint
    -- playback timeline; 1 for the ordinary one-File-per-Slot case and ignored for
    -- the settled states.
    ordinal       INTEGER NOT NULL DEFAULT 1,
    -- orphaned=1 once a scan finds no file at path (renamed, moved or deleted). A
    -- Placement pointing at nothing is broken rather than done, so it is surfaced
    -- in the Needs-Fixing queue, never silently dropped — the same posture
    -- match_overrides.orphaned already has for folder anchors. Only 'placed' rows
    -- are orphaned: a settled decision about a File that has gone is not a broken
    -- correction, and re-surfacing it would be pure noise (and would un-settle an
    -- ignore that correctly re-applies if the File ever comes back).
    orphaned      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    -- A Slot belongs to the placed state and only to it, so the two settled states
    -- cannot smuggle in half a Placement that later code would read as real.
    CHECK ((state =  'placed' AND group_number IS NOT NULL AND slot_number IS NOT NULL)
        OR (state <> 'placed' AND group_number IS     NULL AND slot_number IS     NULL)),
    -- One decision per (file, Slot). Re-placing the same File on the same Slot
    -- updates that row rather than adding a second; placing it on a second Slot, or
    -- a second File on the same Slot, is a genuinely different row and is allowed.
    UNIQUE (library_id, path, group_number, slot_number)
);

-- SQLite treats NULLs as distinct in a UNIQUE constraint, so the constraint above
-- says nothing at all about the settled states (their group/slot are NULL, so two
-- 'ignored' rows for one path would both be accepted). This partial index is what
-- actually pins "at most one settled decision per File".
CREATE UNIQUE INDEX IF NOT EXISTS idx_file_decisions_settled
    ON file_decisions(library_id, path) WHERE state <> 'placed';

-- ...and neither index can express the other half of the invariant, which is a
-- statement ACROSS rows: a File is either placed or settled, never both. A path
-- holding a 'placed' row and an 'ignored' row at once is a contradiction with no
-- defensible reading — resolve would have to decide whether to build a Title from a
-- File it was also told to skip — so it is refused at the schema, not merely by the
-- store's write path. This is the first trigger in the schema; it earns that
-- because the invariant is cross-row and the alternative (trusting one Go function
-- to be the only writer, forever) is the kind of promise that quietly stops being
-- true. Written as two triggers because SQLite has no multi-event trigger.
CREATE TRIGGER IF NOT EXISTS file_decisions_one_kind_per_path_insert
BEFORE INSERT ON file_decisions
WHEN EXISTS (
    SELECT 1 FROM file_decisions d
     WHERE d.library_id = NEW.library_id AND d.path = NEW.path
       AND (d.state = 'placed') <> (NEW.state = 'placed')
)
BEGIN
    SELECT RAISE(ABORT, 'file_decisions: a File is either placed or settled, never both');
END;

CREATE TRIGGER IF NOT EXISTS file_decisions_one_kind_per_path_update
BEFORE UPDATE OF state, path, library_id ON file_decisions
WHEN EXISTS (
    SELECT 1 FROM file_decisions d
     WHERE d.library_id = NEW.library_id AND d.path = NEW.path AND d.id <> NEW.id
       AND (d.state = 'placed') <> (NEW.state = 'placed')
)
BEGIN
    SELECT RAISE(ABORT, 'file_decisions: a File is either placed or settled, never both');
END;

-- No further indexes on purpose. The read the Scanner performs is a per-Library
-- whole-table load (FileDecisionsByLibrary) and the write path deletes by
-- (library_id, path) before re-inserting a Show's set — both served by the leftmost
-- columns of the UNIQUE constraint above. A separate library_id index (as
-- match_overrides carries) would be redundant with it and would only cost writes.
