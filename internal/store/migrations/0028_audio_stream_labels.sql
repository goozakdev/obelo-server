-- 0028_audio_stream_labels: the read-path foundation for selectable audio
-- Streams (.scratch/audio-streams/, ADR-0022). Per CONTEXT.md the embedded audio
-- Stream is itself the selectable unit — there is no coined "Audio track" — so the
-- menu label and later slices' Remembered-audio trait matching read straight off
-- the Stream row. Embedded audio Streams keep living in `streams` (FFmpeg-pure,
-- like subtitle Streams); they only lacked the label + disposition columns the
-- Audio menu needs. ffprobe already exposes all three; the scanner just dropped
-- them.
--
-- Constant-default columns, so existing stream rows backfill cleanly (no title,
-- not a commentary, not hearing-impaired) and a rescan captures the real values
-- with no manual migration step — an already-scanned library lights the feature
-- up on its next scan. No table rebuild; watch state and Remembered audio are in
-- other tables and are untouched.

-- title: the stream's embedded title tag (ffprobe tags.title), e.g.
-- "Director's Commentary" on an audio Stream. '' when untagged.
ALTER TABLE streams ADD COLUMN title TEXT NOT NULL DEFAULT '';

-- commentary / hearing_impaired: the ffprobe "comment" / "hearing_impaired"
-- dispositions, so the menu can label a commentary or SDH mix even when the file
-- carried no title tag.
ALTER TABLE streams ADD COLUMN commentary INTEGER NOT NULL DEFAULT 0;
ALTER TABLE streams ADD COLUMN hearing_impaired INTEGER NOT NULL DEFAULT 0;
