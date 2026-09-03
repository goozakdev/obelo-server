// Render-time helpers for the API's RFC3339 timestamps (PRD user story 35:
// timestamps shown in the user's local time). The wire layer keeps the raw
// string; these parse it to the browser's locale/zone for display. Kept tiny
// and dependency-free.

/** Format an RFC3339 timestamp as a local date (e.g. "Jun 22, 2026"). Returns
 * "" for an absent or unparseable value, so a missing `addedAt` renders as
 * nothing rather than "Invalid Date". */
export function formatDate(rfc3339: string | undefined | null): string {
  const d = parse(rfc3339);
  if (!d) return "";
  return d.toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

/** Format an RFC3339 timestamp as a local date + time. Returns "" when absent
 * or unparseable. */
export function formatDateTime(rfc3339: string | undefined | null): string {
  const d = parse(rfc3339);
  if (!d) return "";
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}

/** How long ago an RFC3339 instant was, in one short phrase: "just now",
 * "12m ago", "2h ago", "3d ago". Returns "" for an absent or unparseable value.
 *
 * Coarse ON PURPOSE, and never a clock time. It answers a liveness question —
 * is the other household's Server still talking to us? — where "2h ago" and
 * "2h 14m ago" mean exactly the same thing to the person reading it, and the
 * exact instant would invite a precision the Device's last-seen does not have
 * (it moves on every request, so it is "recently" or "not lately").
 *
 * A timestamp in the future (a clock skewed between two households, which is
 * exactly the situation linking creates) reads as "just now" rather than as a
 * negative age. */
export function formatAgo(
  rfc3339: string | undefined | null,
  now: number = Date.now(),
): string {
  const d = parse(rfc3339);
  if (!d) return "";
  const seconds = Math.floor((now - d.getTime()) / 1000);
  if (seconds < 60) return "just now";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

/** Format a duration in milliseconds as "1h 58m" / "47m" / "" (for 0/absent).
 * Used for File durations on the detail page. */
export function formatDuration(ms: number | undefined | null): string {
  if (!ms || ms <= 0) return "";
  const totalMinutes = Math.round(ms / 60000);
  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${minutes}m`;
}

/** A resume position in ms as "resume at 12:34" style mm:ss / h:mm:ss. */
export function formatTimecode(ms: number | undefined | null): string {
  if (!ms || ms <= 0) return "0:00";
  const totalSeconds = Math.floor(ms / 1000);
  const seconds = totalSeconds % 60;
  const minutes = Math.floor(totalSeconds / 60) % 60;
  const hours = Math.floor(totalSeconds / 3600);
  const ss = String(seconds).padStart(2, "0");
  if (hours > 0) return `${hours}:${String(minutes).padStart(2, "0")}:${ss}`;
  return `${minutes}:${ss}`;
}

function parse(rfc3339: string | undefined | null): Date | null {
  if (!rfc3339) return null;
  const d = new Date(rfc3339);
  if (Number.isNaN(d.getTime())) return null;
  return d;
}
