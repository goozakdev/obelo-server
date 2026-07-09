import type { ReactNode } from "react";
import AppHeader from "../browse/AppHeader";
import "./music.css";

// MusicShell is the music section's app frame: the SHARED AppHeader (the header is
// intentionally common across TV/Movies and Music) + a `music-theme` root that
// scopes every music style. EVERY music screen renders inside it, the way the
// browse screens render inside `app-shell` + AppHeader. The `music-theme` class is
// the single CSS seam — rules in music.css are written under `.music-theme`, so
// restyling the music body can never leak into the TV/Movie side.
//
// `wide` mirrors the browse screens' `app-main-wide` (the grid/detail layouts
// want the wider main column); it defaults on since every current music screen is
// a grid or a detail.

export default function MusicShell({
  children,
  testId,
  wide = true,
}: {
  children: ReactNode;
  testId?: string;
  wide?: boolean;
}) {
  return (
    <div className="app-shell music-theme" data-testid={testId}>
      <AppHeader />
      <main className={`app-main${wide ? " app-main-wide" : ""}`}>{children}</main>
    </div>
  );
}
