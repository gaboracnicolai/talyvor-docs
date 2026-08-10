#!/usr/bin/env python3
"""Positive controls for frontend/src/api/page.cached-required-keys.test.ts.

THAT TEST HAS NO RED-FIRST MOMENT — it pins a status quo — so this file is its entire
justification. It also passed on its first run, which is the standing reason to distrust it.

THE VERDICT IS THE SET OF ASSERTIONS THAT FIRED, NOT PASS/FAIL. A per-test verdict cannot tell
you that one assertion inside a multi-assertion test is justified by nothing: the sibling
harness w31-sharelink-viewcount-controls.py scored 7/7 that way while two of its four
assertions were earned by no mutation at all. Each assertion below is matched on a fragment of
the message it prints, and the harness FAILS if any declared assertion is claimed by no
control.

THE PAIR THAT CARRIES THE WHOLE GUARD IS T1/T4. They add the SAME key name, differing by one
character — `archived_at: string` vs `archived_at?: string`. T1 must red and T4 must stay
green. Either alone is ambiguous: a guard that reds on every new field would pass T1 and fail
T4, and a guard that reds on nothing would pass T4 and fail T1. Together they show the
predicate actually reads the `?`, which is the only thing that makes this tripwire survivable
rather than noise somebody deletes.

Run: python3 scripts/w31-page-required-keys-controls.py
"""
import hashlib
import os
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TYPES = os.path.join(REPO, "frontend/src/api/types.ts")
TEST = "src/api/page.cached-required-keys.test.ts"

# Assertion tag -> a fragment of the message that assertion prints. If a message is reworded the
# fragment stops matching and every control naming it FAILS LOUD, rather than quietly passing
# on a tag nothing can set.
ASSERTIONS = [
    ("ADDED", "required key(s) added to Page"),
    ("REMOVED", "required key(s) missing from Page"),
    ("PARSE", "the parser found NOTHING"),
    ("OPTEMPTY", "no optional keys parsed"),
    ("BOTH", "counted as BOTH required and optional"),
    ("OPTREQUIRED", "must stay OPTIONAL"),
]

ANCHOR_COST = "  ai_cost_usd: number;\n"
# NOT `  title: string;\n` — THE ANCHOR ASSERTION CAUGHT THAT ON THE FIRST RUN. It appears
# TWICE in types.ts (Page and PageVersion both declare a title), so T3 would have deleted the
# wrong one half the time and, worse, would have been mutating a type this guard says nothing
# about while reporting a verdict about Page. `stale_after_days` is Page's alone — verified by
# count, not by looking.
ANCHOR_STALE = "  stale_after_days: number;\n"
ANCHOR_PAGETYPE = '  page_type?: "document" | "changelog";\n'
ANCHOR_IFACE = "export interface Page {"


def read():
    with open(TYPES, "r") as f:
        return f.read()


def write(text):
    with open(TYPES, "w") as f:
        f.write(text)


def assert_anchor(text, needle, want):
    got = text.count(needle)
    if got != want:
        sys.exit(f"FATAL ANCHOR: {needle!r} appears {got}x, expected {want}")


def sub(old, new, expect=1):
    def apply():
        text = read()
        assert_anchor(text, old, expect)
        write(text.replace(old, new))
    return apply


def run_test():
    """Run the guard file once and return (exit_ok, set_of_assertion_tags_that_fired).

    Reads the tags out of the printed output rather than off a pass/fail, because "the test
    reddened" and "the assertion I claim exists reddened" are different claims.
    """
    p = subprocess.run(
        ["npx", "vitest", "run", TEST],
        cwd=os.path.join(REPO, "frontend"), capture_output=True, text=True, timeout=900,
    )
    out = p.stdout + p.stderr
    # A run that never executed the file is not a run that passed.
    if "Test Files" not in out:
        return "NO_RUN", set(), out[-400:]
    fired = {tag for tag, frag in ASSERTIONS if frag in out}
    return ("PASS" if p.returncode == 0 else "FAIL"), fired, ""


CONTROLS = [
    ("T1", "THE CLASS GOING LIVE: a new REQUIRED field. A record cached before it shipped will "
           "not carry it, and the offline path will serve that record.",
     sub(ANCHOR_COST, ANCHOR_COST + "  archived_at: string;\n"),
     {"ADDED"}),

    ("T4", "MUST-NOT-CATCH, and T1's pair: the SAME key added as OPTIONAL — one character "
           "different. Adding an optional field is the safe change and must stay GREEN, or this "
           "tripwire is noise that reds on every type edit and gets deleted.",
     sub(ANCHOR_COST, ANCHOR_COST + "  archived_at?: string;\n"),
     set()),

    ("T2", "a required field DEMOTED to optional. Deliberately fine, accidentally not — and a "
           "parse-the-source guard cannot see its source shrink unless the list is pinned.",
     sub(ANCHOR_COST, "  ai_cost_usd?: number;\n"),
     {"REMOVED"}),

    ("T3", "a required field DELETED outright. Same catcher as T2 and recorded as such: it is "
           "here because 'made optional' and 'gone' are the two ways the set can shrink and "
           "only one of them is visible in a diff as a type change.",
     sub(ANCHOR_STALE, ""),
     {"REMOVED"}),

    ("T6", "AN EXISTING OPTIONAL FIELD PROMOTED TO REQUIRED — `page_type`. This is the real "
           "shape of the event: not a brand-new column but a field the API has been sending, "
           "which somebody decides is always present. TWO assertions must fire, and the second "
           "is the only control that earns the `must stay OPTIONAL` case.",
     sub(ANCHOR_PAGETYPE, '  page_type: "document" | "changelog";\n'),
     {"ADDED", "OPTREQUIRED"}),

    ("T5", "THE INSTRUMENT READING NOTHING: the interface renamed so the regex matches no "
           "block. A source-parsing guard's silent failure is two empty sets comparing equal; "
           "this must RED, not report agreement.",
     sub(ANCHOR_IFACE, "export interface PageRenamedByControl {"),
     {"PARSE"}),
]


def main():
    pristine = open(TYPES, "rb").read()
    pristine_sha = hashlib.sha256(pristine).hexdigest()
    print(f"pristine types.ts sha256 = {pristine_sha}\n")

    state, fired, why = run_test()
    print(f"BASELINE  {state}  fired={sorted(fired)} {why}")
    if state != "PASS" or fired:
        sys.exit("FATAL: baseline is not green — every verdict below would be meaningless")

    claimed = {t for _, _, _, want in CONTROLS for t in want}
    declared = {t for t, _ in ASSERTIONS}
    unclaimed = declared - claimed
    if unclaimed:
        # OPTEMPTY and BOTH are structural self-checks on the parser rather than product
        # assertions; they are listed here so that a future edit which makes one of them the
        # ONLY thing standing between a broken parser and a green run cannot go unnoticed.
        print(f"⚠ assertion(s) justified by no control: {sorted(unclaimed)} — "
              f"these are parser self-checks, recorded as unearned rather than claimed.\n")

    results = []
    for cid, desc, apply, want in CONTROLS:
        # try/finally: an exception between the mutation and the restore leaves the mutated
        # type on disk, where the closing sha256 check cannot run to notice it.
        try:
            apply()
            if open(TYPES, "rb").read() == pristine:
                sys.exit(f"FATAL {cid}: the mutation changed NO BYTES ON DISK")
            state, fired, why = run_test()
        finally:
            open(TYPES, "wb").write(pristine)

        if state == "NO_RUN":
            verdict = f"DID NOT RUN — NOT A CATCH ({why})"
        elif fired != want:
            verdict = (f"PREDICTION WRONG: fired {sorted(fired) or 'nothing'} "
                       f"(predicted {sorted(want) or 'nothing'})")
        else:
            verdict = "AS PREDICTED"

        results.append((cid, verdict, sorted(fired)))
        print(f"{cid}  {desc}")
        print(f"    {state:6s} fired={sorted(fired)}")
        print(f"    VERDICT: {verdict}\n")

    final = hashlib.sha256(open(TYPES, "rb").read()).hexdigest()
    print(f"restored types.ts sha256 = {final}")
    if final != pristine_sha:
        sys.exit("FATAL: tree NOT restored to pristine bytes")

    print("\nSUMMARY (the set of ASSERTIONS each mutation made fire)")
    for cid, verdict, fired in results:
        print(f"  {cid}: fired={fired or '-'}  {verdict}")
    wrong = [r for r in results if not r[1].startswith("AS PREDICTED")]
    if wrong:
        print(f"\n{len(wrong)} PREDICTION(S) WRONG — keep them wrong in the record.")
        sys.exit(1)
    print("\nALL PREDICTIONS HELD.")


if __name__ == "__main__":
    main()
