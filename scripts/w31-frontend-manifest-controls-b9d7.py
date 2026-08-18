#!/usr/bin/env python3
"""w31-frontend-manifest-controls-b9d7.py — positive controls for the frontend test-loss gate.

THE GUARD PASSES ON A CLEAN TREE BY CONSTRUCTION, so every claim about what it catches is made
here, by real edits to real files, and never by reading it.

Two halves are under test and they are deliberately in different CI jobs:

  frontend/scripts/check-test-manifest.mjs   the rule       (job: frontend, via `npm test`)
  internal/testutil/frontendmanifest_test.go the wiring lock (job: test, via `go test ./...`)

Each control names its PREDICTED catcher BEFORE it runs. Every mutated file is restored from a
byte copy in a `finally` and sha256-verified. Nothing here mutates the guard's own rule file — the
two controls that DO mutate a guard input (C4 the wiring, C9 the manifest) are the blinding
controls, and that is their entire point.

  C0  inert                            -> NOT CAUGHT (the floor under every CAUGHT below)
  C1  describe.skip on the money file  -> checker NOT RUN x6; count rule SILENT (135 either way)
  C2  it.skip on one case              -> checker NOT RUN x1
  C3  it.todo on one case              -> checker NOT RUN x1, status "todo" (a DIFFERENT string)
  C4  wiring reverted + C1 on top      -> `npm test` GREEN AGAIN; caught only by the Go wiring lock
  C5  one `it` block DELETED           -> checker SHRANK (count rule), NOT RUN silent
  C6  a whole test FILE deleted        -> checker MISSING + Go disk/manifest disagreement
  C7  vitest `include` narrowed + accept -> checker GREEN; caught only by Go disk/manifest
  C8  `|| true` appended to the script -> Go wiring lock (exit code swallowed)
  C9  manifest emptied                 -> checker NEW xN + Go manifest floor
  C10 ci.yaml's `npm test` step removed-> Go CI floor
  C11 that step replaced by a COMMENT  -> Go CI floor still red (prose is not an invocation)
"""

import hashlib
import json
import re
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
FE = ROOT / "frontend"

MONEY = FE / "src/components/SearchModal.cost.test.tsx"
PKG = FE / "package.json"
MANIFEST = FE / "test-manifest.json"
VITEST_CFG = FE / "vitest.config.ts"
CI = ROOT / ".github/workflows/ci.yaml"
VICTIM_FILE = FE / "src/api/search.wire-census.test.ts"

DSN = "postgres://postgres:postgres@localhost:55499/postgres?sslmode=disable"

GO_TESTS = (
    "TestFrontendTestScriptRunsTheManifestCheck|"
    "TestCIRunsTheFrontendSuite|"
    "TestFrontendManifestCoversEveryTestFileOnDisk"
)


def sha(p: Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def run_npm_test():
    """Runs the EXACT command ci.yaml's frontend job runs. Exit read from npm's own status."""
    r = subprocess.run(
        ["npm", "test"], cwd=FE, capture_output=True, text=True, timeout=600
    )
    return r.returncode, r.stdout + r.stderr


def run_go_guards():
    import os

    env = dict(os.environ, DOCS_TEST_DATABASE_URL=DSN)
    r = subprocess.run(
        ["go", "test", "-run", GO_TESTS, "-count=1", "-v", "./internal/testutil/"],
        cwd=ROOT, capture_output=True, text=True, timeout=600, env=env,
    )
    return r.returncode, r.stdout + r.stderr


def verdict(name, predicted, mutate, restore_paths):
    """Applies `mutate`, runs both halves, restores, and prints what actually fired."""
    saves = {p: p.read_bytes() for p in restore_paths if p.exists()}
    existed = {p: p.exists() for p in restore_paths}
    before = {p: sha(p) for p in restore_paths if p.exists()}
    print(f"\n{'='*78}\n{name}\n  PREDICTED: {predicted}")
    try:
        mutate()
        fe_code, fe_out = run_npm_test()
        go_code, go_out = run_go_guards()

        fe_lines = [l.strip() for l in fe_out.splitlines()
                    if re.search(r"NOT RUN|SHRANK|GREW|MISSING|NEW |test-manifest:|Tests +\d", l)]
        go_fails = [l.strip() for l in go_out.splitlines() if l.strip().startswith("--- FAIL")]

        print(f"  npm test EXIT={fe_code}   go guards EXIT={go_code}")
        for l in fe_lines[:12]:
            print(f"    fe| {l}")
        for l in go_fails:
            print(f"    go| {l}")
        caught = []
        if fe_code != 0:
            caught.append("check-test-manifest.mjs")
        if go_code != 0:
            caught.append("frontendmanifest_test.go(" + ",".join(
                f.split()[2] for f in go_fails) + ")")
        print(f"  ACTUAL:    {'CAUGHT by ' + ' + '.join(caught) if caught else 'NOT CAUGHT'}")
        return fe_code, go_code, fe_out, go_out
    finally:
        for p in restore_paths:
            if existed.get(p):
                p.write_bytes(saves[p])
            elif p.exists():
                p.unlink()
        for p, h in before.items():
            assert sha(p) == h, f"RESTORE FAILED for {p}"
        print("  restored, sha256 verified")


def sub(path: Path, old: str, new: str, count=1):
    t = path.read_text()
    assert old in t, f"anchor not found in {path}: {old[:70]}"
    path.write_text(t.replace(old, new, count))


def main():
    for p in (MONEY, PKG, MANIFEST, VITEST_CFG, CI, VICTIM_FILE):
        assert p.exists(), f"missing {p}"

    ALL = [MONEY, PKG, MANIFEST, VITEST_CFG, CI, VICTIM_FILE]

    verdict("C0  inert (no mutation)",
            "NOT CAUGHT — both halves green; this is the floor under every CAUGHT below",
            lambda: None, ALL)

    verdict("C1  describe.skip on SearchModal.cost.test.tsx (THE DEFECT, VERBATIM)",
            "CAUGHT by check-test-manifest.mjs: 6 NOT RUN lines. The COUNT rule stays SILENT — "
            "135 tests either way — which is the measured blindness the status rule exists for",
            lambda: sub(MONEY,
                        'describe("the search list reports what a document actually cost"',
                        'describe.skip("the search list reports what a document actually cost"'),
            ALL)

    verdict("C2  it.skip on ONE case",
            "CAUGHT by check-test-manifest.mjs: exactly 1 NOT RUN line, status skipped",
            lambda: sub(MONEY, '  it("C genuinely free', '  it.skip("C genuinely free'),
            ALL)

    verdict("C3  it.todo on ONE case",
            "CAUGHT by check-test-manifest.mjs with status \"todo\" — a DIFFERENT string from "
            "\"skipped\", which is what justifies an ALLOWLIST rather than a denylist",
            lambda: sub(MONEY, '  it("C genuinely free', '  it.todo("C genuinely free'),
            ALL)

    def c4():
        sub(MONEY,
            'describe("the search list reports what a document actually cost"',
            'describe.skip("the search list reports what a document actually cost"')
        pkg = json.loads(PKG.read_text())
        pkg["scripts"]["test"] = "vitest run"
        PKG.write_text(json.dumps(pkg, indent=2) + "\n")

    verdict("C4  THE WIRING REVERTED to a bare `vitest run`, with C1's skip still in place",
            "npm test GREEN AGAIN (nothing in the frontend can see the skip once the wiring is "
            "gone — the closure is the WIRING, not the script); CAUGHT ONLY by the Go wiring lock",
            c4, ALL)

    def c5():
        t = MONEY.read_text()
        start = t.index('  it("C genuinely free')
        end = t.index('  it("E semantic-only')
        MONEY.write_text(t[:start] + t[end:])

    verdict("C5  one `it` block DELETED instead of skipped (must-stay-distinct companion)",
            "CAUGHT by the COUNT rule (SHRANK 6 -> 5) with NOT RUN SILENT — the direction the "
            "status rule cannot see, which is why both rules are present",
            c5, ALL)

    verdict("C6  a whole test FILE deleted",
            "CAUGHT twice: checker MISSING (in the manifest, absent from the run) AND the Go "
            "disk/manifest test (in the manifest, not on disk)",
            lambda: VICTIM_FILE.unlink(), ALL)

    def c7():
        # Narrow the glob so src/api/*.test.ts leaves the RUN, then ACCEPT the narrowed manifest —
        # the two the checker compares now agree about a suite that shrank.
        sub(VITEST_CFG, 'include: ["src/**/*.test.{ts,tsx}"]',
            'include: ["src/components/**/*.test.{ts,tsx}", "src/pages/**/*.test.{ts,tsx}"]')
        subprocess.run(["npx", "vitest", "run", "--reporter=json",
                        "--outputFile=.vitest-report.json"], cwd=FE,
                       capture_output=True, text=True, timeout=600)
        subprocess.run(["node", "scripts/check-test-manifest.mjs", "--update"], cwd=FE,
                       capture_output=True, text=True, timeout=300)

    verdict("C7  vitest `include` NARROWED and the manifest ACCEPTED over the narrowed run",
            "checker GREEN (manifest and report agree about a suite that shrank); CAUGHT ONLY by "
            "the Go disk/manifest test — the third source neither can be narrowed against",
            c7, ALL)

    def c8():
        pkg = json.loads(PKG.read_text())
        pkg["scripts"]["test"] = pkg["scripts"]["test"] + " || true"
        PKG.write_text(json.dumps(pkg, indent=2) + "\n")

    verdict("C8  `|| true` appended to the `test` script (invocation kept, verdict swallowed)",
            "CAUGHT by the Go wiring lock's `||` assertion — every other assertion about the "
            "script is still satisfied",
            c8, ALL)

    def c9():
        m = json.loads(MANIFEST.read_text())
        m["files"] = {}
        m["totalFiles"] = 0
        MANIFEST.write_text(json.dumps(m, indent=2) + "\n")

    verdict("C9  manifest EMPTIED",
            "CAUGHT by both: checker NEW xN, and the Go manifest FLOOR",
            c9, ALL)

    verdict("C10 ci.yaml's `cd frontend && npm test` step REMOVED",
            "CAUGHT by the Go CI floor — the wiring is locked to a command nobody runs",
            lambda: sub(CI, "      - run: cd frontend && npm test\n", ""),
            ALL)

    verdict("C11 that step replaced by a COMMENT containing the same words",
            "CAUGHT — still 0 executed invocations. Prose is not an invocation, and a `name:` "
            "value counted as one already made a guard in this package unfailable once",
            lambda: sub(CI, "      - run: cd frontend && npm test\n",
                        "      # - run: cd frontend && npm test\n"),
            ALL)

    print("\n" + "=" * 78)
    print("controls complete — read each ACTUAL against its PREDICTED above.")


if __name__ == "__main__":
    sys.exit(main())
