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
# polls its own $PPID (bash's $PPID is fixed at shell start, so it stays the
# original make pid even after reparenting) and sends itself SIGTERM the first
# time that parent is gone — which reduces "make died of ALRM without telling
# anyone" to the ordinary TERM path above.
set -uo pipefail
set -m

embed_dir="${EMBED_DIR:-internal/webui/dist}"
skip_amd64="${CHECK_RELEASE_SKIP_AMD64:-}"

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

watch_parent() {
  while kill -0 "$self_pid" 2>/dev/null; do
    current_ppid=$(ps -o ppid= -p "$self_pid" 2>/dev/null | tr -d ' ')
    if [ -n "$current_ppid" ] && [ "$current_ppid" != "$PPID" ]; then
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
  trap - TERM INT HUP QUIT ALRM
  kill "$watchdog_pid" 2>/dev/null || true
  if [ -n "$step_pid" ]; then
    kill_tree "$step_pid"
    wait "$step_pid" 2>/dev/null
  fi
  if [ -n "$signo" ]; then
    status=$((128 + signo))
  fi
  git checkout -- "$embed_dir/index.html" || { [ "$status" -eq 0 ] && status=1; }
  make --no-print-directory plugins || { [ "$status" -eq 0 ] && status=1; }
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

if run_step make --no-print-directory plugins \
  && run_step make --no-print-directory web \
  && run_step make --no-print-directory check-bundle \
  && run_step make --no-print-directory test-e2e; then
  if [ -z "$skip_amd64" ]; then
    run_step make --no-print-directory check-amd64 || status=$?
  fi
else
  status=$?
fi

cleanup
