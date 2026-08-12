#!/usr/bin/env python3
"""Positive controls for the delete-cascade staleness guard (W3.1, tab-9c4e).

THE DEFECT. Store.Delete re-parents the deleted page's children before removing the row, and
that statement also wrote `updated_at = NOW()`. GetStalePages keys on pages.updated_at, so
deleting ONE page silently cleared the freshness clock of EVERY child of it, for a full
stale_after_days window, with nobody editing or verifying any of them.

THE GUARD. internal/page/reparent_staleness_realpg_test.go, four assertions:

  [FIXTURE]  the seed landed (the children are stale, the parent link exists)
  [REPARENT] the cascade still runs — the child lands on the GRANDPARENT
  [CLOCK]    the measurement: the child is still on the stale list afterwards
  [CONTROL]  the stale list still responds to an edit made through page.Store.Update

Same discipline as scripts/w31-viewbump-owner-controls.py: every anchor's occurrence count is
asserted BEFORE any write, all of a control's edits go in ONE write, the file is restored in a
`finally` and sha256-compared, and THE VERDICT IS THE SET OF ASSERTIONS THAT FIRED — failing
test names plus their message — not an exit code. A build error is detected explicitly, because
`go test` cannot tell a caught mutation from a file that stopped parsing.

⚠ WHY [REPARENT] EXISTS AT ALL, and R2 is the whole reason. The obvious "fix" for the clock is
to delete the UPDATE statement, which passes [CLOCK] perfectly and orphans every child of every
deleted page. Without R2 the guard would reward exactly the wrong repair.

⚠ WHY [CONTROL] EXISTS, and R5 is its sole speaker. A stale list that never cleared for anyone
would satisfy [CLOCK] for entirely the wrong reason. R5 neuters the list's updated_at predicate
so it returns everything; only [CONTROL] can see that.

Run:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-reparent-controls-9c4e.py
"""

from __future__ import annotations

import hashlib
import os
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]

PAGE = "internal/page/store.go"
GUARD = "internal/page/reparent_staleness_realpg_test.go"

ASSERTIONS = {
    "FIXTURE": "the seed never landed, so every assertion below it would be vacuous",
    "REPARENT": "the delete cascade stopped re-parenting the children",
    "CLOCK": "deleting a page dropped its child off the stale list",
    "CONTROL": "the stale list does not respond to an edit — the instrument is dead",
}

# The live re-parent statement, post-fix. Several controls key off it.
LIVE = "`UPDATE pages SET parent_id = $1 WHERE parent_id = $2`"

CONTROLS = [
    {
        "id": "R1",
        "why": ("THE DEFECT ITSELF, restored: `updated_at = NOW()` back on the re-parent. This "
                "is the mutation the whole guard exists for, and NOTHING ELSE IN THE REPOSITORY "
                "SEES IT — the two sibling staleness guards each pin a different statement "
                "(analytics.RecordView, page.PriceAISpend) and the mock test "
                "TestDelete_ReparentsChildren matches the prefix `UPDATE pages SET parent_id` "
                "either way. Sole speaker for CLOCK."),
        "file": PAGE,
        "edits": [(LIVE, "`UPDATE pages SET parent_id = $1, updated_at = NOW() WHERE parent_id = $2`")],
        "expect": {"CLOCK"},
    },
    {
        "id": "R2",
        "why": ("THE WRONG REPAIR: the re-parent statement neutered so it matches no row. Passes "
                "[CLOCK] flawlessly and orphans every child of every deleted page. This is what "
                "earns [REPARENT] — and the prediction includes the pre-existing mock test, "
                "which counts the statement's arguments and reds too."),
        "file": PAGE,
        "edits": [(LIVE, "`UPDATE pages SET parent_id = $1 WHERE parent_id = $2 AND false`")],
        "expect": {"REPARENT"},
    },
    {
        "id": "R3",
        "why": ("THE PREDICATE POINTED AT THE WRONG COLUMN (`WHERE id` rather than "
                "`WHERE parent_id`): the deleted page re-parents ITSELF and its children keep "
                "pointing at a row that is about to vanish. [REPARENT] again — stated separately "
                "from R2 because 'matched nothing' and 'matched the wrong row' are different "
                "failures and a guard that only catches one of them is half a guard."),
        "file": PAGE,
        "edits": [(LIVE, "`UPDATE pages SET parent_id = $1 WHERE id = $2`")],
        "expect": {"REPARENT"},
    },
    {
        "id": "R4",
        "why": ("THE STALE LIST STOPS LISTING ANYTHING (its stale_after_days predicate "
                "inverted). [CLOCK] is an 'is it still on the list' assertion, so an EMPTY list "
                "satisfies its negation and the test would red for a reason that has nothing to "
                "do with the cascade. [FIXTURE] is what stops that being read as a catch. "
                "⚠ PREDICTION CORRECTED RATHER THAN RETARGETED: I named one pre-existing test "
                "and there are four — every guard in this package that reads the stale list at "
                "all reds here. FIXTURE is therefore NOT earned by R4 alone; R6 is what earns "
                "it, and R4's value is showing how much of the package this predicate carries."),
        "file": PAGE,
        "edits": [("WHERE workspace_id = $1 AND stale_after_days > 0",
                   "WHERE workspace_id = $1 AND stale_after_days < 0")],
        "expect": {"FIXTURE",
                   "EXISTING:TestGetStalePages_FilterByTTL",
                   "EXISTING:TestPricedAISpend_DoesNotResetTheStalenessClock_RealPG",
                   "EXISTING:TestPageSearchAndStale_PrivateSpace_NotVisibleWithoutGrant_RealPG",
                   "EXISTING:TestPageSearchAndStale_PrivateSpace_NotVisibleWithoutGrant_RealPG/pages/stale",
                   "EXISTING:TestSEC4_WorkspaceRoutes_SearchAndStale_CrossTenant"},
    },
    {
        "id": "R5",
        "why": ("THE STALE LIST STOPS RESPONDING TO EDITS (its updated_at predicate neutered, so "
                "it returns every page with a window). Without [CONTROL] a list that never "
                "cleared for anyone would satisfy [CLOCK] trivially. ⚠ THIS CONTROL IS ALSO "
                "WHAT CAUGHT THE HARNESS ITSELF: it reported CLOCK on its first run, which is "
                "impossible — under a list that returns everything the child IS on the list and "
                "[CLOCK] cannot fail. The cause was classify() reading '[CLOCK]' out of the "
                "[CONTROL] message, which quotes it. See classify()."),
        "file": PAGE,
        "edits": [("AND updated_at < NOW() - INTERVAL '1 day' * stale_after_days",
                   "AND updated_at < NOW() + INTERVAL '3650 days'")],
        "expect": {"CONTROL",
                   "EXISTING:TestPricedAISpend_DoesNotResetTheStalenessClock_RealPG"},
    },
    {
        "id": "R6",
        "why": ("THE GUARD'S OWN ORACLE BLINDED — it mutates the TEST, not the product. [CLOCK] "
                "asserts membership of the list returned by GetStalePages; if the guard read the "
                "list of a DIFFERENT workspace it would find nothing and red on the fixture "
                "instead. Isolates [FIXTURE] from the product mutation in R4, which also reds an "
                "existing test."),
        "file": GUARD,
        "edits": [("out, err := pages.GetStalePages(ctx, ws)",
                   'out, err := pages.GetStalePages(ctx, ws+"zz")')],
        "expect": {"FIXTURE"},
    },
    {
        "id": "R7",
        "why": ("MUST-NOT-CATCH: an unrelated edit inside the same method (its error wrapper). A "
                "guard that reds on any edit to the file it watches is a guard the next merge "
                "deletes."),
        "file": PAGE,
        "edits": [('"page: reparent: %w"', '"page: delete reparent: %w"')],
        "expect": set(),
    },
    {
        "id": "R8",
        "why": ("THE OTHER WRITER OF THE CLOCK: Update's own bump made a lie — it stamps a "
                "year-old timestamp, so a genuine edit no longer clears the page. [CONTROL] is "
                "the only assertion that can see it, and it is what stops [CLOCK] from being "
                "satisfied by a stale list nobody can ever leave. ⚠ FIRST VERSION OF THIS "
                "MUTANT WAS INVALID AND THE HARNESS SAID SO RATHER THAN SCORING IT: wrapping "
                "the `set = append(...)` line in `if false` left the neighbouring `n++` and "
                "`args = append(...)` in place, so the statement bound three parameters for two "
                "placeholders and Update returned an error — 17 tests red, and my guard failed "
                "at its Update call with a message none of its four assertions describes. That "
                "is what ?UNCLASSIFIED is for. ⚠ AND THE CORRECTED MUTANT'S PREDICTION WAS ALSO "
                "INCOMPLETE: TestPricedAISpend_DoesNotResetTheStalenessClock_RealPG carries the "
                "same in-run positive control and reds too — which is exactly why the summary "
                "below reports [CONTROL] as a precondition rather than as coverage this file "
                "earned."),
        "file": PAGE,
        "edits": [("args = append(args, time.Now().UTC())",
                   "args = append(args, time.Now().UTC().Add(-365*24*time.Hour))")],
        "expect": {"CONTROL",
                   "EXISTING:TestPricedAISpend_DoesNotResetTheStalenessClock_RealPG"},
    },
]


GUARD_TEST = "TestDeletingAParent_DoesNotResetItsChildrensStalenessClock_RealPG"


def classify(name: str, msg: str) -> str:
    """Map one FAILING test to the assertion that fired.

    ⚠ THIS FUNCTION SHIPPED WRONG TWICE AND THE CONTROLS ARE WHAT CAUGHT IT — recorded here
    because a control harness that mis-attributes a catch is worth less than no harness.

      1. IT DID NOT CHECK THE TEST NAME. Any failing test in the package whose output happened
         to contain one of these substrings was credited to one of MY assertions. R4's run
         showed it: six tests failed and only five tags appeared, because a pre-existing test's
         message collapsed into FIXTURE. A control that reports "my guard caught it" when
         something else did is the exact failure this file exists to prevent.
      2. THE TAG ORDER WAS WRONG. [CONTROL]'s failure message QUOTES "[CLOCK]" — it says the
         clock assertion proves nothing without it — so a [CONTROL] failure classified as CLOCK.
         R5 is what exposed it: neutering the stale list's window predicate makes the list
         return everything, under which [CLOCK] cannot fail (the child IS on the list), and the
         harness reported CLOCK anyway. An impossible result is the only reason it was found.

    So: the name gates everything, and CONTROL is tested before CLOCK.
    """
    if name != GUARD_TEST and not name.startswith(GUARD_TEST + "/"):
        # A test that existed before this campaign. Naming these rather than lumping them under
        # "?" is the difference between "my guard caught it" and "something caught it".
        return f"EXISTING:{name}"
    if "fixture is wrong" in msg:
        return "FIXTURE"
    if "[REPARENT]" in msg:
        return "REPARENT"
    if "[CONTROL]" in msg:
        return "CONTROL"
    if "[CLOCK]" in msg:
        return "CLOCK"
    # My guard failed for a reason none of its four assertions describes — a broken fixture, a
    # store error. Named rather than folded into an assertion it did not come from.
    return f"?UNCLASSIFIED:{name}"


def run() -> tuple[set[str], list[str], str | None]:
    p = subprocess.run(
        ["go", "test", "-count=1", "-v", "./internal/page/"],
        cwd=ROOT, capture_output=True, text=True, env=dict(os.environ),
    )
    blob = p.stdout + p.stderr
    if "[build failed]" in blob or "cannot use" in p.stderr or "syntax error" in blob:
        return set(), [], "BUILD ERROR — not a catch:\n" + (p.stderr or p.stdout)[-1200:]
    tags, failed, buf, cur = set(), [], [], None
    for line in p.stdout.splitlines():
        if line.startswith("=== RUN"):
            cur, buf = line.split()[-1], []
        elif line.lstrip().startswith("--- FAIL:"):
            name = line.split()[2]
            failed.append(name)
            tags.add(classify(name, "\n".join(buf)))
            buf = []
        elif line.startswith("--- PASS") or line.startswith("=== CONT"):
            buf = []
        elif cur:
            buf.append(line)
    return tags, failed, None


def main() -> int:
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL is not set — these controls need a real Postgres.")
        return 2

    print("BASELINE (unmodified tree):")
    tags, failed, err = run()
    print(f"  failing={sorted(failed) or 'none'}{'  ERR=' + err if err else ''}")
    if failed or err:
        print("BASELINE IS NOT GREEN — stopping; no control verdict would mean anything.")
        return 1

    results, claimed = [], set()
    for c in CONTROLS:
        path = ROOT / c["file"]
        original = path.read_bytes()
        digest = hashlib.sha256(original).hexdigest()
        text = original.decode()

        bad = None
        for anchor, _ in c["edits"]:
            n = text.count(anchor)
            if n != 1:
                bad = f"ANCHOR {anchor[:60]!r}… appears {n}x in {c['file']}, expected 1x"
                break
        if bad:
            print(f"\n{c['id']}: {bad}")
            results.append((c["id"], "ANCHOR", set(), c["expect"], []))
            continue

        mutated = text
        for anchor, repl in c["edits"]:
            mutated = mutated.replace(anchor, repl, 1)
        applied = all(repl in mutated for _, repl in c["edits"] if repl)

        try:
            path.write_text(mutated)
            tags, failed, err = (set(), [], "CONTROL DID NOT FULLY APPLY") if not applied else run()
        finally:
            path.write_bytes(original)
            if hashlib.sha256(path.read_bytes()).hexdigest() != digest:
                print(f"FATAL: {c['file']} not restored")
                return 1

        ok = (tags == c["expect"]) and not err
        claimed |= tags
        results.append((c["id"], "OK" if ok else "MISMATCH", tags, c["expect"], failed))
        print(f"\n{c['id']} {'AS PREDICTED' if ok else '*** NOT AS PREDICTED ***'}")
        print(f"    why      : {c['why']}")
        print(f"    predicted: {sorted(c['expect']) or 'NOTHING (must-not-catch)'}")
        print(f"    fired    : {sorted(tags) or 'NOTHING'}")
        print(f"    tests    : {sorted(set(failed)) or 'none'}")
        if err:
            print(f"    ERROR    : {err}")

    print("\n" + "=" * 78)
    bad = [r for r in results if r[1] != "OK"]
    unclaimed = sorted(set(ASSERTIONS) - claimed)
    print(f"{len(results) - len(bad)}/{len(results)} controls as predicted")
    if unclaimed:
        print(f"*** ASSERTIONS CLAIMED BY NO CONTROL: {unclaimed}")
    for tag in sorted(claimed):
        speakers = [r[0] for r in results if tag in r[2]]
        note = "  (SOLE SPEAKER)" if len(speakers) == 1 else ""
        print(f"  {tag:<14} fired by {speakers}{note}")
    # An assertion of MINE that never fires alone is a precondition nothing here justifies, and
    # it is printed as such rather than left to look earned by the row above.
    for tag in sorted(set(ASSERTIONS) & claimed):
        alone = [r[0] for r in results if r[2] == {tag}]
        if not alone:
            print(f"  ⚠ {tag} is never the ONLY thing that fires — no control justifies it on "
                  f"its own; it is a precondition, not coverage.")
    return 1 if (bad or unclaimed) else 0


if __name__ == "__main__":
    sys.exit(main())
