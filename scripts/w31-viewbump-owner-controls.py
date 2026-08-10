#!/usr/bin/env python3
"""Positive controls for the one-owner view bump and the staleness clock (W3.1).

Same discipline as scripts/w31-inbox-space-controls.py: every anchor's occurrence count is
asserted BEFORE any write, all of a control's edits go in ONE write, the file is restored in a
`finally` and sha256-compared, and the verdict is THE SET OF ASSERTIONS THAT FIRED — failing
test names plus the first line of each message — not an exit code. A compile error is detected
explicitly, because `go test` cannot tell a caught mutation from a file that stopped parsing.

The two guards under test are deliberately blind to each other:

  · TestViewCountBump_HasExactlyOneWriter counts WRITERS of the bump. It cannot tell a correct
    copy from a divergent one.
  · TestRecordedView_DoesNotResetTheStalenessClock_RealPG reads the BEHAVIOUR of the live copy.
    It cannot see a second copy that no request reaches yet.

The defect was exactly a second copy that was BOTH divergent AND unreachable, so a control that
only one of them catches is what earns that one. S3 is the pair's whole point.

Run:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-viewbump-owner-controls.py
"""

from __future__ import annotations

import hashlib
import os
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]

ANALYTICS = "internal/analytics/store.go"
PAGE = "internal/page/store.go"
FRESHNESS = "internal/page/store.go"

ASSERTIONS = {
    "CENSUS-DUP": "a second writer of the view bump exists",
    "CENSUS-FLOOR": "the census found NO site at all (a predicate that read nothing)",
    "STALE-RESET": "recording a view dropped the page off the stale list",
    "STALE-BUMP": "the view did not land, so the staleness assertion would be vacuous",
    "STALE-CONTROL": "the stale list does not respond to an edit — the instrument is dead",
}

# The exact text of the live bump, used by several controls.
LIVE = ("`UPDATE pages SET view_count = view_count + 1, last_viewed_at = NOW() WHERE id = $1`")

CONTROLS = [
    {
        "id": "S1",
        "why": ("THE DIVERGENCE ITSELF, on the LIVE statement: the deleted copy's extra "
                "`updated_at = NOW()`. Sole speaker for the behaviour guard — the census "
                "counts one writer either way, so it is structurally blind to this."),
        "file": ANALYTICS,
        "edits": [(LIVE,
                   "`UPDATE pages SET view_count = view_count + 1, last_viewed_at = NOW(), "
                   "updated_at = NOW() WHERE id = $1`")],
        "expect": {"STALE-RESET"},
    },
    {
        "id": "S2",
        "why": ("THE BUMP STOPS BUMPING (`+ 1` -> `+ 0`). This is the mutation the mock in "
                "this repo cannot see — pgxmock never executes SQL, and the expectation "
                "matches `UPDATE pages SET view_count` either way. It reds through the "
                "PRECONDITION, not through the staleness assertion, and that is the point of "
                "asserting the view landed. ⚠ MY PREDICTION WAS INCOMPLETE AND IS CORRECTED "
                "HERE RATHER THAN RETARGETED: analytics's OWN "
                "TestRecordView_CrossTenant_GateHoldsWithoutEnforcer_RealPG reads the counter "
                "back and reds too, so this write is NOT unheld and STALE-BUMP is not earned "
                "by this control alone."),
        "file": ANALYTICS,
        "edits": [(LIVE,
                   "`UPDATE pages SET view_count = view_count + 0, last_viewed_at = NOW() "
                   "WHERE id = $1`")],
        "expect": {"STALE-BUMP",
                   "EXISTING:TestRecordView_CrossTenant_GateHoldsWithoutEnforcer_RealPG"},
    },
    {
        "id": "S3",
        "why": ("THE DEFECT AS IT ACTUALLY WAS: the second writer restored, unreachable, with "
                "the divergent statement. ONLY the census reds — the behaviour guard exercises "
                "the live path and cannot see a copy no request reaches. This is the control "
                "that earns having two guards instead of one."),
        "file": PAGE,
        "edits": [(
            "// ─── RecordView ────────────────────────────────────────────\n",
            "// ─── RecordView ────────────────────────────────────────────\n"
            "\n"
            "func (s *Store) recordViewShadow(ctx context.Context, pageID string) error {\n"
            "\t_, err := s.pool.Exec(ctx,\n"
            "\t\t`UPDATE pages SET view_count = view_count + 1, last_viewed_at = NOW(), "
            "updated_at = NOW() WHERE id = $1`, pageID)\n"
            "\treturn err\n"
            "}\n"
            "\n"
            "var _ = (*Store).recordViewShadow\n",
        )],
        "expect": {"CENSUS-DUP"},
    },
    {
        "id": "S4",
        "why": ("THE LIVE BUMP DELETED OUTRIGHT. The census's FLOOR is the only thing that can "
                "speak: a 'no duplicates' check over an empty set compares equal and reports "
                "clean — the strongest possible pass from an instrument that read nothing. "
                "⚠ CORRECTED PREDICTION: two pre-existing analytics tests red as well, so the "
                "FLOOR is what this control uniquely earns and nothing else here is."),
        "file": ANALYTICS,
        "edits": [(LIVE, "`SELECT 1 WHERE $1 = $1`")],
        "expect": {"CENSUS-FLOOR", "STALE-BUMP",
                   "EXISTING:TestRecordView_CrossTenant_GateHoldsWithoutEnforcer_RealPG",
                   "EXISTING:TestRecordView_InsertsRowAndIncrementsPageCounter"},
    },
    {
        "id": "S5",
        "why": ("THE STALE LIST STOPS RESPONDING TO EDITS (its updated_at predicate neutered). "
                "Sole speaker for the behaviour guard's own positive control — without it, a "
                "stale list that never cleared for anyone would satisfy 'a view did not clear "
                "it'."),
        "file": FRESHNESS,
        "edits": [("AND updated_at < NOW() - INTERVAL '1 day' * stale_after_days",
                   "AND updated_at < NOW() + INTERVAL '3650 days'")],
        "expect": {"STALE-CONTROL"},
    },
    {
        "id": "S7",
        "why": ("THE CENSUS PREDICATE BLINDED — it mutates the GUARD, not the product, which is "
                "the only way to isolate a floor from the thing it sits behind. Without it "
                "CENSUS-FLOOR is earned by nothing: S4 also fires STALE-BUMP, so deleting the "
                "floor outright would still leave S4 red."),
        "file": "internal/page/viewbump_one_owner_test.go",
        "edits": [('strings.Contains(string(b), "UPDATE pages SET view_count")',
                   'strings.Contains(string(b), "UPDATE pages SET view_countZZZ")')],
        "expect": {"CENSUS-FLOOR"},
    },
    {
        "id": "S6",
        "why": ("MUST-NOT-CATCH: an unrelated edit in the same live method (its error wrapper). "
                "A guard that reds on any edit to the file it watches is a guard someone "
                "deletes at the next merge."),
        "file": ANALYTICS,
        "edits": [('"analytics: bump counter: %w"', '"analytics: view counter bump: %w"')],
        "expect": set(),
    },
]


def classify(name: str, msg: str) -> str:
    if "census found NO" in msg:
        return "CENSUS-FLOOR"
    if "must have exactly ONE writer" in msg:
        return "CENSUS-DUP"
    if "dropped it off the stale list" in msg:
        return "STALE-RESET"
    if "the bump did not happen" in msg:
        return "STALE-BUMP"
    if "STILL on the stale list" in msg:
        return "STALE-CONTROL"
    if "fixture is wrong" in msg:
        return "?FIXTURE"
    # A test that already existed before this campaign. Naming these rather than lumping them
    # under "?" is the difference between "my guard caught it" and "something caught it".
    return f"EXISTING:{name}"


def run() -> tuple[set[str], list[str], str | None]:
    p = subprocess.run(
        ["go", "test", "-count=1", "-v", "./internal/page/", "./internal/analytics/"],
        cwd=ROOT, capture_output=True, text=True, env=dict(os.environ),
    )
    if "[build failed]" in p.stdout + p.stderr or "cannot use" in p.stderr:
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
