#!/usr/bin/env python3
"""Positive controls for the PageView AI-cost guards (W3.1, finding 4).

Same protocol as scripts/w31-mcp-cost-controls.py: mutate the PRODUCT only, assert every anchor
count BEFORE any write, verify the bytes moved ON DISK, name a must-red target AND a must-stay-green
companion, restore every touched file to its pristine sha256 after each control.

THE MUST-RED TARGET IS A SINGLE TEST CASE, NOT THE FILE. Five of the six cases in the cost guard
overlap heavily — a whole-file red would say "something noticed" and nothing about WHICH case, and
several controls below exist precisely to prove that ONE named case is the only thing standing
between the product and a specific wrong number.

THE COMPANION IS PageView.editsession.test.tsx: it renders the SAME component through the same
mocks and is blind to cost, so every CAUGHT also demonstrates that the file which was green through
this defect's whole life stays green through the defect being reintroduced.
"""
import hashlib
import os
import shutil
import subprocess
import sys
import tempfile

FE = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "frontend")
V = os.path.join(FE, "src", "pages", "PageView.tsx")
T = os.path.join(FE, "src", "api", "types.ts")
PRODUCT = [V, T]

COST = "src/pages/PageView.cost.test.tsx"
COMPANION = ("src/pages/PageView.editsession.test.tsx", None)

CASE_A = (COST, "A own-funded")
CASE_B = (COST, "B track-funded")
CASE_C = (COST, "C genuinely free")
CASE_D = (COST, "D both halves")
STALE = (COST, "pre-0018 cached page")
INFO = (COST, "Page info shows the total")

GATE = "{totalCost > 0 ? ("
HEAD = "✨ Total AI cost: ${totalCost.toFixed(2)}"
RESOLVE = "  const totalCost = page.total_ai_cost_usd ?? ownCost + trackCost;"

CONTROLS = [
    {
        "id": "F1",
        "what": "MAIN REPRODUCED: gate AND headline on the Track half, exactly as shipped",
        "edits": [(V, GATE, "{trackCost > 0 ? (", 1),
                  (V, HEAD, "✨ Total AI cost: ${trackCost.toFixed(2)}", 1)],
        "must_red": CASE_A,
    },
    {
        "id": "F2",
        "what": "the GATE alone reads the Track half — the inversion, headline left correct",
        # THE MUTATION ONLY CASE A CAN SEE. B and D both have a non-zero Track half, so the panel
        # still appears and their headlines are still right; C has no panel either way. A document
        # funded ENTIRELY by its own AI writing is the only one this hides, which is the whole
        # reason case A exists rather than being folded into B.
        "edits": [(V, GATE, "{trackCost > 0 ? (", 1)],
        "must_red": CASE_A,
        "extra_green": CASE_B,
    },
    {
        "id": "F3",
        "what": "the headline is the OWN half rather than the sum",
        "edits": [(V, HEAD, "✨ Total AI cost: ${ownCost.toFixed(2)}", 1)],
        "must_red": CASE_B,
    },
    {
        "id": "F4",
        "what": "the headline is the LARGER half, not the sum — right whenever a half is zero",
        # THE MUTATION ONLY CASE D CAN SEE, and what earns case D rather than assuming it: A, B and
        # C each have a zero half, where max() and + agree. Only the page carrying BOTH halves can
        # tell a sum from a saturating pick.
        "edits": [(V, HEAD, "✨ Total AI cost: ${Math.max(ownCost, trackCost).toFixed(2)}", 1)],
        "must_red": CASE_D,
        "extra_green": CASE_A,
    },
    {
        "id": "F5",
        "what": "the stale-cache fallback becomes a plain zero default",
        # THE MUTATION ONLY THE STALE CASE CAN SEE, and it is the naive form of this fix: `?? 0`
        # reads as defensive and silently hides the panel for every page served from a record
        # cached before these fields shipped. Every other case supplies both fields, so the
        # fallback never fires for them and they stay green.
        "edits": [(V, RESOLVE, "  const totalCost = page.total_ai_cost_usd ?? 0;", 1)],
        "must_red": STALE,
        "extra_green": CASE_D,
    },
    {
        "id": "F6",
        "what": "the gate is removed — a document that cost nothing grows an AI-cost panel",
        # ONE-DIRECTIONAL, AND WHAT EARNS CASE C. Without it, "always show the panel" satisfies
        # every other case here, so the repair for an inverted gate would be no gate at all.
        "edits": [(V, GATE, "{true ? (", 1)],
        "must_red": CASE_C,
        "extra_green": CASE_A,
    },
    {
        "id": "F7",
        "what": "own/total declared REQUIRED in the Page type — the crash this fix nearly shipped",
        # The type is the claim about what a Page can be. api/client.ts serves IndexedDB-cached
        # pages when fetch rejects, so a record without these fields is a real value; declaring
        # them required is what makes `.toFixed()` on them look safe. Paired with the resolve line
        # so the mutation is the whole mistake rather than half of it.
        "edits": [(T, "  own_ai_cost_usd?: number;\n  total_ai_cost_usd?: number;",
                   "  own_ai_cost_usd: number;\n  total_ai_cost_usd: number;", 1),
                  (V, RESOLVE, "  const totalCost = page.total_ai_cost_usd;", 1),
                  (V, "  const ownCost = page.own_ai_cost_usd ?? 0;",
                   "  const ownCost = page.own_ai_cost_usd;", 1)],
        "must_red": STALE,
        # ⚠ THE COMPANION GOES RED HERE TOO, AND THAT IS THE RESULT RATHER THAN A FAULT.
        # The companion check exists to separate "a guard noticed" from "the tree no longer
        # builds". This mutation BUILDS — `typecheck_must_pass` asserts tsc is clean under it, so
        # the reds are behavioural, not structural. What they show is that declaring these fields
        # required is not survivable ANYWHERE a Page can arrive without them: it takes down the
        # cost guard AND an unrelated edit-session file that never mentions cost. That breadth is
        # the finding, so it is asserted rather than scored as damage.
        "expect_companion_red": True,
        "typecheck_must_pass": True,
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


def main():
    tmp = tempfile.mkdtemp(prefix="w31d-pristine-")
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

    for tgt in [COMPANION, CASE_A, CASE_B, CASE_C, CASE_D, STALE, INFO]:
        ok, out = run(tgt)
        if ok is None:
            print("PRECONDITION FAILED: target %r matched no test — the filter is wrong." % (tgt,))
            return 2
        if not ok:
            print("PRECONDITION FAILED: %r is not green before mutation.\n%s" % (tgt, out[-2000:]))
            return 2
    print("precondition: every target matched a real test and is green before any control ran\n")

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

        # Re-read per edit so two edits to one file compose instead of the second erasing the first.
        for path, old, new, _ in c["edits"]:
            with open(path) as fh:
                src = fh.read()
            with open(path, "w") as fh:
                fh.write(src.replace(old, new))

        applied = all(
            new in open(path).read() for path, _o, new, _w in c["edits"]
        ) and any(sha(p) != saved[p][1] for p in PRODUCT)
        if not applied:
            print("%s WRITE NOT APPLIED" % c["id"])
            restore()
            results.append((c["id"], "SUSPECT (write)", c["what"]))
            continue

        red_ok, red_out = run(c["must_red"])
        green_ok, green_out = run(COMPANION)
        extra_ok, tc_ok = True, True
        if c.get("extra_green"):
            extra_ok, _ = run(c["extra_green"])
        if c.get("typecheck_must_pass"):
            tc_ok = subprocess.run(["npm", "run", "typecheck"], cwd=FE,
                                   capture_output=True, text=True).returncode == 0

        failed = restore()
        if failed:
            print("%s RESTORE FAILED for %s — stop and inspect the tree" % (c["id"], failed))
            return 2

        if red_ok is None:
            verdict = "SUSPECT (must-red target matched no test)"
        elif c.get("typecheck_must_pass") and not tc_ok:
            verdict = "SUSPECT (tree does not typecheck — a build break, not a behaviour)"
        elif c.get("expect_companion_red"):
            if green_ok:
                verdict = "SUSPECT (companion stayed GREEN — the mutation was expected to reach it)"
            elif red_ok:
                verdict = "NOT CAUGHT (EXPECTED CAUGHT-EVERYWHERE)"
            else:
                verdict = "CAUGHT-EVERYWHERE"
                print("  %s: tsc clean under the mutation, so both reds are behavioural" % c["id"])
        elif not green_ok:
            verdict = "SUSPECT (companion red — the mutation broke the render)"
            print("  %s companion:\n%s" % (c["id"], green_out[-600:]))
        elif c.get("extra_green") and not extra_ok:
            verdict = "SUSPECT (a case that must be blind to this mutation reddened)"
        else:
            got = "CAUGHT" if not red_ok else "NOT CAUGHT"
            verdict = got if got == expect else "%s (EXPECTED %s)" % (got, expect)
            if not red_ok:
                # Read the verdict by ASSERTION TEXT, not exit code — a CAUGHT can name a case that
                # threw before the assertion the control exists for was ever reached.
                hit = [l for l in red_out.splitlines()
                       if "AssertionError" in l or "Unable to find" in l or "TypeError" in l]
                if hit:
                    print("  %s red: %s" % (c["id"], hit[0].strip()[:180]))
        results.append((c["id"], verdict, c["what"]))

    print("\n" + "=" * 100)
    ok = True
    for cid, verdict, what in results:
        print("%-4s %-24s %s" % (cid, verdict, what))
        if "SUSPECT" in verdict or "EXPECTED" in verdict:
            ok = False
    print("=" * 100)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
