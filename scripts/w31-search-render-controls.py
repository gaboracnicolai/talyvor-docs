#!/usr/bin/env python3
"""Positive controls for the search-list AI-cost guards (W3.1, finding 5).

Same protocol as scripts/w31-spa-cost-controls.py: mutate the PRODUCT only, assert every anchor
count BEFORE any write, verify the bytes moved ON DISK, name a must-red target AND a
must-stay-green companion, restore every touched file to its pristine sha256 after each control.

THE MUST-RED TARGET IS A SINGLE TEST CASE, NOT A FILE. Most of the cases in the render guard
overlap; a whole-file red would say "something noticed" and nothing about WHICH case. Several
controls below exist precisely to prove that ONE named case is the only thing standing between
the product and a specific wrong number on screen.

THE COMPANION IS src/router/layout-smoke.test.tsx. It mounts the REAL Layout — Sidebar, Header
and this very SearchModal — under the router, and is completely blind to cost. So every CAUGHT
also demonstrates that the tree still builds and still renders, i.e. that the red is a guard
noticing a behaviour rather than the component failing to mount.

⚠ EVERY CONTROL ALSO REQUIRES tsc TO STAY CLEAN. `noUnusedLocals` is on in this tsconfig, so a
mutation that orphans a local turns a behavioural control into a compile error, and `#53`
recorded exactly that trap: a lint failure read as CI catching the blinding when it was catching
the mutation's own litter. A control that breaks the build is not a control, in either direction.

⚠ TWO PRODUCT LANGUAGES. The census guard's subject is the AGREEMENT between a Go struct and a
TS interface, so its controls mutate internal/search/handler.go. That file is in PRODUCT and is
restored by sha like the rest.
"""
import hashlib
import os
import shutil
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FE = os.path.join(ROOT, "frontend")
SM = os.path.join(FE, "src", "components", "SearchModal.tsx")
TS = os.path.join(FE, "src", "api", "search.ts")
GO = os.path.join(ROOT, "internal", "search", "handler.go")
PRODUCT = [SM, TS, GO]

RENDER = "src/components/SearchModal.cost.test.tsx"
CENSUS = "src/api/search.wire-census.test.ts"
COMPANION = ("src/router/layout-smoke.test.tsx", None)

CASE_A = (RENDER, "A own-funded")
CASE_B = (RENDER, "B track-funded")
CASE_C = (RENDER, "C genuinely free")
CASE_D = (RENDER, "D both halves")
CASE_E = (RENDER, "E semantic-only")
CASE_PAIR = (RENDER, "C and E do not render the same bytes")
CEN_FLOOR = (CENSUS, "the parsers found a real struct")
CEN_MISSING = (CENSUS, "no field the wire emits is missing")
CEN_PHANTOM = (CENSUS, "no field the SPA type declares has vanished")
CEN_COST = (CENSUS, "all three cost fields are on both sides")

RESOLVE = "  const total = r.total_ai_cost_usd;"
ABSENT = "  if (total === undefined || total === null) {"
GATE = "  if (total <= 0) return null;"
NUMBER = "<Sparkles size={9} />${total.toFixed(2)}"
# The head of the not-reported branch, used where a control has to replace the branch rather
# than just its condition.
ABSENT_HEAD = (
    '  if (total === undefined || total === null) {\n'
    '    return (\n'
    '      <span\n'
    '        data-testid="search-cost-unknown"'
)

CONTROLS = [
    {
        "id": "G1",
        "what": "MAIN REPRODUCED: gate AND number on the Track half, as shipped",
        # Reproduces the measured behaviour of the tree at 8613c89 exactly: A no badge,
        # B $1.50, C nothing, D $1.50, E nothing.
        "edits": [(SM, RESOLVE + "\n" + ABSENT,
                   "  const total = r.ai_cost_usd;\n"
                   "  if (total === undefined || total === null) {\n"
                   "    return null;\n"
                   "  }\n"
                   "  if (false) {", 1)],
        "must_red": CASE_A,
    },
    {
        "id": "G2",
        "what": "the GATE alone reads the Track half — the inversion, number left correct",
        # THE MUTATION ONLY CASE A CAN SEE. B and D both have a non-zero Track half so the badge
        # still appears with the right total; C is hidden either way; E never reaches the gate.
        # A document funded ENTIRELY by its own AI writing is the only one this hides, which is
        # why case A exists rather than being folded into B.
        "edits": [(SM, GATE, "  if ((r.ai_cost_usd ?? 0) <= 0) return null;", 1)],
        "must_red": CASE_A,
        "extra_green": [CASE_D, CASE_E],
    },
    {
        "id": "G3",
        "what": "the number is the OWN half rather than the sum",
        "edits": [(SM, NUMBER, "<Sparkles size={9} />${(own ?? 0).toFixed(2)}", 1)],
        "must_red": CASE_B,
        "extra_green": [CASE_A],
    },
    {
        "id": "G4",
        "what": "the number is the LARGER half, not the sum — right whenever a half is zero",
        # THE MUTATION ONLY CASE D CAN SEE, and what earns case D rather than assuming it: A, B
        # and C each have a zero half, where max() and + agree. Only the row carrying BOTH halves
        # can tell a sum from a saturating pick.
        "edits": [(SM, NUMBER,
                   "<Sparkles size={9} />${Math.max(own ?? 0, track ?? 0).toFixed(2)}", 1)],
        "must_red": CASE_D,
        "extra_green": [CASE_A, CASE_B],
    },
    {
        "id": "G5",
        "what": "NOT REPORTED renders as nothing — the rejected alternative, made a control",
        # THE MUTATION ONLY CASE E CAN SEE, and it is not a strawman: "show nothing" is the
        # measured status quo and one of the two options the queue left open for this state.
        # Under it, a semantic-only row (no pages row was read, so no cost is KNOWN) renders
        # byte-identically to a document measured at zero. This control is what makes the
        # decision in SearchModal.tsx a claim the suite enforces rather than a comment.
        "edits": [(SM, ABSENT_HEAD,
                   "  if (total === undefined || total === null) {\n"
                   "    return null;\n"
                   "  }\n"
                   "  if (false) {\n"
                   "    return (\n"
                   "      <span\n"
                   '        data-testid="search-cost-unknown"', 1)],
        "must_red": CASE_E,
        "extra_green": [CASE_C, CASE_D],
    },
    {
        "id": "G6",
        "what": "the fabricated zero: NOT REPORTED renders $0.00",
        # The failure a renderer written by analogy to PageView produces — `?? 0` reads as
        # defensive and prints a currency amount for a document NOBODY LOOKED AT. Case C reds
        # here too (a free document also grows $0.00); that breadth is the point, so it is
        # recorded rather than scored against the control.
        "edits": [(SM, RESOLVE + "\n" + ABSENT_HEAD.split("\n", 1)[0],
                   "  const total = r.total_ai_cost_usd ?? 0;\n  if (false) {", 1),
                  (SM, GATE, "  if (total < 0) return null;", 1)],
        "must_red": CASE_E,
    },
    {
        "id": "G7",
        "what": "the gate is removed — a document that cost nothing grows a number",
        # ONE-DIRECTIONAL, AND WHAT EARNS CASE C. Without it, "always render the badge"
        # satisfies every other render case here, so the repair for an inverted gate would be
        # no gate at all.
        "edits": [(SM, GATE, "  if (total < 0) return null;", 1)],
        "must_red": CASE_C,
        "extra_green": [CASE_A, CASE_E],
    },
    {
        "id": "G8",
        "what": "a FOURTH cost field lands on the wire and misses the SPA type",
        # THE CLASS, REPLAYED. This is migration 0018's own history: the Go side grew a field,
        # the TS type did not, and the value arrived and was dropped at the type boundary with
        # nothing red. The render guard is BLIND to it — which is what earns the census as a
        # second guard rather than a restatement of the first.
        "edits": [(GO, '\tTotalAICostUSD *float64 `json:"total_ai_cost_usd,omitempty"`',
                   '\tTotalAICostUSD *float64 `json:"total_ai_cost_usd,omitempty"`\n'
                   '\tRefundAICostUSD *float64 `json:"refund_ai_cost_usd,omitempty"`', 1)],
        "must_red": CEN_MISSING,
        "extra_green": [CASE_D, CEN_PHANTOM],
    },
    {
        "id": "G9",
        "what": "the wire RENAMES a field the SPA still declares — the other direction",
        # WHAT EARNS SET EQUALITY OVER A SUBSET CHECK. `TS ⊇ GO` passes this: every Go tag
        # would still have a TS key. Only the phantom direction sees a client reading a field
        # the server stopped sending.
        "edits": [(GO, '`json:"own_ai_cost_usd,omitempty"`', '`json:"own_ai_cost,omitempty"`', 1)],
        "must_red": CEN_PHANTOM,
        "extra_green": [CASE_D],
    },
    {
        "id": "G10",
        "what": "the census parser stops finding the Go struct — the instrument's own floor",
        # INVERT THE PREDICATE RATHER THAN PLANT A FIXTURE. Both sides of this census are
        # derived by parsing source, so its real failure mode is a regex that quietly matches
        # nothing: two empty sets compare equal and the run is green over a question never
        # asked. Two spaces after `type` is legal Go that compiles and breaks the match.
        "edits": [(GO, "type Result struct {", "type  Result struct {", 1)],
        "must_red": CEN_FLOOR,
        "extra_green": [CASE_D],
    },
]


def sha(p):
    with open(p, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def run(target):
    path, name = target
    cmd = ["npx", "vitest", "run", path]
    if name:
        cmd += ["-t", name]
    p = subprocess.run(cmd, cwd=FE, capture_output=True, text=True)
    out = p.stdout + p.stderr
    # A name filter that matches NOTHING exits 0 with "no tests" — that would score as green and
    # quietly turn a must-red target into a target that never ran. Treat it as a harness fault.
    if name and ("No test found" in out or "no tests" in out.lower()):
        return None, out
    return p.returncode == 0, out.strip()


def typecheck():
    return subprocess.run(["npm", "run", "typecheck"], cwd=FE,
                          capture_output=True, text=True).returncode == 0


def main():
    tmp = tempfile.mkdtemp(prefix="w31-searchrender-pristine-")
    saved = {}
    for p in PRODUCT:
        dst = os.path.join(tmp, os.path.basename(p))
        shutil.copyfile(p, dst)
        saved[p] = (dst, sha(p))

    def restore():
        for p, (dst, want) in saved.items():
            shutil.copyfile(dst, p)
            if sha(p) != want:
                return p
        return None

    targets = [COMPANION, CASE_A, CASE_B, CASE_C, CASE_D, CASE_E, CASE_PAIR,
               CEN_FLOOR, CEN_MISSING, CEN_PHANTOM, CEN_COST]
    for tgt in targets:
        ok, out = run(tgt)
        if ok is None:
            print("PRECONDITION FAILED: target %r matched no test — the filter is wrong." % (tgt,))
            return 2
        if not ok:
            print("PRECONDITION FAILED: %r is not green before mutation.\n%s" % (tgt, out[-2000:]))
            return 2
    if not typecheck():
        print("PRECONDITION FAILED: the pristine tree does not typecheck.")
        return 2
    print("precondition: %d targets matched a real test, all green, tsc clean, before any "
          "control ran\n" % len(targets))

    results = []
    for c in CONTROLS:
        expect = c.get("expect", "CAUGHT")
        bad = False
        for path, old, _new, want in c["edits"]:
            with open(path) as fh:
                n = fh.read().count(old)
            if n != want:
                print("%s ANCHOR MISS in %s: %d occurrences, want %d"
                      % (c["id"], os.path.basename(path), n, want))
                bad = True
        if bad:
            results.append((c["id"], "SUSPECT (anchor)", c["what"]))
            continue

        # Re-read per edit so two edits to one file compose instead of the second erasing the
        # first — a harness in this fleet shipped that bug and reported a working guard blind.
        for path, old, new, _ in c["edits"]:
            with open(path) as fh:
                src = fh.read()
            with open(path, "w") as fh:
                fh.write(src.replace(old, new))

        applied = all(new in open(path).read() for path, _o, new, _w in c["edits"]) and any(
            sha(p) != saved[p][1] for p in PRODUCT)
        if not applied:
            print("%s WRITE NOT APPLIED" % c["id"])
            restore()
            results.append((c["id"], "SUSPECT (write)", c["what"]))
            continue

        red_ok, red_out = run(c["must_red"])
        green_ok, green_out = run(COMPANION)
        tc_ok = typecheck()
        extra_bad = None
        for eg in c.get("extra_green", []):
            eok, _ = run(eg)
            if not eok:
                extra_bad = eg
                break

        failed = restore()
        if failed:
            print("%s RESTORE FAILED for %s — stop and inspect the tree" % (c["id"], failed))
            return 2

        if red_ok is None:
            verdict = "SUSPECT (must-red target matched no test)"
        elif not tc_ok:
            verdict = "SUSPECT (tree does not typecheck — a build break, not a behaviour)"
        elif not green_ok:
            verdict = "SUSPECT (companion red — the mutation broke the mount)"
            print("  %s companion:\n%s" % (c["id"], green_out[-600:]))
        elif extra_bad:
            verdict = "SUSPECT (a case that must be blind to this mutation reddened: %s)" % (
                extra_bad[1],)
        else:
            got = "CAUGHT" if not red_ok else "NOT CAUGHT"
            verdict = got if got == expect else "%s (EXPECTED %s)" % (got, expect)
            if not red_ok:
                # Read the verdict by ASSERTION TEXT, not exit code — a CAUGHT can name a case
                # that threw before the assertion the control exists for was ever reached.
                hit = [l for l in red_out.splitlines()
                       if "AssertionError" in l or "Unable to find" in l or "TypeError" in l
                       or "Error:" in l]
                if hit:
                    print("  %s red: %s" % (c["id"], hit[0].strip()[:200]))
        results.append((c["id"], verdict, c["what"]))

    print("\n" + "=" * 104)
    ok = True
    for cid, verdict, what in results:
        print("%-5s %-26s %s" % (cid, verdict, what))
        if "SUSPECT" in verdict or "EXPECTED" in verdict:
            ok = False
    print("=" * 104)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
