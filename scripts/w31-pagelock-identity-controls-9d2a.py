#!/usr/bin/env python3
"""Positive controls for the page-lock self-identity finding (W3.1, tab-9d2a).

GUARD: frontend/src/pages/PageView.pagelock.selfidentity.test.tsx
Tags:  [LOCK-TAKEN] [LOCK-SELF-RECOGNISED] [LOCKER-CAN-UNLOCK] [LOCKER-CAN-EDIT]
       [STRANGER-STILL-LOCKS]

As with the edit-session guard, the two cases pull in OPPOSITE directions: any patch that just
stops calling a lock "someone else's" satisfies the first case and destroys the second. C2 and
C3 are the two ways to get that wrong and each is caught by the half it breaks.

C5 is the anti-overfit direction. This guard is deliberately coupled to exactly two product
strings — "You locked this page." and the "You locked this — click to unlock" title, which ARE
the screen's answer to "is this lock mine" — and to nothing else. C5 rewords the OTHER branch's
prose (the stranger banner's CTA and the stranger badge's title) and must not be caught, which
is what shows the stranger case settles on the editor's readOnly rather than on copy.

Every mutation is applied to PRODUCT source; C6 additionally deletes the guard. Every file is
restored in a `finally` and sha256-compared before exit.

Usage:  python3 scripts/w31-pagelock-identity-controls-9d2a.py
"""

from __future__ import annotations

import hashlib
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
FE = ROOT / "frontend"

HOOK = ROOT / "frontend/src/hooks/usePageLock.ts"
BADGE = ROOT / "frontend/src/components/LockBadge.tsx"
BANNER = ROOT / "frontend/src/components/LockBanner.tsx"
GUARD = ROOT / "frontend/src/pages/PageView.pagelock.selfidentity.test.tsx"

TOUCHED = [HOOK, BADGE, BANNER, GUARD]

LEARNED = "  const memberID = learnedSelf || storedMemberID;"
STORED_ONLY = "  const memberID = storedMemberID;"

COMPUTE = "  return !!s?.locked && !!memberID && s.locked_by === memberID;"
COMPUTE_NAIVE = "  return !!s?.locked && (!memberID || s.locked_by === memberID);"

QUERYFN = "    queryFn: () => pagelockApi.get(spaceID, pageID),"
QUERYFN_LEARNS = (
    "    queryFn: async () => {\n"
    "      const s = await pagelockApi.get(spaceID, pageID);\n"
    "      if (s?.locked_by) setLearnedSelf(s.locked_by);\n"
    "      return s;\n"
    "    },"
)

LOCK_ONCLICK = "        onClick={onLock}"
LOCK_ONCLICK_DEAD = "        onClick={() => undefined}"

COMMENT = "  // localStorage stays as the fallback for the window before the first lock lands. What"
COMMENT_REWORDED = "  // localStorage remains the fallback until the first lock has landed. What"

STRANGER_CTA = "        Request unlock"
STRANGER_CTA_REWORDED = "        Ask for access"
STRANGER_TITLE = '      title={`Locked by ${by}${at ? " at " + at : ""}`}'
STRANGER_TITLE_REWORDED = '      title={`Held by ${by}${at ? " since " + at : ""}`}'


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
    # "no member id means every lock is mine" — the patch that makes the Unlock button reappear
    # without ever answering who the caller is.
    c1_defect()
    sub(HOOK, COMPUTE, COMPUTE_NAIVE)


def c3_learn_from_get() -> None:
    sub(HOOK, QUERYFN, QUERYFN_LEARNS)


def c4_lock_affordance_dead() -> None:
    sub(BADGE, LOCK_ONCLICK, LOCK_ONCLICK_DEAD)


def c5_stranger_copy() -> None:
    sub(BANNER, STRANGER_CTA, STRANGER_CTA_REWORDED)
    sub(BADGE, STRANGER_TITLE, STRANGER_TITLE_REWORDED)


def c6_vacuity_floor() -> None:
    c1_defect()
    GUARD.unlink()


def run_guard() -> tuple[int, str]:
    p = subprocess.run(
        ["npx", "vitest", "run", "src/pages/PageView.pagelock.selfidentity.test.tsx"],
        cwd=FE, capture_output=True, text=True,
    )
    return p.returncode, p.stdout + p.stderr


def run_suite() -> tuple[int, str]:
    p = subprocess.run(["npx", "vitest", "run"], cwd=FE, capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


CONTROLS = [
    ("C0 inert — reword a comment in the hook",
     c0_inert, run_guard, [], [], "must NOT be caught"),
    ("C1 the defect exactly — memberID back to localStorage only",
     c1_defect, run_guard, ["[LOCK-SELF-RECOGNISED]"], ["[STRANGER-STILL-LOCKS]"],
     "the red-first proof: the locker is shown their own id as the stranger"),
    ("C2 the plausible WRONG fix — no member id means every lock is mine",
     c2_naive_fix, run_guard, ["[STRANGER-STILL-LOCKS]"], ["[LOCK-SELF-RECOGNISED]"],
     "the Unlock button comes back AND a stranger's lock becomes unlockable: proves the "
     "guard is not satisfied by restoring the affordance"),
    ("C3 the over-learn — take the id from the GET instead of from a granted lock",
     c3_learn_from_get, run_guard, ["[STRANGER-STILL-LOCKS]"], ["[LOCK-SELF-RECOGNISED]"],
     "proves the 'only from a lock the server granted ME' boundary is load-bearing"),
    ("C4 the precondition — the Lock control's onClick made dead",
     c4_lock_affordance_dead, run_guard, ["[LOCK-TAKEN]"], [],
     "proves the self-lock assertions cannot pass by there being no lock"),
    ("C5 the OTHER branch's copy reworded (stranger CTA + stranger badge title)",
     c5_stranger_copy, run_guard, [], [],
     "must stay GREEN: the stranger case settles on the editor's readOnly, not on prose"),
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
