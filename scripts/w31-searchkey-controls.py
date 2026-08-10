#!/usr/bin/env python3
"""Positive controls for useSearch's cache key (W3.1, tab-b74e).

Each control mutates PRODUCT code, runs the frontend suite, and is scored against a catcher
PREDICTED BEFORE THE RUN. Verdicts are read from the set of failing test NAMES — the three cases
live in three separate `it` blocks, so a name IS an assertion here.

⚠⚠ TWO RESULTS HERE ARE CORRECTIONS, NOT CATCHES, AND THEY ARE PRINTED RATHER THAN TIDIED AWAY:

  · F3 FALSIFIED THE REASON I WROTE IT. I predicted that reverting the key under a staleTime-0
    client would go GREEN — the argument for exporting the app's query-client options so a test
    could drive them instead of a `{ retry: false }` stand-in. It reds anyway: react-query
    refetches on a MOUNT or a KEY CHANGE, not on a rerender, and the defect IS that the key does
    not change, so no staleTime can rescue it. The `~/lib/queryClient` module that argument
    justified was REVERTED rather than shipped as ceremony no control could earn.
  · F5 IS IMPRECISE AND STAYS THAT WAY. A key carrying a fresh value every render reddens all
    three cases, because destroying the cache breaks every statement about it. So the
    unchanged-offset refusal is NOT independently earned by any control in this set — said here
    rather than left for a reader to infer from a clean-looking score.

INHERITED: restore in a finally and prove it with sha256 · a build/type failure is NOT a catch ·
assert the anchor before writing · every control declares its must-stay-green set.

Usage:  python3 scripts/w31-searchkey-controls.py
"""

import hashlib
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FE = os.path.join(ROOT, "frontend")

HOOK = "frontend/src/hooks/useSearch.ts"
TEST = "frontend/src/hooks/useSearch.offset.test.tsx"
TOUCHED = (HOOK, TEST)

T_DEFECT = "a different offset is a different request, not the previous page from cache"
T_SAME = "MUST STAY GREEN: an unchanged offset is still one request, not two"
T_SIBLING = "MUST STAY GREEN: the sibling options still key the cache"

KEY = "    queryKey: [\"search\", workspaceId, debounced, opts.type, opts.spaceId, opts.limit, opts.offset],"
STALE = "defaultOptions: { queries: { staleTime: 30_000, retry: 1, refetchOnWindowFocus: false } },"

CONTROLS = [
    ("F1", "THE REVERT: `opts.offset` is dropped from the key (the shipped defect)",
     [(HOOK, KEY, KEY.replace(", opts.offset]", "]"))],
     [T_DEFECT], [T_SAME, T_SIBLING]),
    ("F2", "TAUTOLOGY: a CONSTANT takes offset's place — the key is the right LENGTH and never varies",
     # A guard that counted key members, or diffed the key's shape, passes this. Only a guard that
     # observes REQUESTS fails it.
     [(HOOK, KEY, KEY.replace("opts.offset]", "0]"))],
     [T_DEFECT], [T_SAME, T_SIBLING]),
    ("F3", "the revert AND staleTime 0 — PREDICTION CORRECTED: staleTime is not what makes it visible",
     # I predicted BLIND. It reds. Two edits applied as one write per file: revert the key and drop
     # the test client's staleTime to the stand-in's 0. The guard sees the defect either way, which
     # is the measurement that sent the ~/lib/queryClient module back.
     [(HOOK, KEY, KEY.replace(", opts.offset]", "]")),
      (TEST, STALE, "defaultOptions: { queries: { staleTime: 0, retry: 1, refetchOnWindowFocus: false } },")],
     [T_DEFECT], [T_SAME, T_SIBLING]),
    ("F5", "OVER-CORRECTION: the key gains a fresh value every render, so nothing is ever cached",
     # RECORDED IMPRECISE ON PURPOSE. It reddens all three: a key that never repeats defeats the
     # cache, and every case in the file is a statement about the cache. No control isolates the
     # unchanged-offset refusal, so that assertion is kept for what it refuses and is NOT claimed
     # as earned.
     [(HOOK, KEY, KEY.replace("opts.offset]", "opts.offset, Math.random()]"))],
     [T_SAME], [T_SIBLING]),
    ("F4", "`opts.limit` is dropped from the key instead (a sibling lost on the way past)",
     [(HOOK, KEY, KEY.replace("opts.limit, ", ""))],
     [T_SIBLING], [T_DEFECT, T_SAME]),
]


def sha(p):
    with open(os.path.join(ROOT, p), "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def read(p):
    with open(os.path.join(ROOT, p), encoding="utf-8") as fh:
        return fh.read()


def write(p, body):
    with open(os.path.join(ROOT, p), "w", encoding="utf-8") as fh:
        fh.write(body)


def run():
    """Return (ok, failing test names, raw)."""
    proc = subprocess.run(
        ["npx", "vitest", "run", "src/hooks/useSearch.offset.test.tsx", "--reporter=verbose"],
        cwd=FE, capture_output=True, text=True, timeout=600,
    )
    out = proc.stdout + proc.stderr
    broken = "Failed to load" in out or "Transform failed" in out or "No test files found" in out
    # ⚠ THE FIRST DRAFT OF THIS LINE SCORED 0/4 WITH THE ASSERTION MESSAGE PRINTED IN THE SAME
    # OUTPUT. vitest --reporter=verbose prints `× <file> > <describe> > <name> <ms>`, so a regex
    # anchored on the whole line captured the path too and every name failed the membership test —
    # a working control set read as four dead controls. Take the text after the LAST " > ".
    failing = []
    for line in out.splitlines():
        s = line.strip()
        if not s.startswith(("×", "x ")) or " > " not in s:
            continue
        name = re.sub(r"\s+\d+ms$", "", s.rsplit(" > ", 1)[1]).strip()
        if name in (T_DEFECT, T_SAME, T_SIBLING):
            failing.append(name)
    failing = sorted(set(failing))
    return (not broken), failing, out


def main():
    before = {p: sha(p) for p in TOUCHED}
    ok, failing, out = run()
    if not ok or failing:
        print("BASELINE IS NOT GREEN — controls cannot be scored against it.")
        print(out[-3000:])
        return 2
    print("baseline: 3 tests, 0 failing\n")

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
            built, failed, raw = run()
            caught = [t for t in predicted if t in failed]
            broke = [t for t in stay_green if t in failed]
            if not built:
                verdict = "ERROR (control does not load — a defect in the CONTROL)"
            elif broke:
                verdict = f"IMPRECISE — also reddened must-stay-green {broke}"
            elif not predicted:
                verdict = ("BLIND AS PREDICTED (the stand-in client cannot see it)" if not caught
                           and not failed else f"UNEXPECTED — predicted blind, failed {failed}")
            elif sorted(caught) == sorted(predicted):
                verdict = "CAUGHT by the test named before the run"
            elif caught:
                verdict = f"PARTIAL — predicted {predicted}, caught {caught}"
            else:
                verdict = f"NOT CAUGHT — predicted {predicted}, failures were {failed or 'none'}"
            said = re.findall(r"(offset MISSING FROM THE CACHE KEY: .{0,90})", raw)
            results.append((cid, desc, predicted, failed, verdict, said[:1]))
        finally:
            for path, body in originals.items():
                write(path, body)

    after = {p: sha(p) for p in TOUCHED}
    restored = after == before
    print("=" * 100)
    for cid, desc, predicted, failed, verdict, said in results:
        print(f"\n{cid}  {desc}")
        print(f"     predicted : {predicted}")
        print(f"     failed    : {failed or '(none)'}")
        print(f"     verdict   : {verdict}")
        for s in said:
            print(f"     said      : {s.strip()}")
    print("\n" + "=" * 100)
    print(f"tree restored (sha256 on {len(before)} files): {restored}")
    if not restored:
        for p in before:
            if before[p] != after[p]:
                print(f"  ⚠ NOT RESTORED: {p}")
        return 1
    good = sum(1 for r in results if r[4].startswith(("CAUGHT", "BLIND AS PREDICTED")))
    print(f"{good}/{len(results)} controls caught by the test named before the run")
    return 0 if good == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
