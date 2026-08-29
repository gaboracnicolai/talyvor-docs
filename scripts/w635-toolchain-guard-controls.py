#!/usr/bin/env python3
"""W6.35 control campaign — the toolchain floor and the go.mod↔ci.yaml lockstep."""
import hashlib, os, pathlib, signal, subprocess, sys

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

    # ⚠ W3 WAS ONE CONTROL AND IT HAD BEEN UNABLE TO RUN. Its anchor was the bare
    # `go-version: "1.26.6"` line, which was unique when it was written and is not any more:
    # ci.yaml pins the version TWICE, once in `test` and once in `vuln`. `anchored()` refused
    # (ANCHOR-FAILED) and the campaign reported 6/7 — a number that reads like a score.
    #
    # ⚠⚠ THE FIX IS NOT `count=2`. That would mutate BOTH pins at once and stop distinguishing
    # the two jobs, which is the thing the control is about — the `test` job's pin governs the
    # gofmt gate and the real-Postgres suite; the `vuln` job's governs what govulncheck grades
    # the stdlib against. One control that lowers both proves less than the two below.
    #
    # ⚠⚠⚠ AND THE GUARD WAS NEVER THE PROBLEM — MEASURED BEFORE THIS WAS WRITTEN, NOT ASSUMED.
    # Each pin was lowered ALONE against the unmodified guard and
    # TestEveryCIGoVersionPinMeetsTheFloor reds on both, at toolchainguard_test.go:101. The lost
    # coverage was the CONTROL's, never the guard's. Splitting it makes each half provable.
    #
    # The anchors are disambiguated by the line that FOLLOWS the pin (`cache: true` vs
    # `cache: false`) rather than by the comment above it, because the comments are prose that
    # gets edited and the cache setting is a functional difference between the two jobs.
    ("W3a the CI pin lowered below the floor — test job", CI,
     '          go-version: "1.26.6"\n          cache: true\n',
     '          go-version: "1.25"\n          cache: true\n', LCK, MOD,
     "the number that actually governs the gofmt gate and the real-PG suite is compared, not "
     "trusted — track's W6.34 lesson"),

    ("W3b the CI pin lowered below the floor — vuln job", CI,
     '          go-version: "1.26.6"\n          cache: false\n',
     '          go-version: "1.25"\n          cache: false\n', LCK, MOD,
     "govulncheck grades the stdlib against the toolchain it runs under, so this pin is a "
     "separate claim from the test job's and had never been controlled at all"),

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

# ─── --check-anchors: the cheap half, and the ONLY half CI can afford to run ───────────────
#
# ⚠ WHY THIS EXISTS. W3's anchor was unique when it was written and stopped being unique the day
# ci.yaml grew a second `go-version` pin. The control then refused to fire, this campaign printed
# `6/7 controls CAUGHT`, and NOTHING NOTICED — because the only thing that reports an ANCHOR-FAILED
# is this script's own run, and this script is run by hand. A control that has gone inert looks
# exactly like a control that passed, to everyone except the person who happens to run it.
#
# The full campaign cannot go in CI: it mutates tracked files and runs the guard suite once per
# control. But the ANCHOR CHECK is pure string counting over three files — milliseconds, no
# mutation, no `go test` — and it is the part that rots. So CI runs this half on every push, and
# the day someone adds a third `go-version` pin the build says so instead of a hand-run saying 8/9.
#
# ⚠ THIS DOES NOT CLAIM THE CONTROLS STILL CATCH ANYTHING. It claims only that each one can still
# be APPLIED. A control whose anchor matches but whose mutation has become inert would pass here
# and prove nothing — that is what the full campaign is for, and why this mode is named for what
# it checks rather than for the campaign.
ANCHOR_FLOOR = 8  # controls whose anchors are checked. A loop over an empty list checks nothing.


def _test_sources():
    """Every Go test source in the module, as one string.

    ⚠ THE VACUITY RISK IS THE WHOLE REASON THIS IS A NAMED FUNCTION. If this walk ever returns
    nothing, every test name below verifies FOR FREE against a tree it never read, and the check
    reports a healthy campaign — the exact shape of the defect it exists to catch. The caller
    asserts `func Test` appears at all; control E4 blinds this walk and requires a red.
    """
    out = []
    for f in pathlib.Path(ROOT).rglob("*_test.go"):
        try:
            out.append(f.read_text(encoding="utf-8"))
        except OSError:
            pass
    return "\n".join(out)


def check_anchors(ci_mode):
    bad = []
    sources = _test_sources()
    if "func Test" not in sources:
        bad.append("FLOOR: no Go test sources were read, so every expected-test name below would "
                   "verify for free. The walk is broken, not the tree.")
    if len(CONTROLS) < ANCHOR_FLOOR:
        bad.append("FLOOR: only %d controls to check, floor is %d — a loop over a shrunken list "
                   "reports clean anchors rather than a missing campaign. If controls were "
                   "deleted, lower the floor in the same diff." % (len(CONTROLS), ANCHOR_FLOOR))
    for ctl in CONTROLS:
        name, path, old = ctl[0], ctl[1], ctl[2]
        expect = ctl[7] if len(ctl) > 7 else 1
        n = open(path).read().count(old)
        status = "ok" if n == expect else "STALE"
        print("  %-52s %s (%dx, want %d)" % (name, status, n, expect))
        if n != expect:
            bad.append(
                "%s: its anchor occurs %dx in %s, want %d — this control CANNOT RUN. It is not a "
                "failing guard, it is an absent one, and the campaign would report a score with it "
                "silently missing. Re-anchor it on something still unique; do NOT just raise the "
                "expected count, which mutates every copy at once and stops distinguishing them."
                % (name, n, os.path.basename(path), expect))
        # ⚠ THE OTHER END A CONTROL ROTS FROM. A campaign breaks in two independent ways: the
        # thing it MUTATES can drift, and the TEST IT EXPECTS TO BREAK can be renamed away.
        # talyvor-code #76 is the demonstration — w420's C4 expected a test deleted in the same
        # refactor that moved its anchor, and that rot stayed hidden behind the anchor one.
        #
        # ⚠⚠ AND HERE IT FAILS IN A DIRECTION THAT MISDIRECTS THE READER, WHICH IS WHY IT IS WORTH
        # A GUARD RATHER THAN A NOTE. The campaign runs its expected catcher as
        # `go test -run ^NAME$`. A name matching nothing runs ZERO tests and `go test` EXITS 0, so
        # the harness reads it as "the guard did not fire" and prints MISSED — sending whoever
        # reads it hunting for a hole in internal/toolchainguard that is not there.
        for t in [x for x in (ctl[4], ctl[5]) if x]:
            if ("func " + t + "(") not in sources:
                print("       expects %-56s NO SUCH TEST" % t)
                bad.append(
                    "%s names %s as a test to run and no `func %s(` exists in this module. The "
                    "campaign would run `go test -run ^%s$`, match nothing, exit 0, and report "
                    "MISSED — which reads as a broken GUARD rather than a broken CONTROL."
                    % (name, t, t, t))
    if bad:
        for b in bad:
            print(("::error::" if ci_mode else "") + b)
        return 1
    print("anchor check: all %d control anchors still apply and every expected test exists"
          % len(CONTROLS))
    return 0


if "--check-anchors" in sys.argv:
    sys.exit(check_anchors("--ci" in sys.argv))

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
