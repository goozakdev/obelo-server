# Original-format subtitle delivery, negotiated per track by the Capability profile

Amends [ADR-0020](./0020-subtitle-delivery-in-band-hls-out-of-band-track-image-burn-in.md), which flattened every text Subtitle track to WebVTT (ASS/SSA "downgraded to plain cues").

Text-subtitle delivery format is now **negotiated per track** through the Capability profile's previously-unenforced `textSubtitleFormats`:

- A client that declares a track's **original format** (`srt`, `ass`; aliases `subrip`/`ssa` fold) gets the **original bytes, styling intact**, from the same endpoint family with the format as the extension: `GET /titles/{id}/subtitles/{subId}.srt|.ass`. Sidecar and Fetched originals are read raw off disk; embedded Streams are **codec-copied** out of the container by ffmpeg (never transcoded between subtitle formats). The decision's subtitle entry carries a `format` field naming what its `url` serves.
- Every other client gets exactly the pre-existing behavior: the WebVTT conversion at `.vtt`. Embedded `mov_text` stays WebVTT-only (no raw-servable original). A format the track is not in is a `404`, not a conversion.
- To make originals *available*, a **Fetched subtitle is now cached in its original format** (converted on demand at serve time, exactly like a Sidecar) instead of being pre-converted to WebVTT at pick time. Pick still validates convertibility up front. Rows fetched before this change carry codec `vtt` and serve unchanged through the conversion path's passthrough.

## Why

The tvOS client plays through **libmpv**, whose libass renderer is the reference implementation for ASS styling (karaoke, signs, positioning) — the very things the WebVTT downgrade destroys. ADR-0020 already named the loss and sketched libass *burn-in* as the future fix; serving the original to a client that renders it natively is strictly better than burn-in for that client: no transcode tier, no cap slot, instant switching, and the styling arrives as authored. The negotiation lever (`textSubtitleFormats`) was already in the contract, decoded and recorded — this gives it its intended meaning. Browsers keep WebVTT because `<track>` renders nothing else.

## Consequences

- `textSubtitleFormats` is now load-bearing: a client that omits it gets WebVTT everywhere (safe default); one that declares `ass`/`srt` must actually render them.
- The decision's `subtitles[].format` tells the client what it is getting; clients should key parsing on it, not on sniffing bytes.
- ADR-0020's burn-in path is untouched: image subs still burn on transcode, and ASS **burn-in** for non-mpv clients remains a possible future enhancement — this ADR only removes the need for it on mpv-family clients.
- The HLS in-band SUBTITLES rendition stays WebVTT (segmented VTT is the only HLS-native text carriage); original-format delivery applies to the out-of-band path, which is where an mpv client fetches tracks on every tier anyway.
- Subtitle bytes now leave the server in three content types (`text/vtt`, `application/x-subrip`, `text/x-ssa`), all under the same cookie-or-bearer media-GET auth.
