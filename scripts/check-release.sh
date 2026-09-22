#!/usr/bin/env bash
# check-release.sh — the check-release recipe body (Makefile's check-release
# target just execs this). Moved out of the Makefile so `make -n check-release`
# is a real dry run: GNU make ALWAYS executes a recipe line that contains the
# literal text "$(MAKE)", even under -n, and the old inline recipe called
# $(MAKE) five times. This script's own body is invisible to that rule.
#
# It also fixes cleanup-on-kill. MEASURED: GNU make 3.81 CATCHES
# HUP/INT/QUIT/TERM but only FORWARDS SIGTERM to the recipe's direct child —
# `kill -HUP/-INT/-QUIT <make pid alone>` is swallowed and this script never
# sees it (unfixable: the script cannot react to a signal make caught and
# didn't forward). HUP/INT/QUIT work when delivered the way a terminal or
# Ctrl-C delivers them — to the whole process group, which this script is a
# member of and so receives directly, make's own forwarding aside. TERM and
# ALRM (via the watchdog below) work when sent to make's pid because TERM is
# the one signal make does forward, and ALRM never reaches make's fatal-signal
# handling at all (see below). A POSIX shell only runs a trap once its
# FOREGROUND child returns — which never happens if that child is the
# minutes-long test-e2e/Playwright tree. So no step here runs in the
# foreground: the step is backgrounded and the script `wait`s on it. bash
# interrupts a `wait` the instant a trapped signal arrives and runs the trap
# immediately, so the trap can reach the whole tree.
#
# "whole tree" is walked by PID, not by process group: MEASURED — Playwright's
# own webServer (test-e2e's `node e2e/boot-server.mjs`, which builds and boots
# the real obelo binary on :8099) puts itself in a NEW process group once it is
# actually up, on purpose, so Playwright's own teardown can kill it
# independently. A plain `kill -TERM -$pgid` on the step's original group
# stops npm/playwright but leaves that regrouped subtree — obelo included —
# running. `kill_tree` below walks live ppid links (pgrep -P), which
# setpgid/detached never breaks, so it still finds and kills it.
#
# `set -m` and running each step's direct child with stdin from /dev/null put
# every step (npm, playwright, and whatever it forks) in ITS OWN process
# group, separate from make's and this script's. MEASURED this was required,
# not cosmetic: without it, `kill -HUP/-QUIT -<group>` reached npm/playwright
# directly (raw, since neither traps HUP/QUIT) and killed them on the spot,
# which orphaned the not-yet-regrouped webServer/obelo subtree to pid 1 BEFORE
# kill_tree's ppid walk (rooted at step_pid, run after the trap fires) could
# reach it — 3/3 HUP and 1/1 QUIT runs left `node e2e/boot-server.mjs` +
# `obelo` on :8099 even though the trap itself fired every time. With the
# step in its own group, a group-delivered signal now reaches only make and
# this script; the step's tree survives intact until kill_tree walks it, so
# the same ppid-walk path already proven for TERM/INT handles HUP/QUIT too.
#
# SIGKILL cannot be caught, so a `kill -9` on `make` or on this script skips
# all of the above and leaves the tree exactly as it was at the instant of the
# kill — there is no way to survive it.
#
# SIGALRM needs the watchdog below for a reason specific to it: MEASURED (not
# reasoned) — GNU make's own fatal-signal set is HUP/INT/QUIT/TERM only, so
# `kill -ALRM <make pid>` is never one make catches and forwards; make just
# dies on the spot (under 1s), on the OS's own default disposition for
# SIGALRM, and this script never receives anything. A background watchdog
# polls the actual make pid found by find_make_pid below (not this script's
# own $PPID, which on Linux names an intervening /bin/sh -c — see
# watch_parent's comment) and sends itself SIGTERM the first time that pid is
# gone — which reduces "make died of ALRM without telling anyone" to the
# ordinary TERM path above.
#
# MEASURED: because make dies on ALRM the instant it receives it (see above),
# `make check-release` returns rc 142 up to ~8s before this orphaned script
# (still running under the watchdog's 1s poll of make_pid) finishes cleanup and
# actually exits. A caller that treats make's return as "cleanup is done" —
# e.g. checking out index.html or starting another build right after — can
# race this script's own cleanup(). Wait for the tree to go quiet, not just
# for `make` to return.
#
# MEASURED this same "make returns before cleanup is actually done" race
# also applies to every OTHER signal above (TERM/HUP/INT/QUIT), but only on
# Linux: GNU make 3.81 (macOS) forks this script as its own direct child, so
# on macOS `wait`ing on make's pid does block until this script's cleanup()
# has exited. GNU make 4.4.1 (Linux) runs the same VAR=val-prefixed recipe
# line through an intervening `/bin/sh -c`, and that intervening shell dies
# on the forwarded signal without waiting for ITS OWN child (this script) —
# so on Linux, `make check-release` can return while cleanup is still
# running orphaned, for TERM/HUP/INT/QUIT exactly as it already could for
# ALRM. A caller on any platform should wait for the tree to go quiet, not
# just for `make` to return — see scripts/check-release-selftest.sh, which
# polls for the orphaned script rather than trusting make's own wait().
set -uo pipefail
set -m

embed_dir="${EMBED_DIR:-internal/webui/dist}"
skip_amd64="${CHECK_RELEASE_SKIP_AMD64:-}"

# The steps below call the same make binary that invoked this script, not a
# hardcoded "make". This matters when the invoking binary isn't literally
# named "make" (e.g. a `gmake` install). The Makefile hands it over as
# CHECK_RELEASE_MAKE := $(MAKE), set OUTSIDE the recipe and only REFERENCED
# on the recipe line — MEASURED (F007): that survives `make -n` untouched
# (unlike a literal $(MAKE) on the recipe line, which GNU make always
# executes even under -n, the reason this script exists as a separate file
# at all). Run by hand, with no CHECK_RELEASE_MAKE set, this defaults to the
# plain "make" on PATH.
#
# $(MAKE) can carry more than a binary name: MEASURED (D012) on GNU make 3.81
# (macOS) and 4.4.1 (Linux) alike, `make MAKE="gmake -j2" check-release` on
# the command line overrides the MAKE variable's own value to the literal
# text "gmake -j2" — an ordinary command-line variable assignment, nothing
# make-specific about $(MAKE) stops it — and CHECK_RELEASE_MAKE inherits that
# same text verbatim. Split on whitespace into argv words so "gmake -j2"
# execs as two words, not one nonexistent binary literally named with an
# embedded space. The same split makes a make binary whose OWN path contains
# whitespace unsupported (MEASURED: "/…/my dir/mk" execs as "/…/my", rc
# 127) — put such a binary on PATH or behind a symlink instead. A blank or
# whitespace-only value falls back to plain "make" like an unset one.
make_bin="${CHECK_RELEASE_MAKE:-make}"
read -ra make_cmd <<< "$make_bin"
[ "${#make_cmd[@]}" -eq 0 ] && make_cmd=(make)

# Finds the PID of the process actually running $make_bin, for the ALRM
# watchdog (watch_parent below) — needed because on Linux (GNU make 4.4.1)
# $PPID names an intervening /bin/sh -c, not make itself, MEASURED (see
# watch_parent's comment). Walks up to 3 ancestors from $PPID comparing
# `ps -o comm=`'s basename against ${make_cmd[0]}'s own basename (the first
# word of $make_bin — any following words are arguments, never part of the
# binary's own name); NEVER executes a candidate ancestor to ask what it is
# (the earlier approach did, running each candidate with --version —
# rejected once a plain Makefile variable indirection was measured to hand
# over the real binary without one, see D007 in the issue bucket). No match
# — e.g. run by hand, where an intervening shell isn't make at all — leaves
# make_pid empty; watch_parent then never runs, which is harmless: no
# ALRM-to-TERM translation happens, exactly as if there were no watchdog.
find_make_pid() {
  local target base pid="$PPID" cand depth=0
  target=$(basename -- "${make_cmd[0]}")
  while [ "$depth" -lt 3 ] && [ -n "$pid" ] && [ "$pid" != 0 ]; do
    cand=$(ps -o comm= -p "$pid" 2>/dev/null | tr -d ' ')
    base=$(basename -- "$cand")
    if [ -n "$cand" ] && [ "$base" = "$target" ]; then
      printf '%s' "$pid"
      return 0
    fi
    pid=$(ps -o ppid= -p "$pid" 2>/dev/null | tr -d ' ')
    depth=$((depth + 1))
  done
  return 1
}
make_pid=$(find_make_pid) || make_pid=""

status=0
step_pid=""
self_pid=$$

# Kills a process and everything still linked to it by ppid, deepest
# descendants first. This does not prevent orphaning: a child that exits
# mid-walk can leave its own not-yet-visited children reparented to pid 1,
# outside this walk.
kill_tree() {
  local pid="$1" child
  for child in $(pgrep -P "$pid" 2>/dev/null); do
    kill_tree "$child"
  done
  kill -TERM "$pid" 2>/dev/null || true
}

# MEASURED this can't watch for "my $PPID changed" (the original approach,
# and still correct on macOS): on Linux, make's own direct child is the
# intervening /bin/sh -c (see find_make_pid above), and THAT survives an
# ALRM to make untouched — it just sits in its own wait() for this script,
# unaware its own parent died — so this script's $PPID never changes and
# never would. Watch the actual make pid found above instead, on every
# platform; that pid is what SIGALRM kills out from under this script. If
# find_make_pid found no match (e.g. run by hand, $make_pid empty), there is
# nothing to watch — this exits immediately without ever touching self_pid,
# so a missing watchdog target never gets mistaken for "make died".
watch_parent() {
  [ -n "$make_pid" ] || return 0
  while kill -0 "$self_pid" 2>/dev/null; do
    if ! kill -0 "$make_pid" 2>/dev/null; then
      kill -TERM "$self_pid" 2>/dev/null
      break
    fi
    sleep 1
  done
}
watch_parent &
watchdog_pid=$!
disown "$watchdog_pid" 2>/dev/null || true

cleanup() {
  local signo="${1:-}"
  # Ignore (not restore-to-default) further TERM/INT/HUP/QUIT/ALRM here: a
  # SECOND signal arriving mid-cleanup used to hit the default disposition
  # (kill) and could cut `make plugins` off partway through, leaving the
  # gitignored internal/bundled/modules/ partially rebuilt. Cleanup itself is
  # not interruptible; SIGKILL still is (nothing can stop that).
  trap '' TERM INT HUP QUIT ALRM
  kill "$watchdog_pid" 2>/dev/null || true
  if [ -n "$step_pid" ]; then
    kill_tree "$step_pid"
    wait "$step_pid" 2>/dev/null
  fi
  if [ -n "$signo" ]; then
    status=$((128 + signo))
  fi
  git checkout -- "$embed_dir/index.html" || { [ "$status" -eq 0 ] && status=1; }
  "${make_cmd[@]}" --no-print-directory plugins || { [ "$status" -eq 0 ] && status=1; }
  exit "$status"
}
trap 'cleanup 1' HUP
trap 'cleanup 2' INT
trap 'cleanup 3' QUIT
trap 'cleanup 15' TERM
trap 'cleanup 14' ALRM

# Runs "$@" backgrounded and waits for it, so a signal caught during the wait
# can kill_tree the whole thing, not just wait out the direct child.
run_step() {
  "$@" </dev/null &
  step_pid=$!
  wait "$step_pid"
  local rc=$?
  step_pid=""
  return $rc
}

if run_step "${make_cmd[@]}" --no-print-directory plugins \
  && run_step "${make_cmd[@]}" --no-print-directory web \
  && run_step "${make_cmd[@]}" --no-print-directory check-bundle \
  && run_step "${make_cmd[@]}" --no-print-directory test-e2e; then
  if [ -z "$skip_amd64" ]; then
    run_step "${make_cmd[@]}" --no-print-directory check-amd64 || status=$?
  fi
else
  status=$?
fi

cleanup
