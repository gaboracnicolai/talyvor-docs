#!/usr/bin/env python3
"""Positive-control harness for the SearchWithRank alias-prefix fix (W3.1).

A guard that passes on the first run proves nothing until something has been shown to make it
fail. Each control below mutates the PRODUCT (internal/page/store.go), never a test, and each
declares two targets:

  must-red    the guard that is supposed to notice. FAIL here == the guard works.
  must-green  a test in the SAME package that must keep passing. It is the compile check: a
              mutation that does not build reds everything, and a red must-green turns the
              control into SUSPECT rather than evidence.

The must-green companion is deliberately `TestSearchWithRank_ReturnsRankedResultsWithHeadline` —
the pgxmock test that was green throughout the entire life of this defect. Every control that
scores CAUGHT is therefore also a demonstration that the mock could not see it.

Protocol per control, in this order:
  1. assert every anchor occurs EXACTLY the expected number of times, BEFORE any write
  2. write, then verify on disk that the file's sha256 changed and the replacement is present
  3. run must-red, run must-green
  4. restore from the pristine byte copy and assert the sha256 matches the pristine one

Usage:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-searchrank-controls.py
"""

import hashlib
import os
import shutil
import subprocess
import sys
import tempfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = os.path.join(REPO, "internal/page/store.go")

MUST_GREEN = ("./internal/page/", "TestSearchWithRank_ReturnsRankedResultsWithHeadline")

# The fixed list, as it stands on the branch. Anchors are taken from it.
ANCHOR_CREATED_AT = 'p + "created_at", p + "updated_at",'
ANCHOR_PAGETYPE = '"COALESCE(" + p + "page_type, \'document\') AS page_type",'
ANCHOR_OWNCOST = 'p + "linked_issues", p + "ai_cost_usd", p + "own_ai_cost_usd",'
ANCHOR_TITLE = 'p + "id", p + "space_id", p + "workspace_id", p + "parent_id", p + "title", p + "slug",'
ANCHOR_PREFIXFN = """func prefixedColumns(alias string) string {
	return strings.Join(columnExprs(alias), ", ")
}"""

# C1 reproduces main exactly: the old splitter over a multi-line unaliased string.
C1_OLD_SPLITTER = """func prefixedColumns(alias string) string {
	parts := strings.Split(mainShapedColumns, ", ")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ", ")
}

// mainShapedColumns is `columns` written the way main 34ab2d5 wrote it: a multi-line raw string.
const mainShapedColumns = `id, space_id, workspace_id, parent_id, title, slug,
    content, content_text, icon, cover_url,
    position, depth, is_template, created_by, updated_by,
    linked_issues, ai_cost_usd, own_ai_cost_usd,
    view_count, last_viewed_at,
    last_verified_at, verified_by, stale_after_days,
    doc_status,
    locked, locked_by, locked_at,
    COALESCE(page_type, 'document') AS page_type,
    created_at, updated_at`"""

# C5 must break the ALIASED form only — see the note on the control itself.
C5_ALIASED_ONLY_DROP = """func prefixedColumns(alias string) string {
	e := columnExprs(alias)
	for i, x := range e {
		if strings.HasSuffix(x, "own_ai_cost_usd") {
			e[i] = "0::double precision AS own_ai_cost_usd"
		}
	}
	return strings.Join(e, ", ")
}"""

CONTROLS = [
    {
        "id": "C1",
        "what": "MAIN REPRODUCED EXACTLY — the old ', ' splitter over the old multi-line string",
        "edits": [(STORE, ANCHOR_PREFIXFN, C1_OLD_SPLITTER, 1)],
        "must_red": ("./internal/page/", "TestSearchWithRank_RealPG_CompilesAndReturnsTheRow"),
    },
    {
        "id": "C2",
        "what": "the misplaced qualifier alone: COALESCE(page_type, p.'document') — SQLSTATE 42601",
        "edits": [(STORE, ANCHOR_PAGETYPE,
                   '"COALESCE(" + p + "page_type, " + p + "\'document\') AS page_type",', 1)],
        "must_red": ("./internal/page/", "TestSearchWithRank_RealPG_CompilesAndReturnsTheRow"),
    },
    {
        "id": "C3",
        "what": "created_at unqualified alone — ambiguous against the JOINed spaces table (42702)",
        "edits": [(STORE, ANCHOR_CREATED_AT, '"created_at", p + "updated_at",', 1)],
        "must_red": ("./internal/page/", "TestSearchWithRank_RealPG_CompilesAndReturnsTheRow"),
    },
    {
        "id": "C4",
        "what": "the HTTP consumer: same misplaced qualifier, judged at GET /search",
        "edits": [(STORE, ANCHOR_PAGETYPE,
                   '"COALESCE(" + p + "page_type, " + p + "\'document\') AS page_type",', 1)],
        "must_red": ("./internal/search/", "TestSearch_RealPG_ReturnsTheMatchingPage"),
    },
    {
        "id": "C5",
        "what": "a column silently dropped from the ALIASED form only (own_ai_cost_usd -> 0)",
        # ⚠ THE FIRST VERSION OF THIS CONTROL EDITED REAL BYTES AND PROVED NOTHING. It rewrote the
        # entry inside columnExprs, which BOTH forms read, so the unaliased reader lost the column
        # too, the two sides agreed at 0, and the guard was scored NOT CAUGHT for being right. The
        # mutation has to be conditional on the alias to create the divergence it claims to create.
        "edits": [(STORE, ANCHOR_PREFIXFN, C5_ALIASED_ONLY_DROP, 1)],
        "must_red": ("./internal/page/", "TestSearchWithRank_RealPG_ProjectionMatchesTheUnaliasedReader"),
        # This is the control that justifies the second test existing at all: the first test never
        # looks at cost, so it must stay GREEN here.
        "extra_green": ("./internal/page/", "TestSearchWithRank_RealPG_CompilesAndReturnsTheRow"),
    },
    {
        "id": "C6",
        "what": "ONE-DIRECTIONAL BY DESIGN: title unqualified — legal SQL, so nothing may red",
        "edits": [(STORE, ANCHOR_TITLE,
                   'p + "id", p + "space_id", p + "workspace_id", p + "parent_id", "title", p + "slug",', 1)],
        "must_red": ("./internal/page/", "TestSearchWithRank_RealPG_CompilesAndReturnsTheRow"),
        "expect": "NOT CAUGHT",
    },
    {
        "id": "C7",
        "what": "ONE-DIRECTIONAL BY DESIGN: the COALESCE default changed to 'wiki'",
        "edits": [(STORE, ANCHOR_PAGETYPE,
                   '"COALESCE(" + p + "page_type, \'wiki\') AS page_type",', 1)],
        "must_red": ("./internal/page/", "TestSearchWithRank_RealPG_ProjectionMatchesTheUnaliasedReader"),
        "expect": "NOT CAUGHT",
    },
]


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def run_test(pkg, name):
    """True == the test PASSED."""
    p = subprocess.run(
        ["go", "test", "-count=1", "-run", "^" + name + "$", pkg],
        cwd=REPO, capture_output=True, text=True,
    )
    return p.returncode == 0, (p.stdout + p.stderr).strip()


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set — these controls need a real Postgres.")
        return 2

    pristine_dir = tempfile.mkdtemp(prefix="w31-pristine-")
    files = {STORE}
    pristine = {}
    for f in files:
        dst = os.path.join(pristine_dir, os.path.basename(f))
        shutil.copyfile(f, dst)
        pristine[f] = (dst, sha(f))

    # The tree must be GREEN before any control runs, or a red below means nothing.
    for pkg, name in [MUST_GREEN, CONTROLS[0]["must_red"], CONTROLS[3]["must_red"],
                      CONTROLS[4]["must_red"]]:
        ok, out = run_test(pkg, name)
        if not ok:
            print("PRECONDITION FAILED: %s in %s is not green before mutation.\n%s" % (name, pkg, out))
            return 2
    print("precondition: the tree is green on every target before any control ran\n")

    results = []
    for c in CONTROLS:
        expect = c.get("expect", "CAUGHT")

        # 1. assert every anchor count BEFORE any write
        bad = False
        for path, old, _new, want in c["edits"]:
            with open(path) as fh:
                n = fh.read().count(old)
            if n != want:
                print("%s ANCHOR MISS: %d occurrences of anchor in %s, want %d"
                      % (c["id"], n, os.path.basename(path), want))
                bad = True
        if bad:
            results.append((c["id"], "SUSPECT (anchor)", c["what"]))
            continue

        # 2. write, then prove on disk that the bytes moved
        for path, old, new, _ in c["edits"]:
            with open(path) as fh:
                src = fh.read()
            with open(path, "w") as fh:
                fh.write(src.replace(old, new))
        moved = all(sha(p) != pristine[p][1] for p, *_ in c["edits"])
        present = True
        for path, _old, new, _ in c["edits"]:
            with open(path) as fh:
                present = present and (new in fh.read())
        if not (moved and present):
            print("%s WRITE NOT APPLIED (moved=%s present=%s)" % (c["id"], moved, present))
            for path, *_ in c["edits"]:
                shutil.copyfile(pristine[path][0], path)
            results.append((c["id"], "SUSPECT (write)", c["what"]))
            continue

        # 3. must-red, then must-green
        red_ok, red_out = run_test(*c["must_red"])
        green_ok, green_out = run_test(*MUST_GREEN)
        extra_ok = True
        if c.get("extra_green"):
            extra_ok, extra_out = run_test(*c["extra_green"])

        # 4. restore and prove the restore
        for path, *_ in c["edits"]:
            shutil.copyfile(pristine[path][0], path)
        restored = all(sha(p) == pristine[p][1] for p, *_ in c["edits"])
        if not restored:
            print("%s RESTORE FAILED — stop and inspect the tree" % c["id"])
            return 2

        if not green_ok:
            verdict = "SUSPECT (must-green companion went red — the mutation broke the build)"
            print("  %s companion output:\n%s" % (c["id"], green_out[:600]))
        elif c.get("extra_green") and not extra_ok:
            verdict = "SUSPECT (a test that must be blind to this mutation reddened)"
        else:
            caught = not red_ok
            got = "CAUGHT" if caught else "NOT CAUGHT"
            verdict = got if got == expect else "%s (EXPECTED %s)" % (got, expect)
            if caught:
                first = [l for l in red_out.splitlines() if ".go:" in l]
                if first:
                    print("  %s red message: %s" % (c["id"], first[0].strip()[:180]))
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
