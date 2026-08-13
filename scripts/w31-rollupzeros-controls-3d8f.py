#!/usr/bin/env python3
"""Positive controls for the ranked-cohort figures (W3.1, tab-3d8f, second claim).

Each control mutates ONE thing in the product tree, runs the guard, and reports the SET of [TAG]s
that fired. Every mutation is restored in a `finally`, verified by sha256 against the pre-mutation
bytes. The catcher is PREDICTED before the run; a control whose actual catcher differs from its
prediction is a finding about the guard, not a pass. Tags are scraped as the LEADING tag of a
failure line only.

Run:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-rollupzeros-controls-3d8f.py
"""
import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = os.path.join(REPO, "internal/analytics/store.go")
GUARD = "TestWorkspaceRollupFiguresAreMeasured_RealPG"

TAGS = ["ROLLUP-AGREES", "DISTINCT", "WINDOW", "TOTAL-KEPT", "LEAST-SAME", "RANK-KEPT"]

SELECT_HEAD = """		`SELECT pv.page_id, MAX(p.title), COUNT(*)::int,
                COUNT(DISTINCT pv.viewer_id)::int,
                COALESCE(AVG(pv.duration_sec)::int, 0),
                MAX(pv.created_at)"""

SCAN = """		if err := rows.Scan(&r.PageID, &r.Title, &r.TotalViews,
			&r.UniqueViewers, &r.AvgDurationSec, &lastViewed); err != nil {"""

RANKED_WINDOW = "          AND pv.created_at > NOW() - INTERVAL '1 day' * $2\n"

LASTVIEWED_SET = """		if lastViewed.Valid {
			tv := lastViewed.Time
			r.LastViewedAt = &tv
		}"""

# The PER-PAGE route's own average — C8 breaks THAT side to prove [ROLLUP-AGREES] is an agreement
# assertion and not a constant pinned to the roll-up.
PERPAGE_AVG = "                COALESCE(AVG(duration_sec)::int, 0),"


def sha(path):
    return hashlib.sha256(open(path, "rb").read()).hexdigest()


def run_guard():
    p = subprocess.run(["go", "test", "./internal/analytics/", "-run", GUARD, "-count=1", "-v"],
                       cwd=REPO, env=dict(os.environ), capture_output=True, text=True, timeout=900)
    out = p.stdout + p.stderr
    if "build failed" in out or re.search(r"\.go:\d+:\d+: ", out):
        return {"BUILD-FAILED"}, out
    fired = set()
    for line in out.split("\n"):
        m = re.search(r"\[([A-Z][A-Z-]+)\]", line)
        if m and m.group(1) in TAGS:
            fired.add(m.group(1))
    # ⚠ A FAILING RUN WITH NO TAG IS NOT "NOT CAUGHT" — IT IS AN UNRUNNABLE CONTROL, AND THE
    # DIFFERENCE MATTERS BECAUSE THEY PRINT THE SAME WORD. C3's first version deleted the ranked
    # query's window predicate and left `days` in the argument list, so Postgres refused the bind
    # ("2 parameters, but prepared statement requires 1"), the route answered 500, and the
    # fixture's own untagged t.Fatalf fired. It scored NOT CAUGHT — which reads as a hole in the
    # guard when the guard never ran. Anything that fails without a tag is reported as such.
    if p.returncode != 0 and not fired:
        return {"UNTAGGED-FAIL"}, out
    return fired, out


def mutate(src, kind):
    if kind == "C0":
        if src.count(SCAN) != 1:
            return None
        return src.replace(SCAN, "		// control C0: an inert comment\n" + SCAN)

    if kind == "C1":  # the WHOLE fix reverted — the defect exactly as found
        if src.count(SELECT_HEAD) != 1 or src.count(SCAN) != 1 or src.count(LASTVIEWED_SET) != 1:
            return None
        return (src.replace(SELECT_HEAD, "		`SELECT pv.page_id, MAX(p.title), COUNT(*)::int")
                   .replace(SCAN, "		_ = lastViewed\n		if err := rows.Scan(&r.PageID, &r.Title, &r.TotalViews); err != nil {")
                   .replace(LASTVIEWED_SET, ""))

    if kind == "C2":  # DISTINCT dropped
        a = "                COUNT(DISTINCT pv.viewer_id)::int,"
        if src.count(a) != 1:
            return None
        return src.replace(a, "                COUNT(pv.viewer_id)::int,")

    if kind == "C3":  # the ranked query's window WIDENED to ~82 years
        # NOT deleted: `$2` must stay referenced or Postgres refuses the bind and the control
        # measures a broken query instead of a widened window. See run_guard's UNTAGGED-FAIL note.
        if src.count(RANKED_WINDOW) != 1:
            return None
        return src.replace(
            RANKED_WINDOW,
            "          AND pv.created_at > NOW() - INTERVAL '1 day' * ($2 * 1000)\n")

    if kind == "C4":  # AVG → MAX
        a = "                COALESCE(AVG(pv.duration_sec)::int, 0),"
        if src.count(a) != 1:
            return None
        return src.replace(a, "                COALESCE(MAX(pv.duration_sec)::int, 0),")

    if kind == "C5":  # last_viewed_at never carried onto the row
        if src.count(LASTVIEWED_SET) != 1:
            return None
        return src.replace(LASTVIEWED_SET, "		_ = lastViewed")

    if kind == "C6":  # "fixed" with a constant instead of a measurement
        if src.count(LASTVIEWED_SET) != 1:
            return None
        return src.replace(LASTVIEWED_SET, LASTVIEWED_SET + "\n		r.UniqueViewers = 3")

    if kind == "C7":  # the two figures collapsed into one
        a = "		`SELECT pv.page_id, MAX(p.title), COUNT(*)::int,"
        if src.count(a) != 1:
            return None
        return src.replace(a, "		`SELECT pv.page_id, MAX(p.title), COUNT(DISTINCT pv.viewer_id)::int,")

    if kind == "C8":  # break the OTHER side of the agreement
        if src.count(PERPAGE_AVG) != 1:
            return None
        return src.replace(PERPAGE_AVG, "                0,")

    raise AssertionError(kind)


CONTROLS = [
    ("C0", "an inert comment beside the ranked scan", set(),
     "nothing may be caught by a comment"),
    ("C1", "the WHOLE fix reverted — the defect exactly as measured at 8b1ccab",
     {"ROLLUP-AGREES", "DISTINCT", "WINDOW", "RANK-KEPT"},
     "the red-first proof. [RANK-KEPT] joins because the 1-view page also reports 0 unique "
     "viewers — the fabricated zero is on EVERY row, not the busy one"),
    ("C2", "`COUNT(DISTINCT viewer_id)` → `COUNT(viewer_id)`",
     {"ROLLUP-AGREES", "DISTINCT"},
     "carol read it twice; distinctness is the whole content of that figure. ⚠ THIS CONTROL "
     "CHANGED THE GUARD: its first run also fired [WINDOW], because that case carried a "
     "`unique_viewers > 3` assertion which 4 satisfies for a reason unrelated to any window. "
     "The assertion was deleted (it was subsumed by [DISTINCT]) and [WINDOW] given a page whose "
     "entire readership is outside the window instead"),
    ("C3", "the ranked query's window widened from 30 days to ~82 years",
     {"ROLLUP-AGREES", "DISTINCT", "WINDOW", "TOTAL-KEPT", "RANK-KEPT"},
     "the 400-day-old views enter every aggregate at once, and the 400-day-old PAGE joins the "
     "ranking — this is why the fixture carries both. ⚠ [RANK-KEPT] WAS MISSING FROM THE FIRST "
     "PREDICTION even though this very sentence named the reason: the ranking grows from 2 rows "
     "to 3. The prose was right and the set was wrong, which is the failure mode a predicted "
     "SET catches and a predicted paragraph does not"),
    ("C4", "`AVG(duration_sec)` → `MAX(duration_sec)`",
     {"ROLLUP-AGREES", "WINDOW"},
     "a plausible wrong aggregate that still returns a non-zero number"),
    ("C5", "`last_viewed_at` never carried onto the ranked row",
     {"ROLLUP-AGREES"},
     "ALONE — the third field is its own assertion, and it is the one that was honest before "
     "the fix (absent, not zero)"),
    ("C6", "'fixed' with the constant 3 instead of a measurement",
     {"RANK-KEPT"},
     "the positive control: the 1-view page must still report 1, so a hard-coded answer that "
     "satisfies the busy page fails"),
    ("C7", "`COUNT(*)` replaced by the distinct count — the two figures collapsed",
     {"TOTAL-KEPT"},
     "ALONE, and I PREDICTED {TOTAL-KEPT, ROLLUP-AGREES} AND WAS WRONG — the guard is more "
     "precise than the prediction. This mutation leaves unique_viewers, avg and last_viewed "
     "correct, so the two routes still AGREE; only the total is wrong. total_views is a "
     "DIFFERENT question from unique_viewers and [TOTAL-KEPT] is the only case that asks it"),
    ("C8", "the PER-PAGE route's own average broken instead",
     {"ROLLUP-AGREES"},
     "ALONE, and I PREDICTED {ROLLUP-AGREES, WINDOW} AND WAS WRONG for the same reason: this "
     "mutation moves the PER-PAGE side, and [WINDOW]'s literal pins the ROLL-UP side, which is "
     "untouched. What it proves is the thing it was written for — [ROLLUP-AGREES] is an "
     "agreement between two routes, not a constant pinned to one, so breaking EITHER side reds it"),
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
        print(out[-3000:])
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

    print("ALL CONTROLS AS PREDICTED" if ok else "AT LEAST ONE CONTROL DIFFERED — read it")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
