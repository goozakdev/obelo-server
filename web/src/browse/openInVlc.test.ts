import { describe, it, expect } from "vitest";
import { buildXspf } from "./openInVlc";

describe("buildXspf", () => {
  it("embeds the location and title in a one-track XSPF playlist", () => {
    const xspf = buildXspf("http://host/api/v1/files/f1/download", "Dune");
    expect(xspf).toContain('<playlist version="1" xmlns="http://xspf.org/ns/0/">');
    expect(xspf).toContain("<location>http://host/api/v1/files/f1/download</location>");
    // The title appears both as the playlist title and the track title.
    expect(xspf.match(/<title>Dune<\/title>/g)).toHaveLength(2);
  });

  it("XML-escapes the URL's query separators so they are not read as markup", () => {
    // A real download URL carries ?token=…&… — the & MUST become &amp; or the
    // playlist is malformed and VLC drops the rest of the location.
    const url = "http://host/api/v1/files/f1/download?token=ab%26cd&x=1";
    const xspf = buildXspf(url, "Movie");
    expect(xspf).toContain(
      "<location>http://host/api/v1/files/f1/download?token=ab%26cd&amp;x=1</location>",
    );
    expect(xspf).not.toContain("&x=1<"); // raw & never survives
  });

  it("XML-escapes the title", () => {
    const xspf = buildXspf("http://host/f", 'Tom & Jerry <"S1">');
    expect(xspf).toContain("Tom &amp; Jerry &lt;&quot;S1&quot;&gt;");
  });
});
