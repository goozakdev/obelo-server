#!/usr/bin/env bash
#
# The differential run: this build against the 2026-09-17 build, end to end.
#
# It proves two of the bundled-plugins PRD's success criteria that issue 08
# could only argue (.scratch/bundled-plugins/PRD.md, "Success criteria"):
#
#   A. a fresh server "enriches a movie and an album on the first pass exactly
#      as the 2026-09-17 build does"
#   B. "an existing server upgraded across issue 08 keeps every provider key,
#      every per-Library override and every item pin; the Cover Art Archive row
#      is gone; nothing re-enriches"
#
# Everything it needs it builds itself, into a temp directory it also cleans up:
# the 2026-09-17 binary out of a THROWAWAY CLONE checked out at cf5da34, this
# tree's binary, the stand-in provider server and the driver. It never writes to
# bin/, never touches the developer's data directory, never binds a fixed port,
# and never modifies a tracked file. The only thing it writes inside the tree is
# internal/bundled/modules/, which is gitignored build output and which `make
# plugins` writes anyway.
#
# It is NOT part of `make check`: it needs a second checkout and two full Go
# builds, which is minutes rather than seconds, and `check` is a pre-commit gate.
#
# Usage:  make differential            (or: scripts/differential/run.sh)
#         DIFFERENTIAL_KEEP=1 make differential    keep the work dir on success
#         DIFFERENTIAL_BASE=<sha> make differential  compare against another build

set -euo pipefail

BASE_COMMIT="${DIFFERENTIAL_BASE:-cf5da34}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

if ! command -v ffmpeg >/dev/null 2>&1; then
  echo "ERROR: ffmpeg is not on PATH, and the fixture library is generated with it." >&2
  echo "       This FAILS rather than skipping: a differential that silently compared" >&2
  echo "       two empty libraries would report green having proven nothing." >&2
  exit 1
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/obelo-differential-XXXXXX")"
echo "differential: work directory $WORK"

cleanup() {
  status=$?
  if [ "$status" -eq 0 ] && [ -z "${DIFFERENTIAL_KEEP:-}" ]; then
    rm -rf "$WORK"
  else
    echo "differential: work directory kept at $WORK"
  fi
}
trap cleanup EXIT

# --- the 2026-09-17 binary, from a throwaway clone ---------------------------
#
# A clone rather than a second worktree or a `git stash`: a worktree shares the
# repository's index and HEAD bookkeeping with whatever the developer is doing,
# and the stash stack is shared with every other session. A clone into $TMPDIR
# is a separate repository that can be deleted without consequence.
#
# It MUST live outside the tree. `go.work` is discovered by walking UP from the
# build directory, so a clone under the worktree would be pulled into this
# tree's workspace and fail to resolve its own main module.
echo "differential: cloning $BASE_COMMIT into $WORK/old-src"
git clone --quiet --no-checkout "$REPO_ROOT" "$WORK/old-src"
( cd "$WORK/old-src" && git checkout --quiet "$BASE_COMMIT" )
echo "differential: building the 2026-09-17 binary"
GOWORK=off go build -C "$WORK/old-src" -o "$WORK/obelo-old" ./cmd/obelo

# --- this tree's binary ------------------------------------------------------
echo "differential: building this tree's binary"
go build -C "$REPO_ROOT" -o "$WORK/obelo-new" ./cmd/obelo

# --- the harness itself ------------------------------------------------------
go build -C "$REPO_ROOT" -o "$WORK/standin" ./scripts/differential/standin
go build -C "$REPO_ROOT" -o "$WORK/driver" ./scripts/differential/driver

mkdir -p "$WORK/run"
"$WORK/driver" \
  -old "$WORK/obelo-old" \
  -new "$WORK/obelo-new" \
  -standin "$WORK/standin" \
  -work "$WORK/run"
