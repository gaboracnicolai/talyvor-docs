#!/usr/bin/env python3
"""
Positive controls for the truncated-decision-stream guard (tab-6f3d).

THE DEFECT: Store.aggregate counted review_decisions rows without ever reading rows.Err(). A
Postgres error raised WHILE the rows stream leaves pgx handing over every row produced so far,
each with a nil Scan error, and Next() returning false exactly as at a clean end of stream. The
counters make that silence one-directional — an unread row cannot be `rejected` and cannot be
`pending` — so a truncated read can only move the verdict TOWARD approval. Measured through the
shipped Decide on real Postgres: 2 of 4 rows read, a reviewer's `rejected` row unread, Decide
returning nil, approval_requests.status and pages.doc_status both written "approved".

WHAT EACH CONTROL HAS TO SHOW, because "the guard went red" is not by itself evidence:

  E1  the defect verbatim               → the three consequence tags, and nothing else
  E2  rows.Err() called and DISCARDED   → the same three: the assertion pins the RETURN, not
                                          the presence of a call somebody can grep for
  E3  the error swallowed into Pending  → [NO-VERDICT-FROM-PARTIAL] ALONE. This is the control
                                          that earns the error assertion: a "graceful"
                                          degradation writes nothing, so both write tags stay
                                          GREEN and only the return tag can see it
  E4  an error returned UNCONDITIONALLY → the happy-path tags. A fix that refuses every stream
                                          satisfies every truncation assertion in the file
  E5  THE VACUITY CONTROL — the fixture disarmed so the injected view never errors →
                            [TRUNCATION-IS-REAL] ALONE. Every other assertion in the file is
                            green against a fault that stopped firing, which is the exact
                            shape of a guard that cannot fail
  E6  a different aggregate bug         → NOT CAUGHT here, CAUGHT by the package's own tests.
                                          Scope, said rather than implied: this file is not a
                                          catch-all for aggregate

⚠ TWO PREDICTIONS WERE WRONG ON THE FIRST RUN AND ARE RECORDED HERE RATHER THAN RE-SCOPED,
because both were claims of ISOLATION that the measurement refused:

  E4 was predicted to fire the intact-stream test ALONE. It fires that test AND FIVE of the
     package's existing Decide tests (AllApproved, AnyRejected, StillPending, the authorized-page
     real-PG test, the empty-space-segment one) — obvious afterwards: breaking aggregate for
     every stream breaks every test that drives Decide to a verdict. So the intact-stream test
     is CORROBORATING, not load-bearing; no mutation in this set is caught by it alone. It is
     kept because it is the paired control on the SAME fixture — same four rows, fault vs no
     fault — which is what makes the truncated verdict attributable to the injected fault.

  E5 was predicted to fire [TRUNCATION-IS-REAL] ALONE. It also fires [NO-VERDICT-FROM-PARTIAL],
     and that is correct rather than noisy: with the fault disarmed the stream is COMPLETE, so
     Decide rightly returns nil and the "must return an error" assertion has nothing to see.
     The consequence is worth stating plainly — [NO-VERDICT-FROM-PARTIAL] cannot by itself tell
     "the fix works" from "the fault never fired", and [TRUNCATION-IS-REAL] is the only thing
     that can. The two firing TOGETHER is the signal for a disarmed fixture.

Verdicts are the SET of failing test names plus every [TAG] seen in the output, predicted
before the run. A build failure is detected and scored as BUILD, never as a catch.

Restores are `cp` from bytes saved before the run, in a `finally`, sha256-compared at the end.

Run with a live Postgres:
    DSN=postgres://postgres:postgres@localhost:55436/postgres?sslmode=disable \\
        python3 scripts/w31-decisionstream-controls-6f3d.py
"""

import hashlib
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile

REPO = pathlib.Path(__file__).resolve().parent.parent
DSN = os.environ.get(
    "DSN", "postgres://postgres:postgres@localhost:55436/postgres?sslmode=disable"
)

STORE = "internal/approval/store.go"
GUARD = "internal/approval/decisionstream_realpg_test.go"
FILES = [STORE, GUARD]

PKG = "./internal/approval/"

TRUNCATED = "TestApprovalDecide_TruncatedDecisionStreamIsNotAVerdict_RealPG"
INTACT = "TestApprovalDecide_IntactDecisionStreamStillDecides_RealPG"

FIX = """	// A STREAM THAT FAILED MID-FLIGHT IS NOT A COMPLETE COUNT"""


def sh(args, **kw):
    env = dict(os.environ, DOCS_TEST_DATABASE_URL=DSN)
    return subprocess.run(args, cwd=REPO, capture_output=True, text=True, env=env, **kw)


def sha256(p):
    return hashlib.sha256((REPO / p).read_bytes()).hexdigest()


def run_tests():
    """Return (failing test names | tag:TAG, build_broke, raw)."""
    r = sh(["go", "test", "-timeout", "300s", "-count=1", PKG])
    out = r.stdout + r.stderr
    if "build failed" in out or re.search(r"^\S+\.go:\d+:\d+: ", out, re.M):
        return set(), True, out
    failed = set(re.findall(r"^--- FAIL: (\S+)", out, re.M))
    tags = set(re.findall(r"\[([A-Z0-9-]+)\]", out))
    return failed | {"tag:" + t for t in tags}, False, out


def sub(path, old, new, count=1):
    """Replace, FAILING LOUDLY when the anchor is absent or ambiguous.

    An anchor that matches nothing applies the control to nothing, and its silence reads
    exactly like a mutation the guard survived. An anchor that matches twice mutates a place
    that was not intended — my own doc comment quoting the code is the usual culprit.
    """
    p = REPO / path
    s = p.read_text()
    n = s.count(old)
    if n != 1:
        raise SystemExit(
            f"ANCHOR MATCHED {n}x in {path}: {old[:90]!r} — a control applied to nothing (or to "
            f"the wrong thing) would have been scored as a result"
        )
    p.write_text(s.replace(old, new, count))


# ── controls ────────────────────────────────────────────────────────────────

def e0_inert(_):
    """A comment reworded in the mutated function. Nothing about behaviour changes."""
    sub(STORE, "// aggregate returns the final ApprovalStatus given the current",
        "// aggregate computes the final ApprovalStatus from the current")


def e1_defect_verbatim(_):
    """The shipped defect: the rows.Err() check deleted outright."""
    s = (REPO / STORE).read_text()
    start = s.index(FIX)
    end = s.index("\tif rejected > 0 {", start)
    (REPO / STORE).write_text(s[:start] + s[end:])


def e2_err_called_and_discarded(_):
    """rows.Err() is called — and thrown away. A grep for `rows.Err` finds it; it does nothing."""
    e1_defect_verbatim(None)
    sub(STORE, "\tif rejected > 0 {", "\t_ = rows.Err()\n\tif rejected > 0 {")


def e3_swallowed_into_pending(_):
    """The plausible 'graceful degradation': notice the failure, answer Pending, return nil.

    Decide returns early on Pending and writes NOTHING, so both write assertions stay green.
    Only the return-value assertion can see this.
    """
    sub(STORE,
        '\tif err := rows.Err(); err != nil {\n\t\treturn "", fmt.Errorf("approval: aggregate decisions: %w", err)\n\t}',
        "\tif err := rows.Err(); err != nil {\n\t\treturn ApprovalPending, nil\n\t}")


def e4_always_errors(_):
    """A 'fix' that refuses every stream, intact or not. Satisfies every truncation assertion."""
    sub(STORE,
        '\tif err := rows.Err(); err != nil {\n\t\treturn "", fmt.Errorf("approval: aggregate decisions: %w", err)\n\t}',
        '\treturn "", fmt.Errorf("approval: aggregate decisions: %w", errors.New("always"))')


def e5_fixture_disarmed(_):
    """THE VACUITY CONTROL. The injected view stops erroring; the fault under test is gone.

    Nothing about the production code changes. If the file still passes in full, then every
    assertion in it was being satisfied by an ordinary approval and the guard is theatre.
    """
    sub(GUARD,
        "		          CASE WHEN decision = 'rejected'\n"
        "		               THEN (1/(position('r' in decision) - 1))::text\n"
        "		               ELSE decision END AS decision,",
        "		          decision AS decision,")


def e6_pending_ignored(_):
    """A DIFFERENT aggregate defect: approve while reviewers are still pending.

    Predicted NOT CAUGHT by this file — it is about truncation, not about the quorum rule —
    and CAUGHT by the package's existing pgxmock test. CAUGHT is not a catch-all.
    """
    sub(STORE, "\tif pending == 0 && approved > 0 {", "\tif approved > 0 {")


CONTROLS = [
    ("E0-inert-comment-reworded", e0_inert, set(),
     "an inert edit in the mutated function. A control set whose 'inert' entry reddens is "
     "measuring the suite's mood, not the guard"),
    ("E1-defect-verbatim-rows-Err-deleted", e1_defect_verbatim,
     {TRUNCATED, "tag:NO-VERDICT-FROM-PARTIAL", "tag:REQUEST-NOT-FLIPPED", "tag:PAGE-NOT-FLIPPED"},
     "the defect as it shipped. All three consequence tags, and the intact-stream test stays "
     "green — the fix is about a failed read, not about the verdict rules"),
    ("E2-rows-Err-called-then-discarded", e2_err_called_and_discarded,
     {TRUNCATED, "tag:NO-VERDICT-FROM-PARTIAL", "tag:REQUEST-NOT-FLIPPED", "tag:PAGE-NOT-FLIPPED"},
     "the call is present and inert. Identical verdict to E1, which is the point: the "
     "assertion is on what aggregate RETURNS, not on a line somebody can grep for"),
    ("E3-error-swallowed-into-Pending", e3_swallowed_into_pending,
     {TRUNCATED, "tag:NO-VERDICT-FROM-PARTIAL"},
     "THE ISOLATING CONTROL. Pending makes Decide return before either write, so both write "
     "tags stay GREEN and only the return tag fires. Without this the error assertion would "
     "look redundant with the two row assertions"),
    ("E4-error-returned-unconditionally", e4_always_errors,
     {INTACT, "tag:HAPPY-PATH-STILL-WORKS", "tag:A-PREMISE",
      "TestApprovalDecide_MustBelongToTheAuthorizedPage_RealPG",
      "TestDecide_AllApproved_FlipsRequestAndPageToApproved",
      "TestDecide_AnyRejected_FlipsToRejected",
      "TestDecide_StillPending_LeavesRequestStatusAlone",
      "TestDecide_WithAnEmptySpaceSegment_StillReachesTheHandler_RealPG"},
     "MIS-PREDICTED AS THE INTACT TEST ALONE (see the header). A fix that refuses every stream "
     "breaks every test that drives Decide to a verdict, so this file's happy-path assertion is "
     "corroborating rather than load-bearing — said here instead of left to look earned"),
    ("E5-VACUITY-fixture-disarmed", e5_fixture_disarmed,
     {TRUNCATED, "tag:TRUNCATION-IS-REAL", "tag:NO-VERDICT-FROM-PARTIAL"},
     "THE FLOOR. The production code is untouched and the injected fault is gone. MIS-PREDICTED "
     "AS THE FLOOR ALONE: a disarmed fixture leaves a COMPLETE stream, so the error assertion "
     "fires too — which is the proof that it cannot tell a working fix from an absent fault"),
    ("E6-pending-reviewers-ignored", e6_pending_ignored,
     {"TestDecide_StillPending_LeavesRequestStatusAlone"},
     "a real but DIFFERENT aggregate defect. Caught by the package's own pgxmock test and NOT "
     "by this file — scope stated rather than left to be assumed"),
]


def main():
    for f in FILES:
        if not (REPO / f).exists():
            raise SystemExit(f"missing {f}")
    before = {f: sha256(f) for f in FILES}
    backup = pathlib.Path(tempfile.mkdtemp(prefix="w31-decisionstream-6f3d-"))
    for f in FILES:
        shutil.copy2(REPO / f, backup / f.replace("/", "__"))

    def restore():
        for f in FILES:
            shutil.copy2(backup / f.replace("/", "__"), REPO / f)

    rows = []
    try:
        # E-pristine — MUST STAY GREEN, or every CAUGHT below could be pre-existing red.
        failed, broke, _ = run_tests()
        rows.append(("E-pristine-must-stay-green", set(), failed, broke,
                     "the tree under test is green before any mutation"))
        for label, apply, predicted, why in CONTROLS:
            restore()
            apply(None)
            failed, broke, _ = run_tests()
            rows.append((label, predicted, failed, broke, why))
            restore()
    finally:
        restore()
        after = {f: sha256(f) for f in FILES}
        bad = [f for f in FILES if before[f] != after[f]]
        if bad:
            print("!! RESTORE FAILED, TREE IS DIRTY:", bad)
            return 3
        shutil.rmtree(backup, ignore_errors=True)

    ok = 0
    for label, predicted, observed, broke, why in rows:
        if broke:
            verdict, passed = "!! BUILD BROKE — scored as NOT a catch", False
        else:
            passed = observed == predicted
            verdict = "AS PREDICTED" if passed else "!! NOT AS PREDICTED"
        ok += bool(passed)
        print(f"{verdict:<34} {label}")
        print(f"{'':34}   why: {why}")
        if not passed:
            print(f"{'':34}   predicted: {sorted(predicted)}")
            print(f"{'':34}   observed:  {sorted(observed)}")
    print(f"\n{ok}/{len(rows)} as predicted")
    return 0 if ok == len(rows) else 1


if __name__ == "__main__":
    sys.exit(main())
