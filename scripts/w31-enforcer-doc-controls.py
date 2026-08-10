#!/usr/bin/env python3
"""Positive controls for TestHandlerDocs_NeverCallAMissingEnforcerUnguarded.

The guard is RED-FIRST already: on the tree before this merge it named all 15 handlers. That
proves it can fail in BULK. These controls answer the three questions bulk failure does not:

  R1  can it catch ONE reintroduction, in a file it must find by walking rather than by a list?
  R2  does its FLOOR actually fire when the census stops reading the tree, or does a blinded
      census report clean? (A source-derived phrase guard cannot see the tree shrink. The floor
      exists for exactly that, and a floor nobody has fired is a floor nobody has tested.)
  R3  does the floor protect the PREDICATE? Recorded as NOT CAUGHT, one-directional by design:
      it does not, it cannot, and naming that is the point. A floor and a predicate need
      DIFFERENT catchers; the property that makes the floor independent is why it is blind here.

R2/R3 mutate the GUARD ITSELF, not the product — the instrument is a thing to be tested too.

Usage:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-enforcer-doc-controls.py
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GUARD = os.path.join(REPO, "internal", "permission", "enforcer_doc_test.go")
VICTIM = os.path.join(REPO, "internal", "space", "handler.go")
TEST = "TestHandlerDocs_NeverCallAMissingEnforcerUnguarded"


def sha(p):
    return hashlib.sha256(open(p, "rb").read()).hexdigest()


def run():
    p = subprocess.run(
        ["go", "test", "-count=1", "-run", TEST, "./internal/permission/"],
        cwd=REPO, env=dict(os.environ), capture_output=True, text=True, timeout=600,
    )
    out = p.stdout + p.stderr
    if "[build failed]" in out or re.search(r"^# ", out, re.M):
        return "BUILD_FAILED", out.strip().split("\n")[0], out
    if p.returncode == 0:
        return "PASS", "", out
    line = next((l.strip() for l in out.split("\n") if "enforcer_doc_test.go:" in l), "")
    return "FAIL", line[:240], out


def patch(path, old, new, *, expect):
    src = open(path, encoding="utf-8").read()
    n = src.count(old)
    if n != expect:
        sys.exit(f"FATAL ANCHOR: {old!r} appears {n}x in {os.path.basename(path)}, expected {expect}")
    open(path, "w", encoding="utf-8").write(src.replace(old, new, 1))


CONTROLS = [
    ("R1", "ONE handler (internal/space) has the false phrase put back",
     VICTIM,
     lambda: patch(VICTIM,
                   "Without it those routes FAIL CLOSED:",
                   "Without it the routes mount unguarded (tests). Ignore:",
                   expect=1),
     "FAIL", "internal/space/handler.go"),

    ("R2", "the census is BLINDED — it no longer matches any file, so it reads nothing. The "
           "FLOOR must fire; a clean pass here would mean the guard's green says nothing",
     GUARD,
     lambda: patch(GUARD,
                   'if info.Name() != "handler.go" {',
                   'if info.Name() != "handler_this_file_does_not_exist.go" {',
                   expect=1),
     "FAIL", "census found only 0"),

    ("R3", "the PREDICATE is blinded (the offender check can never be true) while the census "
           "still reads all 15 files — expected NOT CAUGHT, and that is the recorded limit",
     GUARD,
     lambda: patch(GUARD,
                   'if strings.Contains(lines[j], "unguarded") {',
                   'if false && strings.Contains(lines[j], "unguarded") {',
                   expect=1),
     "PASS", ""),
]


def main():
    saved = {p: open(p, "rb").read() for p in (GUARD, VICTIM)}
    shas = {p: sha(p) for p in saved}

    state, detail, _ = run()
    print(f"BASELINE (must be green): {state} {detail}\n")
    if state != "PASS":
        sys.exit("FATAL: baseline is not green — every verdict below would be meaningless")

    results = []
    for cid, desc, path, apply, want, needle in CONTROLS:
        apply()
        if open(path, "rb").read() == saved[path]:
            for p, b in saved.items():
                open(p, "wb").write(b)
            sys.exit(f"FATAL {cid}: mutation changed NO BYTES ON DISK")
        state, detail, full = run()
        for p, b in saved.items():
            open(p, "wb").write(b)

        if state == "BUILD_FAILED":
            verdict = "BUILD ERROR — NOT A CATCH"
        elif state != want:
            verdict = f"PREDICTION WRONG (got {state}, predicted {want})"
        elif needle and needle not in full:
            verdict = f"RED FOR THE WRONG REASON — message does not mention {needle!r}"
        else:
            verdict = "AS PREDICTED" + (" (NOT CAUGHT, one-directional by design)" if want == "PASS" else "")

        results.append((cid, verdict))
        print(f"{cid}  {desc}")
        print(f"    result : {state} {detail}")
        print(f"    VERDICT: {verdict}\n")

    for p, s in shas.items():
        if sha(p) != s:
            sys.exit(f"FATAL: {p} NOT restored to pristine bytes")
    print("both files restored to pristine sha256")

    bad = [r for r in results if not r[1].startswith("AS PREDICTED")]
    print("\nSUMMARY")
    for cid, v in results:
        print(f"  {cid}: {v}")
    if bad:
        sys.exit(f"\n{len(bad)} PREDICTION(S) WRONG — keep them wrong in the record.")
    print("\nALL PREDICTIONS HELD.")


if __name__ == "__main__":
    main()
