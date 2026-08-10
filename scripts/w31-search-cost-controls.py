#!/usr/bin/env python3
"""Positive-control harness for the search cost-projection fix (W3.1, second merge).

Same protocol as scripts/w31-searchrank-controls.py: mutate the PRODUCT only, assert every anchor
count BEFORE any write, verify the bytes moved on disk, name a must-red target AND a must-stay-green
companion, restore to the pristine sha256.

⚠ ONE OF THE TWO GUARDS PASSED ON THE UNFIXED TREE and is therefore the one that most needs a
control. TestSearch_SemanticOnlyRow_ReportsNoCostRatherThanAFabricatedZero was green at `ad20b1c`
for the wrong reason — the fields did not exist at all, so of course none was emitted. Its job is to
constrain the FIX rather than to detect the defect: it is what stops "drop omitempty" from being an
acceptable answer, because that would make a page nobody read report a cost of zero. D3 and D5 are
the controls that earn it.

Usage:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-search-cost-controls.py
"""

import hashlib
import os
import shutil
import subprocess
import sys
import tempfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
H = os.path.join(REPO, "internal/search/handler.go")

MUST_GREEN = ("./internal/search/", "TestHandler_FullTextOnly_NoLens")
COST_TEST = ("./internal/search/", "TestSearch_RealPG_ReportsBothHalvesOfAPagesCost")
FABRICATED_ZERO_TEST = ("./internal/search/", "TestSearch_SemanticOnlyRow_ReportsNoCostRatherThanAFabricatedZero")

FIELDS = '''	AICostUSD      *float64 `json:"ai_cost_usd,omitempty"`
	OwnAICostUSD   *float64 `json:"own_ai_cost_usd,omitempty"`
	TotalAICostUSD *float64 `json:"total_ai_cost_usd,omitempty"`'''

FILL = '''				AICostUSD:      ptr(f.Page.AICostUSD),
				OwnAICostUSD:   ptr(f.Page.OwnAICostUSD),
				TotalAICostUSD: ptr(f.Page.TotalAICostUSD),'''

BARE_FIELDS = '''	AICostUSD      float64 `json:"ai_cost_usd,omitempty"`
	OwnAICostUSD   float64 `json:"own_ai_cost_usd,omitempty"`
	TotalAICostUSD float64 `json:"total_ai_cost_usd,omitempty"`'''

BARE_FILL = '''				AICostUSD:      f.Page.AICostUSD,
				OwnAICostUSD:   f.Page.OwnAICostUSD,
				TotalAICostUSD: f.Page.TotalAICostUSD,'''

CONTROLS = [
    {
        "id": "D1",
        "what": "MAIN REPRODUCED: the Track half alone, on a bare float with omitempty",
        "edits": [
            (H, FIELDS, '	AICostUSD *float64 `json:"ai_cost_usd,omitempty"`', 1),
            (H, FILL, '				AICostUSD: ptr(f.Page.AICostUSD),', 1),
        ],
        "must_red": COST_TEST,
    },
    {
        "id": "D2",
        "what": "own_ai_cost_usd filled from the WRONG column (the Track half)",
        "edits": [(H, '				OwnAICostUSD:   ptr(f.Page.OwnAICostUSD),',
                   '				OwnAICostUSD:   ptr(f.Page.AICostUSD),', 1)],
        "must_red": COST_TEST,
    },
    {
        "id": "D3",
        "what": "bare floats instead of pointers — 'drop omitempty and hope'",
        "edits": [(H, FIELDS, BARE_FIELDS, 1), (H, FILL, BARE_FILL, 1)],
        # ⚠ MY FIRST TWO GUESSES AT THIS ROW WERE BOTH WRONG AND THE HARNESS REFUSED BOTH.
        # I first named FABRICATED_ZERO_TEST as the catcher with COST_TEST as a must-stay-green.
        # Neither half held. Bare floats KEEP omitempty, so a semantic-only row still omits its
        # costs by accident and the fabricated-zero guard stays green — it is blind to D3. What
        # actually catches D3 is the real-Postgres test, via the page that GENUINELY cost nothing
        # and must therefore SAY zero rather than vanish.
        # So each guard has exactly one mutation only it can see: D3 for the real-PG cost test,
        # D5 for the fabricated-zero test. That is why both exist, and it is measured here rather
        # than asserted in a comment.
        "must_red": COST_TEST,
        "extra_green": FABRICATED_ZERO_TEST,
    },
    {
        "id": "D4",
        "what": "the total forgets the document's own spend (total = the Track half)",
        "edits": [(H, '				TotalAICostUSD: ptr(f.Page.TotalAICostUSD),',
                   '				TotalAICostUSD: ptr(f.Page.AICostUSD),', 1)],
        "must_red": COST_TEST,
    },
    {
        "id": "D5",
        "what": "pointers kept but omitempty dropped — nulls for rows nobody read",
        "edits": [(H, FIELDS, '''	AICostUSD      *float64 `json:"ai_cost_usd"`
	OwnAICostUSD   *float64 `json:"own_ai_cost_usd"`
	TotalAICostUSD *float64 `json:"total_ai_cost_usd"`''', 1)],
        "must_red": FABRICATED_ZERO_TEST,
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

    tmp = tempfile.mkdtemp(prefix="w31b-pristine-")
    dst = os.path.join(tmp, "handler.go")
    shutil.copyfile(H, dst)
    pristine_sha = sha(H)

    for pkg, name in [MUST_GREEN, COST_TEST, FABRICATED_ZERO_TEST]:
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
                print("%s ANCHOR MISS: %d occurrences, want %d" % (c["id"], n, want))
                bad = True
        if bad:
            results.append((c["id"], "SUSPECT (anchor)", c["what"]))
            continue

        for path, old, new, _ in c["edits"]:
            with open(path) as fh:
                src = fh.read()
            with open(path, "w") as fh:
                fh.write(src.replace(old, new))
        with open(H) as fh:
            after = fh.read()
        if sha(H) == pristine_sha or not all(new in after for _p, _o, new, _w in c["edits"]):
            print("%s WRITE NOT APPLIED" % c["id"])
            shutil.copyfile(dst, H)
            results.append((c["id"], "SUSPECT (write)", c["what"]))
            continue

        red_ok, red_out = run_test(*c["must_red"])
        green_ok, green_out = run_test(*MUST_GREEN)
        extra_ok = True
        if c.get("extra_green"):
            extra_ok, _ = run_test(*c["extra_green"])

        shutil.copyfile(dst, H)
        if sha(H) != pristine_sha:
            print("%s RESTORE FAILED — stop and inspect the tree" % c["id"])
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
                first = [l for l in red_out.splitlines() if ".go:" in l]
                if first:
                    print("  %s red: %s" % (c["id"], first[0].strip()[:170]))
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
