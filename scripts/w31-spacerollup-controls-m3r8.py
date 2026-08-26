#!/usr/bin/env python3
"""
W3.1 — THE SPACE ROLL-UP: positive controls (tab-m3r8).

Each control MUTATES the shipped product and predicts which named test goes RED. A guard that
stays green under its own mutation is not a guard, whatever it asserted a moment ago.

⚠ THE FAILURE MODE THESE ARE AIMED AT. The org and space roll-ups are ONE statement narrowed,
which is what stops them drifting — and it is also a trapdoor, because "every space" is expressed
as an EMPTY spaceID. Any path that reaches that statement with "" answers with the WHOLE
WORKSPACE under a space's name, and every figure looks plausible. So the controls widen the scope
in three different ways and require the difference to be visible.

⚠ AND THE SECOND FAILURE MODE IS THE ONE THE PREVIOUS MERGE FIXED: a correct figure nobody can
reach. C6/C7 aim at the SPA half for that reason — the route existing is not the feature.

Verdicts are read from each runner's own per-test reporting, never from the process exit code
(a build failure and a caught defect are both non-zero and mean opposite things), and a run that
never reached an assertion is INVALID rather than green.

⚠ THE FRONTEND RUNNER USES THE JSON REPORTER DELIBERATELY. Its sibling harness
(w31-versioncostsplit-route-controls-m3r8.py) shipped with `--reporter=basic`, which is NOT a
reporter in this vitest — it dies with ERR_LOAD_URL before collecting a single test, and a stdout
scrape then reports a confident "nothing failed" for a suite that never ran. That harness scored
four controls NOT CAUGHT and one GREEN prediction CONFIRMED, all against nothing.

⚠ WHAT IS NOT CONTROLLED HERE, SAID PLAINLY RATHER THAN PADDED WITH A CONTROL THAT PROVES NOTHING.
`SpaceStats` is gated twice — the route's SPACE enforcer (grants) and the handler's workspace
assertion (tenancy) — and C4 covers the first. There is no control isolating the SECOND, and the
reason is structural: the tenancy block in `SpaceStats` is BYTE-IDENTICAL to the one in
`WorkspaceStats`, so no textual anchor can name one without also matching the other. The first
attempt did exactly that and the anchor check caught it ("ANCHOR DEAD — handler.go holds it 2×"),
which is the check doing its job; a control that mutates the wrong one of two identical blocks
would silently probe a route this file is not about. Beyond the anchor, the two gates also overlap
on every input a test can construct — a caller outside the workspace fails both. So the tenancy
half is covered end to end by
TestSpaceRollup_RefusesASpaceInAnotherWorkspace_RealPG and not in isolation. Recorded rather than
faked: a control that compiles, mutates nothing meaningful and predicts GREEN is decoration, and
it would have made this harness read as ten-for-ten.

Usage: DOCS_TEST_DATABASE_URL=... python3 scripts/w31-spacerollup-controls-m3r8.py
"""

import hashlib
import json
import os
import re
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
STORE = REPO / "internal/analytics/store.go"
HANDLER = REPO / "internal/analytics/handler.go"
MAIN = REPO / "cmd/docs/main.go"
COMPONENT = REPO / "frontend/src/pages/SpaceView.tsx"

GO_TESTS = "SpaceRollup|GetSpaceStats_Refuses|MainWiresThePageReadGate"
FE_TEST = "src/pages/SpaceView.rollup.test.tsx"

if not os.environ.get("DOCS_TEST_DATABASE_URL"):
    print("DOCS_TEST_DATABASE_URL unset — the real-PG guards cannot run; refusing to score")
    sys.exit(2)


def sha(p: Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def go_failures() -> list[str] | None:
    p = subprocess.run(
        ["go", "test", "-count=1", "-run", GO_TESTS, "./internal/analytics/"],
        cwd=REPO, capture_output=True, text=True,
    )
    out = p.stdout + p.stderr
    if "build failed" in out or "[build failed]" in out or "cannot find package" in out:
        return None
    if "no test files" in out:
        return None
    fails = re.findall(r"--- FAIL: (\S+)", out)
    if not fails and p.returncode != 0 and "FAIL" not in out:
        return None
    return fails


def fe_failures() -> list[str] | None:
    report = REPO / "frontend" / ".vitest-spacerollup-report.json"
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
        return None
    return [a.get("fullName", "") for a in results if a.get("status") == "failed"]


# (name, file, anchor, replacement, runner, expect: None = nothing may red, else substring, why)
CONTROLS = [
    ("C1 the space scope is dropped from the RANKING", STORE,
     "          AND ($3 = '' OR p.space_id = $3)",
     "          AND ($3 = '' OR $3 <> '')", "go", "ReportsITSOwnSpace",
     "⚠ AN INERT PREDICATE THAT IS STILL SYNTACTICALLY THERE — the shape this queue keeps "
     "finding. The clause still mentions $3 and still parses; it just cannot exclude anything, "
     "so the space roll-up becomes the workspace ranking."),

    ("C2 the space scope is dropped from NEVER-READ", STORE,
     "          AND ($2 = '' OR p.space_id = $2)",
     "          AND ($2 = '' OR $2 <> '')", "go", "ReportsITSOwnSpace",
     "The cohorts must narrow TOGETHER: a never-read count spanning the workspace, beside a "
     "space's own ranking, is a figure about a different subject in the same response. ⚠ THIS "
     "CONTROL SCORED NOT CAUGHT TWICE AND WAS RIGHT BOTH TIMES. First the fixture had NO unread "
     "pages at all, so the cohort read 0 whether it was scoped or not — a cohort empty in both "
     "directions cannot tell you which one it measured; an unread page in the OTHER space fixed "
     "that. Then it was still aimed at the agreement case, which has one space and therefore no "
     "scoping to get wrong."),

    ("C3 GetSpaceStats accepts an empty scope and widens", STORE,
     '\tif spaceID == "" {\n\t\treturn nil, ErrNoSpaceScope\n\t}',
     "\tif false {\n\t\treturn nil, ErrNoSpaceScope\n\t}", "go", "RefusesAnEmptyScope",
     "The trapdoor. With the refusal gone, a space roll-up called with \"\" — a renamed URL "
     "param, a handler reading the wrong key — answers with the whole workspace and looks normal."),

    ("C4 the route drops its space gate", HANDLER,
     "r.With(h.spaceEnf.Require(permission.AccessView)).Get(\"/spaces/{spaceID}/analytics/pages\", h.SpaceStats)",
     'r.Get("/spaces/{spaceID}/analytics/pages", h.SpaceStats)', "go", "RefusesAPrivateSpace",
     "⚠ RETARGETED BECAUSE THIS CONTROL PROVED THE FIRST TARGET COULD NOT SEE IT. Aimed at "
     "RefusesASpaceInAnotherWorkspace it scored NOT CAUGHT, correctly: an attacker in a different "
     "workspace is stopped by the TENANCY assertion, so that case proves one gate exists rather "
     "than two. The gate the space enforcer alone holds is a colleague in the SAME workspace "
     "reading a private space they were never granted — which is why "
     "TestSpaceRollup_RefusesAPrivateSpaceInTheCallersOwnWorkspace_RealPG now exists."),

    ("C6 the SPA computes the roll-up and never paints it", COMPONENT,
     'data-testid="space-rollup"',
     'data-testid="space-rollup-DISABLED"', "fe", "shows the space's roll-up figures",
     "⚠ THE DEFECT THE PREVIOUS MERGE FIXED, ONE AFTERNOON EARLIER. A route serving a correct "
     "figure that no screen renders is unreachable, and unreachable is where #190 left "
     "VersionCostSplit."),

    ("C7 the heading stops saying which scope it means", COMPONENT,
     "Readership in this space (30d)",
     "Readership (30d)", "fe", "says the figures are the SPACE",
     "The same four numbers render the ORG roll-up on Analytics.tsx. Under an unqualified "
     "heading they read as the workspace's — the confusion the server-side scope prevents, "
     "reintroduced in words."),

    ("C8 a failed read renders as zero readership", COMPONENT,
     "  if (!stats.data) return null;",
     "  const _unused = stats.error;", "fe", "a failed read is not reported as zero",
     "A space nobody has opened and a roll-up that could not be read are different facts. "
     "Printing 0 for both states something this screen does not know."),

    ("C9 main.go forgets to wire the space gate", MAIN,
     "\tanalyticsHandler.WithSpaceAccess(spaceEnf)",
     "\t// wiring removed by control C9", "go", "MainWiresThePageReadGate",
     "⚠ THE SILENT ONE. A nil enforcer FAILS CLOSED, so the route 404s for every space forever "
     "— a shipped feature that answers nothing, and every test in the package builds its own "
     "router and stays green. Only the main-wiring tripwire can see it."),

    ("C10 NEGATIVE CONTROL — a comment-only edit", STORE,
     "// GetSpaceStats is the SPACE roll-up",
     "// GetSpaceStats (a comment changed by the negative control)", "go", None,
     "Nothing may red. A harness that reds on any edit is measuring the edit, not the product."),
]


def main() -> int:
    print("=" * 78)
    print("W3.1 — THE SPACE ROLL-UP: positive controls")
    print("=" * 78)

    base_go, base_fe = go_failures(), fe_failures()
    if base_go is None or base_fe is None:
        print("FATAL: a runner did not reach its assertions on the clean tree")
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
                detail = (f"caught by: {hit[0][:86]}" if hit
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
