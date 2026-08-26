#!/usr/bin/env python3
"""Positive controls for W3.1's cost-per-revision guards (tab-9m4x).

Each case mutates an IMPLEMENTATION file, runs the named guard, and requires the
result predicted.  The last case is a NEGATIVE control.

⚠ THE OFF-BY-ONE CASE (BIND-MAX) IS THE ONE THAT MATTERS.  page_versions row N is
the state AFTER save N -- measured on real Postgres, not read -- so the spend that
PRODUCED revision N is bound while MAX(version) is N-1, and the bind records
MAX+1.  Recording MAX instead puts every completion on the revision it came after.
Both readings print a plausible non-zero number on every row; only a fixture whose
spends straddle two saves separates them, which is why the test binds before save
1, twice between save 1 and save 2, and once after save 2.

⚠ VACUITY: C-FLOOR restores a defect with the guard file DELETED and requires the
rest of the suite to stay GREEN.  A guard whose absence nothing notices is the
state this repo was in, and the floor is what proves the new file is load-bearing.

Usage: DOCS_TEST_DATABASE_URL=... python3 scripts/w31-versioncost-controls-9m4x.py
"""
import hashlib
import os
import pathlib
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
STORE = REPO / "internal/page/store.go"
SPEND = REPO / "internal/page/ai_spend.go"
VH = REPO / "frontend/src/components/VersionHistory.tsx"
GUARD_GO = REPO / "internal/page/versioncost_realpg_test.go"

GO_RUN = "TestVersionHistory|TestVersionCostSplit"

if not os.environ.get("DOCS_TEST_DATABASE_URL"):
    print("DOCS_TEST_DATABASE_URL unset — the real-PG guards cannot run; refusing to score")
    sys.exit(2)


def go(pattern=GO_RUN):
    p = subprocess.run(["go", "test", "-count=1", "-run", pattern, "./internal/page/"],
                       cwd=REPO, capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def fe():
    p = subprocess.run(["npm", "test", "--silent"], cwd=REPO / "frontend",
                       capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


CASES = [
    ("BIND-MAX: the bind records MAX(version), not MAX+1 (the off-by-one)", SPEND, go,
     "COALESCE((SELECT MAX(version) FROM page_versions WHERE page_id = $2), 0) + 1",
     "COALESCE((SELECT MAX(version) FROM page_versions WHERE page_id = $2), 0)", True),
    ("BIND-NULL: the bind stops recording a revision at all", SPEND, go,
     "INSERT INTO page_ai_spend_events (request_id, page_id, workspace_id, operation, page_version)",
     "INSERT INTO page_ai_spend_events (request_id, page_id, workspace_id, operation)", True),
    # ⚠ THESE TWO REPLACED THE OBVIOUS PAIR, AND THE REPLACEMENT IS THE FINDING.
    # The first run of this harness mutated the subquery's two conjuncts --
    # `page_version IS NOT NULL` and `cost_usd IS NOT NULL` -- and BOTH stayed GREEN.
    # Neither can change an answer: a NULL page_version can never satisfy the LEFT
    # JOIN's equality, and SUM() already skips NULL costs, so a group of only-unpriced
    # rows sums to NULL and COALESCEs to the same 0 as no group. They are kept for
    # index selectivity and intent (see store.go), not for correctness. What DOES hold
    # this query up is the join key and the aggregate, so those are what get mutated.
    ("READ-JOIN: the rollup lands each revision's cost on the wrong revision", STORE, go,
     "        ) c ON c.page_version = v.version",
     "        ) c ON c.page_version = v.version - 1", True),
    ("READ-AGG: the rollup counts events instead of summing dollars", STORE, go,
     "SELECT page_version, SUM(cost_usd) AS cost",
     "SELECT page_version, COUNT(cost_usd) AS cost", True),
    ("SPLIT-DROP: pending spend silently dropped from the split", SPEND, go,
     "WHERE page_version IS NOT NULL AND page_version > maxv.v), 0),",
     "WHERE false), 0),", True),
    ("SPLIT-FOLD: unattributable folded into attributed", SPEND, go,
     "WHERE page_version IS NOT NULL AND page_version BETWEEN 1 AND maxv.v), 0),",
     "WHERE page_version IS NULL OR page_version BETWEEN 1 AND maxv.v), 0),", True),
    ("SCOPE: VersionCostSplit drops its workspace check", SPEND, go,
     "\tif err := s.assertInWorkspaces(ctx, pageID, wsIDs); err != nil {\n\t\treturn VersionCostSplit{}, err\n\t}",
     "", True),
    ("FE-ROUND: the SPA formats a revision with the two-decimal money format", VH, fe,
     '  if (usd < 0.0001) return "<$0.0001";\n  if (usd < 0.01) return `$${usd.toFixed(4)}`;\n  return `$${usd.toFixed(2)}`;',
     "  return `$${usd.toFixed(2)}`;", True),
    ("FE-ZERO: zero rendered as a price instead of an em dash", VH, fe,
     '  if (!(usd > 0)) return "—";', '  if (usd < 0) return "—";', True),
    ("NEGATIVE: a no-op comment edit", STORE, go,
     "func (s *Store) GetVersions(", "// control: no behavioural change\nfunc (s *Store) GetVersions(", False),
]


def sha(p):
    return hashlib.sha256(p.read_bytes()).hexdigest()


def main():
    files = {c[1] for c in CASES} | {GUARD_GO}
    pristine = {f: f.read_text() for f in files}
    shas = {f: sha(f) for f in files}

    rc, out = go()
    if rc != 0:
        print("BASELINE go guards NOT GREEN:\n" + out[-2000:])
        return 2
    rc, out = fe()
    if rc != 0:
        print("BASELINE frontend NOT GREEN:\n" + out[-2000:])
        return 2

    bad = 0
    for name, f, runner, old, new, want_red in CASES:
        src = pristine[f]
        if src.count(old) != 1:
            print(f"SKIP-BROKEN  {name}: anchor appears {src.count(old)}x")
            bad += 1
            continue
        f.write_text(src.replace(old, new))
        try:
            rc, out = runner()
        finally:
            f.write_text(src)
            assert sha(f) == shas[f], f"restore of {f} did not match its original sha256"
        got_red = rc != 0
        ok = got_red == want_red
        bad += 0 if ok else 1
        why = ""
        if got_red:
            ls = [l.strip() for l in out.splitlines()
                  if "_test.go:" in l or "AssertionError" in l or "FAIL" in l or "✓" == l[:1] or "×" in l[:2]]
            why = "  | " + (ls[0] if ls else "")[:100]
        print(f"{'PASS' if ok else '**FAIL**'}  want={'RED' if want_red else 'GREEN'} "
              f"got={'RED' if got_red else 'GREEN'}  {name}{why}")

    # ⚠ THE VACUITY FLOOR. Restore the off-by-one with the guard file DELETED and
    # require the rest of the Go suite to stay GREEN: that is the state this repo
    # was in, and it is what makes the new file load-bearing rather than decorative.
    print()
    saved = GUARD_GO.read_text()
    GUARD_GO.unlink()
    SPEND.write_text(pristine[SPEND].replace(
        "COALESCE((SELECT MAX(version) FROM page_versions WHERE page_id = $2), 0) + 1",
        "COALESCE((SELECT MAX(version) FROM page_versions WHERE page_id = $2), 0)"))
    try:
        rc, _ = go(".")
    finally:
        SPEND.write_text(pristine[SPEND])
        GUARD_GO.write_text(saved)
    floor_ok = rc == 0
    bad += 0 if floor_ok else 1
    print(f"{'PASS' if floor_ok else '**FAIL**'}  C-FLOOR: with this guard DELETED the off-by-one "
          f"leaves internal/page {'GREEN — the guard is the only thing that sees it' if floor_ok else 'RED (something else already covered it)'}")

    print()
    rc, _ = go()
    rc2, _ = fe()
    print(f"restored: go={'GREEN' if rc == 0 else 'RED'}  frontend={'GREEN' if rc2 == 0 else 'RED'}")
    for f in files:
        assert sha(f) == shas[f], f"{f} not restored"
    print("every touched file restored to its original sha256")
    print(f"{len(CASES) + 1 - bad}/{len(CASES) + 1} controls behaved as predicted")
    return 0 if bad == 0 and rc == 0 and rc2 == 0 else 1


sys.exit(main())
