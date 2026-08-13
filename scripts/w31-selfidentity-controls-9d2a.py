#!/usr/bin/env python3
"""Positive controls for the edit-session self-identity finding (W3.1, tab-9d2a).

THE GUARD UNDER CONTROL
-----------------------
frontend/src/pages/PageView.editsession.selfidentity.test.tsx, three cases and six tags:

  self-held slot   [SESSION-ACQUIRED] [NO-SELF-AS-STRANGER] [HOLDER-CAN-EDIT] [SESSION-HEARTBEATED]
  foreign slot     [STRANGER-STILL-LOCKS] [NO-FOREIGN-HEARTBEAT]

The two halves pull in OPPOSITE directions and that is the whole point: "stop showing the
banner" satisfies the first and destroys the second, so C2 (the plausible wrong fix) has to be
caught by both, and C3 (learn the id from the GET instead of from a 2xx claim) has to be caught
by the second alone. A control set that only restored the defect would not distinguish the real
fix from either of them.

Every mutation is applied to PRODUCT source; C6 additionally deletes the guard, which is what a
vacuity floor is for. Every file is restored in a `finally` and sha256-compared before exit.

Usage:  python3 scripts/w31-selfidentity-controls-9d2a.py
"""

from __future__ import annotations

import hashlib
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
FE = ROOT / "frontend"

HOOK = ROOT / "frontend/src/hooks/useEditSession.ts"
PAGEVIEW = ROOT / "frontend/src/pages/PageView.tsx"
GUARD = ROOT / "frontend/src/pages/PageView.editsession.selfidentity.test.tsx"

TOUCHED = [HOOK, PAGEVIEW, GUARD]

LEARNED = "  const memberID = learnedSelf || storedMemberID;"
STORED_ONLY = "  const memberID = storedMemberID;"

HELD_BY_OTHER = "  const heldByOther = live && !!holder && holder !== memberID;"
HELD_BY_OTHER_NAIVE = "  const heldByOther = live && !!holder && !!memberID && holder !== memberID;"

QUERYFN = "    queryFn: () => editSessionApi.get(spaceID, pageID),"
QUERYFN_LEARNS = (
    "    queryFn: async () => {\n"
    "      const s = await editSessionApi.get(spaceID, pageID);\n"
    "      learnSelf(s);\n"
    "      return s;\n"
    "    },"
)

AUTOACQUIRE = '    autoAcquire: !readOnly && !!page && page.doc_status !== "approved",'
AUTOACQUIRE_OFF = "    autoAcquire: false,"

HEARTBEAT_CONST = "export const HEARTBEAT_MS = 10_000;"
HEARTBEAT_CONST_FAST = "export const HEARTBEAT_MS = 5_000;"

COMMENT = "  // The localStorage value stays as the fallback for the window before the first claim lands."
COMMENT_REWORDED = "  // The localStorage value remains the fallback until the first claim has landed."


def sub(path: pathlib.Path, old: str, new: str) -> None:
    s = path.read_text()
    n = s.count(old)
    if n != 1:
        raise SystemExit(
            f"HARNESS BUG: anchor occurs {n}x in {path.relative_to(ROOT)} (want exactly 1).\n"
            f"anchor:\n{old}"
        )
    path.write_text(s.replace(old, new))


def c0_inert() -> None:
    sub(HOOK, COMMENT, COMMENT_REWORDED)


def c1_defect() -> None:
    sub(HOOK, LEARNED, STORED_ONLY)


def c2_naive_fix() -> None:
    # "an empty member id means nobody is a stranger" — the fix that makes the banner go away
    # without ever answering who the caller is.
    c1_defect()
    sub(HOOK, HELD_BY_OTHER, HELD_BY_OTHER_NAIVE)


def c3_learn_from_get() -> None:
    # Learning the id from the OBSERVE call instead of from a 2xx claim: the GET reports
    # whoever holds the slot, so a stranger's holder becomes "me".
    sub(HOOK, QUERYFN, QUERYFN_LEARNS)


def c4_no_autoacquire() -> None:
    sub(PAGEVIEW, AUTOACQUIRE, AUTOACQUIRE_OFF)


def c5_heartbeat_period() -> None:
    sub(HOOK, HEARTBEAT_CONST, HEARTBEAT_CONST_FAST)


def c6_vacuity_floor() -> None:
    c1_defect()
    GUARD.unlink()


def run_guard() -> tuple[int, str]:
    p = subprocess.run(
        ["npx", "vitest", "run", "src/pages/PageView.editsession.selfidentity.test.tsx"],
        cwd=FE, capture_output=True, text=True,
    )
    return p.returncode, p.stdout + p.stderr


def run_suite() -> tuple[int, str]:
    p = subprocess.run(["npx", "vitest", "run"], cwd=FE, capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


CONTROLS = [
    ("C0 inert — reword a comment in the hook",
     c0_inert, run_guard, [], [],
     "must NOT be caught"),
    ("C1 the defect exactly — memberID back to localStorage only",
     c1_defect, run_guard, ["[NO-SELF-AS-STRANGER]"], ["[STRANGER-STILL-LOCKS]"],
     "the red-first proof: the holder is named as the stranger"),
    ("C2 the plausible WRONG fix — an empty member id means nobody is a stranger",
     c2_naive_fix, run_guard, ["[SESSION-HEARTBEATED]", "[STRANGER-STILL-LOCKS]"], [],
     "the banner goes away and BOTH the heartbeat and the foreign-lock halves break: "
     "proves the guard is not satisfied by silencing the symptom"),
    ("C3 the over-learn — take the id from the GET instead of from a 2xx claim",
     c3_learn_from_get, run_guard, ["[STRANGER-STILL-LOCKS]"], ["[NO-SELF-AS-STRANGER]"],
     "proves the 'only from a claim the server granted ME' boundary is load-bearing"),
    ("C4 the precondition — autoAcquire forced off, so no slot is ever held",
     c4_no_autoacquire, run_guard, ["[SESSION-ACQUIRED]"], [],
     "proves the self-held assertions cannot pass by there being no session"),
    ("C5 HEARTBEAT_MS 10s -> 5s",
     c5_heartbeat_period, run_guard, [], [],
     "must stay GREEN: the guard advances 2x the exported constant, not a literal"),
    ("C6 vacuity floor — defect restored AND this guard deleted",
     c6_vacuity_floor, run_suite, [], [],
     "the whole frontend suite must stay GREEN: nothing else in it can see this"),
]


def main() -> int:
    originals = {p: p.read_text() for p in TOUCHED}
    digests = {p: hashlib.sha256(originals[p].encode()).hexdigest() for p in TOUCHED}

    results = []
    for name, mutate, runner, expect, forbid, note in CONTROLS:
        try:
            mutate()
            code, out = runner()
        finally:
            for p in TOUCHED:
                p.write_text(originals[p])
        seen = [t for t in expect if t in out]
        bad = [t for t in forbid if t in out]
        if expect:
            ok = len(seen) == len(expect) and not bad and code != 0
            verdict = "CAUGHT" if ok else "MISSED"
        else:
            ok = code == 0 and not bad
            verdict = "NOT CAUGHT (as predicted)" if ok else "CAUGHT (UNEXPECTED)"
        results.append(ok)
        print(f"[{'ok' if ok else 'FAIL'}] {name}\n"
              f"        {verdict}; exit={code}; tags seen={seen or '-'}; forbidden seen={bad or '-'}\n"
              f"        {note}")

    dirty = [p for p in TOUCHED
             if hashlib.sha256(p.read_text().encode()).hexdigest() != digests[p]]
    if dirty:
        print("\nRESTORE FAILED for: " + ", ".join(str(p.relative_to(ROOT)) for p in dirty))
        return 1

    passed = sum(1 for r in results if r)
    print(f"\n{passed}/{len(results)} controls behaved as predicted; "
          f"all {len(TOUCHED)} touched files restored (sha256 verified)")
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
