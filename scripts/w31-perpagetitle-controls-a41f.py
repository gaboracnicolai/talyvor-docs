#!/usr/bin/env python3
"""Positive controls for perpagetitle_realpg_test.go — can each case actually fail?

A guard that has only ever been seen passing on the real tree is a guard nobody has heard speak.
Each control below MUTATES the shipped source, runs the WHOLE internal/analytics package against a
real Postgres, records which named cases redden, and restores the file in a `finally`. The restore
is verified by sha256 against the bytes read before the mutation, so a control that dies mid-run
cannot leave the tree edited.

Run:
  DOCS_TEST_DATABASE_URL=postgres://... python3 scripts/w31-perpagetitle-controls-a41f.py

Every control states its PREDICTION. A control whose prediction is wrong is the interesting
outcome, not a nuisance — it means the instrument measures something other than what its author
believed, and that is the whole reason this file exists.
"""

import hashlib
import os
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
STORE = ROOT / "internal" / "analytics" / "store.go"

# The shipped fix, verbatim. Every mutation is expressed as a replacement of one of these.
FIX_ASSIGN = "\tout.Title = title.String\n"
ROLLUP_TITLE = "`SELECT pv.page_id, MAX(p.title), COUNT(*)::int,"

# The whole totals statement, so C3 swaps one WELL-FORMED implementation for another rather than
# editing two fragments and hoping both anchors still exist.
#
# ⚠ C3 IS THE CONTROL THAT WAS WRONG BEFORE THE GUARD WAS, AND THE RUN SAID SO. Its first version
# grafted `JOIN pages p ON p.id = page_views.page_id` onto the statement WITHOUT qualifying the
# column references. `created_at` exists on BOTH tables, so Postgres refused the query as
# ambiguous, GetReadStats returned an error, the route answered 500, and the test died in its
# fetch helper — suite FAIL, and not one of the five named cases red. A control that reddens by
# breaking the query measures nothing about the fix it is aiming at: it would have "passed" for a
# reason that has no bearing on whether [UNVIEWED] can fail. The alias-qualified form below is the
# implementation a reviewer would actually have written.
TOTALS_SHIPPED = """		`SELECT (SELECT title FROM pages WHERE id = $1),
                COUNT(*)::int, COUNT(DISTINCT viewer_id)::int,
                COALESCE(AVG(duration_sec)::int, 0),
                MAX(created_at)
        FROM page_views
        WHERE page_id = $1
          AND created_at > NOW() - INTERVAL '1 day' * $2`,"""

TOTALS_JOINED = """		`SELECT MAX(p.title),
                COUNT(*)::int, COUNT(DISTINCT pv.viewer_id)::int,
                COALESCE(AVG(pv.duration_sec)::int, 0),
                MAX(pv.created_at)
        FROM page_views pv JOIN pages p ON p.id = pv.page_id
        WHERE pv.page_id = $1
          AND pv.created_at > NOW() - INTERVAL '1 day' * $2`,"""

CASES = ["TITLE-REPORTED", "WIRE", "ROUTES-AGREE", "NOT-A-CONSTANT", "UNVIEWED"]


def sha(p: pathlib.Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def run_suite() -> tuple[bool, str]:
    """Run the whole analytics package. Returns (passed, combined output)."""
    env = dict(os.environ)
    r = subprocess.run(
        ["go", "test", "-race", "-count=1", "./internal/analytics/"],
        cwd=ROOT, env=env, capture_output=True, text=True,
    )
    return r.returncode == 0, r.stdout + r.stderr


def cases_red(output: str) -> list[str]:
    return [c for c in CASES if re.search(r"\[" + re.escape(c) + r"\]", output)]


def control(name: str, prediction: str, mutate) -> dict:
    before = STORE.read_text()
    digest = sha(STORE)
    try:
        after = mutate(before)
        if after == before:
            raise SystemExit(f"{name}: the mutation changed nothing — the anchor has moved and "
                             f"this control would have measured the UNMUTATED tree")
        STORE.write_text(after)
        passed, out = run_suite()
        red = cases_red(out)
    finally:
        STORE.write_text(before)
        if sha(STORE) != digest:
            raise SystemExit(f"{name}: RESTORE FAILED — {STORE} does not match its pre-mutation sha256")
    return {"name": name, "prediction": prediction, "suite_passed": passed, "red": red}


def main() -> int:
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL is unset — the real-PG tests would FAIL rather than run, "
              "and every control below would 'red' for the wrong reason.", file=sys.stderr)
        return 2

    baseline_pass, baseline_out = run_suite()
    baseline_red = cases_red(baseline_out)
    print(f"BASELINE (unmutated): suite {'PASS' if baseline_pass else 'FAIL'}, cases red: {baseline_red or 'none'}")
    if not baseline_pass or baseline_red:
        print("The tree is not green before any control runs. Nothing below is interpretable.", file=sys.stderr)
        return 1

    results = []

    # C1 — THE DEFECT ITSELF. Drop the assignment; Title falls back to the Go zero value.
    results.append(control(
        "C1 the defect: Title never assigned",
        "ALL FIVE red — this is the state main shipped",
        lambda s: s.replace(FIX_ASSIGN, ""),
    ))

    # C2 — A CONSTANT. The field is filled, non-empty, and wrong for every page but one.
    results.append(control(
        "C2 Title filled with a constant",
        "NOT-A-CONSTANT and UNVIEWED red; TITLE-REPORTED, WIRE, ROUTES-AGREE green",
        lambda s: s.replace(FIX_ASSIGN, '\tout.Title = "Runbook"\n'),
    ))

    # C3 — THE JOIN TRAP the fix's comment claims. Take the title off the aggregated page_views
    # rows instead of off `pages`, which is what a reviewer would reach for first.
    results.append(control(
        "C3 title via JOIN page_views→pages instead of the scalar subquery",
        "UNVIEWED red ALONE — a page with no traffic has no row to take a title from",
        lambda s: s.replace(TOTALS_SHIPPED, TOTALS_JOINED),
    ))

    # C4 — BLIND THE OTHER SIDE. The roll-up stops reporting a title. If ROUTES-AGREE is a real
    # two-sided comparison it reds here; if it merely restates TITLE-REPORTED it cannot.
    results.append(control(
        "C4 the ROLL-UP's title blinded (MAX(p.title) → '')",
        "ROUTES-AGREE red ALONE — the per-page route is untouched and still correct",
        lambda s: s.replace(ROLLUP_TITLE, "`SELECT pv.page_id, '', COUNT(*)::int,"),
    ))

    # C5 — A REAL TITLE, OFF THE WRONG ROW. Deterministically the alphabetically-first page in the
    # database, so one of the two viewed pages is right by coincidence.
    results.append(control(
        "C5 title read off a fixed OTHER page (ORDER BY title LIMIT 1)",
        "TITLE-REPORTED, WIRE, ROUTES-AGREE, UNVIEWED red; NOT-A-CONSTANT GREEN — "
        "the two cases catch different wrongnesses and neither subsumes the other",
        lambda s: s.replace(
            "(SELECT title FROM pages WHERE id = $1)",
            "(SELECT title FROM pages ORDER BY title LIMIT 1)",
        ),
    ))

    print()
    ok = True
    for r in results:
        print(f"── {r['name']}")
        print(f"   predicted: {r['prediction']}")
        print(f"   measured : suite {'PASS' if r['suite_passed'] else 'FAIL'}, red: {r['red'] or 'none'}")
        if r["suite_passed"]:
            print("   ⚠ THE SUITE STAYED GREEN UNDER THIS MUTATION — the guard did not see it.")
            ok = False
        print()
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
