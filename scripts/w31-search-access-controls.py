#!/usr/bin/env python3
"""Positive controls for the search access gate (W3.1, tab-4a71).

Each control mutates PRODUCT code, runs a named subset of the suite, and is scored against a
catcher PREDICTED BEFORE THE RUN. A control is only useful if it names which guard should speak:
"something went red" is compatible with a compile error, a panic, or an unrelated test.

WHAT THIS HARNESS LEARNED FROM ITS PREDECESSORS IN THIS REPO:

  · RESTORE IN A finally, NOT AFTER THE RUN. A crash between mutate and restore leaves mutated
    product code on disk. Every file is restored from an in-memory copy in a finally, and the
    closing sha256 comparison is what PROVES the tree came back.
  · A COMPILE ERROR IS NOT A CATCH. `go test` exits non-zero for a build failure exactly as it
    does for a failed assertion, and an earlier harness in this repo scored a broken control as
    its most emphatic guard. Build failures are scored ERROR and are a defect IN THE CONTROL.
  · READ THE FAILING TEST NAMES, NOT THE EXIT CODE. `go test` prints no PASS lines without -v and
    a panic takes the whole package binary with it, so a name list is the only way to tell "my
    guard fired" from "the package died".
  · EVERY CONTROL DECLARES ITS MUST-STAY-GREEN SET. A mutation that reds everything proves
    nothing about the guard it was written for.

Usage:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-search-access-controls.py
"""

import hashlib
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

HANDLER = "internal/search/handler.go"
STORE = "internal/page/store.go"
MAIN = "cmd/docs/main.go"

# The three guards this change ships. Referred to by short name below.
G_PRIVATE = "TestSearch_PrivateSpace_NotVisibleWithoutGrant_RealPG"
G_SEMANTIC = "TestSearch_SemanticOnlyRow_RespectsPageAccess"
G_WIRING = "TestSearch_MainWiresTheAccessGate"

VISIBLE_BODY = """	if h.access == nil {
		return rows
	}
	out := rows[:0]
	for _, r := range rows {
		found, canView := h.access.AuthorizePageRead(ctx, r.PageID)
		if !found || !canView {
			continue
		}
		out = append(out, r)
	}
	return out"""

WIRING_LINE = "\tsearchHandler.WithAccess(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))"

SQL_TEMPLATE_FILTER = "          AND p.is_template = false"

FETCH_BLOCK = """	fetchLimit := limit * maxFetchFactor
	if fetchLimit > maxFetchRows {
		fetchLimit = maxFetchRows
	}
	if h.access == nil {
		fetchLimit = limit
	}"""


# (id, description, [(file, anchor, replacement), ...], predicted catchers, must-stay-green)
CONTROLS = [
    (
        "C1",
        "the gate is removed entirely: visibleTo returns every row",
        [(HANDLER, VISIBLE_BODY, "\treturn rows")],
        [G_PRIVATE, G_SEMANTIC],
        [G_WIRING],
    ),
    (
        "C2",
        "canView is ignored; only a missing page is dropped (the found/canView pair collapsed)",
        # ⚠ REPAIRED, NOT RETARGETED. The first draft replaced only the `if` line, which leaves
        # canView declared and not used — Go refuses to build, and the harness scored it ERROR.
        # The mutation intended here is "the tier is ignored", so the discard keeps it compiling
        # and keeps the control about the gate rather than about the compiler.
        [(
            HANDLER,
            "\t\tfound, canView := h.access.AuthorizePageRead(ctx, r.PageID)\n\t\tif !found || !canView {",
            "\t\tfound, canView := h.access.AuthorizePageRead(ctx, r.PageID)\n\t\t_ = canView\n\t\tif !found {",
        )],
        [G_PRIVATE, G_SEMANTIC],
        [G_WIRING],
    ),
    (
        "C3",
        "over-correction: every row is dropped",
        [(HANDLER, VISIBLE_BODY, "\treturn nil")],
        [G_PRIVATE, G_SEMANTIC],
        [G_WIRING],
    ),
    (
        "C4",
        "THE WRONG FIX: a blanket `AND sp.private = false` in the SQL instead of the rule engine",
        [(STORE, SQL_TEMPLATE_FILTER, SQL_TEMPLATE_FILTER + "\n          AND sp.private = false")],
        [G_PRIVATE],
        [G_SEMANTIC, G_WIRING],
    ),
    (
        "C5",
        "the production wiring line is deleted from main.go",
        [(MAIN, WIRING_LINE + "\n", "")],
        [G_WIRING],
        [G_PRIVATE, G_SEMANTIC],
    ),
    (
        "C6",
        "the wiring line is COMMENTED OUT (does the guard tell a mention from a call?)",
        [(MAIN, WIRING_LINE, "\t//" + WIRING_LINE.lstrip("\t"))],
        [G_WIRING],
        [G_PRIVATE, G_SEMANTIC],
    ),
    (
        "C7",
        "the over-fetch is removed: the store is asked for exactly `limit`",
        [(HANDLER, FETCH_BLOCK, "\tfetchLimit := limit")],
        [G_PRIVATE, G_SEMANTIC],
        [G_WIRING],
    ),
]

TESTS = [G_PRIVATE, G_SEMANTIC, G_WIRING]
RUN_RE = "|".join(TESTS)


def sha(path):
    with open(os.path.join(ROOT, path), "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def read(path):
    with open(os.path.join(ROOT, path), encoding="utf-8") as fh:
        return fh.read()


def write(path, body):
    with open(os.path.join(ROOT, path), "w", encoding="utf-8") as fh:
        fh.write(body)


def run_suite():
    """Return (build_ok, failing_test_names, raw_output)."""
    proc = subprocess.run(
        ["go", "test", "-count=1", "-v", "-run", RUN_RE, "./internal/search/"],
        cwd=ROOT, capture_output=True, text=True, timeout=600,
    )
    out = proc.stdout + proc.stderr
    # A build failure is NOT a catch. go test reports it with these markers and no test output.
    build_broken = ("[build failed]" in out) or ("cannot use" in out and "--- FAIL" not in out)
    failing = sorted(set(re.findall(r"--- FAIL: (\w+)", out)))
    return (not build_broken), failing, out


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set — these controls need the real-PG guard to RUN.")
        return 2

    before = {p: sha(p) for p in (HANDLER, STORE, MAIN)}

    ok, failing, out = run_suite()
    if not ok or failing:
        print("BASELINE IS NOT GREEN — controls cannot be scored against it.")
        print(out[-4000:])
        return 2
    print(f"baseline: build ok, 0 failures across {len(TESTS)} guards\n")

    results = []
    for cid, desc, edits, predicted, stay_green in CONTROLS:
        originals = {}
        try:
            applied = 0
            for path, anchor, repl in edits:
                body = read(path)
                originals.setdefault(path, body)
                # ⚠ ASSERT THE ANCHOR BEFORE WRITING. An anchor that matches 0 sites makes an inert
                # control read as a blind guard; one that matches 2 makes the control ambiguous.
                n = body.count(anchor)
                if n != 1:
                    raise AssertionError(f"{cid}: anchor matched {n} sites in {path}, want exactly 1")
                write(path, body.replace(anchor, repl, 1))
                applied += 1
            assert applied == len(edits), f"{cid}: applied {applied} of {len(edits)} edits"

            built, failed, raw = run_suite()
            if not built:
                verdict = "ERROR (control does not compile — a defect in the CONTROL)"
            else:
                caught = [t for t in predicted if t in failed]
                broke = [t for t in stay_green if t in failed]
                if broke:
                    verdict = f"IMPRECISE — also reddened must-stay-green {broke}"
                elif sorted(caught) == sorted(predicted):
                    verdict = "CAUGHT by the predicted guard(s)"
                elif caught:
                    verdict = f"PARTIAL — predicted {predicted}, caught {caught}"
                else:
                    verdict = f"NOT CAUGHT — predicted {predicted}, failures were {failed or 'none'}"
            msgs = re.findall(r"^\s+\w+\.go:\d+: (.{0,110})", raw, re.M)
            results.append((cid, desc, predicted, failed, verdict, msgs[:2]))
        finally:
            for path, body in originals.items():
                write(path, body)

    after = {p: sha(p) for p in (HANDLER, STORE, MAIN)}
    restored = after == before

    print("=" * 100)
    for cid, desc, predicted, failed, verdict, msgs in results:
        print(f"\n{cid}  {desc}")
        print(f"     predicted : {', '.join(predicted)}")
        print(f"     failed    : {', '.join(failed) if failed else '(none)'}")
        print(f"     verdict   : {verdict}")
        for m in msgs:
            print(f"     said      : {m.strip()}")
    print("\n" + "=" * 100)
    print(f"tree restored (sha256 on all {len(before)} files): {restored}")
    if not restored:
        for p in before:
            if before[p] != after[p]:
                print(f"  ⚠ NOT RESTORED: {p}")
        return 1

    good = sum(1 for r in results if r[4].startswith("CAUGHT"))
    print(f"{good}/{len(results)} controls caught by the guard named before the run")
    return 0 if good == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
