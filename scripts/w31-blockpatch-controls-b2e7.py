#!/usr/bin/env python3
"""Positive-control harness for the block PATCH partial-update rule (tab-b2e7, W3.1).

A guard that passes on a pristine tree has told you nothing. Each control below mutates ONE
thing, names the test that MUST catch it BEFORE the run, and runs the FULL suite so "only the
new guard reds" is measured rather than assumed. Every mutation is restored in a `finally` and
the restoration is verified by sha256 against the bytes read at start.

Run:
    DOCS_TEST_DATABASE_URL=postgres://... python3 scripts/w31-blockpatch-controls-b2e7.py
"""

import hashlib
import os
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = os.path.join(REPO, "internal/block/store.go")
HANDLER = os.path.join(REPO, "internal/block/handler.go")
GUARD = os.path.join(REPO, "internal/block/patchsemantics_realpg_test.go")

POSITION_ONLY = "TestBlockPatch_PositionOnly_LeavesContentIntact_RealPG"
CONTENT_ONLY = "TestBlockPatch_ContentOnly_LeavesPositionIntact_RealPG"
BOTH_FIELDS = "TestBlockPatch_BothFields_WriteBoth_RealPG"
CROSS_TENANT = "TestBlockPatch_CrossTenant_Is404_RealPG"
FAILCLOSED = "TestBlock_InWorkspaces_CrossTenant_GateHoldsWithoutEnforcer_RealPG"

# The UPDATE as it ships. Anchors are whole statements so a partial match cannot silently
# mutate something else.
SHIPPED_SQL = """		`UPDATE blocks
        SET content  = COALESCE($1::text, content),
            position = COALESCE($2::float8, position),
            updated_at = NOW()
        WHERE id = $3 RETURNING `+columns,"""

UPDATE_BODY_TOP = """	if s.pool == nil {
		return nil, errors.New("block: store has no pool")
	}
	// nosemgrep: docs-by-id-write-requires-workspace-scope"""

ASSERT_IN_WS = """	if err := s.assertInWorkspaces(ctx, id, wsIDs); err != nil {
		return nil, err
	}
	return s.Update(ctx, id, content, position)"""

CONTENT_TAG = '		Content  *string  `json:"content"`'

# The assertion the BLIND control removes — the one sentence that reads the content column.
CONTENT_ASSERTION = """	content, position := f.row(t, id)
	if content != seededContent {"""


class Control:
    def __init__(self, name, why, predicted, edits, expect_caught=True, note=""):
        self.name = name
        self.why = why
        self.predicted = predicted  # tests that MUST fail
        self.edits = edits  # list of (path, old, new)
        self.expect_caught = expect_caught
        self.note = note


CONTROLS = [
    Control(
        "C0 baseline (no mutation)",
        "A control set whose baseline is not green is measuring the tree, not the guard.",
        [],
        [],
        expect_caught=False,
    ),
    Control(
        "C1 nil content treated as \"\" (the shipped defect, content half)",
        "This is exactly what the value struct did: an absent `content` key reached the UPDATE "
        "as the empty string and the block's document was destroyed.",
        [POSITION_ONLY],
        [(STORE, UPDATE_BODY_TOP,
          "\tif s.pool == nil {\n\t\treturn nil, errors.New(\"block: store has no pool\")\n\t}\n"
          "\tif content == nil {\n\t\te := \"\"\n\t\tcontent = &e\n\t}\n"
          "\t// nosemgrep: docs-by-id-write-requires-workspace-scope")],
    ),
    Control(
        "C2 nil position treated as 0 (the shipped defect, position half)",
        "The other direction: a content edit silently moved the block to the top of the page.",
        [CONTENT_ONLY],
        [(STORE, UPDATE_BODY_TOP,
          "\tif s.pool == nil {\n\t\treturn nil, errors.New(\"block: store has no pool\")\n\t}\n"
          "\tif position == nil {\n\t\tz := 0.0\n\t\tposition = &z\n\t}\n"
          "\t// nosemgrep: docs-by-id-write-requires-workspace-scope")],
    ),
    Control(
        "C3 the decoder stops binding `content`",
        "Proves the guard reads the DECODED body and not just the store: a sent field that never "
        "binds is the mirror failure of an absent field that does.",
        [CONTENT_ONLY, BOTH_FIELDS],
        [(HANDLER, CONTENT_TAG, '\t\tContent  *string  `json:"contents"`')],
    ),
    Control(
        "C4 the UPDATE writes NEITHER column",
        "The must-stay-green companion made active. 'Keep what was not sent' is satisfied by a "
        "route that writes nothing at all, so something has to red when it does. Both arguments "
        "are dropped IN GO rather than out of the SQL: removing $1/$2 from the statement leaves "
        "three bound parameters for one placeholder, and pgx rejects the query — that reds the "
        "package for a broken query, not for an inert write, and it is a different measurement.",
        [POSITION_ONLY, CONTENT_ONLY, BOTH_FIELDS],
        [(STORE, UPDATE_BODY_TOP,
          "\tif s.pool == nil {\n\t\treturn nil, errors.New(\"block: store has no pool\")\n\t}\n"
          "\tcontent, position = nil, nil\n"
          "\t// nosemgrep: docs-by-id-write-requires-workspace-scope")],
    ),
    Control(
        "C5 the workspace scope check removed from UpdateInWorkspaces",
        "Keeps CAUGHT from being a catch-all: the partial-update cases must stay GREEN while a "
        "tenancy case reds, or they are not measuring what they claim. ⚠ AND IT CORRECTS A "
        "PREDICTION I GOT WRONG — see the printed note below.",
        [FAILCLOSED],
        [(STORE, ASSERT_IN_WS, "\treturn s.Update(ctx, id, content, position)")],
        note="MEASURED, AND IT FALSIFIED MY FIRST PREDICTION: I predicted the HTTP case "
             f"{CROSS_TENANT} would red too. IT STAYS GREEN, and it should. Alice is denied by the "
             "ROUTE enforcer (blockEnf → PageResolverFromBlock → a page lookup already scoped to "
             "her workspaces), so the request 404s before the handler — and therefore before the "
             "store — is ever reached. The HTTP case pins the ROUTE's denial; the store's own "
             "in-method gate is pinned by failclosed_gate_test.go, which calls the store directly. "
             "Two different claims, and only the control tells them apart.",
    ),
    Control(
        "C6 BLIND: the content assertion removed, with C1's defect on top",
        "The measured blindness. With the one sentence that reads the content column gone, the "
        "destroyed document is invisible to the whole repository.",
        [],
        [
            (GUARD, CONTENT_ASSERTION,
             "\tcontent, position := f.row(t, id)\n\t_ = content\n\tif false {"),
            (GUARD, """	if body.Content != seededContent || body.Position != 9 {""",
             """	if body.Position != 9 {"""),
            (STORE, UPDATE_BODY_TOP,
             "\tif s.pool == nil {\n\t\treturn nil, errors.New(\"block: store has no pool\")\n\t}\n"
             "\tif content == nil {\n\t\te := \"\"\n\t\tcontent = &e\n\t}\n"
             "\t// nosemgrep: docs-by-id-write-requires-workspace-scope"),
        ],
        expect_caught=False,
    ),
]


def sha(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def run_suite():
    """Full suite. Returns (exit_code, set_of_failed_test_names)."""
    env = dict(os.environ)
    proc = subprocess.run(
        ["go", "test", "-count=1", "-json", "./..."],
        cwd=REPO, env=env, capture_output=True, text=True,
    )
    failed = set()
    for line in proc.stdout.splitlines():
        if '"Action":"fail"' in line and '"Test":"' in line:
            name = line.split('"Test":"', 1)[1].split('"', 1)[0]
            failed.add(name)
    return proc.returncode, failed, proc.stderr


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL is unset — the real-PG cases would FAIL, not skip, and this "
              "harness would be measuring the harness.", file=sys.stderr)
        return 2

    originals = {p: open(p, "rb").read() for p in (STORE, HANDLER, GUARD)}
    digests = {p: sha(p) for p in originals}
    results = []
    try:
        for c in CONTROLS:
            print(f"\n=== {c.name}")
            print(f"    why: {c.why}")
            print(f"    PREDICTION (stated before the run): "
                  f"{'CAUGHT by ' + ', '.join(c.predicted) if c.expect_caught else 'NOT CAUGHT — suite stays green'}")
            for path, old, new in c.edits:
                src = open(path).read()
                if src.count(old) != 1:
                    print(f"    ANCHOR MISS in {os.path.basename(path)}: {src.count(old)} matches — "
                          f"the harness is stale, not the product.")
                    return 3
                open(path, "w").write(src.replace(old, new))
            code, failed, stderr = run_suite()
            if "build failed" in stderr or "cannot use" in stderr:
                print(f"    BUILD FAILED — a mutation that does not compile measures nothing:\n{stderr[:800]}")
                return 3
            got = sorted(t for t in failed if t.startswith("TestBlock"))
            print(f"    exit={code} failed={got if got else '(none)'}")
            if c.note:
                print(f"    NOTE: {c.note}")
            if c.expect_caught:
                missing = [t for t in c.predicted if t not in failed]
                extra = [t for t in got if t not in c.predicted]
                ok = code != 0 and not missing
                print(f"    -> {'MATCHES' if ok and not extra else 'DIVERGES'}"
                      + (f" (missing: {missing})" if missing else "")
                      + (f" (also red, unpredicted: {extra})" if extra else ""))
                results.append((c.name, ok and not extra))
            else:
                ok = code == 0
                print(f"    -> {'MATCHES (green as predicted)' if ok else 'DIVERGES (suite red)'}")
                results.append((c.name, ok))
            # restore between controls
            for path, blob in originals.items():
                open(path, "wb").write(blob)
    finally:
        for path, blob in originals.items():
            open(path, "wb").write(blob)
        bad = [p for p in digests if sha(p) != digests[p]]
        print("\nRESTORE: " + ("VERIFIED (sha256 unchanged)" if not bad else f"FAILED for {bad}"))
        if bad:
            return 4

    print("\n=== SUMMARY")
    for name, ok in results:
        print(f"  {'PASS' if ok else 'FAIL'}  {name}")
    return 0 if all(ok for _, ok in results) else 1


if __name__ == "__main__":
    sys.exit(main())
