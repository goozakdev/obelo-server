import { useMemo } from "react";
import { encodeQr, type EccLevel } from "./qr";

// An <svg> rendering of a QR symbol. SVG rather than a <canvas> for two reasons:
// it is in the DOM (so a test can assert what was drawn without a pixel buffer,
// and a screen reader gets the <title>), and it stays crisp at whatever size the
// page gives it, which is what a phone camera pointed at a laptop needs.
//
// The whole matrix is drawn as ONE <path> of "move to, draw a 1x1 box" segments
// rather than a few hundred <rect> elements. A version-14 symbol is 73x73 —
// over five thousand modules — and five thousand DOM nodes is a real cost for a
// picture that never changes.

/** The light margin every QR needs around it (the "quiet zone"). Four modules
 * is the standard's requirement; without it a camera cannot find the symbol's
 * edges against a dark page. */
const QUIET_ZONE = 4;

export default function QrSvg({
  text,
  ecc = "L",
  label,
  className,
  testId,
}: {
  /** The exact string to encode. What the QR decodes to is this, byte for byte. */
  text: string;
  ecc?: EccLevel;
  /** The accessible name — a QR is an image of a string, and a reader that
   * cannot see it needs to be told what it is. */
  label: string;
  className?: string;
  testId?: string;
}) {
  const drawn = useMemo(() => {
    try {
      const { size, modules } = encodeQr(text, ecc);
      let path = "";
      for (let y = 0; y < size; y++) {
        for (let x = 0; x < size; x++) {
          if (modules[y][x]) {
            path += `M${x + QUIET_ZONE} ${y + QUIET_ZONE}h1v1h-1z`;
          }
        }
      }
      return { extent: size + QUIET_ZONE * 2, path, error: null as string | null };
    } catch {
      // Too long for a version-40 symbol. The string itself is still on screen
      // beside this, and it is the mechanism — the QR is the convenience
      // (ADR-0055, Consequences) — so this says so rather than blanking.
      return { extent: 0, path: "", error: "too long to show as a QR code" };
    }
  }, [text, ecc]);

  if (drawn.error) {
    return (
      <p className="field-hint" data-testid={testId ? `${testId}-error` : undefined}>
        This invite is {drawn.error}. Copy the text instead.
      </p>
    );
  }

  return (
    <svg
      className={className}
      data-testid={testId}
      data-qr-modules={drawn.extent - QUIET_ZONE * 2}
      viewBox={`0 0 ${drawn.extent} ${drawn.extent}`}
      role="img"
      aria-label={label}
      shapeRendering="crispEdges"
    >
      <title>{label}</title>
      <rect width={drawn.extent} height={drawn.extent} fill="#ffffff" />
      <path d={drawn.path} fill="#000000" />
    </svg>
  );
}
