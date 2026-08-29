#!/usr/bin/env python3
"""Positive controls for the semantic half's OFFSET (W3.1, tab-b74e).

Each control mutates PRODUCT code, runs the search package, and is scored against a catcher
PREDICTED BEFORE THE RUN.

⚠ THE VERDICT IS READ AT ASSERTION GRANULARITY, NOT TEST GRANULARITY, AND THAT IS THE WHOLE
POINT HERE. This change ships ONE test with EIGHT assertions. Every predecessor harness in this
repo scores `--- FAIL: <TestName>`, and under that scoring O1 through O7 would all read
"CAUGHT by TestSearch_Offset_..." while saying NOTHING about which of the eight assertions is
load-bearing and which is decoration. Four of the controls below exist only to isolate ONE
assertion each; they are unreadable without message-level scoring.

⚠ A PREMISE THAT FIRES IS AN ERROR, NOT A CATCH. The two premise assertions are t.Fatalf, so
anything they abort NEVER RUNS — a control scored CAUGHT off a premise would be naming an
assertion that did not execute. Those are scored ERROR.

INHERITED FROM THIS REPO'S EARLIER HARNESSES, AND NOT RE-DERIVED:
  · RESTORE IN A finally, and PROVE it with a closing sha256 over every touched file.
  · A COMPILE ERROR IS NOT A CATCH — `go test` exits non-zero for a build failure exactly as it
    does for a failed assertion. Build failures are scored ERROR, a defect IN THE CONTROL.
  · ASSERT THE ANCHOR BEFORE WRITING. 0 sites makes an inert control read as a blind guard;
    2 sites makes it ambiguous.
  · EVERY CONTROL DECLARES ITS MUST-STAY-GREEN SET.

⚠ ONE HONEST NOTE ON O1, STATED RATHER THAN HIDDEN: reverting the SQL changes the ARITY of the
pgxmock expectations updated alongside the signature (semantic_test.go, semanticaccess_test.go,
metering_test.go), so those tests red under O1 for a mechanical reason — an argument count — and
not because they can observe a page repeating. They are reported below as `also red` and are
deliberately NOT in any must-stay-green set.

Usage:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-search-offset-controls.py
"""

import hashlib
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

HANDLER = "internal/search/handler.go"
SEMANTIC = "internal/search/semantic.go"
TOUCHED = (HANDLER, SEMANTIC)

G_OFFSET = "TestSearch_Offset_AppliesToSemanticHalf_RealPG"
G_SPACE = "TestSearch_SpaceFilter_AppliesToSemanticHalf_RealPG"

# The eight assertions of G_OFFSET, keyed by a distinct prefix of the message each one prints.
# A reworded message makes every control read NOT CAUGHT, which is loud rather than silent.
ASSERTIONS = {
    "PREMISE-SEM": "PREMISE FAILED: unpaged semantic search",
    "PREMISE-FT": "PREMISE FAILED: unpaged full-text search",
    "OVERCORRECT-SEM-P1": "OVER-CORRECTION: semantic page 1",
    "SEM-PAGE2": "offset IGNORED by the semantic half",
    "SEM-TAIL": "offset PAST THE END returned",
    "OVERCORRECT-ALL-P1": "OVER-CORRECTION on type=all",
    "ALL-PAGE2": "offset IGNORED on type=all",
    "FT-REGRESSION": "REGRESSION on the full-text half",
}
PREMISES = ("PREMISE-SEM", "PREMISE-FT")

SEM_LIMIT = "        LIMIT $3 OFFSET $5`,\n\t\tencoded, workspaceID, limit, spaceID, offset,\n"
SEM_LIMIT_LINE = "        LIMIT $3 OFFSET $5`"
SEM_CALL = "sem, semEr = h.semantic.Search(ctx, wsID, q, spaceID, fetchLimit, sqlOffset)"
FT_CALL = "ft, ftEr = h.pages.SearchWithRank(r.Context(), wsID, q, spaceID, fetchLimit, sqlOffset)"
CLAMP = "\tif offset < 0 {\n\t\toffset = 0\n\t}\n"

# ⚠ THE SEAM THAT DECIDES A type=all PAGE MOVED, AND IT MOVED OUT OF SQL ENTIRELY.
# 4417532 ("paging a merged search stranded the rows the merge had not seen") made the
# two-source path fetch from row 0 with a WIDER window and apply the caller's offset AFTER the
# merge and AFTER the access filter — the only place a position in the merged answer exists.
# `sqlOffset` is therefore the caller's offset on a SINGLE-source search and hard 0 on type=all,
# which handler.go says in its own comment. Everything below follows from that.
MERGED_OFFSET = ("\tif twoSources {\n\t\tif offset >= len(rows) {\n\t\t\trows = rows[:0]\n"
                 "\t\t} else {\n\t\t\trows = rows[offset:]\n\t\t}\n\t}\n")

# (id, description, [(file, anchor, replacement), ...], predicted assertion tags, must-stay-green tags)
CONTROLS = [
    (
        "O1",
        "THE REVERT: the semantic SQL loses its OFFSET and its bound argument (the shipped defect)",
        # ONE atomic edit covering BOTH the SQL and the argument list. Two separate edits in one
        # file is how a harness applies half of itself: the second read would not see the first
        # write, and a partially reverted query is a different mutation from the one named here.
        [(SEMANTIC, SEM_LIMIT, "        LIMIT $3`,\n\t\tencoded, workspaceID, limit, spaceID,\n")],
        # ⚠ ALL-PAGE2 WAS PREDICTED HERE AND IS NOT REACHABLE FROM THIS MUTATION ANY MORE, AND
        # THAT IS A FACT ABOUT THE PRODUCT RATHER THAN ABOUT THIS CONTROL. Since 4417532 the
        # type=all path passes sqlOffset=0 to BOTH stores on purpose and pages after the merge,
        # so no defect in the semantic query's OFFSET can move a type=all page. Measured: with
        # the anchors mechanically re-pointed and nothing else changed, O1, O2 and O3 each fired
        # SEM-PAGE2 and SEM-TAIL and NOT ALL-PAGE2 — three controls predicting an assertion none
        # of them can produce. O6 below is what claims ALL-PAGE2 now.
        ["SEM-PAGE2", "SEM-TAIL"],
        ["OVERCORRECT-SEM-P1", "OVERCORRECT-ALL-P1", "FT-REGRESSION"],
    ),
    (
        "O2",
        "THE CALL SITE, not the query: the handler passes a literal 0 to a store that pages correctly",
        [(HANDLER, SEM_CALL, SEM_CALL.replace("fetchLimit, sqlOffset)", "fetchLimit, 0)"))],
        # ⚠ ALL-PAGE2 WAS PREDICTED HERE AND IS NOT REACHABLE FROM THIS MUTATION ANY MORE, AND
        # THAT IS A FACT ABOUT THE PRODUCT RATHER THAN ABOUT THIS CONTROL. Since 4417532 the
        # type=all path passes sqlOffset=0 to BOTH stores on purpose and pages after the merge,
        # so no defect in the semantic query's OFFSET can move a type=all page. Measured: with
        # the anchors mechanically re-pointed and nothing else changed, O1, O2 and O3 each fired
        # SEM-PAGE2 and SEM-TAIL and NOT ALL-PAGE2 — three controls predicting an assertion none
        # of them can produce. O6 below is what claims ALL-PAGE2 now.
        ["SEM-PAGE2", "SEM-TAIL"],
        ["OVERCORRECT-SEM-P1", "OVERCORRECT-ALL-P1", "FT-REGRESSION"],
    ),
    (
        "O3",
        "TAUTOLOGY: the parameter is bound and referenced by the SQL, and always evaluates to 0",
        # The control that makes 'is the offset passed?' an inadequate question. A guard that
        # inspected the argument list — or a pgxmock WithArgs — passes this. Only reading ROWS fails it.
        [(SEMANTIC, SEM_LIMIT_LINE, "        LIMIT $3 OFFSET ($5::int * 0)`")],
        # ⚠ ALL-PAGE2 WAS PREDICTED HERE AND IS NOT REACHABLE FROM THIS MUTATION ANY MORE, AND
        # THAT IS A FACT ABOUT THE PRODUCT RATHER THAN ABOUT THIS CONTROL. Since 4417532 the
        # type=all path passes sqlOffset=0 to BOTH stores on purpose and pages after the merge,
        # so no defect in the semantic query's OFFSET can move a type=all page. Measured: with
        # the anchors mechanically re-pointed and nothing else changed, O1, O2 and O3 each fired
        # SEM-PAGE2 and SEM-TAIL and NOT ALL-PAGE2 — three controls predicting an assertion none
        # of them can produce. O6 below is what claims ALL-PAGE2 now.
        ["SEM-PAGE2", "SEM-TAIL"],
        ["OVERCORRECT-SEM-P1", "OVERCORRECT-ALL-P1", "FT-REGRESSION"],
    ),
    (
        "O4",
        "OVER-CORRECTION: the offset is applied twice (page 2 skips past the rows it should show)",
        # ISOLATES SEM-PAGE2's exact-equality requirement. offset=2 becomes 4 ⇒ the semantic page is
        # EMPTY, which SEM-TAIL (which wants empty at offset=4 ⇒ 8) cannot see, and which ALL-PAGE2
        # cannot see either because the full-text half then supplies [three four] on its own.
        [(SEMANTIC, SEM_LIMIT_LINE, "        LIMIT $3 OFFSET ($5::int * 2)`")],
        ["SEM-PAGE2"],
        ["OVERCORRECT-SEM-P1", "OVERCORRECT-ALL-P1", "FT-REGRESSION", "SEM-TAIL", "ALL-PAGE2"],
    ),
    (
        "O5",
        "the offset works for small values and is dropped past the end (the tail stays unreachable)",
        # ISOLATES SEM-TAIL. Without that assertion, a fix that pages correctly for offset ≤ 3 and
        # silently re-serves page 1 beyond it would be indistinguishable from a correct one — which
        # is precisely the shape the shipped defect had at EVERY offset.
        [(SEMANTIC, SEM_LIMIT_LINE,
          "        LIMIT $3 OFFSET (CASE WHEN $5::int > 3 THEN 0 ELSE $5::int END)`")],
        ["SEM-TAIL"],
        ["OVERCORRECT-SEM-P1", "OVERCORRECT-ALL-P1", "FT-REGRESSION", "SEM-PAGE2", "ALL-PAGE2"],
    ),
    (
        "O6",
        "the offset reaches the semantic half only when type=semantic — the DEFAULT type stays broken",
        # ISOLATES ALL-PAGE2. One predicate spelled twice: the explicitly-asked-for type is fixed and
        # the type the frontend actually sends is not, which no type=semantic case can see.
        # ⚠⚠ THIS CONTROL MUTATED THE PRODUCT INTO AN EXACT ALIAS OF ITSELF AND SCORED
        # "NOT CAUGHT — fired nothing". Its original mutation inserted
        #     semOffset := offset; if kind == "all" { semOffset = 0 }
        # which is, character for character, what handler.go now does as `sqlOffset` (line 244:
        # `sqlOffset := offset`, line 249: `sqlOffset = 0` under `twoSources`). 4417532 turned
        # the DEFECT this control describes into the DESIGN. A byte-level void check passes it —
        # the inserted lines really are new bytes — so nothing but running it and reading the
        # tags could have found this.
        #
        # ⚠⚠⚠ SO THE ANCHOR WAS NEVER THE WHOLE PROBLEM, AND RE-POINTING IT WOULD HAVE SHIPPED A
        # CONTROL THAT CANNOT FAIL. The mutation is re-aimed at the seam that decides a type=all
        # page TODAY: the post-merge slice. Deleting it makes every type=all page start at row 0,
        # which is the same user-visible defect the original described — the default search type
        # stuck on page 1 — reached through the code that now causes it.
        [(HANDLER, MERGED_OFFSET, MERGED_OFFSET.replace("\tif twoSources {", "\tif false {", 1))],
        ["ALL-PAGE2"],
        ["OVERCORRECT-SEM-P1", "OVERCORRECT-ALL-P1", "FT-REGRESSION", "SEM-PAGE2", "SEM-TAIL"],
    ),
    (
        "O7",
        "the FULL-TEXT half loses its offset instead (the half that was already right)",
        # ISOLATES FT-REGRESSION, the must-stay-green of the pair. Every other assertion in the file
        # can be satisfied by the semantic rows alone, so without this control nothing says the
        # working half is still protected while the broken one is being fixed.
        [(HANDLER, FT_CALL, FT_CALL.replace("fetchLimit, sqlOffset)", "fetchLimit, 0)"))],
        ["FT-REGRESSION"],
        ["OVERCORRECT-SEM-P1", "OVERCORRECT-ALL-P1", "SEM-PAGE2", "SEM-TAIL", "ALL-PAGE2"],
    ),
    (
        "O8",
        "the store's negative-offset clamp is deleted — PREDICTED INERT, and recorded as such",
        # ONE-DIRECTIONAL BY DESIGN, and it is a limit rather than a result. Handler.Search already
        # clamps a negative offset to 0 before the store is called, so the store's own clamp is an
        # invariant held twice and NO one-line control can breach it. It exists to mirror
        # SearchWithRank, which clamps identically, so the two halves cannot drift. Scored INERT if
        # nothing reddens — deleting it instead of recording it would leave the next person to
        # rediscover why the guard "failed".
        [(SEMANTIC, CLAMP, "")],
        [],
        list(ASSERTIONS),
    ),
]


def sha(path):
    with open(os.path.join(ROOT, path), "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def read(path):
    with open(os.path.join(ROOT, path), encoding="utf-8") as fh:
        return fh.read()


def write(path, body):
    with open(os.path.join(ROOT, path), "w", encoding="utf-8") as fh:
        fh.write(body)


def run_suite():
    """Return (build_ok, fired_assertion_tags, failing_test_names, raw)."""
    proc = subprocess.run(
        ["go", "test", "-count=1", "-v", "./internal/search/"],
        cwd=ROOT, capture_output=True, text=True, timeout=900,
    )
    out = proc.stdout + proc.stderr
    build_broken = ("[build failed]" in out) or ("--- FAIL" not in out and proc.returncode != 0
                                                 and "FAIL" in out and "ok " not in out)
    fired = sorted(tag for tag, needle in ASSERTIONS.items() if needle in out)
    failing = sorted(set(re.findall(r"--- FAIL: (\w+)", out)))
    return (not build_broken), fired, failing, out


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set — these controls need the real-PG guards to RUN.")
        return 2

    before = {p: sha(p) for p in TOUCHED}

    ok, fired, failing, out = run_suite()
    if not ok or failing:
        print("BASELINE IS NOT GREEN — controls cannot be scored against it.")
        print(out[-4000:])
        return 2
    print(f"baseline: build ok, 0 failing tests, 0 of {len(ASSERTIONS)} assertions fired\n")

    results = []
    for cid, desc, edits, predicted, stay_green in CONTROLS:
        originals = {}
        try:
            for path, anchor, repl in edits:
                body = read(path)
                originals.setdefault(path, body)
                n = body.count(anchor)
                if n != 1:
                    raise AssertionError(f"{cid}: anchor matched {n} sites in {path}, want exactly 1")
                write(path, body.replace(anchor, repl, 1))

            built, fired, failed, raw = run_suite()
            premises = [t for t in fired if t in PREMISES]
            caught = [t for t in predicted if t in fired]
            broke = [t for t in stay_green if t in fired]
            if not built:
                verdict = "ERROR (control does not compile — a defect in the CONTROL)"
            elif premises:
                # An aborted premise means every assertion after it never executed, so a CAUGHT
                # read off this run would be naming an assertion that did not run.
                verdict = f"ERROR (premise {premises} aborted the test — later assertions never ran)"
            elif broke:
                verdict = f"IMPRECISE — also fired must-stay-green {broke}"
            elif not predicted:
                verdict = ("INERT AS PREDICTED (one-directional by design)" if not fired
                           else f"UNEXPECTED — predicted inert, fired {fired}")
            elif sorted(caught) == sorted(predicted):
                verdict = "CAUGHT by the assertion(s) named before the run"
            elif caught:
                verdict = f"PARTIAL — predicted {predicted}, fired {caught}"
            else:
                verdict = f"NOT CAUGHT — predicted {predicted}, fired {fired or 'nothing'}"
            also = [t for t in failed if t not in (G_OFFSET,)]
            results.append((cid, desc, predicted, fired, also, verdict))
        finally:
            for path, body in originals.items():
                write(path, body)

    after = {p: sha(p) for p in TOUCHED}
    restored = after == before

    print("=" * 100)
    for cid, desc, predicted, fired, also, verdict in results:
        print(f"\n{cid}  {desc}")
        print(f"     predicted : {', '.join(predicted) if predicted else '(nothing — inert by design)'}")
        print(f"     fired     : {', '.join(fired) if fired else '(none)'}")
        print(f"     also red  : {', '.join(also) if also else '(no other test)'}")
        print(f"     verdict   : {verdict}")
    print("\n" + "=" * 100)
    print(f"tree restored (sha256 on {len(before)} files): {restored}")
    if not restored:
        for p in before:
            if before[p] != after[p]:
                print(f"  ⚠ NOT RESTORED: {p}")
        return 1

    good = sum(1 for r in results if r[5].startswith(("CAUGHT", "INERT AS PREDICTED")))
    print(f"{good}/{len(results)} controls scored as predicted before the run")
    return 0 if good == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
