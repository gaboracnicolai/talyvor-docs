#!/usr/bin/env python3
"""W6.35 control campaign — the toolchain floor and the go.mod↔ci.yaml lockstep."""
import hashlib, os, signal, subprocess, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GOMOD = os.path.join(ROOT, "go.mod")
CI    = os.path.join(ROOT, ".github/workflows/ci.yaml")
TEST  = os.path.join(ROOT, "internal/toolchainguard/toolchainguard_test.go")
FILES = [GOMOD, CI, TEST]

def sha(p): return hashlib.sha256(open(p, "rb").read()).hexdigest()

def run(test):
    r = subprocess.run(["go", "test", "-count=1", "-run", "^%s$" % test, "./internal/toolchainguard/"],
                       cwd=ROOT, capture_output=True, text=True)
    return r.returncode == 0, r.stdout + r.stderr

def anchored(path, old, new, count=1):
    s = open(path).read()
    n = s.count(old)
    if n != count:
        raise AssertionError("anchor appears %d times, want %d: %r" % (n, count, old[:60]))
    open(path, "w").write(s.replace(old, new, 1))

MOD = "TestGoModPinsTheToolchainFloor"
LCK = "TestEveryCIGoVersionPinMeetsTheFloor"
RUN = "TestTheRunningToolchainMeetsTheFloor"
WHY = "TestTheFloorsRationaleNamesTheAdvisories"

CONTROLS = [
    ("W1 the toolchain directive deleted", GOMOD,
     "\ntoolchain go1.26.6\n", "\n", MOD, LCK,
     "removing it restores nine reachable stdlib advisories to local and Docker builds"),

    ("W2 the shipped pin lowered below the floor", GOMOD,
     "toolchain go1.26.6", "toolchain go1.26.5", MOD, LCK,
     "1.26.5 leaves eight of the nine reachable"),

    ("W3 the CI pin lowered below the floor", CI,
     '          go-version: "1.26.6"\n', '          go-version: "1.25"\n', LCK, MOD,
     "the number that actually governs CI is compared, not trusted — track's W6.34 lesson"),

    ("W4 the CI pin parse finds nothing", TEST,
     'goVersionRe = regexp.MustCompile(`go-version:\\s*"(\\d+)\\.(\\d+)(\\.\\d+)?"`)',
     'goVersionRe = regexp.MustCompile(`NEVERMATCH:\\s*"(\\d+)\\.(\\d+)(\\.\\d+)?"`)',
     LCK, MOD, "a parse finding no pins must fail, not report perfect lockstep"),

    ("W5 the rationale's advisory list stripped", GOMOD,
     "GO-2026-6218,\n// GO-2026-6090, GO-2026-6089, GO-2026-6088, GO-2026-5972, GO-2026-5039, GO-2026-5037 and\n// GO-2026-5026;",
     "(list removed);", WHY, MOD,
     "a floor defended by one advisory is a different claim from one defended by nine"),

    # W6's first version made atLeastFloor always return TRUE and expected MOD to go red. It did
    # not, and correctly so: the real pin is AT the floor, so "always true" and the real comparison
    # agree on it — the control was a tautology. Inverting it instead proves the comparison's RESULT
    # is actually consulted: if the guard ignored it, MOD would still pass.
    ("W6 the floor comparison's result ignored", TEST,
     "func atLeastFloor(maj, min, pat int) bool {", "func atLeastFloor(maj, min, pat int) bool {\n\treturn false\n\t//",
     MOD, None, "the comparison is consulted, not computed and discarded"),

    ("W7 the toolchain regex stops matching", TEST,
     'toolchainRe = regexp.MustCompile(`(?m)^toolchain go(\\d+)\\.(\\d+)\\.(\\d+)$`)',
     'toolchainRe = regexp.MustCompile(`(?m)^NEVERMATCH go(\\d+)\\.(\\d+)\\.(\\d+)$`)',
     MOD, LCK, "a parse that finds nothing fails loudly rather than reporting a pinned repo"),
]

before = {p: sha(p) for p in FILES}
print("BASELINE sha256")
for p in FILES:
    print("  %-32s %s" % (os.path.basename(p), before[p]))
ok, out = run("Test")
if not ok:
    sys.exit("not green before the campaign:\n" + out[-2000:])
print("\nbaseline: GREEN\n")


def restore_on_signal(snapshot):
    """Put every snapshotted file back, then die of the signal we were sent.

    A `finally` DOES NOT RUN ON SIGTERM. Measured in talyvor-suite (W1.7, 78c69c8): a 2-minute
    command timeout killed a control mid-mutation and left a GATE REMOVED in the working tree,
    with a green suite and a `git status` that showed only files the session had edited on
    purpose. Reproduced on demand in 5de27e3 — same kill, same file: with a handler nothing was
    stranded, without one the mutated file stayed.

    Re-raising with SIG_DFL keeps the exit status honest: a caller that killed this process still
    sees it die of that signal rather than exit 0 with a tidy tree. SIGKILL still strands and
    nothing in Python can change that.

    Deliberately self-contained rather than an import, so the next script is a paste. The
    population and the rule live in scripts/check-restore-signal-handlers.py.
    """
    def handler(signum, _frame):
        for path, blob in snapshot.items():
            try:
                open(path, "wb").write(blob)
            except OSError:
                pass
        sys.stderr.write("\n!! signal %d — restored %d mutated file(s) before exiting\n"
                         % (signum, len(snapshot)))
        signal.signal(signum, signal.SIG_DFL)
        os.kill(os.getpid(), signum)

    for s in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(s, handler)


results = []
for ctl in CONTROLS:
    name, path, old, new, red, green, proves = ctl[:7]
    expect = ctl[7] if len(ctl) > 7 else 1
    backup = open(path).read()
    # Installed AFTER the snapshot exists and re-installed each control, because `path` differs
    # per control. The `finally` below is the normal path; this is the one a SIGTERM takes.
    restore_on_signal({path: backup.encode("utf-8")})
    try:
        anchored(path, old, new, expect)
        red_ok, red_out = run(red)
        green_ok = True
        if green:
            green_ok, _ = run(green)
        verdict = "CAUGHT" if (not red_ok and green_ok) else ("MISSED" if red_ok else "COLLATERAL")
    except AssertionError as e:
        verdict, red_out = "ANCHOR-FAILED: %s" % e, ""
    finally:
        open(path, "w").write(backup)
    print("%-46s %s" % (name, verdict))
    print("     proves: %s" % proves)
    if verdict == "CAUGHT":
        hit = [l for l in red_out.splitlines() if "_test.go:" in l]
        if hit:
            print("     red says: %s" % hit[0].strip()[:130])
    results.append(verdict)
    print()

after = {p: sha(p) for p in FILES}
clean = all(before[p] == after[p] for p in FILES)
print("RESTORE PROOF")
for p in FILES:
    print("  %-32s %s" % (os.path.basename(p), "IDENTICAL" if before[p] == after[p] else "!! MUTATED !!"))
ok, _ = run("Test")
print("\ngreen after restore: %s" % ok)
c = results.count("CAUGHT")
print("\n%d/%d controls CAUGHT" % (c, len(results)))
sys.exit(0 if (c == len(results) and clean and ok) else 1)
