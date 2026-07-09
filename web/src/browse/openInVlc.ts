// "Open in VLC": hand a desktop player the original media file.
//
// The browser cannot launch VLC directly, but it can offer a one-track XSPF
// playlist for download; the OS opens .xspf in VLC (its default handler), and
// VLC streams the <location> URL — the sessionless direct-file download, which
// self-authenticates via its ?token= query param (see ApiClient
// .directFileDownloadUrl). So the flow is: build XSPF → download → VLC plays.

// xmlEscape escapes the five XML predefined entities so a title or URL is safe
// inside element text. The URL matters too: its ?token=…&… separators must not
// be read as markup.
function xmlEscape(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&apos;");
}

/** Build a minimal XSPF v1 playlist with a single track pointing at `location`.
 * `name` is the human label VLC shows for the track/playlist. */
export function buildXspf(location: string, name: string): string {
  const title = xmlEscape(name);
  return (
    `<?xml version="1.0" encoding="UTF-8"?>\n` +
    `<playlist version="1" xmlns="http://xspf.org/ns/0/">\n` +
    `  <title>${title}</title>\n` +
    `  <trackList>\n` +
    `    <track>\n` +
    `      <title>${title}</title>\n` +
    `      <location>${xmlEscape(location)}</location>\n` +
    `    </track>\n` +
    `  </trackList>\n` +
    `</playlist>\n`
  );
}

// safeFilename turns a media title into a tame .xspf base name (no path/illegal
// characters), falling back to "media" for an empty/blank result.
function safeFilename(name: string): string {
  const base = name.replace(/[^\w.-]+/g, "_").replace(/^_+|_+$/g, "");
  return base || "media";
}

/** Trigger a browser download of a one-track XSPF playlist for `location`,
 * named after `mediaName`. The OS opens it in VLC. No-op outside a browser. */
export function downloadVlcPlaylist(location: string, mediaName: string): void {
  if (typeof document === "undefined" || typeof URL.createObjectURL !== "function") {
    return;
  }
  const xspf = buildXspf(location, mediaName);
  const blob = new Blob([xspf], { type: "application/xspf+xml" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = `${safeFilename(mediaName)}.xspf`;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  // Revoke on the next tick so the click's navigation has grabbed the blob.
  setTimeout(() => URL.revokeObjectURL(url), 0);
}
