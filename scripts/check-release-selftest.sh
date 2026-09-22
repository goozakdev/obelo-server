#!/usr/bin/env bash
# check-release-selftest.sh — drives the REAL scripts/check-release.sh
# against a disposable stub repo whose Makefile imitates the two hazards
# check-release's cleanup has to survive: `web` overwriting the tracked
# placeholder embed, and `test-e2e` regrouping a grandchild into its own
# process group the way Playwright's webServer does. See D005 in
# .claude/scratch/issue-10-gate-residue/DECISIONS.md.
#
# Runnable on the macOS host and inside `docker run --rm -v "$PWD":/src:ro
# golang:1.26 bash /src/scripts/check-release-selftest.sh` (copy the repo
# in first — see the docker invocation this file's own header comment in
# the issue bucket documents). No CI wiring, no e2e, no web build: seconds,
# not minutes.
#
# Each case starts the run in its own process group (a signal aimed at it
# can never land on this self-test's own shell) and asserts: the script's
# OWN real exit status (recorded by the stub Makefile's recipe, not a
# literal guess), index.html restored to the placeholder, no process of the
# stub tree left alive, and that cleanup's `plugins` rebuild produced one
# MORE completion than the run's own initial `plugins` step alone (proof
# the rebuild actually finished, not merely started).
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
cr_sh="${CR_SH:-$here/check-release.sh}"

pass=0
fail=0

# Fed into every run's stdin (m8 catch, see stub/e2e-step.sh below): must
# match the literal string that stub compares against.
stdin_marker="OBELO-STDIN-MARKER"

log() { printf '%s\n' "$*" >&2; }

# ---- stub repo ---------------------------------------------------------

make_stub_repo() {
  local dir
  dir=$(mktemp -d "${TMPDIR:-/tmp}/cr-selftest.XXXXXX")
  mkdir -p "$dir/internal/webui/dist" "$dir/stub"
  printf 'PLACEHOLDER\n' > "$dir/internal/webui/dist/index.html"

  cat > "$dir/stub/e2e-grandchild.sh" <<'EOF'
#!/usr/bin/env bash
echo "$$" >> "$1"
exec sleep 300
EOF
  cat > "$dir/stub/e2e-regroup.sh" <<'EOF'
#!/usr/bin/env bash
echo "$$" >> "$1"
"$(dirname "$0")/e2e-grandchild.sh" "$1" &
echo "$!" >> "$1"
wait
EOF
  cat > "$dir/stub/e2e-step.sh" <<'EOF'
#!/usr/bin/env bash
# Observes its own stdin: catches a run_step that forgot the
# `</dev/null` redirect check-release.sh:211 requires (mutant m8). A short,
# bounded read — never a leak with the real redirect in place, since /dev/null
# gives an immediate EOF, not a hang — so this never slows an unmutated run.
if IFS= read -r -t 2 line && [ "$line" = "OBELO-STDIN-MARKER" ]; then
  echo stdin-leaked > stdin-leaked
fi
# Imitates Playwright's webServer: regroups into a NEW process group before
# forking a grandchild that outlives this step — the same shape
# check-release.sh's ppid-walking kill_tree has to survive. Only the
# signal-injection cases need this to hang around; a plain pass/fail case
# just wants test-e2e to return.
if [ -z "${SELFTEST_E2E_SLOW:-}" ]; then
  echo done > e2e.done
  exit 0
fi
echo "$$" >> "$1"
perl -e 'setpgrp(0,0); exec @ARGV' "$(dirname "$0")/e2e-regroup.sh" "$1" &
echo "$!" >> "$1"
wait
EOF
  # Runs $CR_TARGET (the real check-release.sh) and records its REAL exit
  # status to status.txt, without adding a ';'-joined compound recipe line:
  # a compound line forces even GNU make 3.81 (macOS) through an intervening
  # /bin/sh -c, which — like the Linux /bin/sh -c this script's cleanup
  # asynchrony comments already describe — dies on a forwarded TERM without
  # waiting for its own child, so it would never reach the "record rc"
  # step at all. Keeping the recipe a single simple command (this wrapper)
  # preserves the direct-exec-as-make's-child topology the existing
  # HUP/INT/QUIT/TERM cases depend on, and the wrapper itself explicitly
  # relays a signal make forwards to it (TERM, or one it receives directly
  # as a process-group member) down to the real script, since ignoring a
  # trapped signal here does not otherwise propagate it to a child. Target
  # is passed via env ($CR_TARGET), not argv, so ps can tell this wrapper's
  # own process apart from the real script's (both run under bash, and the
  # real script's own path is what identifies its process — see
  # wait_for_script below).
  cat > "$dir/stub/run-and-record.sh" <<'EOF'
#!/usr/bin/env bash
cr_pid=""
forward() { [ -n "$cr_pid" ] && kill "-$1" "$cr_pid" 2>/dev/null; }
trap 'forward TERM' TERM
trap 'forward INT' INT
trap 'forward HUP' HUP
trap 'forward QUIT' QUIT
# Started with `&` from this non-interactive shell, the script would inherit
# SIGINT and SIGQUIT IGNORED (POSIX: asynchronous lists in a shell without job
# control), and bash cannot trap a signal that was ignored on entry — so the
# int-/quit-to-process-group cases would never reach its trap. Reset both to
# default in front of it.
# The same "asynchronous list in a shell without job control" POSIX rule
# above also redirects an async command's OWN stdin from /dev/null before any
# explicit redirection — which would silently defeat the m8 stdin-leak case
# below no matter what check-release.sh does, since the marker would never
# get past this wrapper. An explicit `<&0` redirection on the async command
# itself suppresses that (POSIX: only applies "before any explicit
# redirections"); MEASURED it does NOT also give cr_pid its own process
# group the way enabling job control (`set -m`) would have — the
# group-signal cases below still need cr_pid to stay in the SAME group as
# this wrapper.
perl -e '$SIG{INT} = "DEFAULT"; $SIG{QUIT} = "DEFAULT"; exec @ARGV' "$CR_TARGET" <&0 &
cr_pid=$!
# A trapped signal caught DURING `wait` makes bash's `wait` return early with
# a 128+signal pseudo-status of its own — the real script is still running
# its cleanup at that point, not actually exited yet. Re-wait until cr_pid
# is truly gone so $rc is the script's own real exit status, not "wait was
# interrupted".
rc=""
while :; do
  wait "$cr_pid"
  rc=$?
  kill -0 "$cr_pid" 2>/dev/null || break
done
echo "$rc" > status.txt
exit "$rc"
EOF
  chmod +x "$dir"/stub/*.sh

  cat > "$dir/Makefile" <<'EOF'
.PHONY: plugins web check-bundle test-e2e check-release
plugins:
	@echo run >> plugins.log
	@if [ -n "$$SELFTEST_PLUGINS_SLOW" ] && [ -s plugins.completions ]; then \
		touch plugins-cleanup-started; \
		sleep 3; \
	else \
		sleep 0.4; \
	fi
	@echo done > plugins.done
	@echo done >> plugins.completions

web:
	@echo real-bundle > internal/webui/dist/index.html

check-bundle:
	@test ! -f force-check-bundle-fail
	@echo done > check-bundle.done

test-e2e:
	@./stub/e2e-step.sh pids.txt
EOF
  # CHECK_RELEASE_MAKE := $(MAKE), set outside the recipe and only referenced
  # on it, matches the real Makefile's own mechanism exactly (needed so
  # case_check_release_make_with_args below actually exercises it — the
  # invoking `$(MAKE)` value, arguments included, only reaches
  # check-release.sh through this forwarding).
  {
    # shellcheck disable=SC2016 # literal Makefile text for the stub's own
    # Makefile, not a shell expansion — $(MAKE) must reach the file as-is.
    printf '\nCHECK_RELEASE_MAKE := $(MAKE)\n'
    printf 'check-release:\n'
    # shellcheck disable=SC2016 # same: $(CHECK_RELEASE_MAKE) is Makefile
    # variable syntax for the generated recipe line, not meant to expand here.
    printf '\t@CHECK_RELEASE_SKIP_AMD64=1 EMBED_DIR=internal/webui/dist CR_TARGET=%s CHECK_RELEASE_MAKE="$(CHECK_RELEASE_MAKE)" ./stub/run-and-record.sh\n' "$cr_sh"
  } >> "$dir/Makefile"

  ( cd "$dir" \
    && git init -q \
    && git config user.email selftest@example.com \
    && git config user.name selftest \
    && git add internal/webui/dist/index.html \
    && git commit -q -m placeholder )

  printf '%s' "$dir"
}

# ---- small bounded waits (not open-ended loops: each has a timeout) ----

# On failure, sets $wfl_seen/$wfl_elapsed to what was actually observed (line
# count, whole seconds waited) so a caller's failure message can report the
# real numbers instead of guessing (D041).
wait_for_lines() { # file min-lines timeout-seconds
  local f="$1" n="$2" t="$3" i=0 t0=$SECONDS
  while [ "$i" -lt "$((t * 10))" ]; do
    if [ -f "$f" ] && [ "$(wc -l < "$f" 2>/dev/null || echo 0)" -ge "$n" ]; then
      return 0
    fi
    sleep 0.1
    i=$((i + 1))
  done
  wfl_seen=$([ -f "$f" ] && wc -l < "$f" 2>/dev/null || echo 0)
  wfl_seen=$(printf '%s' "$wfl_seen" | tr -d ' ')
  # Wall-clock, not the ceiling: each poll is a fork plus 0.1 s, so under
  # load the loop overruns $t by exactly the latency this wait exists to
  # survive (measured +24% under 16 nice'd loops).
  wfl_elapsed=$((SECONDS - t0))
  return 1
}

# Finds the process actually running $cr_sh under $1 (make's own pid),
# printing its pid on stdout. Not just the direct child: MEASURED — GNU
# make 3.81 (macOS) runs the recipe as a direct child, but GNU make 4.4.1
# (Linux) routes the same recipe through an intervening `/bin/sh -c`, so the
# actual script is a GRANDCHILD there. Walk down looking for it by name
# rather than assuming a fixed depth.
wait_for_script() { # make-pid timeout-seconds -> prints script pid on stdout
  local root="$1" t="$2" i=0 frontier next c comm cmd
  while [ "$i" -lt "$((t * 10))" ]; do
    frontier="$root"
    while [ -n "$frontier" ]; do
      next=""
      for c in $frontier; do
        # Match by comm=bash AND by $cr_sh's own path, not a hardcoded
        # "check-release.sh" substring: the intervening `/bin/sh -c '...
        # check-release.sh'` wrapper's own args ALSO contain the plain
        # filename (it's literally what sh -c is asked to run), so a
        # substring match on that filename alone finds the wrapper, never
        # the script one level below it — and it would silently stop
        # working for a mutant copy under a different name driven via
        # CR_SH, which must still resolve correctly.
        comm=$(ps -o comm= -p "$c" 2>/dev/null | tr -d ' ')
        cmd=$(ps -o args= -p "$c" 2>/dev/null)
        case "$comm/$cmd" in
          bash/*"$cr_sh"*) printf '%s' "$c"; return 0 ;;
        esac
        next="$next $(pgrep -P "$c" 2>/dev/null)"
      done
      frontier="$next"
    done
    sleep 0.1
    i=$((i + 1))
  done
  return 1
}

wait_for_gone() { # pid timeout-seconds
  local p="$1" t="$2" i=0
  while [ "$i" -lt "$((t * 10))" ]; do
    kill -0 "$p" 2>/dev/null || return 0
    sleep 0.1
    i=$((i + 1))
  done
  return 1
}

# Bounded stand-in for a plain `wait "$runner_pid"`: a signal that never
# lands (delivery is not guaranteed, and this suite deliberately provokes
# it) must not block this suite forever. Reaps the job once it is actually
# gone; gives up (leaving it unreaped, harmless — teardown_case's own
# TERM+wait_for_gone below still gets a chance) after the ceiling.
reap_runner() { # pid timeout-seconds
  local p="$1" t="$2" i=0
  while [ "$i" -lt "$((t * 10))" ]; do
    if ! kill -0 "$p" 2>/dev/null; then
      wait "$p" 2>/dev/null
      return 0
    fi
    sleep 0.1
    i=$((i + 1))
  done
  return 1
}

# ---- case scaffolding ----------------------------------------------------

repo=""
runner_pid=""

setup_case() {
  repo=$(make_stub_repo)
  runner_pid=""
}

teardown_case() {
  # Only signal if $runner_pid is still alive: by the time a case reaches
  # here it has already been wait(2)'d, and the OS is free to recycle a
  # reaped pid — signalling a stale, reused pid/group would be a bug, not a
  # safety net.
  if [ -n "$runner_pid" ] && kill -0 "$runner_pid" 2>/dev/null; then
    kill -TERM -- "-$runner_pid" 2>/dev/null
    wait_for_gone "$runner_pid" 10
  fi
  [ -n "$repo" ] && rm -rf "$repo"
}

# start=("cmd" "args"...) run in the stub repo, own process group, backgrounded.
# Used only by the signal-injection cases, which need test-e2e to hang
# around long enough to be interrupted mid-step. Sets $runner_pid.
#
# MEASURED (F014): a job started with `&` from a shell WITHOUT job control
# (this one) inherits SIGINT and SIGQUIT already IGNORED, and bash cannot
# trap a signal that was ignored on entry — int-to-process-group and
# quit-to-process-group would otherwise fail every run, unmutated, for a
# reason having nothing to do with check-release.sh. Reset both to their
# default disposition in the launcher, before setpgrp/exec, so the real
# script's own `trap ... INT`/`trap ... QUIT` (installed after exec, in a
# fresh process image) can take.
start_run() {
  ( cd "$repo" && export SELFTEST_E2E_SLOW=1 SELFTEST_PLUGINS_SLOW="${SELFTEST_PLUGINS_SLOW:-}" \
    && exec perl -e '$SIG{INT}="DEFAULT"; $SIG{QUIT}="DEFAULT"; setpgrp(0,0); exec @ARGV' -- "$@" ) <<<"$stdin_marker" &
  runner_pid=$!
}

assert_case() { # name want-status want-completions
  local name="$1" want_status="$2" want_completions="${3:-2}" ok=1 reason=""
  local rc=""
  # Bounded poll, not a single check: by the time a caller has confirmed the
  # script pid is gone (wait_for_gone, kill-0-based), the wrapper's own
  # reaping `wait` has already returned — but the wrapper still has to reach
  # its `echo "$rc" > status.txt` line after that, and nothing makes that
  # instantaneous. See F016 in the issue bucket for the confirmed repro.
  if wait_for_lines "$repo/status.txt" 1 15; then
    rc=$(cat "$repo/status.txt")
  else
    ok=0; reason="no-status.txt"
  fi
  if [ -f "$repo/stdin-leaked" ]; then
    ok=0; reason="$reason stdin-leaked"
  fi
  if [ -n "$rc" ] && [ "$rc" != "$want_status" ]; then
    ok=0; reason="$reason rc=$rc want $want_status"
  fi
  local content
  content=$(cat "$repo/internal/webui/dist/index.html" 2>/dev/null)
  if [ "$content" != "PLACEHOLDER" ]; then
    ok=0; reason="$reason index.html=$content"
  fi
  # plugins.done is only written by the FIRST plugins step; it says nothing
  # about cleanup's own rebuild. Count completions instead: one from the
  # run's initial plugins step plus one more from cleanup's rebuild is the
  # only way to reach $want_completions.
  local completions
  if [ -f "$repo/plugins.completions" ]; then
    completions=$(wc -l < "$repo/plugins.completions" | tr -d ' ')
  else
    completions=0
  fi
  if [ "$completions" -ne "$want_completions" ]; then
    ok=0; reason="$reason plugins-completions=$completions want $want_completions"
  fi
  if [ -f "$repo/pids.txt" ]; then
    local p
    while read -r p; do
      [ -z "$p" ] && continue
      # A signal was already delivered and waited on by the time we get
      # here, but the kernel still needs a scheduling tick to deliver it to
      # the target — give it up to 5s (not an open-ended wait; longer under
      # QEMU-emulated Linux, where this needs much more slack than on bare
      # metal) before calling it a real leak.
      if ! wait_for_gone "$p" 5; then
        ok=0; reason="$reason pid-$p-still-alive"
      fi
    done < "$repo/pids.txt"
  fi
  if [ "$ok" -eq 1 ]; then
    echo "PASS $name"
    pass=$((pass + 1))
  else
    echo "FAIL $name -$reason"
    fail=$((fail + 1))
  fi
}

# ---- cases ----------------------------------------------------------------

case_normal() {
  setup_case
  ( cd "$repo" && make --no-print-directory check-release ) >/dev/null 2>&1 <<<"$stdin_marker"
  assert_case "normal-run-exits-0" 0
  teardown_case
}

# CHECK_RELEASE_MAKE (Makefile: `CHECK_RELEASE_MAKE := $(MAKE)`) really does
# carry the invoking binary's own name, not a hardcoded "make": invoke the
# stub repo's check-release through a `gmake` symlink to the real make and
# confirm the run still completes normally (steps and cleanup's plugins
# rebuild both call "$make_bin", which is "gmake" here).
case_differently_named_make() {
  setup_case
  local bindir real_make
  bindir=$(mktemp -d)
  # On macOS, `command -v make` is a thin xcrun-dispatch wrapper that looks
  # up the real binary again BY THE INVOKED NAME — symlinking that as
  # "gmake" makes it look for a "gmake" toolchain component and fail, not
  # exercise the code under test. Resolve past it where present.
  real_make=$(xcrun -f make 2>/dev/null || command -v make)
  ln -s "$real_make" "$bindir/gmake"
  ( cd "$repo" && PATH="$bindir:$PATH" gmake --no-print-directory check-release ) >/dev/null 2>&1 <<<"$stdin_marker"
  assert_case "differently-named-make" 0
  rm -rf "$bindir"
  teardown_case
}

# CHECK_RELEASE_MAKE can carry the invoking $(MAKE)'s own arguments, not
# just a binary name (see check-release.sh:97-109) — MEASURED: `make
# MAKE="make --no-print-directory" ...` on the command line overrides
# $(MAKE) to that literal two-word text on both GNU make 3.81 (macOS) and
# 4.4.1 (Linux); plain `MAKE=...` in the environment does NOT do this (a
# command-line variable assignment is required to override a variable a
# makefile itself also sets). Confirms check-release.sh's word-split execs
# that as two argv words, not one nonexistent binary named with an embedded
# space.
case_check_release_make_with_args() {
  setup_case
  ( cd "$repo" && make MAKE="make --no-print-directory" --no-print-directory check-release ) \
    >/dev/null 2>&1 <<<"$stdin_marker"
  assert_case "check-release-make-with-args" 0
  teardown_case
}

case_failing_step_keeps_status() {
  setup_case
  : > "$repo/force-check-bundle-fail"
  ( cd "$repo" && make --no-print-directory check-release ) >/dev/null 2>&1
  local rc=$?
  # GNU make's own exit status for "a recipe failed" is the conventional 2,
  # not the underlying command's own exit code. That's the OUTER make's
  # verdict on the recipe line; check separately (via status.txt, inside
  # assert_case) that the failing step's own status — the nested
  # `$make_bin check-bundle` invocation's exit code, which check-release.sh
  # propagates untouched as its own exit status — is that same 2, not just
  # "make said it failed".
  if [ "$rc" -ne 2 ]; then
    echo "FAIL failing-step-keeps-status -rc=$rc want 2"
    fail=$((fail + 1))
  else
    assert_case "failing-step-keeps-status" 2
  fi
  teardown_case
}

# Signal a run mid test-e2e (after the stub has regrouped AND forked its
# grandchild, i.e. pids.txt has its first three lines) and assert cleanup
# still finished with the exact status the signalled cleanup() computes
# (128+signo — see check-release.sh), not just "some nonzero value".
run_and_signal() { # name sig target(pid|group|script-pid) want-status
  local name="$1" sig="$2" target="$3" want_status="$4"
  setup_case
  start_run make --no-print-directory check-release
  # 60s ceiling, not 15s: builder-01 (D004, FINDINGS.md) measured ~19-24x
  # wall-clock inflation under 16-core saturation with flat CPU time — pure
  # scheduling latency, not a hang — for a chain whose unloaded start-up is
  # a few seconds; the early-exit poll keeps a healthy run just as fast.
  if ! wait_for_lines "$repo/pids.txt" 3 60; then
    echo "FAIL $name -e2e-did-not-start ($wfl_seen lines after ${wfl_elapsed}s)"
    fail=$((fail + 1))
    teardown_case
    return
  fi
  # Always resolve script_pid: MEASURED — make's own return is not a
  # reliable "cleanup is done" signal for ANY of these signals, not just
  # ALRM. On macOS (GNU make 3.81, no shell layer) `wait $runner_pid`
  # usually already blocks until the script's cleanup has finished. On
  # Linux (GNU make 4.4.1 routes the recipe through an intervening
  # /bin/sh -c) that intervening shell dies without waiting for its own
  # child, so make returns while check-release.sh's cleanup is still
  # running orphaned in the background — the same asynchrony
  # check-release.sh's own header documents for ALRM specifically, just
  # from a different cause. wait(2) can't reap a non-child, so poll for the
  # script to actually exit before asserting, on every platform.
  local script_pid
  script_pid=$(wait_for_script "$runner_pid" 15) || {
    echo "FAIL $name -could-not-find-script-pid"
    fail=$((fail + 1))
    teardown_case
    return
  }
  case "$target" in
    pid) kill "-$sig" "$runner_pid" ;;
    group) kill "-$sig" -- "-$runner_pid" ;;
    script-pid) kill "-$sig" "$script_pid" ;;
  esac
  reap_runner "$runner_pid" 30 || true
  if ! wait_for_gone "$script_pid" 20; then
    echo "FAIL $name -orphaned-script-never-exited"
    fail=$((fail + 1))
    teardown_case
    return
  fi
  assert_case "$name" "$want_status"
  teardown_case
}

# ---- run everything ---------------------------------------------------

echo "bash: $(bash --version | head -1)"
echo "make: $(make --version | head -1)"

case_normal
case_differently_named_make
case_check_release_make_with_args
case_failing_step_keeps_status
run_and_signal "term-to-make-pid"        TERM pid          143
run_and_signal "alrm-to-make-pid"        ALRM pid          143
run_and_signal "hup-to-process-group"    HUP  group        129
run_and_signal "int-to-process-group"    INT  group        130
run_and_signal "quit-to-process-group"   QUIT group        131
run_and_signal "hup-to-script-pid"       HUP  script-pid   129

# A second, DIFFERENT signal during cleanup's own plugins rebuild directly
# exercises what D010's `trap ''` (vs a `trap -` revert, m2) controls.
# MEASURED (F015): bash holds a second delivery of the SAME signal until the
# running trap handler returns, so TERM-then-TERM can't tell `trap - TERM`
# from `trap '' TERM` apart — the slow cleanup child completes either way.
# A DIFFERENT second signal (HUP, or INT) differs deterministically instead:
# cleanup()'s `trap '' TERM INT HUP QUIT ALRM` (real code) IGNORES it at the
# kernel level regardless of which of the five it is, so the plugins rebuild
# runs to completion and the script exits 143 (from the original TERM) with
# completions already at 2. A `trap -` revert restores HUP/INT's own default
# (terminate) disposition, which the kernel applies to the script process
# immediately — no trap-polling involved — killing it mid-rebuild (rc
# 128+signal, completions still 1 at that instant; the now-orphaned `make
# plugins` finishes on its own moments later, which is why completions must
# be read at the instant the script's own exit is observed, not after any
# extra delay). SELFTEST_PLUGINS_SLOW (make_stub_repo) makes the stub
# `plugins` target detect it is running as the cleanup-phase rebuild (the
# completions file already holds the run's first line) and drop a
# plugins-cleanup-started marker before its ~3s sleep, so the second signal
# is sent only once that window has actually opened — no calibrated delay,
# no deep process chain.
second_signal_during_cleanup() { # name second-sig
  local name="$1" sig2="$2"
  setup_case
  SELFTEST_PLUGINS_SLOW=1 start_run make --no-print-directory check-release
  # Same 60s ceiling and basis as run_and_signal above: same stub chain
  # start-up gate, same scheduler-latency evidence (D004/D041).
  if ! wait_for_lines "$repo/pids.txt" 3 60; then
    echo "FAIL $name -e2e-did-not-start ($wfl_seen lines after ${wfl_elapsed}s)"
    fail=$((fail + 1))
    teardown_case
    return
  fi
  local script_pid
  script_pid=$(wait_for_script "$runner_pid" 15) || {
    echo "FAIL $name -could-not-find-script-pid"
    fail=$((fail + 1))
    teardown_case
    return
  }
  kill -TERM "$script_pid"
  if ! wait_for_lines "$repo/plugins-cleanup-started" 0 15; then
    echo "FAIL $name -cleanup-plugins-never-started"
    fail=$((fail + 1))
    teardown_case
    return
  fi
  kill "-$sig2" "$script_pid" 2>/dev/null
  reap_runner "$runner_pid" 30 || true
  if ! wait_for_gone "$script_pid" 20; then
    echo "FAIL $name -orphaned-script-never-exited"
    fail=$((fail + 1))
    teardown_case
    return
  fi
  assert_case "$name" 143
  teardown_case
}
second_signal_during_cleanup "term-then-hup-during-cleanup" HUP
second_signal_during_cleanup "term-then-int-during-cleanup" INT

echo "TOTAL: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
