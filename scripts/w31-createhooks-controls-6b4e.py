#!/usr/bin/env python3
"""Positive controls for the create-time content hooks (W3.1, tab-6b4e).

Each control mutates ONE thing in the product tree, runs the two guards, and reports the SET of
[TAG]s that fired. Every mutation is restored in a `finally` and the restore is verified by
sha256 against the pre-mutation bytes — a control that silently fails to restore would score
every later control against a different tree.

THE CATCHER IS PREDICTED BEFORE THE RUN (below, `expect`). A control whose actual catcher differs
from its prediction is a finding about the guard, not a pass.

Run:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-createhooks-controls-6b4e.py
"""
import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = os.path.join(REPO, "internal/page/store.go")
GUARD = "TestCreate_With"

TAGS = [
    "LINK-ON-CREATE",
    "COST-ON-CREATE",
    "INDEX-ON-CREATE",
    "PATCH-PARITY",
    "TEMPLATE-NOT-INDEXED",
    "BLANK-NOT-INDEXED",
    "CREATE-201",
]

# The create-time hook block, verbatim, as the anchor several controls cut from.
CREATE_LINKER = """		if s.linker != nil {
			_ = s.linker.SyncLinks(ctx, out.ID, out.WorkspaceID, out.Content, p.CreatedBy)
		}"""

CREATE_INDEXER_HEAD = "		if s.indexer != nil && !out.IsTemplate {"

HAS_CONTENT = """	hasContent := (strings.TrimSpace(out.Content) != "" && out.Content != emptyDoc) ||
		strings.TrimSpace(out.ContentText) != ""
	if hasContent {"""


def sha(path):
    return hashlib.sha256(open(path, "rb").read()).hexdigest()


def run_guard():
    """Run the two guards; return the set of tags that appear in FAILING output."""
    env = dict(os.environ)
    p = subprocess.run(
        ["go", "test", "./internal/page/", "-run", GUARD, "-count=1", "-v"],
        cwd=REPO, env=env, capture_output=True, text=True, timeout=900,
    )
    out = p.stdout + p.stderr
    if "build failed" in out or "cannot use" in out or re.search(r"\.go:\d+:\d+: ", out):
        return {"BUILD-FAILED"}, out
    fired = {t for t in TAGS if "[" + t + "]" in out}
    return fired, out


def mutate(src, kind):
    """Return mutated source, or None when the anchor did not match exactly once."""
    if kind == "C0":  # inert
        anchor = "	if hasContent {"
        if src.count(anchor) != 1:
            return None
        return src.replace(anchor, "	// control C0: an inert comment\n" + anchor)

    if kind == "C1":  # revert the whole fix
        i = src.index(HAS_CONTENT)
        j = src.index("	return out, nil\n}", i)
        return src[:i] + src[j:]

    if kind == "C2":  # linker hook only
        if src.count(CREATE_LINKER) != 1:
            return None
        return src.replace(CREATE_LINKER, "")

    if kind == "C3":  # indexer hook only
        i = src.index(CREATE_INDEXER_HEAD)
        j = src.index("		}\n	}\n	return out, nil\n}", i)
        return src[:i] + src[j + len("		}\n"):]

    if kind == "C4":  # template exclusion dropped
        # ⚠ THE ANCHOR IS NOT UNIQUE AND THAT IS THE POINT OF THIS COMMENT: Create's indexer
        # guard and Update's are the SAME LINE, which is what "mirrors Update exactly" means.
        # A count!=1 bail (the rule the other controls use) reported C4 as unrunnable on its
        # first run. Cut the FIRST occurrence — Create is defined above Update in this file,
        # and the assertion below pins that ordering rather than trusting it.
        i = src.index(CREATE_INDEXER_HEAD)
        assert src.index("func (s *Store) Create(") < i < src.index("func (s *Store) Update("), \
            "the first indexer guard is no longer Create's — re-anchor C4"
        return src[:i] + "		if s.indexer != nil {" + src[i + len(CREATE_INDEXER_HEAD):]

    if kind == "C5":  # the predicate the first version of the fix got wrong
        if src.count(HAS_CONTENT) != 1:
            return None
        return src.replace(HAS_CONTENT, "	hasContent := true\n	if hasContent {")

    if kind == "C6":  # MUST-STAY-GREEN target: Update's own hooks removed
        anchor = """		if s.linker != nil {
			updatedBy, _ := updates["updated_by"].(string)
			_ = s.linker.SyncLinks(ctx, id, out.WorkspaceID, out.Content, updatedBy)
		}"""
        if src.count(anchor) != 1:
            return None
        return src.replace(anchor, "")

    raise AssertionError(kind)


CONTROLS = [
    ("C0", "an inert comment beside the hook block",
     set(), "nothing may be caught by a comment"),
    ("C1", "the WHOLE fix reverted — the defect exactly as found",
     {"LINK-ON-CREATE", "COST-ON-CREATE", "INDEX-ON-CREATE"},
     "the red-first proof: all three target assertions, and only those"),
    ("C2", "only the LINKER hook deleted from Create",
     {"LINK-ON-CREATE", "COST-ON-CREATE"},
     "the money pair, without the search half — this is what makes the two hooks two"),
    ("C3", "only the INDEXER hook deleted from Create",
     {"INDEX-ON-CREATE"},
     "the search half alone, for the same reason"),
    ("C4", "the `!out.IsTemplate` exclusion dropped",
     {"TEMPLATE-NOT-INDEXED"},
     "the counter-assertion that keeps the fix a filter rather than a blanket"),
    ("C5", "`hasContent` forced true — the bug the first draft of this fix actually had",
     {"BLANK-NOT-INDEXED"},
     "earns the blank-page predicate; it caught this for real once already"),
    ("C6", "UPDATE's own link hook deleted (must-stay-green target)",
     {"PATCH-PARITY"},
     "proves the fix ADDS a hook rather than MOVING one"),
]


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set — these guards are real-Postgres.")
        return 2
    original = open(STORE, encoding="utf-8").read()
    before = sha(STORE)

    print("BASELINE: the unmutated tree must be GREEN, or every verdict below is noise.")
    fired, out = run_guard()
    if fired:
        print("  BASELINE IS NOT CLEAN — tags fired:", sorted(fired))
        print(out[-4000:])
        return 1
    print("  baseline clean (no tags).\n")

    ok = True
    for kind, what, expect, why in CONTROLS:
        mutated = mutate(original, kind)
        if mutated is None or mutated == original:
            print(f"{kind}: ANCHOR DID NOT MATCH — control not run (this is a failure, not a skip)")
            ok = False
            continue
        try:
            open(STORE, "w", encoding="utf-8").write(mutated)
            fired, out = run_guard()
        finally:
            open(STORE, "w", encoding="utf-8").write(original)
            assert sha(STORE) == before, "RESTORE FAILED — the tree is not as it was"
        verdict = "AS PREDICTED" if fired == expect else "*** DIFFERS FROM PREDICTION ***"
        if fired != expect:
            ok = False
        print(f"{kind}: {what}")
        print(f"     predicted {sorted(expect) or 'NOT CAUGHT'}")
        print(f"     actual    {sorted(fired) or 'NOT CAUGHT'}   {verdict}")
        print(f"     why: {why}\n")

    print("ALL CONTROLS AS PREDICTED" if ok else "AT LEAST ONE CONTROL DIFFERED — read it")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
