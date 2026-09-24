// Where a sign-in returns to afterwards: a path on this server, or Home. A
// protocol-relative "//host" and its backslash form "/\host" start with a slash
// but name another server, so they go Home too. A URL parser drops tab, CR and
// LF before anything else, so "/\t/host" is "//host" by then: the path is judged
// with them gone, by the origin it resolves to.
export function safeReturnPath(path: unknown): string {
  if (typeof path !== "string") return "/";
  const stripped = path.replace(/[\t\r\n]/g, "");
  if (!stripped.startsWith("/")) return "/";
  if (stripped.startsWith("//") || stripped.startsWith("/\\")) return "/";
  let resolved: URL;
  try {
    resolved = new URL(stripped, window.location.origin);
  } catch {
    return "/";
  }
  if (resolved.origin !== window.location.origin) return "/";
  return stripped;
}
