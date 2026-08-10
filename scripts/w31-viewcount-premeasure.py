#!/usr/bin/env python3
"""PRE-MEASUREMENT (throwaway): does anything in this repo notice the share-link view_count
bump misbehaving?

This is NOT the shipped control harness — it is the measurement that decides whether there is
a finding at all. It mutates internal/sharing/store.go's `UPDATE share_links SET view_count`
and runs `go test ./...` REPO-WIDE, because "nothing anywhere notices" is a claim about the
repo and not about one package.

Run: DOCS_TEST_DATABASE_URL=... python3 scripts/w31-viewcount-premeasure.py
"""
import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = os.path.join(REPO, "internal/sharing/store.go")

# The anchor is the WHOLE statement. Censused before this file was written: it appears exactly
# once in the repo's .go files. store_test.go carries a SHORTER prefix (`UPDATE share_links SET
# view_count`) as its pgxmock regex — that is the mock's matcher, not a second product site, and
# every mutation below deliberately leaves that prefix intact so the mock keeps matching.
BUMP = "`UPDATE share_links SET view_count = view_count + 1 WHERE id = $1`"

# The whole discarded-error Exec, for the must-be-caught control.
EXEC_BLOCK = """	if _, err := s.pool.Exec(ctx,
		`UPDATE share_links SET view_count = view_count + 1 WHERE id = $1`,
		link.ID,
	); err != nil {
		// View-count bump failure shouldn't fail the read — log and
		// continue once a logger is wired. Phase 8 keeps it simple.
		_ = err
	}
"""


def read():
    with open(STORE, "r") as f:
        return f.read()


def write(text):
    with open(STORE, "w") as f:
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


def drop_exec():
    def apply():
        text = read()
        assert_anchor(text, EXEC_BLOCK, 1)
        write(text.replace(EXEC_BLOCK, ""))
    return apply


def run_repo():
    """Run the WHOLE repo. Returns (state, detail).

    `go test` prints no PASS line without -v and a panic kills a package binary, so absence
    from a failure list is not green: the exit code plus an explicit build-failure check is
    what decides here.
    """
    p = subprocess.run(
        ["go", "test", "-count=1", "./..."],
        cwd=REPO, env=dict(os.environ), capture_output=True, text=True, timeout=1800,
    )
    out = p.stdout + p.stderr
    if "[build failed]" in out or re.search(r"^# ", out, re.M):
        first = next((ln for ln in out.split("\n") if ln.startswith("#") or ".go:" in ln), "")
        return "BUILD_FAILED", first.strip()[:200]
    if p.returncode == 0:
        return "GREEN", ""
    fails = [ln for ln in out.split("\n") if ln.startswith("FAIL") or ln.startswith("--- FAIL")]
    return "RED", " | ".join(f.strip() for f in fails[:6])[:400]


CONTROLS = [
    ("S1", "the bump stops bumping: `+ 1` -> `+ 0`. Statement runs, row unchanged. The mock's "
           "regex and its one bound arg are untouched, so pgxmock is blind BY CONSTRUCTION.",
     sub(BUMP, "`UPDATE share_links SET view_count = view_count + 0 WHERE id = $1`")),

    ("S2", "` AND FALSE`: the statement runs and matches NO row. Unlike changelog.DeleteEntry "
           "there is no RowsAffected branch here — the error is discarded on purpose — so there "
           "is nothing to red through except a row read.",
     sub(BUMP, "`UPDATE share_links SET view_count = view_count + 1 WHERE id = $1 AND FALSE`")),

    ("S3", "BLAST RADIUS, the inverse of S2: `OR TRUE` bumps EVERY share link in the table, "
           "across pages and across workspaces. One public view inflates every counter.",
     sub(BUMP, "`UPDATE share_links SET view_count = view_count + 1 WHERE id = $1 OR TRUE`")),

    ("S4", "over-count: `+ 1` -> `+ 2`. A guard asserting `> 0` is blind to this; only an exact "
           "value can see it.",
     sub(BUMP, "`UPDATE share_links SET view_count = view_count + 2 WHERE id = $1`")),

    ("S5", "MUST BE CAUGHT ALREADY (#65's work): the whole Exec deleted. This is the control "
           "that proves this harness reaches the file and that the mock still speaks.",
     drop_exec()),
]


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        sys.exit("FATAL: DOCS_TEST_DATABASE_URL unset")

    pristine = open(STORE, "rb").read()
    pristine_sha = hashlib.sha256(pristine).hexdigest()
    print(f"pristine store.go sha256 = {pristine_sha}\n")

    state, detail = run_repo()
    print(f"BASELINE (whole repo): {state} {detail}\n")
    if state != "GREEN":
        sys.exit("FATAL: baseline not green — every verdict below would be meaningless")

    results = []
    for cid, desc, apply in CONTROLS:
        # try/finally: an exception raised between the mutation and the restore leaves the
        # mutated SQL on disk, and the closing sha256 check cannot run to notice. The sibling
        # harness (w31-sharelink-viewcount-controls.py) hit exactly that on its first run.
        try:
            apply()
            if open(STORE, "rb").read() == pristine:
                sys.exit(f"FATAL {cid}: mutation changed NO BYTES — an anchor that matched is "
                         f"not a mutation that meant anything")
            state, detail = run_repo()
        finally:
            open(STORE, "wb").write(pristine)
        results.append((cid, state, detail))
        print(f"{cid}  {desc}")
        print(f"    WHOLE REPO: {state}   {detail}\n")

    final = hashlib.sha256(open(STORE, "rb").read()).hexdigest()
    print(f"restored store.go sha256 = {final}")
    if final != pristine_sha:
        sys.exit("FATAL: tree NOT restored to pristine bytes")

    print("\nSUMMARY (whole repo, real Postgres)")
    for cid, state, detail in results:
        print(f"  {cid}: {state}  {detail}")


if __name__ == "__main__":
    main()
