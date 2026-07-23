// Release-type badge (album-release-identity/02, ADR-0038). Same-titled
// releases now split into separate Albums (an LP and its title single), so a
// non-album release type ("single", "ep", …) renders a small flat badge — the
// visible disambiguator between two otherwise identical-looking entries. A
// plain album, or an untagged one, shows nothing.

/** Display label for an Album's releaseType: "" for a plain/untagged album,
 * "EP" all-caps, else the capitalized type ("Single", "Compilation", …). */
export function releaseTypeLabel(releaseType: string): string {
  if (!releaseType || releaseType === "album") return "";
  if (releaseType === "ep") return "EP";
  return releaseType.charAt(0).toUpperCase() + releaseType.slice(1);
}

export function ReleaseTypeBadge({ releaseType }: { releaseType: string }) {
  const label = releaseTypeLabel(releaseType);
  if (!label) return null;
  return (
    <span className="badge badge-release-type" data-testid="release-type-badge">
      {label}
    </span>
  );
}
