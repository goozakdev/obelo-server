import { useLayoutEffect } from "react";
import { useLocation, useNavigationType } from "react-router-dom";

// ScrollToTop resets the window to the top whenever the user navigates to a new
// screen. Without it, following a link (e.g. a poster on a scrolled library grid
// or an episode on a Show detail) renders the next screen at the previous
// screen's scroll offset, because every screen shares the one document scroll
// (the header is sticky; .app-shell scrolls the window).
//
// It fires only on PUSH/REPLACE navigations — following a link or a
// programmatic navigate — and skips POP (Back/Forward) so the browser's own
// scroll restoration returns the user to where they were on the previous screen.
//
// Keyed on pathname only (not search): changing filters/pagination query params
// on the same screen must not yank the user back to the top.
//
// Rendered inside <BrowserRouter> and renders nothing.
export default function ScrollToTop() {
  const { pathname } = useLocation();
  const navigationType = useNavigationType();

  useLayoutEffect(() => {
    if (navigationType === "POP") return;
    window.scrollTo(0, 0);
  }, [pathname, navigationType]);

  return null;
}
