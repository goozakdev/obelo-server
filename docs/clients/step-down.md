# Automatic step-down — the normative client spec

The one shared rulebook for how a client walks the **Step-down ladder** ([ADR-0071](../adr/0071-automatic-step-down-is-client-driven-over-a-server-provided-ladder.md)). It binds every Obelo client — Apple TV and Android (both libmpv, Cast on Android, AirPlay on Apple) and the web player (hls.js). Client playbooks link here; they do not restate it.

- The **wire contract** is [`../api-contract.md`](../api-contract.md): the `stepDown` paragraph (§3.6), the `replacesSessionId` paragraph, `features` (`stepDown`, `relayCeiling`) and the relay section §3.11. This spec does not repeat it. If the two disagree, the contract wins.
- The Server decides **what steps exist**. The client decides **when and how far** to step. The Server keeps no ladder state.
- MUST / SHOULD / MAY are used as in RFC 2119.
- Every MUST ends with an HTML comment naming its source (a `DECISIONS` date, a `FINDINGS` heading, or the contract). The comments are provenance, not part of the rule.
- **Every number in §12 that is marked "unmeasured" has not been measured on hardware.** Treat it as a starting value, not a truth.

---

## 1. Scope and gating

1. A client MUST send `replacesSessionId` and MUST read `stepDown` only when `features.stepDown` is `true`. It MUST omit the key (not send it empty) otherwise, because an older server answers `400` to an unknown field. The outcome is what binds: a client MAY read the flag once when it builds the play's policy rather than on every negotiation. <!-- api-contract features.stepDown; FINDINGS 2026-10-22 (DisallowUnknownFields); DECISIONS 2026-10-23 ruling 13 -->
2. A client MUST NOT request a Transcode rung or a Transcode-based step on a host that cannot transcode (`features.transcode` is `false`). Two variants are equivalent and allowed: the client tests `features.transcode` itself, or it trusts the Server to list Transcode rungs only when the host can transcode. <!-- api-contract stepDown paragraph; FINDINGS 2026-10-23 (Apple no-transcode host); DECISIONS 2026-10-23 ruling 13 -->
3. **Flag off** (`features.stepDown` false or absent) keeps the pre-ADR-0071 behaviour:
   - Steps are **Edition-only**, over the client's **own** list of the Title's Editions sorted by bitrate. There are no Transcode steps. <!-- ADR-0071 d1; FINDINGS 2026-10-22 (Apple resource-ceiling parity) -->
   - A down-step (bandwidth, resource, or a 503 fallback) MUST skip any Edition taller than the failing rung's **known** height, and stays put when none is left. When the failing height is unknown the guard does nothing. <!-- FINDINGS 2026-10-22 (flag-off guard; Apple confirmed, Android D25b) -->
   - Resource steps stay enabled with the flag off. <!-- FINDINGS 2026-10-22 -->
   - The re-negotiation order is **rule 15: `DELETE` the old session first, then negotiate.** There is no `replacesSessionId`. "Stay put" is then: re-negotiate the current rung at the live position and show the busy notice; if that also fails, the failure card. The race where another client takes the slot between the `DELETE` and the re-negotiation is accepted. <!-- DECISIONS 2026-10-21 (rule 15, interim stay-put) -->
   - A step keeps the viewer's Quality cap in the request (the rung-replaces rule of §2.4 applies **only** with the flag on). <!-- FINDINGS 2026-10-22 (Apple DECISION 63 spec rule) -->
4. **Flag on** supersedes the older client ABR rules 1, 3, 5 and 17. Rule 15 flips to negotiate-first. The shared notice strings are in §13 (confirmed by the owner); the flag-off Edition pair is unchanged. <!-- DECISIONS 2026-10-21 (user ruling); DECISIONS 2026-10-23 ruling 14 -->
5. **Remote receivers (Cast, AirPlay)** are covered in §11. They get a reduced policy, flag-gated.

## 2. The ladder

1. **Wire order is authoritative.** A client MUST NOT re-sort the ladder by bitrate or anything else: bitrates are not monotonic across codecs, and the order is height descending, the unconstrained rung first within a height, then bitrate descending. <!-- FINDINGS 2026-10-22 (Apple DECISION 57, Android D14); DECISIONS 2026-10-22 (ladder order); api-contract stepDown -->
2. **Position by `current`, never by index.** Exactly one rung is `current: true` on every non-empty ladder. Every adopted Decision MUST re-position the client from its own `stepDown`. <!-- DECISIONS 2026-10-22 (ruling under option a); FINDINGS 2026-10-22 (Apple DECISION 57) -->
   "Down" means a rung after `current`; "up" means the rung directly before it.
3. **Identify a rung by its request tuple** `(editionId, maxResolution, maxBitrate)`, never by `label`. Two rungs with different requests MUST NOT collide on one key. A client MAY key an Edition rung that carries no constraints by its `editionId` alone (an equivalent variant). Labels are display-only: they carry the height and can change between ladders. A derived rung's tuple is stable only while the profile, ceiling and picks are unchanged; a Decode exclusion, a track change or a ceiling change can alter it, and an inserted rung is never stable. <!-- FINDINGS 2026-10-22 (Android fd0fec7 rung keys); FINDINGS 2026-10-22 (Android D22) -->
4. **Requesting a rung replaces; it does not merge.** Send the rung's `editionId` (if any) as `editionId`, and the rung's `constraints` as `maxResolution` / `maxBitrate`, dropping your own. A rung with neither means no `editionId` and no constraints. Only these three fields are replaced: `preferredAudioLang` and `preferredSubtitleLang` survive a step. Explicit audio, video and burn-in subtitle stream picks are **kept** on a step: the Server derives every rung with the request's picks applied, so dropping a pick makes the delivery differ from the listed rung and changes the viewer's audio or subtitle choice. A pick that cannot apply to a rung is already absent from the ladder. Picks are kept **only on wire-ladder steps**: a flag-off Edition switch drops per-File stream ids. **Both clients** MUST keep picks on a wire-ladder step; Android today nulls stream ids on every rung step and must change. <!-- DECISIONS 2026-10-22 (user ruling, replace); FINDINGS 2026-10-22 (language prefs); api-contract stepDown; DECISIONS 2026-10-23 ruling 4; DECISIONS 2026-10-24 (ruling 4 applies to Android too) -->
   - With the flag on, a step sends **no Quality cap**. The client enforces the cap by filtering the ladder (§2.7); the Server re-applies only the Playback ceiling. The **first** negotiation of a play still sends the cap as constraints. <!-- FINDINGS 2026-10-22 (Spec rule for step-down.md) -->
5. **The unconstrained rung is sent as "no cap".** A rung of either kind with no `constraints` is requested with its `editionId` (if it has one) and **no** `maxResolution` / `maxBitrate`. Send `maxBitrate: 0` where the client's wire type needs a value: `0` is the same as an omitted key. An unconstrained rung may also carry constraints when it IS a fixed rung with identical delivery; it is requested either way. <!-- FINDINGS 2026-10-22 (Unconstrained rung wire form; spec rule maxBitrate 0); DECISIONS 2026-10-22 (refuter-14 answers); api-contract stepDown -->
6. **Inserted rungs.** A request that matches no derived rung appears as an inserted `current` rung carrying the request exactly as sent. It exists only in the ladder of the Decision it describes and may vanish after a step. The unconstrained rung is always listed, so the way back up always exists. A client MUST NOT assume an inserted rung survives. <!-- DECISIONS 2026-10-22 (inserted current rung, always-listed unconstrained rung) -->
7. **The Quality cap filter.** The cap is read from the **saved Quality setting**, never from the request (after a transcode step the request holds the lowered rung). A client MUST NOT step above the cap and MUST filter the ladder before choosing an up-target. <!-- FINDINGS 2026-10-22 (Android Quality cap; Apple Decision 57); DECISIONS 2026-10-22 (cap dimensions) -->
   - Filter on **height** and, when the cap has `maxBitrate > 0`, on **bitrate**. The height used MUST be the rung's delivered height. Two sources are equivalent and allowed: the rung's `height`, else `constraints.maxResolution`, else Edition detail; or, for an Edition rung, the height of the Edition itself. <!-- DECISIONS 2026-10-22 (cap dimensions); FINDINGS 2026-10-22 (Apple 749280e `height`); DECISIONS 2026-10-23 ruling 13 -->
   - An **Edition rung at or below the cap's height is exempt from the bitrate check** (it mirrors how a play starts: Direct Play with an over-bitrate Edition). Transcode rungs are filtered by both. <!-- DECISIONS 2026-10-22 (user ruling, Edition exemption) -->
   - A rung whose value is **unknown** for a capped dimension is kept only if it is at or below `current`. <!-- DECISIONS 2026-10-22 (cap dimensions) -->
   - `maxResolution` is a limit, not a delivered height; prefer `height`. <!-- FINDINGS 2026-10-22 (Apple resource-ceiling parity) -->
8. With no cap, the full ladder is used. <!-- FINDINGS 2026-10-22 (Apple Quality-cap parity) -->

## 3. Triggers

There are three triggers and two remedies: bandwidth and resource **step**; codec failure **re-negotiates at the same quality**. <!-- ADR-0071 d2 -->

### 3.1 Bandwidth

1. A rung **fits** when its `bitrate` ≤ 0.75 × measured throughput. A rung of unknown `bitrate` never counts as fitting a jump; the no-fit fallback (rule 2) still reaches it. <!-- FINDINGS 2026-10-22 (Thresholds in parity; Android D14); DECISIONS 2026-10-23 ruling 5 -->
2. A bandwidth down-step goes to the **first rung after `current` that fits**. Because a lower rung can carry a higher bitrate than one above it, the client MUST test each rung and MUST NOT assume the next rung is lighter. **If no later rung fits, or there are no throughput samples, step to the next rung down.** There is no step only at the bottom of the ladder, or when every lower rung fails the flag-off height guard (§1.3). On a remote receiver the step always goes to the next rung. <!-- FINDINGS 2026-10-22 (Apple DECISION 57); DECISIONS 2026-10-22 (height-first stands); DECISIONS 2026-10-23 ruling 7 -->
3. Bandwidth stepping is disabled for a play under a manual pin (§8.4) and is inert on a remote receiver except as §11 allows.

### 3.2 Resource

1. A resource trigger (dropped frames, decoder falling behind) steps **one rung down**. <!-- ADR-0071 d2 -->
2. It records a **resource ceiling** (§5) from the failing rung, and the anchor (§5.4), only when the step **lands**, never at emit. An abandoned, stayed-put or refused step records nothing. <!-- FINDINGS 2026-10-22 (Spec; Android review); DECISIONS 2026-10-23 ruling 2 -->
3. Resource steps override a manual pin. <!-- ADR-0071 d8 -->
4. A resource step is attributed to the resource trigger with a flag set when the action is emitted. The flag MUST be cleared on **every** exit (landed, abandoned, halted, stayed put, manual pick, codec failure). Otherwise a dropped resource action makes the next bandwidth stay-put count against the resource gate. <!-- FINDINGS 2026-10-23 (Apple 9661ea4 trap) -->

### 3.3 Codec

Codec failure is **not a step**. It adds a **Decode exclusion** (the codec, or the codec at a resolution) to this Device's Capability profile and re-negotiates at the **same quality**. <!-- ADR-0071 d2, d4 -->

Evidence (libmpv). A client MUST record an exclusion only on one of:

1. **Trigger 1:** the `vd` log line `Failed to initialize a decoder for codec` for the **current load**, AND the `end-file` reason `no audio or video data played`, AND an expected video track with a known codec. There is no `file-loaded` gate, because a video-only file with a decoder failure never fires it (measured, mpv 0.41.0 CLI). <!-- DECISIONS 2026-10-22 (amended codec rule) -->
2. **Trigger 2:** the video track is present but **deselected** for 2 consecutive samples at 1 Hz, AND the same `vd` line for the current load. A client MAY additionally require `file-loaded` for this trigger: a track can only be deselected on a loaded file, so the variant is equivalent. <!-- DECISIONS 2026-10-22 (amended codec rule); DECISIONS 2026-10-23 ruling 13 -->

Guards. A codec step MUST NOT happen:
- during stream-death recovery;
- more than once per play;
- on a remote receiver or while a switch is in flight;
- without a known codec (so never on a Transcode, where the Decision's `videoStream` is the source, not what was delivered);
- on `unrecognized file format` or `loading failed`, which never count. <!-- DECISIONS 2026-10-22 (codec evidence; amended) ; FINDINGS 2026-10-22 (Android 4c8e333 gap 2) -->

The exclusion is withheld from every later Capability profile until the app or OS version changes or the user resets it. It is never reverted within a play, but bandwidth up-steps **resume** after a landed codec step under the normal rules (§4). <!-- ADR-0071 d4; FINDINGS 2026-10-22 (codec step resumes) -->

Exclusions are not applied to music (§10). Vo/filter failures also deselect the track (`vid=no`); the `vd` line is what discriminates a decoder failure. <!-- FINDINGS 2026-10-22 (Android 4c8e333) -->

## 4. Step-up

1. Step-up is for **bandwidth only**. <!-- ADR-0071 d3 -->
2. The client MUST wait **120 s** with throughput at ≥ **1.5×** the target rung's `bitrate` (buffer full, no stalls) before an up-step. This hold **doubles, up to a maximum of 1800 s**, after a down-step that soon follows an up-step (the flap guard); how "soon" is measured is per client (§12). <!-- ADR-0071 d3; DECISIONS 2026-10-21 (user ruling); DECISIONS 2026-10-23 ruling 9 -->
3. A **300 s cooldown** follows. It is started by **any landed automatic switch**: bandwidth down, resource down, or codec re-negotiation. Refused, stay-put and abandoned steps MUST NOT start it. <!-- FINDINGS 2026-10-22 (spec agreed, cooldown) -->
4. The up-target is the rung **directly before `current` among the rungs that pass the Quality cap and the resource ceilings**, and it must also pass the headroom test. Anchor presence and position (§5.4) are read on the **full** server ladder, before any filter (§5.6). <!-- FINDINGS 2026-10-22 (Apple DECISION 57); DECISIONS 2026-10-23 ruling 8 -->
5. A rung with an **unknown `bitrate`** is never a bandwidth up-target. <!-- FINDINGS 2026-10-22 (Apple DECISION 57) -->
6. A resource or codec landing is not a bandwidth switch: it starts only the up-cooldown, adds no flap count, keeps the stall edges and starts no down-cooldown. <!-- FINDINGS 2026-10-22 (Apple 27fed95 trap 1) -->

## 5. Resource ceilings

A resource step limits how far the client may climb again during this play.

1. **What is recorded.** The failing rung's own `height`, else its `constraints.maxResolution`, else Edition detail; plus its `bitrate`. The client MUST NOT use `Decision.videoStream.height`: on a Transcode it is the **source** File's stream, not the delivered height. <!-- FINDINGS 2026-10-22 (TRAP, Android review; service.go:927,939) -->
2. **What it excludes.** A rung is above the ceiling when it is ≤ the ceiling in every dimension known for both and < in at least one. Unknown values are kept only at or below `current`. <!-- FINDINGS 2026-10-22 (Spec, ceiling filter) -->
3. **Up-steps only.** A ceiling never restricts a down-step: rungs below `current` stay available (a resource down-step takes the next rung; a bandwidth down-step uses the fit rule). A second resource step tightens the ceiling. <!-- FINDINGS 2026-10-22 (Spec: a ceiling limits only going UP; Android D25) -->
4. **Fallback (dimensionless anchor).** If the failing rung has neither height nor bitrate, the ceiling is the rung stepped **to**, kept as an **anchor identified by request tuple** (never by label) in later ladders. The anchor is recorded only when a resource step lands, on the rung landed. If the anchor is missing from a later positioned server ladder, a **latch** blocks all automatic up-steps for the rest of the play, even if the anchor reappears. Viewer picks are never refused. <!-- FINDINGS 2026-10-22 (Android D22/D23); 2026-10-23 (Android D24-D27) -->
5. The latch is set **only** when a positioned server ladder is armed, never on a fallback or placeholder ladder. <!-- FINDINGS 2026-10-22 (Apple 27fed95 trap 2); 2026-10-23 (D24-D27) -->
6. Anchor and up-target positions are read on the server ladder **before** the cap filter. <!-- FINDINGS 2026-10-23 (D24-D27; Apple 02729a6) -->
7. The 503 walk (§6.1) is capped at the failing height. <!-- FINDINGS 2026-10-23 (D24-D27) -->

## 6. Refusals

A step is re-negotiated with `replacesSessionId` naming the live session. A refusal leaves the old session playing.

1. **`503 SERVER_BUSY`:** try a **lower Edition rung** if one exists (a walk over the rungs below `current`, filtered as in §2.7 and §5; flag-off fallback lists are filtered too, including codec fallbacks). If none, **stay put**. <!-- ADR-0071 d5; FINDINGS 2026-10-22 (Apple 27fed95 trap 3) -->
2. **`429 STREAM_LIMIT`:** **stay put directly.** Rung choice cannot change a session count, so there is no walk. Under rule 15 (flag off): re-negotiate the current rung. <!-- DECISIONS 2026-10-22 (429) -->
3. **Stay-put effects.** Each stay-put counts toward the flap limit and starts **no** cooldown. Automatic stepping then **continues**; it does not halt, until the flap limit below is reached. Reaching the flap limit (`maxSwitches` entries, stay-puts included, within the flap window; §12) makes **bandwidth** stepping inert for the rest of the play (both clients confirmed: Apple AbrPolicy.swift:557-562, Android sticky SwitchCeiling). <!-- DECISIONS 2026-10-23 (stay-put parity); FINDINGS 2026-10-22 (Apple 02729a6); DECISIONS 2026-10-23 ruling 11 -->
4. **Resource gate.** Resource attempts have their **own 10-minute window**. An entry is a resource attempt that **landed**, **stayed put**, or was **server-disagreed** (a different Edition came back, or a client Edition rung was answered with a Transcode tier: the rung is unchanged). The Transcode-tier case is a disagreement on both clients, and is judged by the Decision's `current` rung: it counts only when that rung has `kind: "transcode"`. A video-copy transcode (tier Transcode with video copied) is **not** a disagreement. With no ladder (flag off) there is no such check. <!-- DECISIONS 2026-10-23 ruling 10; DECISIONS 2026-10-24 (ruling 10 refined) --> Bandwidth switches never enter this window; a stay-put also counts in the bandwidth flap window. The gate **trips at `maxSwitches` entries with at least one non-landing**, after which resource steps are inert. Codec steps stay live (own inert reason). The window MUST expire on its **own timestamps**, not on arrays only the bandwidth path prunes; otherwise it latches forever when bandwidth is inert. <!-- DECISIONS 2026-10-23 (amended gate); FINDINGS 2026-10-23 (Android D30; Apple 02729a6 trap) -->
5. **"Still playing" predicate.** A stay-put is only valid when: a live session exists AND no failure card is up AND no player error or end has occurred (including an error `end-file` arriving during a step) AND no recovery is in progress. "Ended" counts as not playing. <!-- FINDINGS 2026-10-22 (Android review items; Apple D63; Android 5ff7526) -->
6. **A busy refusal with the failure card already up keeps the card.** A card is never cleared by a later rung of the same walk. A walk reports once, at its end. <!-- FINDINGS 2026-10-22 (Apple D63; Android 5ff7526) -->
7. **Notice, not card.** A refused flag-on pick or step shows a **non-blocking notice**, never the failure card, provided the stream is still healthy. Every refusal restores the picker and ABR, and the ticker runs whenever a session is live. <!-- FINDINGS 2026-10-22 (Orchestrator ruling §5; Apple D63) -->
8. **Recovery runs after the switch gate is released.** Recovery owed by a refused step MUST be started only after the step has released its switch gate; otherwise it deadlocks. <!-- FINDINGS 2026-10-22 (Apple brief 33 trap) -->
9. **After a stay-put, restart the progress/keepalive ticker before any `await`.** Otherwise the Server reaps the session after about 90 s. <!-- FINDINGS 2026-10-22 (Apple brief 33 trap) -->
10. A recovery reload re-checks the live session after its progress POST. <!-- FINDINGS 2026-10-22 (Android 5ff7526) -->

## 7. Concurrency

A replacement cannot be undone: the old session is doomed even if the client discards the new one.

1. A client MUST **serialize** negotiations that name the same `replacesSessionId`, keep only the **latest pending** one, and name whichever session is **live at send time**. <!-- DECISIONS 2026-10-22 (contract rule); api-contract replacesSessionId -->
2. **A request that no longer applies at send time MUST NOT be sent.** "Applies" covers player kind, live session and pin; it is dropped before the POST (nothing is yet doomed). The mechanism is the client's: a re-check under the negotiation lock, or a gate plus cancel of the pending request, are equivalent and allowed. <!-- FINDINGS 2026-10-22 (Android four races; Apple audit); DECISIONS 2026-10-23 ruling 13 -->
3. **Adopt unless newer.** A successful negotiation's session is discarded only if a newer session is live; otherwise it is adopted. A deliberate stage end (stop, user switch, exit) closes the session. <!-- FINDINGS 2026-10-22 (Apple cc936b5; Android D11) -->
4. Stale-request races to guard:
   - a late-succeeding step after a mid-flight pick is adopted (race 1);
   - picks queued behind a hand-off to a remote receiver are dropped (race 2);
   - stream-death recovery compares and detaches under the lock, never check, `await`, then detach (race 3);
   - a stream-token mint failure after a to-Cast negotiate restarts playback locally (race 4). <!-- FINDINGS 2026-10-22 (Android four races; Apple audit) -->
5. Flag off: a pick that has begun negotiating always runs its begin, and a switch it outran is closed, not loaded. A pick dropped at send time does not pin ABR. <!-- FINDINGS 2026-10-22 (Android review items) -->
6. The same "re-check" shape applies at every discard point: `sessionWasReaped`, `renewStreamToken`, the hand-off swap and route reload. <!-- FINDINGS 2026-10-22 (Apple audit) -->

## 8. Manual pins and Auto

1. **Auto** is the default. A manually picked rung **disables bandwidth steps, both down and up, for that play**. <!-- ADR-0071 d8 -->
2. **Codec and resource steps still override a pin**; otherwise nothing plays. Every automatic step is announced with a brief notice (§13). <!-- ADR-0071 d8 -->
3. A server-returned Edition different from the requested one follows the same rule as a disagreement: the notice names the delivered rung, and bandwidth stepping is off for that play. <!-- FINDINGS 2026-10-22 (Apple open item 2) -- adopted from the shared D67b rule; not separately ruled -->
   The disagreement is sticky for video, also across a hand-back; it does not apply to music. <!-- FINDINGS 2026-10-23 (Android review Q4) -->
4. **A pin sets bandwidth inert for the play; picking Auto clears only the pin.** Automatic stops the play earned, a sticky server disagreement (§8.3) or the flap limit (§6.3), MUST survive both a pin and a later Auto. A pin MUST NOT overwrite them, and Auto restores them. **Both clients must change:** today both clear everything on Auto (Apple even clears the flap ceiling and the server disagreement). <!-- DECISIONS 2026-10-24 (user ruling, §8.4) -->

## 9. 404 after a rescan

A step naming a stale `editionId` answers `404`.

1. Match the old Edition by its **Name** against the Title's current Editions (**best-effort**: the Server does not guarantee Names survive a rescan). Use the Edition **Name**, never the rung `label`, which carries the height and is not unique.
2. If the Name match is **missing or ambiguous**, re-negotiate **once** without `editionId`, keeping the rung's constraints and `replacesSessionId`. This includes a Transcode rung that carries an `editionId` but no Name. Only after that retry does the failure path apply. <!-- DECISIONS 2026-10-23 ruling 6 -->
3. **Every 404 rescan retry drops explicit stream ids** (audio, video, burn-in), because they are stale after a rescan. This covers the retry without `editionId` and the retry with the uniquely matched Name. <!-- DECISIONS 2026-10-23 (ruling 6, Apple's call); DECISIONS 2026-10-24 (ruling 4/6 refined) -->
4. Otherwise take the failure path.

<!-- FINDINGS 2026-10-22 (Android fd0fec7, "Spec (agreed with Android)"). Re-negotiating without an editionId first could switch cuts, hence the Name match first. -->

## 10. Music

Music rungs come from the same `stepDown` field on an audio-only Decision.

1. **Timing.** A step takes effect at a **Track boundary**. The one exception is the **dry-buffer same-Track step**: when the buffer runs dry within a Track, step on the same Track. <!-- ADR-0071 d9; FINDINGS 2026-10-23 (Android D34) -->
2. **Original.** The first **unconstrained** rung is Original. Clients MUST NOT filter it out. Its tuple is `{kind: edition, editionId: <own Title>, no constraints}`. Height is **ignored** on music. <!-- FINDINGS 2026-10-23 (Android music slice A, item 1; D34); DECISIONS 2026-10-23 (height 0) -->
3. **Identity.** Within a Track a rung is identified by `editionId` + tuple. **Across Tracks** strip `editionId` and send only `maxBitrate`; a carried `editionId` belongs to another Title and gets `404`. <!-- DECISIONS 2026-10-23 (music rung portability) -->
4. **Cap carried across Tracks.** The **Original carries UNCAPPED** (send no `maxBitrate`). Any other rung uses its own `constraints.maxBitrate`; else its own known `bitrate`; else it **borrows** from the **nearest lower rung that has one**. With none, the Track plays uncapped. Pin nearest-first: for `[FLAC, MP3*, AAC 256, AAC 128]`, an MP3 of unknown bitrate carries `256000`. <!-- DECISIONS 2026-10-23 (after refuter-38); FINDINGS 2026-10-23 (Apple ab7c5e6 trap); DECISIONS 2026-10-23 ruling 1 -->
5. **No `replacesSessionId` at a Track boundary**: the next Track is a new Title. <!-- FINDINGS 2026-10-23 (Android D31) -->
6. **Landing is judged by direction, not by exact tuple.** A step lands when the new Decision's delivery moved **toward the asked side and is no better than asked**. Exact tuple equality is not required: a cap that merged into a cheaper Edition counts as landed. The exact predicate, for a Track-boundary or same-Track music step: it lands **iff** `quality(current)` is strictly on the step's side of `quality(previous playing rung)` **AND** `quality(current) ≤ asked`. Here `quality(r)` = `constraints.maxBitrate` if > 0, else the known `bitrate`, else — for a **non-Original** rung only — the borrowed nearest-lower cap (rule 4), else +∞ (an Original with no cap and no known bitrate is +∞, never borrowed, matching "the Original carries uncapped"); and `asked` = the step rung's `maxBitrate` if > 0, else +∞. A plain carry with no step is not judged; it positions on `current`. <!-- DECISIONS 2026-10-24 (ruling 12, exact predicate; Android MusicController.kt:272-325) --> Anything else is a stay-put with no notice and no cooldown. A **landed step — carried or same-Track — counts as agreement on the delivered rung**, even when the delivered rung's id or tuple differs from the asked rung's (rung ids differ across Tracks, and a cap can merge into another Edition within a Track); treating it as a disagreement makes bandwidth inert for the play. A carry is sent on **every** Track; what is carried is the **asked** cap of the last landed step — the step rung's own `quality(r)` as defined above, taken before the viewer's base cap is applied (NOT the `maxBitrate` actually sent, which is 0 for an Edition rung requested by `editionId` alone); the Original, or a rung whose quality is +∞, carries uncapped — not the delivered rung's value. The carried ask survives a streamed Track whose Decision has no `stepDown` ladder (that Track still sends it) and a local or downloaded Track; it is replaced by the next landed step, and **dropped** (back to uncapped) when a carried request with no fresh step behind it is refused and the uncapped fallback plays — so a busy host does not cost two negotiations on every later Track; bandwidth stepping then re-steps if the link still needs it, so a step that landed on a cheaper merged delivery does not ratchet later Tracks further down. <!-- FINDINGS 2026-10-22 (Android slice A item 3); 2026-10-23 (Apple 79bc337 trap; Apple 24f9dbf trap 1); DECISIONS 2026-10-23 ruling 12 -->
7. **Refusals.** Retry **once** at the last quality that played on: `503`, `429 STREAM_LIMIT`, `TRANSCODE_REQUIRED`, `404`. **Never** `429 TOO_MANY_ATTEMPTS`. A **second refusal** of a Track whose **step retry** was refused goes to the Track skip (card, advance, skip count). It MUST NOT go to the negotiate-error path, which halts the Queue. This applies only to a Track that was stepped: a refusal on a never-stepped Track keeps the existing pre-step-down behaviour (changing that is out of scope). <!-- FINDINGS 2026-10-23 (Apple 79bc337; Apple c9de018 trap; Android D39); DECISIONS 2026-10-23 ruling 3; DECISIONS 2026-10-24 (ruling 3 scope) --> **The "stepped" mark** belongs to the Queue entry, not the Title, so repeat-one and a Title queued twice are separate entries. It is set whenever a step was asked for that entry, whatever became of the request; a request carrying a cap (§10.6) counts as a step asked, so a carried request refused and then refused again on its uncapped retry goes to the Track skip. It is cleared **only** when that entry is heard — the **first** playback restart after a load of that entry, while it is the live entry — when another entry is heard, or on stop or a new Play. A "load" here is a Track-level load (a boundary start, a preload adoption, a local or downloaded Track, a repeat or replay); the reload that a same-Track step makes is **not** one, so an entry stays marked after its own step plays. Queuing or issuing a load does not clear it; neither does a seek (on any Track, including the live one), a discard, or a failed start. Several entries can be marked at once (for example, the live entry and a preloaded next entry); hearing any entry clears **every** mark, its own and the others'. A refusal for a marked entry still gets the one retry above before the skip, even when the refused request carried no cap. <!-- FINDINGS 2026-10-24 (Android D51; Android music slice C 14dd03a); DECISIONS 2026-10-24 (carry marks) -->
8. **Decoder failure** skips the Track. There is **no** codec step and **no** audio Decode exclusion. Skip only on the `end-file` error: mpv's decoder-init signal on music can be the **cover-art** decoder failing, and a cover-art `vd` line MUST NOT skip the Track. <!-- DECISIONS 2026-10-23 (user confirmed); FINDINGS 2026-10-23 (Apple 24f9dbf trap 2; Android D37) -->
9. **Overlap (crossfade)** is kept on Transcode rungs, for gapless. <!-- DECISIONS 2026-10-23 (user confirmed) -->
10. **Notice** shows on Now Playing and the mini-player, and is **cleared on a local or downloaded Track**. Music shows only the network pair (§13); a music stay-put is silent. <!-- FINDINGS 2026-10-23 (Android D31; Apple 24f9dbf trap 3); DECISIONS 2026-10-23 ruling 14 -->
11. Music steps **only** from a server `stepDown` ladder. Without the flag, music does not step. <!-- FINDINGS 2026-10-23 (Apple 79bc337 choice 1) -->
12. When `features.transcode` is `false`, do **not** carry a music cap across Tracks; each Track starts uncapped. (Negotiate never checks that the host can transcode — ADR-0040 — so a carried cap on a no-ffmpeg host could produce a Decision the host cannot run.) On such a host a music Decision whose tier is `directStream` **or** `transcode` — both need ffmpeg (ADR-0040: its absence "costs the remux and transcode tiers") — counts as a refusal for §10.7. <!-- FINDINGS 2026-10-23 (Apple 24f9dbf; Apple c9de018); orchestrator 2026-10-24 (remux also needs ffmpeg) -->
13. Two music Editions with an identical bitrate cannot both be reached by a cap-only request; the first in ladder order wins. Within a Track, reach either by `editionId`. <!-- DECISIONS 2026-10-23 (tie) -->

## 11. Remote receivers (Cast, AirPlay)

1. **Reduced policy.** Bandwidth only, from the receiver's buffering state. No Decode exclusions, no resource steps. Steps are **one rung down per debounced buffering episode**, and **never** automatically back up. Remote steps run **only with the flag on**; a client MAY read the flag when the policy is built. <!-- DECISIONS 2026-10-21 (Cast); 2026-10-23 (user confirmed); FINDINGS 2026-10-23 (Android D31); DECISIONS 2026-10-23 ruling 13 -->
2. **Failed load.** If the receiver fails to load after an accepted step, negotiate the **old rung once more**, then take the failure path. <!-- FINDINGS 2026-10-23 (Android D31) -->
3. **A Try again while remote keeps the went-remote record.** <!-- FINDINGS 2026-10-23 (Apple 27ceacc trap) -->
4. **Hand-back** (the player returns to local from a remote receiver) applies to any remote receiver. On hand-back:
   - **bandwidth and remote state reset**;
   - **local resource state carries over** (ceilings, anchor latch, gate window);
   - the first local negotiation asks for the **highest rung passing the Quality cap and the carried ceilings**. It is chosen from the **last local ladder snapshot**, taken **before** the remote Decision repositions, and requested by tuple. The remote Decision's ladder is never used;
   - with no snapshot, negotiate the cap, then step down immediately on that Decision's ladder if `current` is outside a ceiling; this counts as a resource attempt. When nothing is carried (no ceiling), the request is the **bare cap**; a client MAY send the bare cap in that case, which is equivalent;
   - if the stored rung is refused, fall back to the previous path. <!-- DECISIONS 2026-10-23 (R1 interpretation); FINDINGS 2026-10-23 (hand-back ladder source; Apple 27ceacc); DECISIONS 2026-10-23 ruling 13 -->
5. **Only a remote return counts as a hand-back.** A Picture-in-Picture round trip does not. <!-- FINDINGS 2026-10-23 (Apple 27ceacc trap) -->
6. The Cast inert state MUST NOT be sticky across a hand-back (a known Android bug; the fix is Android D32/D38). <!-- FINDINGS 2026-10-23 (Apple 9661ea4; Android D32); FINDINGS 2026-10-23 (Android review errata: D32/D38, not D31) -->
7. **Settling a Cast step.** While a step is in flight:
   - a receiver that goes IDLE for the **old** content takes priority over the step: `INTERRUPTED` is a take-over by another sender, `FINISHED` takes the normal finish;
   - on `ERROR`, an accepted step's load counts as the one reload (rule 2), and a refused step gets the normal recovery;
   - if the receiver had already errored, a landed step keeps that reload spent;
   - a Play that arrives mid-step abandons the step, then runs;
   - every settle path returns the controller to its idle state, so a receiver that disconnects mid-step cannot leave it stuck;
   - refused steps are gated at **3 per 300 s** (measured on Android: 12 negotiations per 1200 s). <!-- FINDINGS 2026-10-24 (Android Cast slice 2 ae8dc5e) -->
8. Apple's AirPlay is ABR-inert today (no sampler); the rules apply once it has one. Nothing here has run on hardware. <!-- FINDINGS 2026-10-23 (Apple 27ceacc) -->

## 12. Thresholds

"Unmeasured" means **not measured on hardware**. Every shared value is unmeasured unless the basis column records a spike measurement. "Per-platform" values differ between clients and are listed in their own rows. <!-- DECISIONS 2026-10-23 ruling 15 -->

**Shared values** (both clients MUST use these):

| Threshold | Value | Basis |
| --- | --- | --- |
| `maxSwitches` (flap limit) | 4 | unmeasured |
| Flap window | 600 s | unmeasured |
| Down-cooldown | 30 s, drain only (a stall bypasses it) | unmeasured |
| Up hold | 120 s, doubling to a 1800 s maximum (§4.2) | unmeasured (120 s is a user ruling 2026-10-21, replacing 60 s) |
| Up cooldown | 300 s, after any landed automatic switch | unmeasured (user ruling 2026-10-21) |
| Up headroom | ≥ 1.5× the target rung's `bitrate` | unmeasured |
| Down-fit | rung fits at `bitrate` ≤ 0.75 × throughput | spike-measured on a link: read rate 0.969–1.019× the link (p5–p95, n=4287), Apple AbrTuning; not measured on device hardware |
| Resource trigger | 30 dropped frames in 10 s | unmeasured |
| Resource gate window | 600 s (10 min); trips at `maxSwitches` entries with at least one non-landing (§6.4) | unmeasured |
| Codec evidence | 2 samples at 1 Hz (trigger 2); at most 1 codec step per play | spike-measured for the vd/end-file behaviour only (mpv 0.41.0 CLI, no device run); the sample count is unmeasured |

**Server constants** (not client thresholds):

| Threshold | Value | Basis |
| --- | --- | --- |
| Server replace grace | 30 s | Server constant, api-contract replacesSessionId |
| Server reap of an unrefreshed session | about 90 s | FINDINGS 2026-10-22 (Apple brief 33); why the ticker restarts first (§6.9) |

**Per-platform values:**

| Threshold | Platform | Value | Basis |
| --- | --- | --- | --- |
| Decoder behind (resource trigger) | Apple | 5 s | per-platform; unmeasured (Apple's guess) |
| Resource drops | Android | ≥ 3 drops/s for 10 s (30 in 10 s, matching the shared value in effect) | per-platform; unmeasured |
| Codec no-picture | Android | after 2 s | per-platform; unmeasured |
| Buffer high water / readahead | Apple | 19 s / 60 s | per-platform; Apple AbrTuning, unmeasured on hardware |
| Buffer marks | Android | low 16 s, high 19 s, drain 8 s with a ≥ 3 s drop, stall 3 s or 2 stalls in 60 s, grace 15 s, readahead 60 s, hysteresis 20 s, cache 256 MiB | per-platform; most spike-derived per Android's review (individual values not itemised) |
| Remote (Cast) | Android | stall 5 s, grace 15 s, down-cooldown 35 s, load timeout 15 s | per-platform; unmeasured on hardware |
| Remote debounce | Apple | not recorded (AirPlay is ABR-inert, no sampler) | per-platform; unmeasured |
| Music | Android | dry when ≤ 1 s ahead or stalled; notice shown 8 s | per-platform; unmeasured, needs a device |
| Music | Apple | not recorded | per-platform; unmeasured, needs a device |
| Flap-guard "soon" window (§4.2) | both | not recorded in the project | per-platform; unmeasured |

## 13. Notice strings

**Confirmed by the owner (2026-10-24).** One shared set for both clients. `{label}` is the delivered rung's label; `{codec}` is the failing codec; `{edition}` is the Edition name. <!-- DECISIONS 2026-10-23 ruling 14; DECISIONS 2026-10-24 (user confirmed) -->

| Event | String |
| --- | --- |
| Bandwidth down-step | `Lowered to {label} (network)` |
| Bandwidth up-step | `Back to {label}.` |
| Resource step | `Lowered to {label} (this device couldn't keep up)` |
| Codec re-negotiation | `This device can't decode {codec} — playing {label}` |
| Busy (503, stayed put) | `Couldn't lower quality right now — the server is busy.` |
| Stream limit (429, stayed put) | `Couldn't lower quality — you're at your stream limit.` |
| Manual pick refused, busy (503, stayed put) | `Server is busy — staying at {label}.` |
| Manual pick refused, stream limit (429, stayed put) | `You're at your stream limit — staying at {label}.` |
| Delivery not in the ladder | label `another version` |
| Flag-off Edition pair (unchanged) | `Switched to {edition} to keep playing.` / `Back to {edition}.` |

- The Edition pair is used only with the flag off. With the flag on, a step onto an Edition rung uses the network or resource strings. In the manual-pick rows, `{label}` is the rung still playing. <!-- DECISIONS 2026-10-24 (user confirmed) -->
- Music shows only the network pair, and a music stay-put is silent (§10.10). <!-- DECISIONS 2026-10-23 ruling 14 -->
- "Delivery not in the ladder" is the `{label}` text used when the delivered rung matches no listed rung. <!-- DECISIONS 2026-10-23 ruling 14 -->

## Parity traps

One line each, every one found by a client or a refuter.

1. Do not read `Decision.videoStream.height` for a ceiling: it is the source's, not the delivered height.
2. A step must not send the Quality cap with the flag on; the request holds the lowered rung, so a cap read from it locks the play low.
3. Never key rungs by `label`; labels carry the height and may collide or drift.
4. Never re-sort the wire order by bitrate.
5. Do not drop a no-constraint Transcode rung; it is legitimate, and may be `current`.
6. An older server answers `400` to an unknown `replacesSessionId`; omit the key, do not send it empty.
7. Two negotiations naming one `replacesSessionId`: the first dooms the session the second names.
8. Recovery owed by a refused step runs after the gate is released, or it deadlocks.
9. Restart the ticker before any `await` after a stay-put, or the Server reaps at about 90 s.
10. A busy refusal while the failure card is up must not clear the card.
11. A resource or codec landing is not a bandwidth switch: no flap count, no down-cooldown.
12. A placeholder or unresolved ladder never latches the missing-anchor block.
13. 503 fallback lists are filtered too, including flag-off codec fallbacks.
14. Read anchor and up-target positions before the cap filter.
15. The resource gate expires on its own timestamps, or it latches forever when bandwidth is inert.
16. A server-disagreed resource step counts as a stay-put in the gate, or the gate never trips.
17. Clear the resource-trigger attribution flag on every exit path.
18. A stay-put counts toward the flap limit but starts no cooldown; stepping continues until the flap limit is reached, which makes bandwidth stepping inert for the play.
19. Flag off: a taller Edition must not be chosen by a down-step when the failing height is known.
20. A PiP round trip is not a hand-back; snapshot the local ladder before the remote Decision repositions; Try again while remote keeps the record.
21. Cast "inert" must not stay sticky after hand-back.
22. Music: a carried landing counts as agreement, or bandwidth goes inert for the play.
23. Music: carry the playing rung on every Track.
24. Music: a cover-art `vd` line is not a decoder failure; skip only on the `end-file` error.
25. Music: a second refusal of a Track skips the Track; the negotiate-error path halts the Queue.
26. Music: clear the notice on a local or downloaded Track.
27. Music: with `features.transcode` false, carry no cap.
28. Music: the Original carries uncapped; any other rung uses its own cap, else its own known bitrate, else the cap of the nearest lower rung that has one.
29. A failed video decoder deselects the track (`vid=no`), so a "no frames" check never fires; use the `vd` line plus the `end-file` reason or the deselect signal.
30. Race fixes: a request that no longer applies is never sent (re-check under the lock, or gate plus cancel); compare-and-detach; adopt unless newer.
31. Do not drop the viewer's explicit audio, video or burn stream picks on a step.
32. Record a resource ceiling and anchor when the step lands, never at emit.
33. A rung of unknown bitrate never counts as fitting a jump, but the no-fit fallback still reaches it.
34. A music step lands by direction ("toward the asked side, no better than asked"), not by exact tuple equality.
35. A client Edition rung answered with a Transcode tier is a server disagreement.
36. Auto after a pin undoes the pin only; a sticky server disagreement and the flap limit survive both. <!-- DECISIONS 2026-10-24 -->
37. Ruling 10 is judged by the `current` rung's kind: a video-copy transcode is not a disagreement, and with the flag off there is no check. <!-- DECISIONS 2026-10-24 -->
38. Every 404 rescan retry (no `editionId`, or unique-name match) drops explicit stream ids. <!-- DECISIONS 2026-10-24 -->
39. Only a music Track whose step retry was refused goes to the skip; a never-stepped Track keeps its old behaviour. <!-- DECISIONS 2026-10-24 -->
40. Keep stream picks on wire-ladder steps on Android too; it must stop nulling ids on every rung step. <!-- DECISIONS 2026-10-24 -->
41. Music landing needs both a strict move to the asked side of the previous rung and `quality(current) ≤ asked`; a plain carry is not judged. <!-- DECISIONS 2026-10-24 -->
