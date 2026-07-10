import { useEffect, useState } from "react";

// The detail hero's identity block: the fetched logo artwork IS the title — it
// names the work in the artist's own lettering — so when one exists it stands in
// for the large-text heading (and the hero shows no poster at all). A title
// without a logo, or whose logo fails to load (404 after a stale URL), falls
// back to the heading so the page always names itself. The <img> stays inside
// an <h1> so the document outline keeps its top heading either way; the alt
// text carries the name for screen readers.

export default function TitleLogo({
  title,
  src,
  testId = "detail-title",
}: {
  title: string;
  /** The logo artwork URL (same-origin, media-cookie authed); undefined when
   * the title has no logo — the text heading renders instead. */
  src?: string;
  /** data-testid for the text-fallback heading (the logo <img> is always
   * "detail-logo"), so each screen keeps its established heading testid. */
  testId?: string;
}) {
  // Reset the error state when the target changes (detail nav / a newly picked
  // logo): a logo that 404'd may now exist.
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    setFailed(false);
  }, [src]);

  if (!src || failed) {
    return (
      <h1 className="detail-title" data-testid={testId}>
        {title}
      </h1>
    );
  }

  return (
    <h1 className="detail-logo-heading">
      <img
        className="detail-logo"
        data-testid="detail-logo"
        src={src}
        alt={title}
        onError={() => setFailed(true)}
      />
    </h1>
  );
}
