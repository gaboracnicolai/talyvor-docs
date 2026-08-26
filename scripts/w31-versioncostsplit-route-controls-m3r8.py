#!/usr/bin/env python3
"""
W3.1 — SERVING THE VERSION-COST SPLIT: positive controls (tab-m3r8).

Each control MUTATES the shipped product and predicts which named test goes RED. A guard that
stays green under its own mutation is not a guard, whatever it asserted a moment ago.

⚠ WHY THIS HARNESS EXISTS AT ALL. The change it scores is a ROUTE plus a RENDER, and both halves
have the same failure mode: the thing is computed and then not carried the last step. #190 shipped
`Store.VersionCostSplit` with a correct query, a real-Postgres guard and eleven of its own
controls — and no caller. Every one of those controls passed. So the controls below are aimed at
the seam that class of defect lives in: whether the number reaches the wire, and whether it reaches
the screen.

⚠ TWO OF THE FOUR SPA CASES PASS ON UNMODIFIED MAIN, and that is exactly why C7 and C8 exist.
"Renders nothing when there is nothing to say" and "the rows survive a failed split" are both
satisfied by a component that renders nothing at all, which is what main did. A case that has
never been red is a hypothesis; only a mutation makes it evidence.

Verdicts are read from the test runner's own per-test reporting, never from the process exit code:
a suite that fails to COMPILE exits non-zero and would score every control as CAUGHT while
asserting nothing. Every touched file is restored and its sha256 compared against the original.

Usage: DOCS_TEST_DATABASE_URL=... python3 scripts/w31-versioncostsplit-route-controls-m3r8.py
"""

import hashlib
import json
import os
import re
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
HANDLER = REPO / "internal/page/handler.go"
COMPONENT = REPO / "frontend/src/components/VersionHistory.tsx"
API = REPO / "frontend/src/api/pages.ts"

GO_TESTS = "VersionCostSplit_IsSERVED|VersionCostSplitRoute_Refuses|VersionCostSplit_ServesTheCOLUMN"
FE_TEST = "src/components/VersionHistory.reconcile.test.tsx"

if not os.environ.get("DOCS_TEST_DATABASE_URL"):
    print("DOCS_TEST_DATABASE_URL unset — the real-PG guards cannot run; refusing to score")
    sys.exit(2)


def sha(p: Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def go_failures() -> list[str] | None:
    """Names of failing Go tests, or None if the package never reached an assertion.

    ⚠ THE None CASE IS THE POINT, and it is a lesson this repo already paid for: a harness that
    scored `exit != 0` as CAUGHT once accepted a guard on the evidence of a Postgres container
    that was not running. A build error and a caught defect are both non-zero and mean opposite
    things.
    """
    p = subprocess.run(
        ["go", "test", "-count=1", "-run", GO_TESTS, "./internal/page/"],
        cwd=REPO, capture_output=True, text=True,
    )
    out = p.stdout + p.stderr
    if "build failed" in out or "[build failed]" in out or "cannot find package" in out:
        return None
    if "no test files" in out:
        return None
    fails = re.findall(r"--- FAIL: (\S+)", out)
    if not fails and p.returncode != 0 and "FAIL" not in out:
        return None  # something went wrong that was not a test failing
    return fails


def fe_failures() -> list[str] | None:
    """Names of failing frontend tests, or None if the runner never reached an assertion.

    ⚠ THIS FUNCTION IS THE HARNESS DEFECT ITS OWN CONTROLS CAUGHT, AND IT IS WRITTEN DOWN HERE
    RATHER THAN QUIETLY FIXED. The first version ran `vitest run <file> --reporter=basic` and
    scraped `FAIL … > <name>` out of stdout. `basic` IS NOT A REPORTER in this vitest: it is
    treated as a custom reporter MODULE PATH, fails to resolve with ERR_LOAD_URL, and the process
    dies before collecting a single test. The scrape then found no `FAIL` lines and returned `[]`
    — an confident, well-typed "nothing failed" for a suite that never ran.

    Every one of the four frontend controls scored NOT CAUGHT against that, and the two that
    PREDICTED GREEN scored as passing for the same empty reason. A harness that cannot tell
    "no test failed" from "no test ran" reports the product as unguarded and its own green
    predictions as confirmed, in the same run.

    So verdicts now come from vitest's JSON report — per-test status, the same source the repo's
    own `npm test` uses — and the absence of that file, or of any test result inside it, is
    INVALID rather than green.
    """
    report = REPO / "frontend" / ".vitest-controls-report.json"
    report.unlink(missing_ok=True)
    subprocess.run(
        ["npx", "vitest", "run", FE_TEST, "--reporter=json", f"--outputFile={report}"],
        cwd=REPO / "frontend", capture_output=True, text=True,
    )
    if not report.exists():
        return None
    try:
        data = json.loads(report.read_text())
    except json.JSONDecodeError:
        return None
    finally:
        report.unlink(missing_ok=True)
    results = [a for f in data.get("testResults", []) for a in f.get("assertionResults", [])]
    if not results:
        return None  # the file exists but nothing was collected — not evidence of green
    return [a.get("fullName", "") for a in results if a.get("status") == "failed"]


# (name, file, anchor, replacement, runner, expect: None = nothing may red, else substring, why)
CONTROLS = [
    ("C1 the route is unmounted", HANDLER,
     'r.With(h.pageEnf.Require(permission.AccessView)).Get("/{pageID}/version-cost", h.GetVersionCostSplit)',
     '// route removed by control C1', "go", "IsSERVED",
     "The whole defect this change closes: the function exists and nothing serves it."),

    ("C2 the route drops the caller's workspaces", HANDLER,
     'h.store.VersionCostSplit(r.Context(), chi.URLParam(r, "pageID"), authz.WorkspaceIDs(r.Context()))',
     'h.store.VersionCostSplit(r.Context(), chi.URLParam(r, "pageID"), nil)',
     "go", "Refuses",
     "assertInWorkspaces lives in the store, but a route that hands it the wrong set defeats it "
     "without changing a line of that function. This is a money read."),

    ("C3 page_total is RECOMPUTED as the sum of its parts", HANDLER,
     "\t\tPageTotal: split.PageTotal,",
     "\t\tPageTotal: split.Attributed + split.Pending + split.Unattributable,",
     "go", "ServesTheCOLUMN",
     "⚠ THE SUBTLE ONE, AND IT WAS UNCAUGHT UNTIL THIS RUN SAID SO. Deriving the whole from the "
     "parts makes the reconciliation true by construction — it would balance in exactly the case "
     "where money had gone missing from the events meant to explain it. The first version of this "
     "control aimed at the main case and scored NOT CAUGHT, correctly: that fixture BALANCES on "
     "purpose, so the derived total and the column agree and no assertion over it can tell them "
     "apart. `TestVersionCostSplit_ServesTheCOLUMNAsTheTotal_NotTheSumOfItsParts_RealPG` exists "
     "because of this control, and seeds a page where the two DISAGREE."),

    ("C4 a bucket is dropped from the response", HANDLER,
     "\t\tUnattributable: split.Unattributable,",
     "\t\tUnattributable: 0,",
     "go", "IsSERVED",
     "Money that is computed and then not carried to the wire — the same shape as C1, one layer in."),

    ("C5 a wire key is renamed", HANDLER,
     '`json:"pending_usd"`',
     '`json:"pending"`',
     "go", "IsSERVED",
     "A renamed money key reads to a client as an ABSENT value and to a reader as a missing "
     "bucket. This is why the test decodes into a typed struct rather than a map."),

    ("C6 the SPA fetches the split and never paints it", COMPONENT,
     'data-testid="version-cost-reconcile"',
     'data-testid="version-cost-reconcile-DISABLED"',
     "fe", "do NOT account for",
     "A component that calls the route and drops the answer is exactly as silent as one that "
     "never called it. ⚠ THE EXPECT STRING ABOVE WAS WRONG IN THE FIRST DRAFT — it read "
     "\"does NOT account for\" while the case is named \"rows do NOT account for\", so this "
     "control scored NOT CAUGHT against a mutation the test WAS catching. A control that names "
     "the wrong test reports the product as unguarded; it is the mirror of a control that names "
     "nothing at all."),

    ("C7 the strip renders unconditionally", COMPONENT,
     "{split.data && split.data.pending_usd + split.data.unattributable_usd > 0 && (",
     "{split.data && (",
     "fe", "says nothing at all",
     "⚠ ONE OF THE TWO CASES THAT PASSED ON UNMODIFIED MAIN. 'Renders nothing when there is "
     "nothing to say' is satisfied by a component that renders nothing EVER, which is what main "
     "did. Without this mutation that case is a hypothesis."),

    ("C8 a failed split takes the ROWS down with it", COMPONENT,
     "  const list = versions.data ?? [];",
     "  const list = split.data ? (versions.data ?? []) : [];",
     "fe", "does not claim completeness",
     "⚠ THE OTHER CASE THAT PASSED ON MAIN. A side-read that fails must not withdraw a feature "
     "that is still correct; on main the case passed because there was no side-read to fail."),

    ("C9 the two buckets are summed into one figure", COMPONENT,
     "{split.data.pending_usd > 0 && (",
     "{false && (",
     "fe", "keeps the two unshown buckets apart",
     "Pending money lands on the next save and pre-0021 money never does. Reporting one figure, "
     "or dropping one of them, tells a reader that all of it is coming."),

    ("C10 the client calls the shadowed path", API,
     "/version-cost`",
     "/versions/cost`",
     "fe", None,
     "⚠ A DELIBERATELY UNCAUGHT CONTROL, AND ITS PURPOSE IS TO SAY SO OUT LOUD. The SPA tests "
     "mock `~/api/pages` entirely, so NOTHING in the frontend suite can see the URL this client "
     "builds. Predicting GREEN here records that limit as measured rather than leaving a reader "
     "to assume the path is covered. The Go tests hold the server's half of the contract; the "
     "join between the two strings is held by neither, and that is the honest state."),

    ("C11 NEGATIVE CONTROL — a comment-only edit", HANDLER,
     "// GetVersionCostSplit serves the reconciliation between what version history SHOWS",
     "// GetVersionCostSplit (a comment changed by the negative control)",
     "go", None,
     "Nothing may red. A harness that reds on any edit is measuring the edit, not the product."),
]


def main() -> int:
    print("=" * 78)
    print("W3.1 — SERVING THE VERSION-COST SPLIT: positive controls")
    print("=" * 78)

    base_go, base_fe = go_failures(), fe_failures()
    if base_go is None or base_fe is None:
        print("FATAL: a runner did not reach its assertions on the clean tree — scoring would be "
              "meaningless")
        return 1
    if base_go or base_fe:
        print(f"FATAL: the clean tree is not green (go={base_go} fe={base_fe})")
        return 1
    print("clean tree: go GREEN, frontend GREEN\n")

    passed = 0
    for name, path, anchor, repl, runner, expect, why in CONTROLS:
        src = path.read_text(encoding="utf-8")
        before = sha(path)
        n = src.count(anchor)
        if n != 1:
            print(f"✗ {name}\n   ANCHOR DEAD — {path.name} holds it {n}×, expected 1. "
                  f"This control probes NOTHING.\n")
            continue
        path.write_text(src.replace(anchor, repl), encoding="utf-8")
        try:
            fails = go_failures() if runner == "go" else fe_failures()
            if fails is None:
                ok, detail = False, "INVALID — the runner never reached an assertion"
            elif expect is None:
                ok = len(fails) == 0
                detail = "nothing red (as predicted)" if ok else f"RED: {fails}"
            else:
                hit = [f for f in fails if expect in f]
                ok = len(hit) >= 1
                detail = (f"caught by: {hit[0][:88]}" if hit
                          else f"NOT CAUGHT. red instead: {fails or '(nothing)'}")
            print(f"{'✓' if ok else '✗'} {name}\n   {why}\n   → {detail}\n")
            passed += 1 if ok else 0
        finally:
            path.write_text(src, encoding="utf-8")
            assert sha(path) == before, f"restore of {path} did not match its original sha256"

    print("every touched file restored to its original sha256")
    print("=" * 78)
    print(f"{passed}/{len(CONTROLS)} controls behaved as predicted")
    print("=" * 78)
    return 0 if passed == len(CONTROLS) else 1


sys.exit(main())
