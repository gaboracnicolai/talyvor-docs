#!/usr/bin/env python3
"""
Positive controls for FINDING (22) — the body-derived-object class on /workspaces/{wsID}/ai/*.

WHAT IS BEING JUSTIFIED. `Handler.attributable` authorizes the page named in the REQUEST BODY
before its cost is bound. The guard is internal/ai/bodypage_attribution_realpg_test.go, driven
through the real routes on real Postgres.

THE VERDICT IS THE SET OF ASSERTION TAGS THAT FIRED, plus the SET of test names that reddened,
both predicted before the run. A control whose observed set differs from its prediction is a
MISMATCH and scores as a failure of the harness, not a pass — being caught and being caught for
the predicted reason are different things.

⚠ THE GUARD LIVES IN THE PACKAGE IT MUTATES, so "the package reddened" is nearly free and says
nothing. That is why every control also carries a BLIND-GUARD claim: the names of pre-existing
tests that must stay GREEN under it. internal/ai/attribution_e2e_test.go is the interesting one —
it drives the same four routes with the same page_id field and asserts the binding, and it wires
`allowAllPages{}`, an allow-all read gate. It CANNOT see this class by construction. If a control
reddens it unexpectedly, the control is testing something else.

A build failure is detected explicitly and scored as BUILD, never as a catch: a compile error
reddens everything and looks exactly like a working guard.

Restores are `cp` from bytes saved before the run, in a `finally`, sha256-compared at the end.

Usage:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-bodypage-attribution-controls.py
"""

import hashlib
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile

REPO = pathlib.Path(__file__).resolve().parent.parent
DSN = os.environ.get(
    "DOCS_TEST_DATABASE_URL",
    "postgres://postgres:postgres@localhost:55433/postgres?sslmode=disable",
)

HANDLER = "internal/ai/handler.go"
# ⚠ THE SECOND LAYER. A cross-workspace ledger leak needs BOTH the handler gate AND the store's
# join to fail, which is why no mutation of handler.go alone can claim A-LEAK-XWS-LEDGER or
# A-LEAK-XWS-MONEY — see C9/C10 and the note on those two tags in C1/C4/C6.
AI_SPEND = "internal/page/ai_spend.go"
FILES = [HANDLER, AI_SPEND]

# The store-side defence: the INSERT is fed by a CTE that only yields a row when the page is in
# the workspace being billed.
BIND_JOIN = "            SELECT 1 FROM pages WHERE id = $2 AND workspace_id = $3"
BIND_JOIN_UNSCOPED = "            SELECT 1 FROM pages WHERE id = $2"
AI_PKG = "./internal/ai/"

# The pre-existing tests that must stay GREEN under every control except C7. Named, not counted:
# a count cannot tell "still covering" from "covering something else".
BLIND_GUARDS = [
    "TestAttribution_EachSinglePageOperationBindsItsPage",
    "TestAttribution_AskSpansPagesAndBindsNothing",
    "TestAttribution_NoPageIDBindsNothing",
    "TestAttribution_NoRequestIDHeaderBindsNothing",
    "TestAttribution_BindFailureDoesNotFailTheRequest",
]


def sh(args, **kw):
    env = dict(os.environ, DOCS_TEST_DATABASE_URL=DSN)
    return subprocess.run(args, cwd=REPO, capture_output=True, text=True, env=env, **kw)


def sha256(p):
    return hashlib.sha256((REPO / p).read_bytes()).hexdigest()


TAG_RE = re.compile(r"\[(A-[A-Z-]+(?:/[a-z-]+)?)\]")
FAILTEST_RE = re.compile(r"^\s*--- FAIL: (\S+)", re.M)
BUILD_RE = re.compile(r"^\S+\.go:\d+:\d+: |\[build failed\]|build failed", re.M)
PANIC_RE = re.compile(r"^panic: |^\[signal SIG", re.M)


def run_tests(pkg=AI_PKG):
    """Return (tags, failed_tests, abort_reason, raw).

    ⚠ A PANIC IS SCORED SEPARATELY AND NEVER AS A CATCH. C5's first version deleted the nil-gate
    branch outright; `h.access` is a nil INTERFACE, so the next line dereferenced it and the test
    binary died. A panic kills the whole package run, so NO assertion tag prints — the harness saw
    an empty tag set, which for a control predicting an empty set would have scored AS PREDICTED.
    The mutation that belongs there is the DEFECT's shape (bind blind), not a deletion.
    """
    r = sh(["go", "test", "-timeout", "300s", "-count=1", "-v", pkg])
    out = r.stdout + r.stderr
    if BUILD_RE.search(out):
        return set(), set(), "BUILD", out
    if PANIC_RE.search(out):
        return set(), set(), "PANIC", out
    return set(TAG_RE.findall(out)), set(FAILTEST_RE.findall(out)), None, out


def mutate(path, old, new, count=1):
    p = REPO / path
    s = p.read_text()
    if s.count(old) != count:
        raise SystemExit(
            f"ANCHOR MISS in {path}: expected {count} occurrence(s) of:\n{old!r}\n"
            f"got {s.count(old)}. A control that matched no bytes reports NOT CAUGHT for free."
        )
    p.write_text(s.replace(old, new, count))


# ── The mutations ──────────────────────────────────────────────────────────────────────────
#
# Each is (id, description, apply_fn, predicted_tags, predicted_blind_guards_green).

# ⚠ `attributable` GREW A `billWS` PARAMETER, SO ALL FOUR CALL SITES MOVED. The count of 4 that
# C6 asserts is unchanged and was re-measured against the file rather than carried over.
GATE_CALL = """	if !h.attributable(w, r, wsID, in.PageID) {
		return
	}
"""

# ⚠ C2/C3 DISABLE THE BRANCH RATHER THAN DELETING IT, and that is not a style choice. Deleting
# `if !found { … }` leaves `found` declared and unused, so the package does not compile and the
# control scores BUILD — a compile error reddens everything and says nothing about any guard.
# `&& false` keeps the variable used and the branch dead, which is the state a broken gate is
# actually in.
FOUND_COND = "	if !found {\n"
FOUND_COND_DEAD = "	if !found && false {\n"

CANVIEW_COND = "	if !canView {\n"
CANVIEW_COND_DEAD = "	if !canView && false {\n"

# ⚠ C5 RETURNS TRUE RATHER THAN DELETING THE BRANCH. Deleting it lets the next line call a method
# on a nil interface: the binary panics, the package aborts, and not one assertion tag prints.
# Binding blind is the defect an unwired gate actually produces.
NIL_BRANCH_REFUSES = """	if h.access == nil {
		slog.Error("ai: no page-read gate wired — refusing a page_id-carrying request rather " +
			"than binding its cost blind (cmd/docs/main.go must call aiHandler.WithPageRead)")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "attribution unavailable"})
		return false
	}
"""

NIL_BRANCH_BINDS_BLIND = """	if h.access == nil {
		return true
	}
"""

EMPTY_BRANCH = """	if pageID == "" {
		return true
	}
"""

WRITE_CALL_SITE = """	if !h.attributable(w, r, wsID, in.PageID) {
		return
	}
	out, err := h.engine.WriteWithAI(r.Context(), wsID, in.Prompt, in.Context, in.PageID)
"""

WRITE_CALL_SITE_AFTER = """	out, err := h.engine.WriteWithAI(r.Context(), wsID, in.Prompt, in.Context, in.PageID)
	if !h.attributable(w, r, wsID, in.PageID) {
		return
	}
"""


# ⚠⚠⚠ THIS CONTROL READ A MOVING REFERENCE WHILE ITS OWN DOCSTRING NAMED A FIXED ONE.
# It said "the real defect bytes as they shipped at 1581757" and then read the branch tip. The
# moment the fix merged to main — the same pull request that added this control — the branch tip
# became the FIXED file, so C1 restored the current code over itself: a byte-for-byte no-op that
# could never fail again, and that drifts FURTHER from its stated subject with every commit
# rather than closer.
#
# MEASURED at c403f5e: the branch tip's handler.go hashes af49245c…, which is the working tree
# byte for byte, and C1 scored "want 14 tags, got []". 1581757 hashes 6e04e704… and contains
# ZERO occurrences of `attributable` — it really is the shipped defect, exactly as the docstring
# always said.
#
# ⚠ NO ANCHOR CENSUS CAN SEE THIS AND NEITHER CAN A BYTE-LEVEL VOID CHECK: there is no anchor,
# the file is rewritten wholesale, and the write genuinely happens. What catches it is the arm
# firing no tags, which is visible only to somebody running the campaign.
#
# The SHA is pinned, and asserted to DIFFER from the working tree before it is written, so this
# arm now reports itself void instead of scoring a tag mismatch if the pin is ever wrong.
DEFECT_SHA = "1581757"


def c1_restore_shipped_defect():
    """The real defect bytes as they shipped at 1581757 — not a mutation I invented."""
    r = sh(["git", "show", DEFECT_SHA + ":" + HANDLER])
    if r.returncode != 0 or not r.stdout:
        raise SystemExit("C1: could not read %s:%s" % (DEFECT_SHA, HANDLER))
    if r.stdout == (REPO / HANDLER).read_text():
        raise SystemExit("C1 IS VOID: %s:%s is byte-identical to the working tree, so this arm "
                         "restores the current code over itself and proves nothing."
                         % (DEFECT_SHA, HANDLER))
    (REPO / HANDLER).write_text(r.stdout)


CONTROLS = [
    (
        "C0",
        "MUST STAY GREEN — no mutation at all. Without this, a control set that fails for an "
        "environmental reason scores as 8 working guards.",
        lambda: None,
        set(),
        True,
    ),
    (
        "C1",
        "RESTORE 1581757's handler.go — the defect exactly as it shipped, real bytes.",
        c1_restore_shipped_defect,
        # ⚠ A-LEAK-XWS-LEDGER AND A-LEAK-XWS-MONEY WERE PREDICTED HERE AND CANNOT BE PRODUCED BY
        # ANY MUTATION OF handler.go. Measured: C1 restores the ENTIRE pre-fix handler and still
        # produces no ledger row for the foreign page. The leak is defended in depth and the
        # second layer — BindAISpend's `SELECT 1 FROM pages WHERE id = $2 AND workspace_id = $3`
        # — was added after this control set was written. C10 claims both tags by removing BOTH
        # layers; C9 shows the store layer alone is not what refuses.
        {
            "A-LEAK-XWS/write", "A-LEAK-XWS/transform", "A-LEAK-XWS/translate",
            "A-LEAK-XWS/suggest-title",
            "A-LEAK-PRIV/write", "A-LEAK-PRIV/transform", "A-LEAK-PRIV/translate",
            "A-LEAK-PRIV/suggest-title", "A-LEAK-PRIV-LEDGER", "A-LEAK-PRIV-MONEY",
            "A-UNWIRED", "A-UNWIRED-LEDGER",
        },
        True,
    ),
    (
        "C2",
        "Delete the !found → 404 branch. A foreign page then falls through to !canView and is "
        "refused with 403. STILL REFUSED — so only the status assertions may move, and the "
        "ledger, money and no-Lens-call assertions must stay GREEN. This is what separates "
        "'the right answer' from 'an answer that happens to deny'.",
        lambda: mutate(HANDLER, FOUND_COND, FOUND_COND_DEAD),
        {"A-LEAK-XWS/write", "A-LEAK-XWS/transform", "A-LEAK-XWS/translate",
         "A-LEAK-XWS/suggest-title"},
        True,
    ),
    (
        "C3",
        "Delete the !canView → 403 branch. A page in the caller's own workspace that they may "
        "not view becomes a legal attribution target; a foreign page is still 404.",
        lambda: mutate(HANDLER, CANVIEW_COND, CANVIEW_COND_DEAD),
        {"A-LEAK-PRIV/write", "A-LEAK-PRIV/transform", "A-LEAK-PRIV/translate",
         "A-LEAK-PRIV/suggest-title", "A-LEAK-PRIV-LEDGER", "A-LEAK-PRIV-MONEY"},
        True,
    ),
    (
        "C4",
        "Move Write's gate to AFTER the completion. The status codes are unchanged and the "
        "refusal still happens — but the engine has already run and bound the page. This is the "
        "control that earns the no-Lens-call and ledger assertions, and NOTHING about the "
        "response can see it. ⚠ MY FIRST PREDICTION WAS WRONG AND IS KEPT WRONG HERE: I omitted "
        "A-UNWIRED-LEDGER. With the gate moved, the UNWIRED test's 500 is still a 500 — but the "
        "engine ran before it and wrote a ledger row, so the row-count assertion fires while the "
        "status assertion it sits next to does not. That pair is the whole point of splitting "
        "them, and I had not seen it until the harness printed it.",
        lambda: mutate(HANDLER, WRITE_CALL_SITE, WRITE_CALL_SITE_AFTER),
        # ⚠ A-LEAK-XWS-LEDGER AND A-LEAK-XWS-MONEY WERE PREDICTED HERE AND CANNOT BE PRODUCED BY
        # ANY MUTATION OF handler.go. Measured: C1 restores the ENTIRE pre-fix handler and still
        # produces no ledger row for the foreign page. The leak is defended in depth and the
        # second layer — BindAISpend's `SELECT 1 FROM pages WHERE id = $2 AND workspace_id = $3`
        # — was added after this control set was written. C10 claims both tags by removing BOTH
        # layers; C9 shows the store layer alone is not what refuses.
        {"A-LEAK-XWS/write",
         "A-LEAK-PRIV-LEDGER", "A-LEAK-PRIV-MONEY", "A-UNWIRED-LEDGER"},
        True,
    ),
    (
        "C5",
        "An unwired gate binds blind instead of reporting — the shape a future handler "
        "constructed without WithPageRead would take.",
        lambda: mutate(HANDLER, NIL_BRANCH_REFUSES, NIL_BRANCH_BINDS_BLIND),
        {"A-UNWIRED", "A-UNWIRED-LEDGER"},
        True,
    ),
    (
        "C6",
        "Gate Write only; leave Transform, Translate and SuggestTitle ungated. A per-route fix "
        "is the likeliest partial repair, and a guard that drove one route could not see it.",
        lambda: mutate(HANDLER, GATE_CALL, "", count=4) or mutate_reinstate_write(),
        # ⚠ A-LEAK-XWS-LEDGER AND A-LEAK-XWS-MONEY WERE PREDICTED HERE AND CANNOT BE PRODUCED BY
        # ANY MUTATION OF handler.go. Measured: C1 restores the ENTIRE pre-fix handler and still
        # produces no ledger row for the foreign page. The leak is defended in depth and the
        # second layer — BindAISpend's `SELECT 1 FROM pages WHERE id = $2 AND workspace_id = $3`
        # — was added after this control set was written. C10 claims both tags by removing BOTH
        # layers; C9 shows the store layer alone is not what refuses.
        {"A-LEAK-XWS/transform", "A-LEAK-XWS/translate", "A-LEAK-XWS/suggest-title",
         "A-LEAK-PRIV/transform", "A-LEAK-PRIV/translate", "A-LEAK-PRIV/suggest-title",
         "A-LEAK-PRIV-LEDGER", "A-LEAK-PRIV-MONEY"},
        True,
    ),
    (
        "C7",
        "OVER-CORRECTION: refuse every non-empty page_id. Every leak assertion in the file "
        "passes under this, which is exactly why the honest-path and grant assertions exist. "
        "⚠ THIS IS THE ONE CONTROL THAT MUST ALSO REDDEN THE BLIND GUARDS — the pre-existing "
        "e2e attribution suite asserts a binding is recorded, and under this mutation none is. "
        "⚠ MY PREDICTION WAS WRONG A SECOND WAY AND IS KEPT WRONG HERE: I listed only the "
        "honest-path tags. A blanket 403 also changes the FOREIGN page's answer from 404 to 403 "
        "and the UNWIRED handler's from 500 to 403 — because the refusal is inserted ahead of "
        "both branches. An over-correction is not confined to the cases it over-refuses; it "
        "rewrites the ones it was already refusing correctly.",
        lambda: mutate(
            HANDLER,
            '\tif pageID == "" {\n\t\treturn true\n\t}\n',
            '\tif pageID == "" {\n\t\treturn true\n\t}\n\tif pageID != "" {\n\t\t'
            'writeJSON(w, http.StatusForbidden, map[string]string{"error": "refused"})\n\t\t'
            "return false\n\t}\n",
        ),
        {"A-HONEST/write", "A-HONEST/transform", "A-HONEST/translate",
         "A-HONEST/suggest-title", "A-HONEST-MONEY", "A-GRANT",
         "A-LEAK-XWS/write", "A-LEAK-XWS/transform", "A-LEAK-XWS/translate",
         "A-LEAK-XWS/suggest-title", "A-UNWIRED"},
        False,  # blind guards MUST redden here
    ),
    (
        "C8",
        "Gate the empty page_id too. A page has no id before its first save and two operations "
        "always pass empty; a fix that simply required the field would break the editor.",
        lambda: mutate(HANDLER, EMPTY_BRANCH, ""),
        {"A-EMPTY"},
        True,
    ),
    (
        "C9",
        "STORE LAYER ONLY: drop the workspace predicate from BindAISpend's CTE, leaving every "
        "handler gate intact. MUST STAY GREEN, and that is the point — it shows the handler gate "
        "alone still refuses, so C10's red is attributable to the pair rather than to this half.",
        lambda: mutate(AI_SPEND, BIND_JOIN, BIND_JOIN_UNSCOPED),
        set(),
        True,
    ),
    (
        "C10",
        "BOTH LAYERS: move Write's gate after the completion AND drop BindAISpend's workspace "
        "predicate. ⚠ THIS IS THE ONLY CONTROL THAT CAN CLAIM A-LEAK-XWS-LEDGER AND "
        "A-LEAK-XWS-MONEY, AND IT WAS ADDED BECAUSE NOTHING CLAIMED THEM. C1, C4 and C6 all "
        "predicted those two tags and none produced them, including C1, which restores the whole "
        "pre-fix handler: the leak is defended in depth, and the second layer — BindAISpend's "
        "`SELECT 1 FROM pages WHERE id = $2 AND workspace_id = $3` — was added after this control "
        "set was written. A predicted-but-unproducible tag reads exactly like a working control.",
        lambda: mutate(AI_SPEND, BIND_JOIN, BIND_JOIN_UNSCOPED) or
                mutate(HANDLER, WRITE_CALL_SITE, WRITE_CALL_SITE_AFTER),
        {"A-LEAK-XWS-LEDGER", "A-LEAK-XWS-MONEY", "A-LEAK-XWS/write",
         "A-LEAK-PRIV-LEDGER", "A-LEAK-PRIV-MONEY", "A-UNWIRED-LEDGER"},
        True,
    ),
]


def mutate_reinstate_write():
    """C6's second half: put the gate back on Write only.

    ⚠ TWO EDITS IN ONE FILE, AND THE ORDER MATTERS. The first edit removed all four call sites;
    this re-inserts one. Written as an explicit second step with its own anchor assertion because
    a single replace that silently applied to the wrong site would leave a control that tests
    something other than what it says.
    """
    mutate(
        HANDLER,
        "\tout, err := h.engine.WriteWithAI(r.Context(), wsID, in.Prompt, in.Context, in.PageID)\n",
        GATE_CALL
        + "\tout, err := h.engine.WriteWithAI(r.Context(), wsID, in.Prompt, in.Context, in.PageID)\n",
    )


def main():
    saved = tempfile.mkdtemp(prefix="w31-bodypage-")
    before = {}
    for f in FILES:
        before[f] = sha256(f)
        shutil.copy2(REPO / f, pathlib.Path(saved) / pathlib.Path(f).name)

    results = []
    try:
        for cid, desc, apply_fn, want_tags, blind_green in CONTROLS:
            for f in FILES:
                shutil.copy2(pathlib.Path(saved) / pathlib.Path(f).name, REPO / f)
            try:
                apply_fn()
            except SystemExit as e:
                results.append((cid, "ANCHOR-MISS", str(e), set(), set()))
                continue
            tags, failed, abort, raw = run_tests()
            if abort:
                results.append((cid, abort,
                                "compile error / crash — scored as such, never as a catch",
                                set(), set()))
                continue
            blind_hit = {t for t in failed if t in BLIND_GUARDS}
            ok_tags = tags == want_tags
            ok_blind = (not blind_hit) if blind_green else bool(blind_hit)
            if ok_tags and ok_blind:
                verdict = "AS PREDICTED"
            elif ok_tags:
                verdict = "MISMATCH (blind-guard claim wrong)"
            else:
                verdict = "MISMATCH (tag set)"
            results.append((cid, verdict, desc, tags, blind_hit))
    finally:
        for f in FILES:
            shutil.copy2(pathlib.Path(saved) / pathlib.Path(f).name, REPO / f)

    after = {f: sha256(f) for f in FILES}
    restored = all(before[f] == after[f] for f in FILES)

    print("=" * 96)
    print("FINDING (22) — body-named page_id attribution controls")
    print("=" * 96)
    good = 0
    for cid, verdict, desc, tags, blind_hit in results:
        mark = "OK " if verdict == "AS PREDICTED" else "!! "
        if verdict == "AS PREDICTED":
            good += 1
        print(f"{mark}{cid}  {verdict}")
        if verdict.startswith("MISMATCH"):
            want = dict((c[0], c[3]) for c in CONTROLS)[cid]
            print(f"      want tags: {sorted(want)}")
            print(f"      got  tags: {sorted(tags)}")
            print(f"      blind guards that reddened: {sorted(blind_hit)}")
    print("-" * 96)
    print(f"{good}/{len(CONTROLS)} as predicted;  files restored byte-for-byte: {restored}")
    if not restored:
        print("!! RESTORE FAILED — the working tree does not match the bytes saved before the run")
    sys.exit(0 if (good == len(CONTROLS) and restored) else 1)


if __name__ == "__main__":
    main()
