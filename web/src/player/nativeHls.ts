// The native-HLS feature check, in its own module so BOTH the attach seam
// (hls.ts) and the capability profile (capabilities.ts) can use it without
// coupling: component tests mock ./hls wholesale (jsdom has no MSE), and the
// profile derivation must keep working under that mock.

/** True when the browser can play an HLS playlist natively in a <video src> AND
 * exposes the `audioTracks` API needed to drive in-band audio switching on that
 * native player. Both checks matter: modern Chrome ALSO answers canPlayType for
 * the HLS MIME (Chromium shipped built-in HLS), but its `video.audioTracks` is
 * flag-gated — on Chrome's native player an audio switch silently does nothing
 * (the reported "UI says commentary, I hear the original" bug), and none of our
 * hls.js error handling/recovery is in play either. So the native path is taken
 * only where it is fully driveable (Safari / iOS, which expose audioTracks);
 * everywhere else the MSE/hls.js path gives us switching, logging, and recovery.
 * Feature-detected, never UA-sniffed. */
export function canPlayHlsNatively(video: HTMLVideoElement): boolean {
  try {
    const verdict = video.canPlayType("application/vnd.apple.mpegurl");
    const audioTracks = (video as { audioTracks?: unknown }).audioTracks;
    return (verdict === "probably" || verdict === "maybe") && audioTracks != null;
  } catch {
    return false;
  }
}
