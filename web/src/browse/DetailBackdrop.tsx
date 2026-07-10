import { useEffect, useRef, useState } from "react";

// DetailBackdrop pins an entity's Background artwork (the fetched TMDB backdrop)
// to the top-right of the viewport behind a detail screen, so the page content
// scrolls over a stationary image. A black veil on top deepens as the user scrolls — from a
// slight at-rest dim (so the hero text stays legible over a bright image) up to
// a hard cap of 50%, so the artwork always shows through. Renders nothing when
// the entity has no Background artwork or the image fails to load.

const FADE_BASE = 0.15; // at-rest veil opacity, top of the page
const FADE_MAX = 0.5; // the cap: never darken past 50%
const FADE_DISTANCE = 600; // px of scroll over which the veil reaches the cap

export default function DetailBackdrop({ src }: { src?: string }) {
  const wrapRef = useRef<HTMLDivElement>(null);
  const [loaded, setLoaded] = useState(false);
  const [failed, setFailed] = useState(false);

  // Drive the veil from window scroll via a CSS variable, rAF-coalesced so a
  // scroll burst costs one style write per frame.
  useEffect(() => {
    if (!src) return;
    let frame = 0;
    const apply = () => {
      frame = 0;
      const progress = Math.min(window.scrollY / FADE_DISTANCE, 1);
      const veil = FADE_BASE + (FADE_MAX - FADE_BASE) * progress;
      wrapRef.current?.style.setProperty("--backdrop-fade", veil.toFixed(3));
    };
    const onScroll = () => {
      if (!frame) frame = requestAnimationFrame(apply);
    };
    apply();
    window.addEventListener("scroll", onScroll, { passive: true });
    return () => {
      window.removeEventListener("scroll", onScroll);
      if (frame) cancelAnimationFrame(frame);
    };
  }, [src]);

  if (!src || failed) return null;

  return (
    <div
      ref={wrapRef}
      className={`detail-backdrop${loaded ? " detail-backdrop-loaded" : ""}`}
      aria-hidden="true"
      data-testid="detail-backdrop"
    >
      <img
        className="detail-backdrop-img"
        src={src}
        alt=""
        onLoad={() => setLoaded(true)}
        onError={() => setFailed(true)}
      />
      <div className="detail-backdrop-fade" />
    </div>
  );
}
