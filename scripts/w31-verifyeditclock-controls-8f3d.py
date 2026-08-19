#!/usr/bin/env python3
"""Positive controls for the Verify edit-clock merge (W3.1, tab-8f3d).

WHY EVERY ONE OF THESE EXISTS. The four new assertions in this merge fall into two files that
CANNOT both be trusted from one run:

  internal/page/verify_editclock_realpg_test.go   real Postgres, the shipped store chain
  internal/freshness/verifywins_test.go           hand-built model.Page, no DB

The freshness file PASSED BEFORE THE PRODUCT CHANGE — it constructs a page state the product
could not produce, which is precisely how the inert branch survived. So "it is green" says
nothing there; only a mutation that makes it RED says the assertion is load-bearing. C3 and C4
are that proof, and C1 is the one that shows the real-PG file is the half which sees the defect.

PROTOCOL, the same one this repo's other harnesses use:
  * every mutation names its PREDICTED catcher BEFORE the run, and a wrong prediction is
    recorded rather than quietly corrected;
  * every mutation is restored in a `finally` and the tree is sha256-verified afterwards;
  * every mutation runs over the FULL Go suite on real Postgres, so "only the new guard reds"
    is MEASURED rather than assumed;
  * a mutation that fails to COMPILE scores VOID, not CAUGHT — a build error is not a caught
    defect.

Usage:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-verifyeditclock-controls-8f3d.py
"""
from __future__ import annotations

import hashlib
import os
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
STORE = ROOT / "internal/page/store.go"
ENGINE = ROOT / "internal/freshness/engine.go"

NEW_TESTS = {
    "page/forge": "TestVerify_DoesNotForgeAnEdit_RealPG",
    "page/version": "TestVerify_WritesNoPageVersion_RealPG",
    "fresh/wins": "TestVerifyWinsOverEditClock_IsReachable",
    "fresh/older": "TestVerifyDoesNotWinWhenOlderThanTheEdit",
}

VERIFY_FIXED = """	_, err := s.pool.Exec(ctx,
		`UPDATE pages SET last_verified_at = NOW(), verified_by = $1
        WHERE id = $2`, verifierID, pageID,
	)"""

EFFECTIVE_FIXED = """	effective := p.UpdatedAt
	if p.LastVerifiedAt != nil && p.LastVerifiedAt.After(effective) {
		effective = *p.LastVerifiedAt
	}"""


def sha(p: Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def run_suite() -> tuple[bool, set[str], str]:
    """Full Go suite on real Postgres. Returns (compiled, failing test names, raw tail)."""
    proc = subprocess.run(
        ["go", "test", "-count=1", "./..."],
        cwd=ROOT, capture_output=True, text=True,
    )
    out = proc.stdout + proc.stderr
    # A build failure is VOID, never CAUGHT. Distinguish it from a test failure explicitly.
    compiled = "build failed" not in out and "[build failed]" not in out and "cannot use" not in out
    failing = set(re.findall(r"^--- FAIL: (\w+)", out, re.M))
    return compiled, failing, out[-1500:]


CONTROLS: list[tuple[str, str, Path, str, str, set[str], set[str]]] = [
    # (id, what it does, file, find, replace, predicted CAUGHT-by, predicted STAYS-GREEN)
    (
        "C1",
        "THE DEFECT RESTORED — Verify sets updated_at = NOW() again",
        STORE,
        VERIFY_FIXED,
        """	_, err := s.pool.Exec(ctx,
		`UPDATE pages SET last_verified_at = NOW(), verified_by = $1,
            updated_at = NOW()
        WHERE id = $2`, verifierID, pageID,
	)""",
        {"TestVerify_DoesNotForgeAnEdit_RealPG"},
        # The freshness unit tests build the page by hand, so they cannot see a product that
        # never produces that state. This is the measured blindness their own header claims.
        {"TestVerifyWinsOverEditClock_IsReachable", "TestVerifyDoesNotWinWhenOlderThanTheEdit"},
    ),
    (
        "C2",
        "THE ATTESTATION DROPPED — Verify stops stamping last_verified_at",
        STORE,
        VERIFY_FIXED,
        """	_, err := s.pool.Exec(ctx,
		`UPDATE pages SET verified_by = $1
        WHERE id = $2`, verifierID, pageID,
	)""",
        # The must-stay-green half of the real-PG test, made to speak: this is the "fix" that
        # would satisfy the edit-clock assertion by destroying the feature.
        {"TestVerify_DoesNotForgeAnEdit_RealPG"},
        {"TestVerifyWinsOverEditClock_IsReachable", "TestVerifyDoesNotWinWhenOlderThanTheEdit"},
    ),
    (
        "C3",
        "THE FRESHNESS RULE BLINDED — buildReport ignores last_verified_at",
        ENGINE,
        EFFECTIVE_FIXED,
        "\teffective := p.UpdatedAt",
        # Only the freshness file can see this. The real-PG test proves the STATE is producible,
        # never that the engine reads it — that gap is why both files are in this merge.
        {"TestVerifyWinsOverEditClock_IsReachable"},
        {"TestVerify_DoesNotForgeAnEdit_RealPG", "TestVerify_WritesNoPageVersion_RealPG"},
    ),
    (
        "C4",
        "THE MAX MADE UNCONDITIONAL — a stale verification also wins",
        ENGINE,
        EFFECTIVE_FIXED,
        """	effective := p.UpdatedAt
	if p.LastVerifiedAt != nil {
		effective = *p.LastVerifiedAt
	}""",
        # This is the mutation that justifies the second freshness case existing at all: without
        # it, "a verification always wins" would leave the first case green.
        {"TestVerifyDoesNotWinWhenOlderThanTheEdit"},
        {"TestVerifyWinsOverEditClock_IsReachable", "TestVerify_DoesNotForgeAnEdit_RealPG"},
    ),
    (
        "C5",
        "VERIFY MADE TO SNAPSHOT — a page_versions row per verification",
        STORE,
        VERIFY_FIXED,
        """	if _, err := s.pool.Exec(ctx,
		`INSERT INTO page_versions (page_id, workspace_id, version, title, content, created_by)
         SELECT id, workspace_id, 9999, title, content, $1 FROM pages WHERE id = $2`,
		verifierID, pageID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE pages SET last_verified_at = NOW(), verified_by = $1
        WHERE id = $2`, verifierID, pageID,
	)""",
        {"TestVerify_WritesNoPageVersion_RealPG"},
        {"TestVerifyWinsOverEditClock_IsReachable"},
    ),
    (
        "C6",
        "MUST-STAY-GREEN COMPANION — warningRatio 0.5 -> 0.9, unrelated to this merge",
        ENGINE,
        "const warningRatio = 0.5",
        "const warningRatio = 0.9",
        # Predicted caught by the PRE-EXISTING warning-threshold cases and by NONE of the four
        # new ones. Without this, "CAUGHT" could just mean "the suite is fragile".
        set(),
        set(NEW_TESTS.values()),
    ),
]


def main() -> int:
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("error: DOCS_TEST_DATABASE_URL must be set — these controls need real Postgres")
        return 2

    before = {p: sha(p) for p in (STORE, ENGINE)}

    print("=== C0 — baseline, no mutation ===")
    compiled, failing, tail = run_suite()
    if not compiled or failing:
        print(f"  BASELINE NOT GREEN (compiled={compiled}, failing={sorted(failing)})")
        print(tail)
        return 1
    print("  green\n")

    results: list[tuple[str, str, bool]] = []
    for cid, what, path, find, repl, want_caught, want_green in CONTROLS:
        print(f"=== {cid} — {what} ===")
        print(f"  PREDICT CAUGHT BY : {sorted(want_caught) or '(none — must stay green)'}")
        print(f"  PREDICT STILL GREEN: {sorted(want_green)}")
        original = path.read_text()
        if find not in original:
            print(f"  VOID — anchor not found in {path.name}; the harness is stale, not the product")
            results.append((cid, "VOID(anchor)", False))
            continue
        try:
            path.write_text(original.replace(find, repl, 1))
            compiled, failing, tail = run_suite()
            if not compiled:
                print("  VOID — mutation did not compile; a build error is not a caught defect")
                print(tail)
                results.append((cid, "VOID(build)", False))
                continue
            caught = failing & set(NEW_TESTS.values())
            green = set(NEW_TESTS.values()) - failing
            ok = caught == want_caught and want_green.issubset(green)
            print(f"  ACTUAL  CAUGHT BY : {sorted(caught) or '(none)'}")
            print(f"  ACTUAL other fails: {sorted(failing - set(NEW_TESTS.values())) or '(none)'}")
            print(f"  -> {'AS PREDICTED' if ok else '*** PREDICTION WRONG ***'}\n")
            results.append((cid, f"caught={sorted(caught)}", ok))
        finally:
            path.write_text(original)

    after = {p: sha(p) for p in (STORE, ENGINE)}
    restored = before == after
    print("=== restore check ===")
    for p in (STORE, ENGINE):
        print(f"  {p.relative_to(ROOT)}: {'RESTORED' if before[p] == after[p] else 'MODIFIED'} ({after[p][:16]})")

    print("\n=== summary ===")
    for cid, detail, ok in results:
        print(f"  {cid}: {'OK ' if ok else 'XX '} {detail}")
    allok = restored and all(ok for _, _, ok in results)
    print(f"\n{'ALL CONTROLS AS PREDICTED' if allok else 'SOMETHING DID NOT MATCH — read it, do not re-run until you have'}")
    return 0 if allok else 1


if __name__ == "__main__":
    sys.exit(main())
