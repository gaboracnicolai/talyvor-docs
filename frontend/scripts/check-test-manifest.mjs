// check-test-manifest.mjs — THE FRONTEND HALF OF THIS REPOSITORY'S SUITE HAD NOTHING COUNTING IT.
//
// `internal/testutil/skipcensus_test.go` is the Go half's class guard against "a test that reports
// SKIP and is counted as passing". Its walk names the directories it does not enter:
//
//	case ".git", "node_modules", "frontend", "vendor": return filepath.SkipDir
//
// `frontend` is in that list, and nothing stood on the other side of it. ci.yaml's frontend job ran
// `npm test` = a bare `vitest run`: no report, no manifest, no count, no status rule. So the SPA's
// money assertions — the search list's cost column, the page cost panel, the search wire census —
// could be switched off or deleted and every gate in the repository stayed green.
//
// ⚠⚠ MEASURED AT `ce997ff`, ONE WORD, `describe(` -> `describe.skip(` ON
// `src/components/SearchModal.cost.test.tsx` — the six assertions that say a document's search row
// prints its TOTAL cost and not one addend, and that a NOT-REPORTED cost is not rendered as $0.00:
//
//	npm test                     -> EXIT 0   Test Files 27 passed | 1 skipped (28)
//	                                         Tests 129 passed | 6 skipped (135)
//	npm run typecheck            -> EXIT 0
//	npm run build                -> EXIT 0
//	gofmt -l . / go vet ./...    -> clean / EXIT 0
//	go test -race (real PG)      -> EXIT 0, 36 packages ok
//	check-compose-secrets.sh     -> EXIT 0, 1 file / 0 findings
//	semgrep --test / scope / scan-> 10/10 pass, ok, 0 findings
//
// The whole gauntlet, green, with the cost-aware-search money assertions disabled.
//
// ⚠⚠ A COUNT RULE ALONE WOULD BE BLIND, AND THAT IS MEASURED RATHER THAN INHERITED. A skipped test
// is still an `assertionResult`, so `numTotalTests` reads **135 either way**. Worse, and this one
// was found by reading the report rather than predicted: the enclosing FILE's own `status` stays
// `"passed"` with five skipped and one todo inside it, so a per-file status check is blind too.
// The rule here is therefore per-ASSERTION, and it is an ALLOWLIST (`status !== "passed"`) rather
// than a denylist of known-bad statuses: vitest reports `.skip` as `"skipped"` and `.todo` as
// `"todo"`, through two DIFFERENT top-level fields (`numPendingTests`, `numTodoTests`), so the set
// of not-run statuses is demonstrably not closed and must not be enumerated.
//
// ⚠ THE COUNT RULE STILL EARNS ITS PLACE — it catches the other direction. DELETING a test moves
// the number and produces no status at all, which is the case the status rule cannot see. Both
// rules are here because each is the other's blind spot. Control C5 holds that.
//
// ⚠ `npm run test:accept` CANNOT BLESS A SKIP, BY CONSTRUCTION. The manifest records the TOTAL
// number of assertions in a file, skipped ones INCLUDED — so accepting after a skip writes the
// same number back, the count rule stays satisfied, and the status rule fires again on the next
// run. Accepting a DELETION is the thing it is for, and that leaves a reviewable diff line.
//
// ⚠ WHAT THIS DOES NOT COVER, said rather than implied: a test that is vacuous WITHOUT skipping.
// Nothing here can see an assertion that asserts nothing. This closes one named class — the
// frontend test that stops running and is still counted as a pass.
//
// Usage:
//   node scripts/check-test-manifest.mjs            check against the committed manifest
//   node scripts/check-test-manifest.mjs --update   rewrite the manifest from the current report
import { readFileSync, writeFileSync, existsSync } from "node:fs";
import { dirname, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const frontendRoot = resolve(here, "..");
const reportPath = resolve(frontendRoot, ".vitest-report.json");
const manifestPath = resolve(frontendRoot, "test-manifest.json");

// fileFloor guards against this check reporting OK over a report that covers almost nothing.
// The tree held 28 test files when this was written; the floor sits below that so an ordinary
// deletion is reported by the file-set rule (with its reviewable diff) rather than by a panic,
// and far above zero so a single-file `vitest run src/one.test.tsx --reporter=json` — the exact
// way to hand this script a report of a run that tested one file — cannot be read as a clean tree.
const fileFloor = 10;

const update = process.argv.includes("--update");

// READ-NOTHING IS NOT FIND-NOTHING. A missing report means the run this check is supposed to be
// reading did not happen; reporting "ok" from that is the failure the whole file exists for.
if (!existsSync(reportPath)) {
  console.error(
    `test-manifest: FAILED — no report at ${relative(frontendRoot, reportPath)}.\n` +
      `This script reads the JSON reporter's output; it is produced by \`npm test\`, which deletes\n` +
      `any previous report first so a stale one can never be read as a fresh run. Run \`npm test\`.`,
  );
  process.exit(1);
}

let report;
try {
  report = JSON.parse(readFileSync(reportPath, "utf8"));
} catch (err) {
  console.error(`test-manifest: FAILED — cannot parse the vitest report: ${err.message}`);
  process.exit(1);
}

const results = Array.isArray(report.testResults) ? report.testResults : [];

// Per file: how many assertions it declared, and which of them did not RUN.
/** @type {Map<string, number>} */
const counts = new Map();
/** @type {string[]} */
const notRun = [];
for (const file of results) {
  const rel = relative(frontendRoot, file.name).split("\\").join("/");
  const assertions = Array.isArray(file.assertionResults) ? file.assertionResults : [];
  counts.set(rel, assertions.length);
  for (const a of assertions) {
    // THE ALLOWLIST. Anything that is not a pass did not run, whatever vitest chose to call it.
    if (a.status !== "passed") {
      notRun.push(`${rel}: [${a.status}] ${a.fullName || a.title}`);
    }
  }
}

if (update) {
  const files = Object.fromEntries([...counts.entries()].sort(([a], [b]) => (a < b ? -1 : 1)));
  const total = [...counts.values()].reduce((n, v) => n + v, 0);
  writeFileSync(
    manifestPath,
    JSON.stringify({ files, totalFiles: counts.size, totalTests: total }, null, 2) + "\n",
  );
  console.log(
    `test-manifest: wrote ${relative(frontendRoot, manifestPath)} ` +
      `(${counts.size} files, ${total} tests). A SKIPPED test is still counted here, so accepting ` +
      `cannot silence the status rule.`,
  );
  process.exit(0);
}

if (!existsSync(manifestPath)) {
  console.error(
    `test-manifest: FAILED — no manifest at ${relative(frontendRoot, manifestPath)}.\n` +
      `Generate it with \`npm run test:accept\` and COMMIT it: the manifest is the reviewable record ` +
      `of what this suite is supposed to run.`,
  );
  process.exit(1);
}

const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
const expected = manifest.files || {};

// THE VACUITY FLOOR, ASSERTED BEFORE ANY VERDICT. A report covering a handful of files satisfies
// every rule below for the files it happens to contain, and says nothing about the rest.
if (counts.size < fileFloor) {
  console.error(
    `test-manifest: FAILED — the report covers only ${counts.size} test file(s), want at least ` +
      `${fileFloor}. This is a partial run, not a clean tree: a verdict from it means nothing. ` +
      `Run the whole suite with \`npm test\`.`,
  );
  process.exit(1);
}

const problems = [];

// FILE SET, BOTH DIRECTIONS. A deleted or renamed file vanishes from the report entirely and
// produces no count to compare and no status to read — it is invisible to both rules below.
for (const f of Object.keys(expected)) {
  if (!counts.has(f)) problems.push(`MISSING  ${f} — in the manifest, absent from the run`);
}
for (const f of counts.keys()) {
  if (!(f in expected)) problems.push(`NEW      ${f} — ran, but is not in the manifest`);
}

// COUNTS. The direction the status rule cannot see: a test that was deleted rather than disabled.
for (const [f, want] of Object.entries(expected)) {
  const got = counts.get(f);
  if (got === undefined) continue; // already reported as MISSING
  if (got !== want) {
    problems.push(`${got < want ? "SHRANK " : "GREW   "} ${f}: ${want} -> ${got} tests`);
  }
}

for (const line of notRun) problems.push(`NOT RUN  ${line}`);

if (problems.length > 0) {
  console.error("test-manifest: FAILED\n  " + problems.join("\n  "));
  // PER-KIND ADVICE. `test:accept` clears a count change and CANNOT clear a NOT RUN line; saying
  // otherwise would send a reviewer to a command that reports success and fixes nothing.
  if (problems.some((p) => p.startsWith("NOT RUN"))) {
    console.error(
      "\nA NOT RUN test is counted as a pass by `vitest run` and by every other gate in this\n" +
        "repository. `npm run test:accept` CANNOT clear these lines — the manifest counts skipped\n" +
        "tests, so accepting writes the same numbers back. Un-skip the test, or delete it and accept\n" +
        "the count change so the loss is a line in the diff.",
    );
  }
  if (problems.some((p) => !p.startsWith("NOT RUN"))) {
    console.error(
      "\nA count or file-set change is deliberate as often as not. If it is, run\n" +
        "`npm run test:accept` and commit the manifest — the point is that the change is REVIEWED,\n" +
        "not that it is forbidden.",
    );
  }
  process.exit(1);
}

const total = [...counts.values()].reduce((n, v) => n + v, 0);
console.log(`test-manifest: ok (${counts.size} files, ${total} tests, all run)`);
