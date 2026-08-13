#!/usr/bin/env python3
"""Positive controls for TestCINeverRunsTheSuiteInShortMode (W3.1, tab-6b4d).

⚠ THIS GUARD PASSES ON ITS FIRST RUN AND CANNOT DO OTHERWISE — it locks a property that holds
today. Nothing in the current tree can make it red, so the test alone is not evidence that it
works; three sessions in this fleet have shipped guards that could not fail, each caught only by a
control. These are the controls: real edits to real files, each run through the real test.

Two directions, because a guard can be wrong in two ways:

  · TOO BLIND — the mutation it exists to catch leaves it green (C1, C3, C5, C6).
  · TOO EAGER — something harmless reds it, which is worse than useless because the cheapest way
    to quiet it is to edit the harmless thing (C2). ci.yaml already carries three comment lines
    containing `go test`, one of them a complete command.

C4 mutates the GUARD rather than the tree, which is the only way to exercise its read-nothing
branch: a workflows directory that cannot be read must FAIL, not pass.

Usage:
    python3 scripts/w31-shortflag-controls-6b4d.py
"""

import os
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CI = os.path.join(ROOT, ".github/workflows/ci.yaml")
MAKEFILE = os.path.join(ROOT, "Makefile")
GUARD = os.path.join(ROOT, "internal/testutil/shortflag_test.go")
EXTRA_WF = os.path.join(ROOT, ".github/workflows/zz-control-6b4d.yml")

TEST = "TestCINeverRunsTheSuiteInShortMode"

REAL_STEP = "        run: go test -timeout 300s -race -count=1 ./..."

# Each control: (id, kind, target, old, new, expect_red, why)
#   kind "edit"   — replace `old` with `new` in `target` (old must appear exactly once)
#   kind "create" — write `new` to `target`, which must not already exist
CONTROLS = [
    (
        "C1 -short added to CI's real go test step",
        "edit", CI,
        REAL_STEP,
        "        run: go test -short -timeout 300s -race -count=1 ./...",
        True,
        "the mutation the guard exists for: one word, 244 real-Postgres tests off, exit 0",
    ),
    (
        "C2 a COMMENT mentioning `go test -short`",
        "edit", CI,
        REAL_STEP,
        "      # a unit-only run is `go test -short ./...` — documented, never executed here\n"
        + REAL_STEP,
        False,
        "MUST STAY GREEN: a guard that reds on prose teaches readers it is about wording, and the "
        "cheapest way to quiet it would be to delete the documentation",
    ),
    (
        "C3 CI's go test step deleted entirely",
        "edit", CI,
        REAL_STEP + "\n",
        "",
        True,
        "the vacuity floor. 'no invocation carries -short' is perfectly satisfied by a CI that "
        "runs no tests at all — and the Makefile's own invocation must not stand in for CI's",
    ),
    (
        "C4 the workflows directory cannot be read",
        "edit", GUARD,
        'wf := filepath.Join(root, ".github", "workflows")',
        'wf := filepath.Join(root, ".github", "workflowz-does-not-exist")',
        True,
        "read-nothing is not find-nothing: a moved or renamed workflow dir must FAIL, not pass",
    ),
    (
        "C5 -short in a SECOND workflow file",
        "create", EXTRA_WF,
        None,
        "name: control\non:\n  push:\njobs:\n  t:\n    runs-on: ubuntu-latest\n"
        "    steps:\n      - run: go test -short ./...\n",
        True,
        "the scan must cover the whole workflows directory, not the one file it was written against",
    ),
    (
        "C6 the -test.short spelling",
        "edit", CI,
        REAL_STEP,
        "        run: go test -test.short -timeout 300s -race -count=1 ./...",
        True,
        "the guard claims to catch every spelling Go accepts; this is the one nobody types by hand "
        "and the one a copied command carries",
    ),
]


def run():
    p = subprocess.run(["go", "test", "-count=1", "-run", TEST, "./internal/testutil/"],
                       cwd=ROOT, capture_output=True, text=True,
                       env={**os.environ, "DOCS_TEST_DATABASE_URL": ""})
    return p.returncode == 0, (p.stdout + p.stderr)


def main():
    print("=== baseline (unmutated) ===")
    ok, out = run()
    if not ok:
        print("BASELINE RED — the harness is not measuring what it claims:\n" + out[-3000:])
        return 1
    print("baseline green — as it must be; the guard locks a property that holds today\n")

    good, bad = 0, 0
    for name, kind, target, old, new, expect_red, why in CONTROLS:
        src = None
        if kind == "edit":
            src = open(target, encoding="utf-8").read()
            if src.count(old) != 1:
                print(f"[SKIP] {name}: anchor matched {src.count(old)} times, want 1 — NOT applied")
                bad += 1
                continue
        elif os.path.exists(target):
            print(f"[SKIP] {name}: {target} already exists — NOT applied")
            bad += 1
            continue
        try:
            if kind == "edit":
                open(target, "w", encoding="utf-8").write(src.replace(old, new, 1))
            else:
                open(target, "w", encoding="utf-8").write(new)
            ok, out = run()
        finally:
            if kind == "edit":
                open(target, "w", encoding="utf-8").write(src)
            elif os.path.exists(target):
                os.remove(target)
        went_red = not ok
        verdict = "CAUGHT" if went_red else "green"
        if went_red == expect_red:
            good += 1
            print(f"[AS PREDICTED: {verdict}] {name}\n                     {why}")
        else:
            bad += 1
            want = "RED" if expect_red else "GREEN"
            print(f"[WRONG — wanted {want}] {name}\n                     {why}")

    print(f"\n{good} as predicted, {bad} not, of {len(CONTROLS)}")
    return 0 if bad == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
