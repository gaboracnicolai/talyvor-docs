#!/usr/bin/env python3
"""W3.64 — controls for `w635-toolchain-guard-controls.py --check-anchors`.

The new mode PASSED ON ITS FIRST RUN. This repo's standing rule is to suspect that, and the defect
it exists to catch is precisely a check that reports clean because it is looking at nothing.

⚠ A4 IS INVERTED ON PURPOSE: an untouched tree must stay GREEN. A check that reds on correct work
gets relaxed until it reds on nothing.

⚠ A5 IS THE ONE THAT DEFENDS THE CI STEP'S PREMISE. `--check-anchors` is in CI only because it
mutates nothing and runs no `go test`; if that ever stops being true the step is a tree-mutating
job on every push. A5 asserts the three target files are byte-identical afterwards.

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
CI = ROOT / ".github/workflows/ci.yaml"
CAMPAIGN = ROOT / "scripts/w635-toolchain-guard-controls.py"
GOMOD = ROOT / "go.mod"
TESTF = ROOT / "internal/toolchainguard/toolchainguard_test.go"
WATCHED = [CI, GOMOD, TESTF]


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


TEST_PIN = '          go-version: "1.26.6"\n          cache: true\n'

CONTROLS = [
    ("A1 a THIRD copy of the test-job pin appears", CI,
     TEST_PIN, TEST_PIN + TEST_PIN, True,
     "the exact way W3 went inert: a literal that was unique stops being unique"),

    ("A2 the test-job anchor stops matching at all", CI,
     "          cache: true\n", "          cache: yes\n", True,
     "0x is as inert as 2x, and it is the direction a rename produces"),

    ("A3 VACUITY: the control list shrinks below the floor", CAMPAIGN,
     "ANCHOR_FLOOR = 8", "ANCHOR_FLOOR = 8\nCONTROLS = CONTROLS[:2]", True,
     "a loop over a shrunken list reports clean anchors rather than a missing campaign"),

    ("A4 INVERTED: an untouched tree must stay green", CI, TEST_PIN, TEST_PIN, False,
     "a check that reds on correct work gets relaxed until it reds on nothing"),
]


def main() -> int:
    originals = {p: p.read_bytes() for p in WATCHED + [CAMPAIGN]}
    hashes = {p: sha(p) for p in originals}
    restore_on_signal(originals)

    ok, out = check()
    if not ok:
        print("BASELINE IS NOT GREEN — every verdict below would be unreadable:\n" + out)
        return 2
    print("baseline: GREEN\n")

    results = []
    try:
        for name, path, find, repl, predict_red, why in CONTROLS:
            src = originals[path].decode()
            if src.count(find) != 1:
                results.append((name, "CONTROL DEFECT",
                                f"needle occurs {src.count(find)}x, want 1 — the probe lands nowhere"))
                print(f"  [CONTROL DEFECT] {name}")
                continue
            try:
                path.write_text(src.replace(find, repl, 1))
                passed, out = check()
            finally:
                path.write_bytes(originals[path])
            red = not passed
            verdict = "OK" if red == predict_red else ("!! BLIND" if not red else "!! REDS ON CORRECT CODE")
            results.append((name, verdict,
                            f"{'RED' if red else 'green'} (predicted {'RED' if predict_red else 'green'}) — {why}"))
            print(f"  [{verdict:22s}] {name}: {'RED' if red else 'green'}")

        # A5 — the premise the CI step rests on.
        pre = {p: sha(p) for p in WATCHED}
        check()
        moved = [p.name for p in WATCHED if sha(p) != pre[p]]
        verdict = "OK" if not moved else "!! MUTATES THE TREE"
        results.append(("A5 --check-anchors mutates nothing", verdict,
                        ("no watched file changed" if not moved else "CHANGED: " + ",".join(moved))
                        + " — the CI step is only safe because this holds"))
        print(f"  [{verdict:22s}] A5 --check-anchors mutates nothing")

        # A6 — the CI step this merge adds must not itself look like a go-version pin.
        r = subprocess.run(["go", "test", "-count=1", "-run",
                            "^TestEveryCIGoVersionPinMeetsTheFloor$", "./internal/toolchainguard/"],
                           cwd=ROOT, capture_output=True, text=True)
        verdict = "OK" if r.returncode == 0 else "!! NEW COMMENT PARSES AS A PIN"
        results.append(("A6 the new CI comment is not read as a pin", verdict,
                        "the added step's prose mentions go-version; the guard must still pass"))
        print(f"  [{verdict:22s}] A6 the new CI comment is not read as a pin")
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
