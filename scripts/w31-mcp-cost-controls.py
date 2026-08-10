#!/usr/bin/env python3
"""Positive controls for the MCP get_page cost guards (W3.1, finding 1).

Same protocol as scripts/w31-search-cost-controls.py: mutate the PRODUCT only, assert every anchor
count BEFORE any write, verify the bytes actually moved ON DISK, name a must-red target AND a
must-stay-green companion for every control, restore the tree to its pristine sha256 after each.

TWO PRODUCT FILES, not one. The curation census is a claim about the RELATIONSHIP between
model.Page and mcp.pageOut, so the mutation that earns it edits the model — and a harness that
snapshotted only server.go would have restored half the tree and scored every later control
against a dirty one.

THE COMPANION IS TestSEC4_MCP_ArgTrust_CrossTenant, deliberately. It drives get_page end to end
over the same real Postgres and is blind to cost, so every CAUGHT below also demonstrates that the
suite which was green through this defect's entire life stays green through the defect being
reintroduced.
"""
import hashlib
import os
import shutil
import subprocess
import sys
import tempfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
S = os.path.join(REPO, "internal", "mcp", "server.go")
M = os.path.join(REPO, "internal", "model", "model.go")
PRODUCT = [S, M]

COST_TEST = ("./internal/mcp/", "TestMCPGetPageReportsBothCostHalves_RealPG")
CURATION_TEST = ("./internal/mcp/", "TestMCPPageOutCuratesEveryPageCostField")
MUST_GREEN = ("./internal/mcp/", "TestSEC4_MCP_ArgTrust_CrossTenant")

FIELDS = '''	AICostUSD      float64 `json:"ai_cost_usd"`
	OwnAICostUSD   float64 `json:"own_ai_cost_usd"`
	TotalAICostUSD float64 `json:"total_ai_cost_usd"`'''

FILL = '''		AICostUSD:       p.AICostUSD,
		OwnAICostUSD:    p.OwnAICostUSD,
		TotalAICostUSD:  p.TotalAICostUSD,'''

# The model's derived-sum field, and the line the 4th-column controls hang off.
MODEL_TOTAL = '	TotalAICostUSD float64 `json:"total_ai_cost_usd" db:"-"`'

CONTROLS = [
    {
        "id": "E1",
        "what": "MAIN REPRODUCED: the Track half alone, exactly as shipped at 8b3e1be",
        "edits": [
            (S, FIELDS, '	AICostUSD      float64 `json:"ai_cost_usd"`', 1),
            (S, FILL, '		AICostUSD:       p.AICostUSD,', 1),
        ],
        "must_red": COST_TEST,
        # Both guards red here and that is correct — it IS the defect, seen from both sides.
        # Recorded rather than hidden: a control two guards catch justifies neither of them, so
        # E1 is not what earns either guard. E2/E3/E6 earn the wire guard, E4 earns the census.
        "also_red": CURATION_TEST,
    },
    {
        "id": "E2",
        "what": "own_ai_cost_usd filled from the WRONG column (the Track half)",
        "edits": [(S, '		OwnAICostUSD:    p.OwnAICostUSD,',
                   '		OwnAICostUSD:    p.AICostUSD,', 1)],
        # A mutation only the WIRE guard can see: the json tags do not move, so the census —
        # which compares NAMES — is blind to it by construction, and says so in its own header.
        "must_red": COST_TEST,
        "extra_green": CURATION_TEST,
    },
    {
        "id": "E3",
        "what": "the total forgets the document's own spend (total = the Track half)",
        "edits": [(S, '		TotalAICostUSD:  p.TotalAICostUSD,',
                   '		TotalAICostUSD:  p.AICostUSD,', 1)],
        "must_red": COST_TEST,
        "extra_green": CURATION_TEST,
    },
    {
        "id": "E4",
        "what": "a FOURTH cost column lands on the page and is not curated onto MCP",
        # THE MUTATION THAT EARNS THE CENSUS GUARD, and it is the defect's own history replayed:
        # this is what migration 0018 did when it split the cost column, and nothing was red.
        # The wire guard pins four pages by value and knows nothing about a column it was never
        # told about — it stays green, which is exactly why the census is not redundant with it.
        "edits": [(M, MODEL_TOTAL, MODEL_TOTAL +
                   '\n\tExternalAICostUSD float64 `json:"external_ai_cost_usd" db:"-"`', 1)],
        "must_red": CURATION_TEST,
        "extra_green": COST_TEST,
    },
    {
        "id": "E5",
        "what": "a NON-cost field lands on the page — the census must NOT condemn it",
        # ONE-DIRECTIONAL BY DESIGN. pageOut is a curated subset on purpose; the census is a claim
        # about COST fields, not a demand that MCP mirror the whole page. This is the inverse E4
        # is blind to: it is what proves costJSONNames' predicate DISCRIMINATES rather than
        # matching everything. With a constant-true predicate this control reds.
        "edits": [(M, MODEL_TOTAL, MODEL_TOTAL +
                   '\n\tNickname string `json:"nickname" db:"-"`', 1)],
        "must_red": CURATION_TEST,
        "expect": "NOT CAUGHT",
    },
    {
        "id": "E6",
        "what": "the total is the LARGER half, not the sum — right on every page with a zero half",
        # THE MUTATION THAT EARNS PAGE D. A/B/C alone cannot see it: max(own, track) equals the
        # sum whenever one half is zero, which is true of all three. Only the page carrying BOTH
        # halves non-zero can tell a sum from a saturating pick.
        "edits": [(S, '		TotalAICostUSD:  p.TotalAICostUSD,',
                   '		TotalAICostUSD:  max(p.AICostUSD, p.OwnAICostUSD),', 1)],
        "must_red": COST_TEST,
        "extra_green": CURATION_TEST,
    },
]


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def run_test(pkg, name):
    p = subprocess.run(["go", "test", "-count=1", "-run", "^" + name + "$", pkg],
                       cwd=REPO, capture_output=True, text=True)
    return p.returncode == 0, (p.stdout + p.stderr).strip()


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set — these controls need a real Postgres.")
        return 2

    tmp = tempfile.mkdtemp(prefix="w31c-pristine-")
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

    for pkg, name in [MUST_GREEN, COST_TEST, CURATION_TEST]:
        ok, out = run_test(pkg, name)
        if not ok:
            print("PRECONDITION FAILED: %s is not green before mutation.\n%s" % (name, out))
            return 2
    print("precondition: every target is green before any control ran\n")

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

        applied = True
        for path, _old, new, _w in c["edits"]:
            with open(path) as fh:
                if new not in fh.read():
                    applied = False
        touched = any(sha(p) != saved[p][1] for p in PRODUCT)
        if not applied or not touched:
            print("%s WRITE NOT APPLIED" % c["id"])
            restore()
            results.append((c["id"], "SUSPECT (write)", c["what"]))
            continue

        red_ok, red_out = run_test(*c["must_red"])
        green_ok, green_out = run_test(*MUST_GREEN)
        extra_ok, also_red_ok = True, None
        if c.get("extra_green"):
            extra_ok, _ = run_test(*c["extra_green"])
        if c.get("also_red"):
            also_red_ok, _ = run_test(*c["also_red"])

        failed = restore()
        if failed:
            print("%s RESTORE FAILED for %s — stop and inspect the tree" % (c["id"], failed))
            return 2

        if not green_ok:
            verdict = "SUSPECT (companion red — the mutation broke the build)"
            print("  %s companion:\n%s" % (c["id"], green_out[:500]))
        elif c.get("extra_green") and not extra_ok:
            verdict = "SUSPECT (a guard that must be blind to this mutation reddened)"
        else:
            got = "CAUGHT" if not red_ok else "NOT CAUGHT"
            verdict = got if got == expect else "%s (EXPECTED %s)" % (got, expect)
            if not red_ok:
                # Read the verdict by ASSERTION TEXT, not by exit code — a CAUGHT can name a test
                # that fataled before the assertion the control exists for was ever reached.
                first = [l for l in red_out.splitlines() if "_test.go:" in l]
                if first:
                    print("  %s red: %s" % (c["id"], first[0].strip()[:180]))
        if also_red_ok is not None:
            print("  %s also-red %s: %s" % (c["id"], c["also_red"][1],
                                            "RED" if not also_red_ok else "GREEN"))
        results.append((c["id"], verdict, c["what"]))

    print("\n" + "=" * 100)
    ok = True
    for cid, verdict, what in results:
        print("%-4s %-22s %s" % (cid, verdict, what))
        if "SUSPECT" in verdict or "EXPECTED" in verdict:
            ok = False
    print("=" * 100)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
