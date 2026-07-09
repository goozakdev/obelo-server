# Per-User audio memory: two-level (Title + Show bubble-up), language-keyed

An explicit audio Stream pick is remembered per (User, Title) in Watch state and reapplied on the next play of that Title. Two levels:

- The pick is stored on the **Title** it was made on.
- It also becomes the **Show's** remembered audio, used as the default for Episodes without their own pick — so picking Japanese on S1E1 carries to S1E2, while the one Episode where you picked a commentary keeps commentary without polluting its siblings. Movies use only the Title level.

What is stored is the pick's **meaning, not its position**: the Stream's language plus distinguishing traits (title tag, channel count, commentary disposition). At play time it re-resolves against the current File's Streams — exact-trait match → language match → default-flag fallback — and never errors when nothing matches.

Default-audio resolution order at negotiation: Title's remembered audio → Show's remembered audio → client-sent `preferredAudioLang` → default disposition → first stream.

## Why

Session-only selection (the subtitle posture) was rejected for audio: the motivating case is a dubbed/original-language series, where re-picking the language every episode is exactly the annoyance the feature exists to remove — and per-Title memory alone doesn't fix it, since the next Episode is a different Title; hence the Show bubble-up.

Language-keyed rather than stream-index-keyed for the same reason watch state is keyed to parsed identity ([ADR-0014](./0014-watch-state-keyed-to-parsed-identity.md)): file internals are not stable. A re-rip or Edition switch shuffles stream indexes; an index key would silently select the wrong audio or dangle.

Subtitles remain session-only for now — a deliberate asymmetry. When persistent subtitle preference lands (deferred by the subtitle PRD), it should adopt this same two-level, meaning-keyed shape.

## Consequences

- Watch state's definition widens (resume, watched, rating, **remembered audio**) — schema and the Watch-state API surface change; the glossary entry is updated.
- The bubble-up needs a Show-level storage slot per User (the first per-(User, Show) state; watch state today is per-Title).
- Re-resolution is best-effort by design: after a file replacement, the closest matching Stream wins and no error is surfaced. A user who cares about an exact commentary track may occasionally re-pick — accepted.
- A remembered pick outranking `preferredAudioLang` means a stale pick can override a changed client preference until the user re-picks — accepted as the price of memory.
