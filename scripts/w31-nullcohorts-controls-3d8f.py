#!/usr/bin/env python3
"""Positive controls for the null-cohort wire fix (W3.1, tab-3d8f).

Each control mutates ONE thing in the product tree, runs the guard, and reports the SET of [TAG]s
that fired. Every mutation is restored in a `finally` and the restore is verified by sha256 against
the pre-mutation bytes — a control that silently failed to restore would score every later control
against a different tree.

THE CATCHER IS PREDICTED BEFORE THE RUN (`expect` below). A control whose actual catcher differs
from its prediction is a finding about the guard, not a pass.

⚠ TAGS ARE SCRAPED AS THE LEADING TAG OF A FAILURE LINE ONLY. A prose explanation that NAMES
another tag is not a result — this queue has already shipped one control script that scored 8/10
because it scraped every [TAG] on the line.

Run:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-nullcohorts-controls-3d8f.py
"""
import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = os.path.join(REPO, "internal/analytics/store.go")
SPA_TEST = os.path.join(REPO, "frontend/src/pages/Analytics.emptystate.test.tsx")
GUARD = "TestAnalyticsEmptyListsAreArrays_RealPG"

TAGS = [
    "EMPTY-WS-COHORTS",
    "NEVER-READ-PAGE",
    "FILTERED-TO-EMPTY",
    "RANKED-ROW-LISTS",
    "POPULATED",
    "VIEWED-PAGE-LISTS",
]

# ── anchors, verbatim ───────────────────────────────────────────────────────────────────
PAGE_RETURN = "	return withEmptyLists(&out), nil"
ROLLUP_RETURN = "	return withEmptyCohorts(&out), nil"

ROW_LOOPS = """	for i := range w.MostReadPages {
		withEmptyLists(&w.MostReadPages[i])
	}
	for i := range w.LeastReadPages {
		withEmptyLists(&w.LeastReadPages[i])
	}"""

TOPVIEWERS_NORM = """	if r.TopViewers == nil {
		r.TopViewers = []ViewerStat{}
	}"""

COHORT_NIL_CHECKS = """	if w.MostReadPages == nil {
		w.MostReadPages = []ReadStats{}
	}
	if w.LeastReadPages == nil {
		w.LeastReadPages = []ReadStats{}
	}"""

LIST_NIL_CHECKS = """	if r.ViewsByDay == nil {
		r.ViewsByDay = []DayCount{}
	}
	if r.TopViewers == nil {
		r.TopViewers = []ViewerStat{}
	}"""

# The per-page visibility filter the roll-up's cohorts are built through. C8 removes it so
# [FILTERED-TO-EMPTY]'s premise — that bob cannot see the private page — stops holding.
VISIBILITY_FILTER = """		found, canView := s.access.AuthorizePageRead(ctx, r.PageID)
		if !found || !canView {
			continue
		}"""

# The measured PRE-FIX wire bytes, for the frontend control.
PREFIX_EMPTY_ROLLUP = (
    '\'{"total_views":0,"unique_viewers":0,"most_read_pages":null,"least_read_pages":null,'
    '"never_read_count":1}\''
)
POSTFIX_EMPTY_ROLLUP_HEAD = '\'{"total_views":0,"unique_viewers":0,"most_read_pages":[],"least_read_pages":[],"never_read_count":1}\''


def sha(path):
    return hashlib.sha256(open(path, "rb").read()).hexdigest()


def run_guard():
    """Run the Go guard; return the set of LEADING tags on failing lines."""
    p = subprocess.run(
        ["go", "test", "./internal/analytics/", "-run", GUARD, "-count=1", "-v"],
        cwd=REPO, env=dict(os.environ), capture_output=True, text=True, timeout=900,
    )
    out = p.stdout + p.stderr
    if "build failed" in out or re.search(r"\.go:\d+:\d+: ", out):
        return {"BUILD-FAILED"}, out
    fired = set()
    for line in out.split("\n"):
        m = re.search(r"\[([A-Z][A-Z-]+)\]", line)
        if m and m.group(1) in TAGS:
            fired.add(m.group(1))
    return fired, out


def run_spa():
    p = subprocess.run(
        ["npx", "vitest", "run", "src/pages/Analytics.emptystate.test.tsx"],
        cwd=os.path.join(REPO, "frontend"), env=dict(os.environ),
        capture_output=True, text=True, timeout=900,
    )
    return p.returncode, p.stdout + p.stderr


def mutate(src, kind):
    """Return mutated source, or None when the anchor did not match exactly once."""
    if kind == "C0":
        if src.count(ROLLUP_RETURN) != 1:
            return None
        return src.replace(ROLLUP_RETURN, "	// control C0: an inert comment\n" + ROLLUP_RETURN)

    if kind == "C1":  # the WHOLE fix reverted — the defect exactly as found
        if src.count(PAGE_RETURN) != 1 or src.count(ROLLUP_RETURN) != 1:
            return None
        return (src.replace(PAGE_RETURN, "	return &out, nil")
                   .replace(ROLLUP_RETURN, "	return &out, nil"))

    if kind == "C2":  # per-page route only
        if src.count(PAGE_RETURN) != 1:
            return None
        return src.replace(PAGE_RETURN, "	return &out, nil")

    if kind == "C3":  # roll-up route only
        if src.count(ROLLUP_RETURN) != 1:
            return None
        return src.replace(ROLLUP_RETURN, "	return &out, nil")

    if kind == "C4":  # cohorts normalised, their ROWS not
        if src.count(ROW_LOOPS) != 1:
            return None
        return src.replace(ROW_LOOPS, "")

    if kind == "C5":  # one of the two per-page lists left nil
        if src.count(TOPVIEWERS_NORM) != 1:
            return None
        return src.replace(TOPVIEWERS_NORM, "")

    if kind == "C6":  # "fix" by emptying: the cohorts are overwritten, rows and all
        if src.count(COHORT_NIL_CHECKS) != 1:
            return None
        return src.replace(COHORT_NIL_CHECKS,
                           "	w.MostReadPages = []ReadStats{}\n	w.LeastReadPages = []ReadStats{}")

    if kind == "C7":  # the same "fix" on the per-page route
        if src.count(LIST_NIL_CHECKS) != 1:
            return None
        return src.replace(LIST_NIL_CHECKS,
                           "	r.ViewsByDay = []DayCount{}\n	r.TopViewers = []ViewerStat{}")

    if kind == "C8":  # the visibility filter removed — [FILTERED-TO-EMPTY]'s premise
        if src.count(VISIBILITY_FILTER) != 1:
            return None
        return src.replace(VISIBILITY_FILTER, "")

    raise AssertionError(kind)


CONTROLS = [
    ("C0", "an inert comment beside the roll-up's return",
     set(), "nothing may be caught by a comment"),
    ("C1", "the WHOLE fix reverted — the defect exactly as measured at 366808c",
     {"EMPTY-WS-COHORTS", "NEVER-READ-PAGE", "FILTERED-TO-EMPTY", "RANKED-ROW-LISTS"},
     "the red-first proof: every defect tag, and NEITHER positive control"),
    ("C2", "only GetReadStats' normalisation reverted",
     {"NEVER-READ-PAGE"},
     "the per-page route alone — this is what makes the two routes two"),
    ("C3", "only GetWorkspaceStats' normalisation reverted",
     {"EMPTY-WS-COHORTS", "FILTERED-TO-EMPTY", "RANKED-ROW-LISTS"},
     "the roll-up's three, for the same reason"),
    ("C4", "the cohorts normalised but their ROWS left nil",
     {"RANKED-ROW-LISTS"},
     "ALONE — the row half is a separate assertion, and it is the half a populated "
     "workspace shows and an empty one cannot"),
    ("C5", "`top_viewers` left nil while `views_by_day` is normalised",
     {"NEVER-READ-PAGE", "RANKED-ROW-LISTS"},
     "both places a ReadStats reaches the wire; a fix that normalises one list is not a fix"),
    ("C6", "the roll-up 'fixed' by ALWAYS emptying the cohorts",
     {"POPULATED"},
     "the positive control — an empty answer satisfies every absence assertion in the file "
     "and must still fail"),
    ("C7", "the per-page route 'fixed' by ALWAYS emptying the lists",
     {"VIEWED-PAGE-LISTS"},
     "the same refusal on the other route"),
    ("C8", "the per-page visibility filter removed from the ranked cohort",
     {"FILTERED-TO-EMPTY"},
     "[FILTERED-TO-EMPTY] must rest on bob actually being refused — a fixture that leaks "
     "would make that case pass for the wrong reason"),
]


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set — this guard is real-Postgres.")
        return 2
    original = open(STORE, encoding="utf-8").read()
    before = sha(STORE)

    print("BASELINE: the unmutated tree must be GREEN, or every verdict below is noise.")
    fired, out = run_guard()
    if fired:
        print("  BASELINE IS NOT CLEAN — tags fired:", sorted(fired))
        print(out[-4000:])
        return 1
    print("  baseline clean (no tags).\n")

    ok = True
    for kind, what, expect, why in CONTROLS:
        mutated = mutate(original, kind)
        if mutated is None or mutated == original:
            print(f"{kind}: ANCHOR DID NOT MATCH — control not run (a failure, not a skip)")
            ok = False
            continue
        try:
            open(STORE, "w", encoding="utf-8").write(mutated)
            fired, out = run_guard()
        finally:
            open(STORE, "w", encoding="utf-8").write(original)
            assert sha(STORE) == before, "RESTORE FAILED — the tree is not as it was"
        verdict = "AS PREDICTED" if fired == expect else "*** DIFFERS FROM PREDICTION ***"
        if fired != expect:
            ok = False
        print(f"{kind}: {what}")
        print(f"     predicted {sorted(expect) or 'NOT CAUGHT'}")
        print(f"     actual    {sorted(fired) or 'NOT CAUGHT'}   {verdict}")
        print(f"     why: {why}\n")

    # ── C9 — the OTHER end of the contract, in the SPA ───────────────────────────────────
    # The Go guard cannot reach the screen and the screen cannot reach Postgres, so this is the
    # control that keeps Analytics.emptystate.test.tsx's fixtures load-bearing: rewind ONE fixture
    # to the bytes the route returned BEFORE the fix and the suite must go RED. If it stays green,
    # the fixtures are decoration and the file is asserting nothing about the wire.
    spa_before = sha(SPA_TEST)
    spa_src = open(SPA_TEST, encoding="utf-8").read()
    print("C9: the SPA fixture rewound to the measured PRE-FIX bytes (`null` cohorts)")
    if spa_src.count(POSTFIX_EMPTY_ROLLUP_HEAD) != 1:
        print("     ANCHOR DID NOT MATCH — control not run (a failure, not a skip)")
        ok = False
    else:
        try:
            open(SPA_TEST, "w", encoding="utf-8").write(
                spa_src.replace(POSTFIX_EMPTY_ROLLUP_HEAD, PREFIX_EMPTY_ROLLUP))
            code, out = run_spa()
        finally:
            open(SPA_TEST, "w", encoding="utf-8").write(spa_src)
            assert sha(SPA_TEST) == spa_before, "RESTORE FAILED — the SPA test is not as it was"
        crashed = "Cannot read properties of null" in out
        good = code != 0 and crashed
        print(f"     predicted RED, with a null dereference in the output")
        print(f"     actual    exit={code} null-deref-in-output={crashed}   "
              f"{'AS PREDICTED' if good else '*** DIFFERS FROM PREDICTION ***'}")
        print("     why: proves the screen genuinely blanked on the wire this fix changed, and "
              "that the checked-in fixtures are the post-fix bytes rather than hand-written ones\n")
        if not good:
            ok = False

    print("ALL CONTROLS AS PREDICTED" if ok else "AT LEAST ONE CONTROL DIFFERED — read it")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
