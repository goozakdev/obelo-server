# An unreadable file is its own attention kind, not an unmatched one

The Unmatched list has always held two failures under one name. One is what the
name says: nothing in the file's name yielded an identity, so the Admin has to
say what the work is. The other is a file whose name parsed perfectly and whose
bytes `ffprobe` refused — a truncated download, a bad remux, a mount that reads
zeroes.

**They are now different kinds on the same list** (`unmatched_files.kind` ∈
`unidentified` | `unreadable`), and only the first may be offered an identity
correction.

## Why one list, two kinds

They belong on one list because they end identically: a recognized media file, on
disk, in no Title, needing a human. Splitting the list would make the Admin ask
which of two places to look before they know what is wrong — the exact failure
the Needs-Fixing queue's collapse into one queue was built to remove.

They cannot be one kind because their fixes are opposites:

| | Unmatched | Unreadable |
|---|---|---|
| What is wrong | the name says nothing | the bytes are broken |
| What fixes it | name the work (fix-match) | replace the file |
| Does a rescan clear it | yes, once named | no, until the file changes |

Told apart only by their reason prose, the unreadable half inherited the
unmatched half's sentence ("Not recognized as a title") and its action (search a
provider, press **Use this**). That action is **inert by construction**: it writes
a folder-keyed identity correction for a file whose identity was never wrong, the
next scan probes the same broken bytes, and the row comes back. The Admin presses
a button that cannot work, on a file the file matcher shows as correctly placed.

## The trap this exists to close

An unreadable file looks *more* correct than everything around it:

- its filename numbers it, so `ResolveEpisodes` — which is pure, and reads names,
  not bytes — places it on its Slot;
- the file matcher renders that placement, so the screen the queue sends the Admin
  to reports the Show as perfectly sorted;
- `assembleTitle` fails on it, so no Episode exists and nothing plays.

Three surfaces agreeing that a file is fine, one silently disagreeing, and the
disagreement is invisible. That is why the flag travels **on a placed file** —
the one state where every other reason is suppressed as noise — and why the
matcher says so on the Slot itself.

## Decisions

- **`kind` is a column, not a prefix on `reason`.** Every layer above branches on
  it; none of them parses English to decide whether to offer a fix.
- **The server withholds the fix-match anchor** (`folderPath`) for an unreadable
  row, so the inert offer cannot be assembled by a client at all, not merely
  discouraged in one.
- **The queue does not fold it into its Show row.** Absorbing a path into a Show
  row is a promise that the file matcher can settle it (ADR-0044, file-matcher/07),
  and the matcher cannot settle this one — it shows it as already placed. Folded
  in, the library's one broken file would disappear behind a screen reporting it
  as fine. It keeps its own row.
- **`ffprobe` runs at `-v error`, not `-v quiet`,** and its last stderr line — its
  verdict, with the path stripped — becomes the reason the Admin reads. "Invalid
  data found when processing input" says replace the file; "Permission denied"
  says fix the mount; "exit status 1" said nothing at all.
- **Ignoring it still settles it.** An Ignored File is never probed
  (scanner/arrangement.go), so the row stops being generated. That is the escape
  hatch for a file the Admin does not intend to replace, and it needs no new
  mechanism.
- **A refused file is NOT counted as `seen` by the walk**, so the soft-delete pass
  marks its stored row Missing. This is the fix for the second, worse half of the
  same bug — see below.

## The duplicate Show, which is the same bug

`sc.seen` drives soft-delete, and it used to be stamped *before* the probe, so a
file ffprobe refused stayed `present=1` forever. That row kept its Episode
visible; the Episode kept its Show visible. Ordinarily invisible — until the
identity of the folder CHANGES.

An Admin who fixes a Show's identity from the queue re-keys the whole work: the
scan builds a new Show row and every Episode under it, reclaiming each File by
path. Every path except the one no Title could be built from. That path stayed
where it was, holding its old Episode, holding the ORIGINAL Show row open:

```
one folder on disk    →    tmdb:228079                13 episodes
                           the marlow murder club|0    3 episodes   ← held open by files
                                                                      the scanner had given up on
```

Two Shows in the grid for one folder, and nothing in the UI can delete a Show —
they only disappear when their Files do. The corrupt file was propping up a Show
that no longer existed.

Marking it Missing is the honest answer, and the narrow one: `seen` means "this
walk produced a live catalog row for this path", the file is one the server
cannot serve either way, and the soft-delete reverses itself the moment a scan
can read the file again (ADR-0008). A cancelled scan cannot trigger it — the
soft-delete pass never runs on that path.

## What was considered and rejected

- **Leave it on the Unmatched list unchanged and improve the reason text.** The
  prose was never the whole problem: the row's *action* was wrong, and a clearer
  sentence above an inert button is a better-explained dead end.
- **Give unreadable files a list of their own.** Rejected: two lists to check
  before knowing which one a file is on, for a distinction the server can carry
  as a field.
- **Delete the Title / hide the file.** Rejected: a corrupt file is exactly the
  thing an Admin must be told about. Silently dropping it is how a season ends up
  one episode short with nothing anywhere saying why.
- **Have the scanner retry or repair.** Out of scope and not the server's business
  (ADR-0002): nothing on disk is ever rewritten.

## Consequences

- A file that was readable and later rotted is **not** re-probed by an incremental
  scan while its size and mtime are unchanged, so it keeps its Title until it
  changes on disk or a full rescan re-probes it. That is the incremental
  contract, unchanged here, and worth knowing when a row seems late to appear.
- **An existing stranded Show heals itself on the next full scan**: its files go
  Missing, its Episodes hide, and `RecomputeHiddenShows` drops it from the grid.
  No migration and no manual deletion.
- **An Episode whose only file is unreadable disappears from browse** rather than
  sitting there failing at playback. It is still listed in the file matcher and
  the queue — the two places whose job is to name work — via the Unmatched row,
  which is what keeps "ignore it" reachable.
- `scanner.ProbeError` and `scanner.UnreadableError` are the seam: a Prober
  reports *which* paths it refused and *why*, so a caller can mark exactly the
  broken file and not the readable sibling that merely shared an Episode with it.
