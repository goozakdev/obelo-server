// A vitest reporter whose entire job is to make a failing run NAME ITSELF in an
// artifact that survives the terminal.
//
// Why this exists (.scratch/web-app/issues/08-...): the web suite failed twice,
// each time reporting `1 failed | 774 passed`, and WHICH test failed is gone —
// both runs were captured with a summary-only grep and the scrollback was lost.
// A flake you cannot name is a flake you cannot fix. So every run, pass or fail,
// writes web/test-results/vitest-last-run.txt with the failing test's full name,
// its file, its duration and its error, plus the slowest tests in the run (the
// working hypothesis is a timing-sensitive assertion losing a race under load,
// and "which tests were closest to their timeout" is the evidence for it).
//
// It also re-prints the failing names as the LAST thing on stdout, after the
// default reporter's summary, so even a `tail` of a captured run identifies them.
//
// DELIBERATELY NO `retry`. Vitest's retry option would turn this flake into a
// green run with a note, and this repo has a scar (CLAUDE.md, "Build artifacts")
// from exactly that shape: a guard reporting success while the thing it guarded
// was wrong. A flake must fail `make check`. The reporter still handles
// retryCount, so if `retry` is ever switched on, a test that only passed on a
// second attempt is reported LOUDLY as flaky rather than silently swallowed.

import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, relative, resolve } from "node:path";
import type { RunnerTask, RunnerTestFile } from "vitest";
import type { Reporter } from "vitest/reporters";
import type { Vitest } from "vitest/node";

/** A single test, flattened out of the suite tree with its full path name. */
interface FlatTest {
  /** e.g. "MediaGrid > renders a poster for every item" */
  fullName: string;
  /** Test file, relative to the web/ root. */
  file: string;
  state: string;
  durationMs: number;
  /** >0 only when the `retry` option is enabled AND the test needed one. */
  retryCount: number;
  errors: string[];
}

const OUTPUT_RELATIVE = "test-results/vitest-last-run.txt";

function formatMs(ms: number): string {
  return ms >= 1000 ? `${(ms / 1000).toFixed(2)}s` : `${Math.round(ms)}ms`;
}

function describeError(error: unknown): string {
  if (error === null || error === undefined) return "(no error object)";
  if (typeof error === "string") return error;
  const err = error as { name?: string; message?: string; stack?: string };
  const head = [err.name, err.message].filter(Boolean).join(": ") || String(error);
  // Keep a few stack frames: enough to locate the assertion, not a wall of vitest internals.
  const frames = (err.stack ?? "")
    .split("\n")
    .filter((line) => line.trim().startsWith("at "))
    .slice(0, 6)
    .join("\n");
  return frames ? `${head}\n${frames}` : head;
}

export class FailureLogReporter implements Reporter {
  private root = process.cwd();
  private startedAt = new Date();

  onInit(ctx: Vitest): void {
    this.root = ctx.config.root;
    this.startedAt = new Date();
  }

  onFinished(files: RunnerTestFile[] = [], errors: unknown[] = []): void {
    const tests: FlatTest[] = [];
    // File-level failures (an import blowing up, a top-level throw) never produce
    // a test task, so they would otherwise vanish from this log entirely.
    const fileLevelFailures: string[] = [];

    for (const file of files) {
      const relPath = relative(this.root, file.filepath ?? file.name);
      if (file.result?.state === "fail" && (file.result.errors?.length ?? 0) > 0) {
        for (const error of file.result.errors ?? []) {
          fileLevelFailures.push(`${relPath}\n${describeError(error)}`);
        }
      }
      collect(file.tasks ?? [], [], relPath, tests);
    }

    const failed = tests.filter((t) => t.state === "fail");
    const passed = tests.filter((t) => t.state === "pass");
    const skipped = tests.filter((t) => t.state === "skip" || t.state === "todo");
    // Passed only because it was retried — invisible in the exit code by design,
    // which is precisely why it has to be shouted about here.
    const flaky = passed.filter((t) => t.retryCount > 0);
    const slowest = [...tests].sort((a, b) => b.durationMs - a.durationMs).slice(0, 10);

    const ok = failed.length === 0 && fileLevelFailures.length === 0 && errors.length === 0;
    const finishedAt = new Date();
    const wall = finishedAt.getTime() - this.startedAt.getTime();

    const out: string[] = [];
    out.push("Obelo web suite (vitest run) — last-run record");
    out.push(`started:  ${this.startedAt.toISOString()}`);
    out.push(`finished: ${finishedAt.toISOString()} (${formatMs(wall)} wall)`);
    out.push(`result:   ${ok ? "PASS" : "FAIL"}`);
    out.push(
      `tests:    ${tests.length} total | ${passed.length} passed | ` +
        `${failed.length} failed | ${skipped.length} skipped`,
    );
    out.push(`files:    ${files.length}`);
    out.push(`flaky:    ${flaky.length} (passed only after a retry)`);
    out.push("");

    if (failed.length > 0) {
      out.push(`FAILED TESTS (${failed.length}) — this is the name the next report needs`);
      out.push("=".repeat(72));
      failed.forEach((t, i) => {
        out.push(`${i + 1}. ${t.fullName}`);
        out.push(`   file:     ${t.file}`);
        out.push(`   duration: ${formatMs(t.durationMs)}   attempts: ${t.retryCount + 1}`);
        for (const e of t.errors) {
          out.push(...e.split("\n").map((line) => `   ${line}`));
        }
        out.push("");
      });
    }

    if (fileLevelFailures.length > 0) {
      out.push(`FILE-LEVEL FAILURES (${fileLevelFailures.length}) — the file never ran its tests`);
      out.push("=".repeat(72));
      for (const f of fileLevelFailures) {
        out.push(...f.split("\n").map((line) => `   ${line}`));
        out.push("");
      }
    }

    if (errors.length > 0) {
      out.push(`UNHANDLED ERRORS (${errors.length})`);
      out.push("=".repeat(72));
      for (const e of errors) {
        out.push(...describeError(e).split("\n").map((line) => `   ${line}`));
        out.push("");
      }
    }

    if (flaky.length > 0) {
      out.push(`FLAKY — PASSED ONLY ON RETRY (${flaky.length})`);
      out.push("=".repeat(72));
      out.push("The exit code will not show these. They are real failures that got a second try.");
      for (const t of flaky) {
        out.push(`   ${t.fullName}  [${t.file}]  attempts: ${t.retryCount + 1}`);
        for (const e of t.errors) {
          out.push(...e.split("\n").map((line) => `      ${line}`));
        }
      }
      out.push("");
    }

    out.push("SLOWEST TESTS (a timing-sensitive assertion shows up here first)");
    out.push("=".repeat(72));
    for (const t of slowest) {
      out.push(`   ${formatMs(t.durationMs).padStart(8)}  ${t.fullName}  [${t.file}]`);
    }
    out.push("");

    const target = resolve(this.root, OUTPUT_RELATIVE);
    try {
      mkdirSync(dirname(target), { recursive: true });
      writeFileSync(target, out.join("\n"), "utf8");
    } catch (writeError) {
      process.stderr.write(
        `\nFailureLogReporter: could not write ${target}: ${String(writeError)}\n`,
      );
    }

    // Stdout, last: the default reporter's summary has already gone by, so this
    // is the final thing in any captured log.
    const banner: string[] = [];
    if (failed.length > 0 || fileLevelFailures.length > 0) {
      banner.push("");
      banner.push("─".repeat(72));
      banner.push(`WEB SUITE FAILED — the failing test(s), by name:`);
      failed.forEach((t, i) => banner.push(`  ${i + 1}. ${t.fullName}  [${t.file}]`));
      fileLevelFailures.forEach((f) => banner.push(`  (file-level) ${f.split("\n")[0]}`));
      banner.push(`Full detail, including errors and the slowest tests, in:`);
      banner.push(`  ${target}`);
      banner.push("─".repeat(72));
      banner.push("");
    } else if (flaky.length > 0) {
      banner.push("");
      banner.push("─".repeat(72));
      banner.push(`WEB SUITE PASSED BUT ${flaky.length} TEST(S) NEEDED A RETRY — that is a flake:`);
      flaky.forEach((t, i) => banner.push(`  ${i + 1}. ${t.fullName}  [${t.file}]`));
      banner.push(`  ${target}`);
      banner.push("─".repeat(72));
      banner.push("");
    }
    if (banner.length > 0) process.stdout.write(banner.join("\n") + "\n");
  }
}

function collect(
  tasks: RunnerTask[],
  suitePath: string[],
  file: string,
  into: FlatTest[],
): void {
  for (const task of tasks) {
    if (task.type === "suite") {
      collect(task.tasks ?? [], [...suitePath, task.name], file, into);
      continue;
    }
    const result = task.result;
    into.push({
      fullName: [...suitePath, task.name].join(" > "),
      file,
      state: result?.state ?? "skip",
      durationMs: result?.duration ?? 0,
      retryCount: result?.retryCount ?? 0,
      errors: (result?.errors ?? []).map(describeError),
    });
  }
}

export default FailureLogReporter;
