#!/usr/bin/env python3
"""Positive controls for the MCP read tools' private-space gate (five tools, two fix shapes).

Same scoring discipline as scripts/w31-stalereport-controls.py and w31-pagesearch-visibility-
controls.py, and for the same reason (#69): knowing WHICH TEST reddened is not enough, so this
scores THE SET OF ASSERTION TAGS THAT FIRED, compares it to a per-control prediction written
BEFORE the run, records the OTHER tests that reddened so a control a pre-existing guard also
catches cannot be read as justifying mine, treats a BUILD FAILURE as its own verdict, carries a
NO-OP that must stay GREEN, and restores every mutated file in a `finally` with a sha256 compare.

⚠ A CONTROL IS A LIST OF EDITS, NOT ONE EDIT, AND THAT IS DELIBERATE. C1 is the defect exactly as
it shipped and it takes FOUR removals in one file. A harness that applies one replacement per
control would have to run four controls, or — worse — apply two writes to the same file in
sequence, where the second write reads the ORIGINAL bytes back from disk and silently erases the
first. Every edit of a control is applied to one in-memory copy and written ONCE.

⚠ WHY THERE ARE MORE CONTROLS THAN LEAK ASSERTIONS. Five of this guard's assertions failed on the
unmodified tree; the rest — the over-correction, owner, grant and slot cases — PASSED NECESSARILY,
because an ungated read shows everything to everyone. Those are earned here or they are earned by
nothing.

Run:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-mcpreadgate-controls.py
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SERVER = os.path.join(REPO, "internal/mcp/server.go")
PERM = os.path.join(REPO, "internal/permission/store.go")
PAGESTORE = os.path.join(REPO, "internal/page/store.go")

# ── The four edits that together are the defect exactly as it shipped.
E_DISPATCH = ("""	if gatedReadTools[name] {
		allowed, err := s.authorizeRead(ctx, name, args, m.MemberID)
		if err != nil || !allowed {
			return nil, &rpcError{Code: errUnauthorized, Message: "not authorized: this action requires view access"}
		}
	}

""", "")
E_GETPAGE = ("""	if !s.canViewPage(ctx, p.ID) {
		return nil, &rpcError{Code: errUnauthorized, Message: "not authorized: this action requires view access"}
	}
""", "")
E_SEARCH = ("""		if !s.canViewPage(ctx, r.Page.ID) {
			continue
		}
""", "")
E_TREE = ("""		if !s.canViewSpace(ctx, sp.ID) {
			continue
		}
""", "")
E_ASK = ("""		if !s.canViewPage(ctx, h.Page.ID) {
			continue
		}
		if len(hits) >= askContextPages {
			break
		}
""", "")

CONTROLS = [
    (
        "C0-noop", [(SERVER, "// gatedReadTools are the READ tools", "// gatedReadTools (C0) are the READ tools")], [],
        "MUST STAY GREEN. A comment-only edit. Without it, a harness in which everything reds — "
        "because the tree stopped building — is indistinguishable from a working control set.",
    ),
    (
        "C1-defect-as-shipped",
        [(SERVER, *E_DISPATCH), (SERVER, *E_GETPAGE), (SERVER, *E_SEARCH), (SERVER, *E_TREE),
         (SERVER, *E_ASK)],
        ["M-LEAK-SEARCH", "M-LEAK-GETPAGE", "M-LEAK-LIST", "M-LEAK-TREE", "M-LEAK-ANALYTICS",
         "M-LEAK-ASK", "M-LEAK-ASKCITE", "M-SLOT"],
        "THE DEFECT EXACTLY AS IT SHIPPED — all FIVE gates removed at once. Every leak assertion "
        "must fire. `also-red` is the number that matters here: it is the census of what the "
        "repo's OTHER tests notice about five ungated read tools.\n"
        "        ⚠ [M-SLOT] WAS NOT IN THE FIRST PREDICTION AND ITS FIRING IS CORRECT. With the "
        "filter gone, bob's limit=1 search returns the HIGHEST-RANKED row — which is the hidden "
        "one — and the readable page is absent for the opposite reason to the one M-SLOT was "
        "written for. The slot assertion is therefore NOT independent of the leak; C11 is the "
        "control that isolates the window, and this one only shows the two states share a symptom.",
    ),
    (
        "C2-getpage-ungated", [(SERVER, *E_GETPAGE)], ["M-LEAK-GETPAGE"],
        "The largest payload of the five, and the tool #78's handed-down list did not name. Only "
        "[M-LEAK-GETPAGE] may fire: the other four gates are independent code.",
    ),
    (
        "C3-listpages-unmapped",
        [(SERVER, '\t"list_pages":         true,\n', "")], ["M-LEAK-LIST"],
        "The tool is REMOVED FROM gatedReadTools rather than having its rule deleted — the exact "
        "shape of a future tool added to the dispatch switch and forgotten here.",
    ),
    (
        "C4-analytics-unmapped",
        [(SERVER, '\t"get_page_analytics": true,\n', "")], ["M-LEAK-ANALYTICS"],
        "Same shape as C3 on the other object-keyed tool, and the second one absent from #78's list.",
    ),
    (
        "C5-search-unfiltered", [(SERVER, *E_SEARCH)], ["M-LEAK-SEARCH", "M-SLOT"],
        "The per-row filter deleted. The over-fetch stays, so this isolates the FILTER from the "
        "WINDOW — C11 is the window. ⚠ [M-SLOT] fires with it for C1's reason: the hidden row "
        "ranks first, so at limit=1 it takes the slot instead of being dropped from it.",
    ),
    (
        "C6-tree-unfiltered", [(SERVER, *E_TREE)], ["M-LEAK-TREE"],
        "The per-space filter deleted. get_space_tree is workspace-keyed, so it ENUMERATES: no id "
        "has to be guessed for this one.",
    ),
    (
        "C7-read-gate-requires-edit",
        [(SERVER, 'return s.access.CanViewSpace(ctx, stringArg(args, "space_id", ""), memberID)',
          'return s.access.CanEditSpace(ctx, stringArg(args, "space_id", ""), memberID)'),
         (SERVER, 'return s.access.CanViewPage(ctx, stringArg(args, "page_id", ""), memberID)',
          'return s.access.CanEditPage(ctx, stringArg(args, "page_id", ""), memberID)')],
        ["M-PUBLIST", "M-PUBANALYTICS", "M-GRANT"],
        "THE OVER-CORRECTION DIRECTION FOR THE TWO DISPATCH-GATED TOOLS. Requiring Edit rather "
        "than View costs a PLAIN MEMBER the PUBLIC space, because resolveAccess's public arm "
        "returns AccessView — #78 recorded exactly this and it is why the tier is named, not "
        "assumed. [M-GRANT] fires too: bob's grant is 'view'. [M-OWNER] must stay SILENT — alice "
        "created the space and owner-is-admin outranks any tier.",
    ),
    (
        "C8-getpage-wrong-object",
        [(SERVER, "if !s.canViewPage(ctx, p.ID) {", "if !s.canViewPage(ctx, p.SpaceID) {")],
        ["M-PUBGET", "M-OWNER", "M-GRANT"],
        "THE OVER-CORRECTION DIRECTION FOR get_page, and a PLAUSIBLE mutation rather than a "
        "manufactured one: the page's SPACE id handed to a PAGE check. The meta looker finds no "
        "page with that id, returns an error, and the gate denies — for EVERY caller, so the "
        "public page goes too. [M-LEAK-GETPAGE] must stay SILENT: denying is the correct answer "
        "there, which is precisely why the leak assertions alone cannot justify this gate.",
    ),
    (
        "C9-owner-is-admin-removed",
        [(PERM, "\tif memberID != \"\" && memberID == res.CreatedBy {\n\t\treturn AccessAdmin\n\t}",
          "\tif false && memberID == res.CreatedBy {\n\t\treturn AccessAdmin\n\t}")],
        ["M-OWNER", "M-ORDER"],
        "THE RULE-ENGINE ARM [M-OWNER] IS ABOUT, mutated in the engine rather than in my gate — so "
        "a green [M-OWNER] means the gate really is delegating. Expect a large `also-red`: this "
        "arm is load-bearing across the repo. That is why it is recorded and not counted as "
        "justification on its own.\n"
        "        ⚠ [M-ORDER] WAS NOT IN THE FIRST PREDICTION AND IT IS A PROPERTY OF MY OWN GUARD: "
        "M-ORDER reads the ORDERING out of ALICE's search payload, and alice reaches the private "
        "row only through the arm this control removes. So it is a fact about the store's ranking "
        "measured THROUGH the gate, and any mutation that takes the private row off alice's screen "
        "moves it. C12 is the control that moves M-ORDER for the reason it was written — a change "
        "to the ranking itself — and it fires M-ORDER alone.",
    ),
    (
        "C10-space-grants-ignored",
        [(PERM, "\tfor _, p := range res.SpacePerms {\n\t\tconsider(p)\n\t}",
          "\tfor _, p := range []Permission(nil) {\n\t\tconsider(p)\n\t}")],
        ["M-GRANT"],
        "THE MUTATION ONLY [M-GRANT] SHOULD SEE, and it is asymmetric on purpose: dropping the "
        "SPACE-inherited grants changes CheckPage and NOT CheckSpace, so bob-with-a-grant loses "
        "search_docs and get_page while list_pages / get_space_tree / analytics still allow him. "
        "[M-OWNER] stays silent (alice is admin by creation, not by grant); [M-PREMISE] stays "
        "silent (the public page needs no grant at all).",
    ),
    (
        "C11-no-over-fetch",
        [(SERVER, "\t\tfetchLimit = limit * searchFetchFactor", "\t\tfetchLimit = limit")],
        ["M-SLOT"],
        "THE WINDOW, ISOLATED FROM THE FILTER. With limit=1 the hidden row is the only row the SQL "
        "LIMIT returns, the filter drops it, and bob is told NOTHING MATCHED for a document he may "
        "open. [M-ORDER] is what makes this control possible at all — see C12.",
    ),
    (
        "C12-title-weight-swapped",
        [(PAGESTORE,
          "                setweight(to_tsvector('english', p.title), 'A') ||\n"
          "                setweight(to_tsvector('english', p.content_text), 'B'),\n"
          "                websearch_to_tsquery('english', $2)\n"
          "            ) AS rank,",
          "                setweight(to_tsvector('english', p.title), 'B') ||\n"
          "                setweight(to_tsvector('english', p.content_text), 'A'),\n"
          "                websearch_to_tsquery('english', $2)\n"
          "            ) AS rank,")],
        ["M-ORDER"],
        "THE CATCHER FOR [M-ORDER], MUTATED IN THE STORE THAT OWNS THE RANKING. Swapping the "
        "title/content weights makes the READABLE row rank first, which is the state where "
        "[M-SLOT] can no longer fail — limit=1 then returns the public page whether or not the "
        "window is over-fetched. [M-SLOT] must stay SILENT here, and that silence is the point: it "
        "is a guard passing for the wrong reason, and [M-ORDER] exists to say so. Only the SELECT's "
        "rank expression is touched; the WHERE clause still matches both rows.",
    ),
    (
        "C13-search-filter-drops-everything",
        [(SERVER, "\t\tif !s.canViewPage(ctx, r.Page.ID) {", "\t\tif true {")],
        ["M-PREMISE"],
        "THE VACUITY DIRECTION FOR THE SEARCH HALF. An EMPTY payload satisfies [M-LEAK-SEARCH] "
        "perfectly — 'the private title is absent' is true of a list with nothing in it. "
        "[M-PREMISE] is the t.Fatalf that makes that state loud, and it is the ONLY tag that may "
        "fire, because a Fatalf aborts everything after it.",
    ),
    (
        "C14-tree-filter-drops-everything",
        [(SERVER, "\t\tif !s.canViewSpace(ctx, sp.ID) {", "\t\tif true {")],
        ["M-PUBTREE", "M-OWNER", "M-GRANT"],
        "THE VACUITY DIRECTION FOR THE TREE HALF: an empty tree satisfies [M-LEAK-TREE] perfectly. "
        "[M-PUBTREE] is what refuses it. [M-OWNER] and [M-GRANT] fire with it because both read "
        "the tree among their five tools — recorded rather than trimmed, since a control that "
        "trips a compound assertion has not isolated it.",
    ),
    (
        "C15-ask-corpus-ungated", [(SERVER, *E_ASK)], ["M-LEAK-ASK", "M-LEAK-ASKCITE"],
        "THE SIXTH TOOL, AND THE ONE THE QUEUE ALREADY RECORDED AS CLOSED. #79 gated "
        "internal/ai/handler.go's REST /ask; toolAskDocs never calls it and runs its own "
        "SearchWithRank. Both tags fire together and are NOT redundant: [M-LEAK-ASK] reads the "
        "PROMPT on the wire to Lens (the disclosure the model is asked to answer FROM) and "
        "[M-LEAK-ASKCITE] reads the `sources` array (the deep link handed back). A fix that "
        "scrubbed the citations and left the grounding would move only the second.",
    ),
    (
        "C16-ask-filter-drops-everything",
        [(SERVER, "\t\tif !s.canViewPage(ctx, h.Page.ID) {", "\t\tif true {")],
        ["M-ASKPREMISE"],
        "THE VACUITY DIRECTION FOR ask_docs. An EMPTY corpus satisfies [M-LEAK-ASK] perfectly — "
        "the private body is certainly absent from a prompt containing no documents at all — and "
        "that state is ALSO the ungrounded-answer defect ask_errors_realpg_test.go exists for. "
        "[M-ASKPREMISE] is the t.Fatalf that refuses it, and it is the only tag that may fire.",
    ),
    (
        "C17-ask-window-not-widened",
        [(SERVER, "\t\taskFetch = askContextPages * searchFetchFactor", "\t\taskFetch = askContextPages")],
        [],
        "RECORDED NOT CAUGHT, DELIBERATELY, AND SHIPPED AS DOCUMENTED-INERT. The grounding window "
        "is THREE and this fixture holds TWO pages, so no hidden row can push a readable one out "
        "of it and nothing in this guard can see the over-fetch disappear. Reaching that state "
        "needs three hidden rows ranking above the readable one. The honest record is that the "
        "guard does not cover it — not a fixture inflated until a control passes.",
    ),
]

PKGS = ["./internal/mcp/"]
WIDE = ["./..."]  # C1 and C9 are run repo-wide: the census of who else notices
WIDE_FOR = {"C1-defect-as-shipped", "C9-owner-is-admin-removed"}
# ⚠⚠ ANCHORED TO THE `file.go:NN: ` PREFIX go test PRINTS, AND THE FIRST DRAFT WAS NOT — WHICH MADE
# THE HARNESS REPORT TAGS THAT NEVER FIRED. [M-ORDER]'s failure message NAMES [M-SLOT] in its prose
# ("[M-SLOT] below is vacuous in that state"), so a bare `\[(M-[A-Z-]+)\]` scraped M-SLOT out of
# M-ORDER's own sentence and scored C9 and C12 as DIVERGED. Re-run focused, only M-ORDER had fired.
# An instrument that reads its subject's OUTPUT cannot tell a tag that fired from a tag that was
# QUOTED, and every assertion message in this repo is written to name its neighbours.
TAG_RE = re.compile(r"\.go:\d+: \[(M-[A-Z-]+)\]")
FAIL_RE = re.compile(r"^\s*--- FAIL: (\S+)", re.M)
MINE = {"TestMCPReadTools_PrivateSpace_NotVisibleWithoutGrant_RealPG"}


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def run_tests(pkgs):
    proc = subprocess.run(
        ["go", "test", *pkgs, "-count=1", "-v"],
        cwd=REPO, env=dict(os.environ), capture_output=True, text=True, timeout=1800,
    )
    raw = proc.stdout + proc.stderr
    if ("[build failed]" in raw or "cannot use" in raw or "undefined:" in raw
            or "syntax error" in raw or "declared and not used" in raw):
        return "build", set(), set(), raw
    return (("green" if proc.returncode == 0 else "red"),
            set(TAG_RE.findall(raw)), set(FAIL_RE.findall(raw)), raw)


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set — this suite needs a real Postgres.")
        return 2

    paths = {SERVER, PERM, PAGESTORE}
    originals = {p: open(p, "rb").read() for p in paths}
    before = {p: sha(p) for p in paths}

    verdict, tags, failed, raw = run_tests(PKGS)
    if verdict != "green":
        print(f"BASELINE NOT GREEN ({verdict}) — nothing below can be read.\n{raw[-4000:]}")
        return 1
    print("baseline: GREEN\n")

    results, claimed = [], set()
    try:
        for cid, edits, predicted, why in CONTROLS:
            # Group the edits by file and apply each file's set to ONE in-memory copy, so two
            # edits to the same file cannot erase each other.
            staged, dead = {}, None
            for path, old, new in edits:
                src = staged.get(path, originals[path].decode())
                if src.count(old) != 1:
                    dead = f"{src.count(old)} matches for {old[:48]!r}"
                    break
                staged[path] = src.replace(old, new, 1)
            if dead:
                results.append((cid, "ANCHOR", set(), set(predicted), set()))
                print(f"{cid}: ANCHOR MISSING/AMBIGUOUS ({dead}) — control is dead")
                continue
            for path, src in staged.items():
                with open(path, "w") as f:
                    f.write(src)
            verdict, tags, failed, raw = run_tests(WIDE if cid in WIDE_FOR else PKGS)
            for path in staged:
                with open(path, "wb") as f:
                    f.write(originals[path])
            if verdict == "build":
                results.append((cid, "BUILD", set(), set(predicted), set()))
                print(f"{cid}: BUILD FAILED — not a catch")
                continue
            others = failed - MINE
            ok = (tags == set(predicted)) and (verdict == ("red" if predicted else "green"))
            results.append((cid, "AS PREDICTED" if ok else "DIVERGED", tags, set(predicted), others))
            claimed |= tags
            extra = f"  also-red={sorted(others)}" if others else ""
            print(f"{cid}: {verdict.upper():5s} fired={sorted(tags) or '(none)'} "
                  f"predicted={sorted(predicted) or '(none)'} -> "
                  f"{'AS PREDICTED' if ok else '⚠ DIVERGED'}{extra}")
    finally:
        for p, b in originals.items():
            with open(p, "wb") as f:
                f.write(b)
        after = {p: sha(p) for p in paths}
        for p in paths:
            state = "restored" if after[p] == before[p] else "⚠⚠ NOT RESTORED"
            print(f"  {os.path.relpath(p, REPO)}: {state}")

    declared = {
        "M-PREMISE", "M-ORDER", "M-ASKPREMISE",
        "M-LEAK-SEARCH", "M-LEAK-GETPAGE", "M-LEAK-LIST", "M-LEAK-TREE", "M-LEAK-ANALYTICS",
        "M-LEAK-ASK", "M-LEAK-ASKCITE",
        "M-PUBGET", "M-PUBLIST", "M-PUBTREE", "M-PUBANALYTICS",
        "M-OWNER", "M-GRANT", "M-SLOT",
    }
    good = sum(1 for r in results if r[1] == "AS PREDICTED")
    print(f"\n{good}/{len(results)} controls as predicted")
    print(f"{len(claimed & declared)}/{len(declared)} product assertions claimed by some control")
    unclaimed = sorted(declared - claimed)
    if unclaimed:
        print(f"⚠ CLAIMED BY NOTHING: {unclaimed} — either give each a catcher or DELETE it. "
              f"An assertion no control can move is decoration.")
    return 0 if good == len(results) and not unclaimed else 1


if __name__ == "__main__":
    sys.exit(main())
