#!/usr/bin/env python3
"""W6.44 — controls for the NAME half of `w635-toolchain-guard-controls.py --check-anchors`.

`--check-anchors` used to verify only that each control's ANCHOR still applies. A campaign rots
from TWO ends: the thing it mutates, and the test it expects to break. talyvor-code #76 is the
demonstration (w420's C4 expected a test deleted in the same refactor that moved its anchor, and
that rot stayed hidden behind the anchor one). This merge adds the second check here.

⚠ THE MEASUREMENT SAID THERE IS NOTHING ROTTEN TO FIX TODAY — all four names this campaign
references exist. So the guard passed on its first run, and that is precisely when this project's
rule says to suspect it. E4 is the control that decides whether it is a guard or a decoration: it
blinds the source walk and requires a red, because a walk returning nothing verifies every name
FOR FREE against a tree it never read.

⚠ E3 IS INVERTED: an untouched tree must stay GREEN.

Mutates tracked files, restores them in a `finally` with sha256 compared, and installs a signal
handler — a `finally` does not run on SIGTERM (scripts/check-restore-signal-handlers.py).
"""
import hashlib
import os
import pathlib
import signal
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
CAMPAIGN = ROOT / "scripts/w635-toolchain-guard-controls.py"
GUARD_TEST = ROOT / "internal/toolchainguard/toolchainguard_test.go"


def restore_on_signal(snapshot: dict) -> None:
    """Put every snapshotted file back, then die of the signal we were sent."""

    def handler(signum, _frame):
        for path, blob in snapshot.items():
            try:
                path.write_bytes(blob)
            except OSError:
                pass
        sys.stderr.write("\n!! signal %d — restored %d mutated file(s) before exiting\n"
                         % (signum, len(snapshot)))
        signal.signal(signum, signal.SIG_DFL)
        os.kill(os.getpid(), signum)

    for s in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(s, handler)


def sha(p: pathlib.Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def check() -> tuple[bool, str]:
    r = subprocess.run([sys.executable, str(CAMPAIGN), "--check-anchors"],
                       cwd=ROOT, capture_output=True, text=True)
    return r.returncode == 0, r.stdout + r.stderr


CASES = [
    ("E1 an expected test is RENAMED away", GUARD_TEST,
     "func TestEveryCIGoVersionPinMeetsTheFloor(",
     "func TestEveryCIGoVersionPinMeetsTheFloorRenamed(",
     True, "the campaign would `-run` a name matching nothing, exit 0, and print MISSED"),

    ("E2 a control names a test that never existed", CAMPAIGN,
     'LCK = "TestEveryCIGoVersionPinMeetsTheFloor"',
     'LCK = "TestThisTestHasNeverExisted"',
     True, "the same rot arriving from the control's side rather than the tree's"),

    ("E3 INVERTED: an untouched tree must stay green", CAMPAIGN,
     'LCK = "TestEveryCIGoVersionPinMeetsTheFloor"',
     'LCK = "TestEveryCIGoVersionPinMeetsTheFloor"',
     False, "a check that reds on correct work gets relaxed until it reds on nothing"),

    ("E4 VACUITY: the source walk returns nothing", CAMPAIGN,
     '    for f in pathlib.Path(ROOT).rglob("*_test.go"):',
     '    for f in pathlib.Path(ROOT).rglob("*_NEVERMATCH.go"):',
     True, "a walk that reads no sources verifies every name for free"),
]


def main() -> int:
    originals = {p: p.read_bytes() for p in (CAMPAIGN, GUARD_TEST)}
    hashes = {p: sha(p) for p in originals}
    restore_on_signal(originals)

    ok, out = check()
    if not ok:
        print("BASELINE IS NOT GREEN — every verdict below would be unreadable:\n" + out)
        return 2
    print("baseline: GREEN\n")

    results = []
    try:
        for name, path, find, repl, predict_red, why in CASES:
            src = originals[path].decode()
            if src.count(find) != 1:
                results.append((name, "CONTROL DEFECT",
                                f"needle occurs {src.count(find)}x, want 1 — probe lands nowhere"))
                print(f"  [CONTROL DEFECT] {name}")
                continue
            try:
                path.write_text(src.replace(find, repl, 1))
                passed, _ = check()
            finally:
                path.write_bytes(originals[path])
            red = not passed
            verdict = "OK" if red == predict_red else ("!! BLIND" if not red else "!! REDS ON CORRECT CODE")
            results.append((name, verdict,
                            f"{'RED' if red else 'green'} (predicted {'RED' if predict_red else 'green'}) — {why}"))
            print(f"  [{verdict:22s}] {name}: {'RED' if red else 'green'}")

        pre = sha(GUARD_TEST)
        check()
        verdict = "OK" if sha(GUARD_TEST) == pre else "!! MUTATES THE TREE"
        results.append(("E5 --check-anchors mutates nothing", verdict,
                        "the CI step is only safe because this holds"))
        print(f"  [{verdict:22s}] E5 --check-anchors mutates nothing")
    finally:
        for p, b in originals.items():
            p.write_bytes(b)
        bad = [p.name for p in originals if sha(p) != hashes[p]]
        print("\nrestore: " + ("BYTE-IDENTICAL" if not bad else "MISMATCH " + ",".join(bad)))

    ok, out = check()
    print("post-control baseline: " + ("GREEN" if ok else "RED\n" + out))
    defects = [r for r in results if r[1] != "OK"]
    print(f"\n{len(results)-len(defects)}/{len(results)} controls behaved as predicted")
    for n, v, d in results:
        print(f"  [{v}] {n}: {d}")
    return 1 if defects or not ok else 0


sys.exit(main())
