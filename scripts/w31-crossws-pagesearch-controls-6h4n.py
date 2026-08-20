#!/usr/bin/env python3
"""Positive controls for the cross-workspace page-search guard (W3.1, tab-6h4n).

THE FINDING THIS EXISTS FOR. `page.Store.Search`'s `WHERE workspace_id = $1` is the ONLY tenancy
scope on two shipped surfaces — `GET /v1/workspaces/{wsID}/pages/search` and
`POST /v1/workspaces/{wsID}/ai/ask` — and the filter under BOTH is `AuthorizePageRead`, which
answers about the CALLER rather than about the WORKSPACE. Measured at `d8d9b4e`: widening that
predicate to `(workspace_id = $1 OR TRUE)` left `go test -count=1 ./...` at EXIT 0. The whole
repository was blind to its removal.

WHY THE CONTROLS ARE SCORED PER ASSERTION TAG AND NOT PER EXIT CODE. Four of the six mutations
below are expected to red — but a guard that reds for the WRONG reason is not a guard. M5 and M6
red at the PREMISE tags ([C-REACH], [P-SEARCH]) rather than the leak tags, and that difference is
the whole content of those two controls: it is what shows the fixture's dual membership and its
public spaces are load-bearing rather than incidental. An exit-code harness scores M1 and M6
identically and learns nothing.

WHY THERE IS AN INVALID CLASS. A run that never reached an assertion — a compile error, a
Postgres that is not up — exits non-zero and would be scored a CATCH by a harness reading
`exit != 0`. That is not hypothetical: it happened to tab-4j7q on this item (the red-first run of
the semantic guard failed with `connection refused` after its own container was removed, and a
looser harness would have accepted the guard on evidence it never produced). A run whose output
contains none of the expected tags and no PASS is reported INVALID, never CATCH.

Every mutation is written, measured, and restored in a `finally`, with a sha256 of every touched
file compared before and after, so a control cannot leave a tree behind.

Usage:
  DOCS_TEST_DATABASE_URL=postgres://... python3 scripts/w31-crossws-pagesearch-controls-6h4n.py
  ... [--only M1,M5] [--list]
"""

import argparse
import hashlib
import re
import os
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

STORE = "internal/page/store.go"
PAGE_HANDLER = "internal/page/handler.go"
AI_HANDLER = "internal/ai/handler.go"
GUARD = "internal/page/crossworkspace_search_realpg_test.go"

TEST = "TestPageSearch_DoesNotCrossWorkspaces_ForADualMember_RealPG"

# The tenancy predicate under test. Anchored on the following line so it cannot match the
# identically-spelled predicate in GetStalePages, which is a DIFFERENT finding (measured NOT
# silent — the stale digest's per-workspace counts red on the same edit).
SEARCH_PREDICATE = "WHERE workspace_id = $1\n          AND to_tsvector"

MUTATIONS = [
    {
        "id": "M1",
        "why": "The finding itself: the workspace predicate made inert while every other line "
               "stands. Both surfaces must red — one on the row, one on the quoted body.",
        "expect_red": ["X-SEARCH", "X-ASK"],
        "expect_green": ["C-REACH", "P-SEARCH", "P-ASK"],
        "edits": [(STORE, SEARCH_PREDICATE,
                   "WHERE (workspace_id = $1 OR TRUE)\n          AND to_tsvector")],
    },
    {
        "id": "M2",
        "why": "The predicate narrowed to NOTHING rather than widened — the opposite failure, and "
               "the one an absence-only guard cannot see. Every leak tag stays green on a tree "
               "that serves no one anything, which is why the PREMISE tags exist. (The first "
               "draft of this control bound the predicate to $2, the query TEXT; Postgres refused "
               "the comparison, the route 500ed, and the run was scored INVALID — correctly. A "
               "mutation the database will not execute measures nothing.)",
        "expect_red": ["P-SEARCH", "C-REACH", "P-ASK"],
        "expect_green": ["X-SEARCH", "X-ASK"],
        "edits": [(STORE, SEARCH_PREDICATE,
                   "WHERE workspace_id = $1 AND workspace_id = 'ws_matches_nothing'"
                   "\n          AND to_tsvector")],
    },
    {
        "id": "M3",
        "why": "The CALLER gate on the search surface made a pass-through. Must stay GREEN here: "
               "this file measures the WORKSPACE scope, and if it reddened on a caller-gate edit "
               "it would be a duplicate of privatespace_realpg_test.go rather than a new guard. "
               "(That file is excluded from this run precisely so this control is readable — it "
               "reds on this mutation, and should.)",
        "expect_red": [],
        "expect_green": ["X-SEARCH", "X-ASK", "C-REACH", "P-SEARCH", "P-ASK"],
        "edits": [(PAGE_HANDLER,
                   "\tout := make([]model.Page, 0, len(rows))\n"
                   "\tfor _, p := range rows {\n"
                   "\t\tfound, canView := h.access.AuthorizePageRead(ctx, p.ID)",
                   "\tif true {\n\t\treturn rows\n\t}\n"
                   "\tout := make([]model.Page, 0, len(rows))\n"
                   "\tfor _, p := range rows {\n"
                   "\t\tfound, canView := h.access.AuthorizePageRead(ctx, p.ID)")],
    },
    {
        "id": "M4",
        "why": "The CALLER gate on the /ask surface made a pass-through. Same reasoning as M3, "
               "for the other consumer of the same store method.",
        "expect_red": [],
        "expect_green": ["X-SEARCH", "X-ASK", "C-REACH", "P-SEARCH", "P-ASK"],
        "edits": [(AI_HANDLER,
                   "\tout := make([]model.Page, 0, len(pages))\n"
                   "\tfor _, p := range pages {\n"
                   "\t\tfound, canView := h.access.AuthorizePageRead(ctx, p.ID)",
                   "\tif true {\n\t\treturn pages\n\t}\n"
                   "\tout := make([]model.Page, 0, len(pages))\n"
                   "\tfor _, p := range pages {\n"
                   "\t\tfound, canView := h.access.AuthorizePageRead(ctx, p.ID)")],
    },
    {
        "id": "M5",
        "why": "THE FIXTURE CONTROL. bob reduced to ONE membership — the shape of every other "
               "cross-tenant fixture in this repo. [C-REACH] must fire: with a single-workspace "
               "caller the caller gate does the separating, the predicate under test becomes "
               "unreachable as a cause, and the leak assertions would pass with it DELETED. This "
               "is why the class was silent for as long as it was.",
        "expect_red": ["C-REACH"],
        "expect_green": ["X-SEARCH", "P-SEARCH", "X-ASK", "P-ASK"],
        "edits": [(GUARD,
                   "\tbobBoth := []authz.Membership{\n"
                   "\t\t{WorkspaceID: wsHome, MemberID: bobHome},\n"
                   "\t\t{WorkspaceID: wsOther, MemberID: bobOther},\n\t}",
                   "\tbobBoth := []authz.Membership{\n"
                   "\t\t{WorkspaceID: wsHome, MemberID: bobHome},\n\t}\n\t_ = bobOther")],
    },
    {
        "id": "M6",
        "why": "THE OTHER FIXTURE CONTROL, and it is the one that would have made this whole file "
               "a decoration. Both spaces made PRIVATE *and* the predicate made inert. The "
               "permission engine then hides B's page for its own reasons, so [X-SEARCH]/[X-ASK] "
               "come back GREEN ON A TREE WITH NO TENANCY SCOPE AT ALL — the defect fully masked. "
               "The PREMISE tags are what fail. This is the measurement behind the header's "
               "'both spaces are PUBLIC' line.",
        "expect_red": ["P-SEARCH", "C-REACH", "P-ASK"],
        "expect_green": ["X-SEARCH", "X-ASK"],
        "edits": [
            (STORE, SEARCH_PREDICATE,
             "WHERE (workspace_id = $1 OR TRUE)\n          AND to_tsvector"),
            (GUARD, 'seedSpaceP(t, d, wsHome, aliceHome, "A Handbook", false)',
             'seedSpaceP(t, d, wsHome, aliceHome, "A Handbook", true)'),
            (GUARD, 'seedSpaceP(t, d, wsOther, aliceOther, "B Ops", false)',
             'seedSpaceP(t, d, wsOther, aliceOther, "B Ops", true)'),
        ],
    },
]


def sha(path):
    with open(os.path.join(REPO, path), "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def read(path):
    with open(os.path.join(REPO, path), encoding="utf-8") as fh:
        return fh.read()


def write(path, text):
    with open(os.path.join(REPO, path), "w", encoding="utf-8") as fh:
        fh.write(text)


def run_guard():
    """Run ONLY the guard under test. Returns (exit, combined output).

    -count=1 so a cached PASS from the previous mutation cannot be reported as this one's result.
    -v so the PASS/FAIL verdict is in the output: without it a green run prints only `ok`, and
    "reached an assertion" — the INVALID test below — cannot be distinguished from "exited 0
    because nothing ran".
    The package's own privatespace test is deliberately NOT in scope: M3/M4 red it by design, and
    mixing that into the tags would make the must-stay-green controls unreadable.
    """
    env = dict(os.environ)
    proc = subprocess.run(
        ["go", "test", "-count=1", "-v", "-run", TEST, "./internal/page/"],
        cwd=REPO, env=env, capture_output=True, text=True, timeout=600,
    )
    return proc.returncode, proc.stdout + proc.stderr


TAGS = ("X-SEARCH", "P-SEARCH", "C-REACH", "X-ASK", "P-ASK")

# A tag counts as FIRED only where it opens a go-test failure line — `foo_test.go:123: [TAG] …`.
#
# ⚠ THE LOOSE VERSION OF THIS WAS WRONG AND THE CONTROLS CAUGHT IT. The first draft asked
# `"[X-SEARCH]" in out`, and scored M6 a CATCH on X-SEARCH because a DIFFERENT assertion's message
# names that tag in prose ("…so this must hold for [X-SEARCH] to mean anything"). The harness
# reported observing a leak that did not happen. That is precisely the failure mode this whole item
# is about — an instrument that asserts coverage it does not have — reproduced inside the
# instrument. The cross-references in those messages have since been reworded too, but the anchor
# is what makes the scoring correct rather than the wording.
FIRED_RE = re.compile(r"^\s*\w+_test\.go:\d+:\s*\[([A-Z-]+)\]", re.M)

# The test logs `[EVAL] TAG` immediately before each assertion. Without it, "the tag did not fire"
# means either "evaluated and passed" or "never ran because an earlier assertion aborted" — and a
# must-stay-green control cannot tell those apart. Green is only credited where EVAL is present.
EVAL_RE = re.compile(r"\[EVAL\]\s+([A-Z-]+)")


def tags_in(out):
    return {m for m in FIRED_RE.findall(out) if m in TAGS}


def evaluated_in(out):
    return {m for m in EVAL_RE.findall(out) if m in TAGS}


def classify(exit_code, out, mut):
    """CATCH / MISS / INVALID, scored on the assertion tags rather than the exit code."""
    fired = tags_in(out)
    seen = evaluated_in(out)
    reached = ("--- PASS" in out) or ("--- FAIL" in out and (fired or seen))
    if not reached:
        return "INVALID", fired, ("the run never reached an assertion — no verdict line and no "
                                  "tagged failure. Compile error, or a query the database "
                                  "refused; scored INVALID, not a catch")
    want_red = set(mut["expect_red"])
    want_green = set(mut["expect_green"])
    if fired & want_green:
        return "MISS", fired, "a must-stay-green tag fired: %s" % sorted(fired & want_green)
    unevaluated = want_green - seen
    if unevaluated:
        return "INVALID", fired, ("green claimed for %s, but the test never reached %s — not "
                                  "evaluated is not the same as passed"
                                  % (sorted(want_green), sorted(unevaluated)))
    if want_red - fired:
        return "MISS", fired, "predicted red did not fire: %s" % sorted(want_red - fired)
    if not want_red and exit_code != 0:
        return "MISS", fired, "expected a clean pass, got exit %d" % exit_code
    return "CATCH", fired, "as predicted"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--only", default="")
    ap.add_argument("--list", action="store_true")
    args = ap.parse_args()

    if args.list:
        for m in MUTATIONS:
            print("%s  red=%-22s green=%s" % (m["id"], ",".join(m["expect_red"]) or "-",
                                              ",".join(m["expect_green"]) or "-"))
        return 0

    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL is unset — every case would be INVALID. Refusing to run.",
              file=sys.stderr)
        return 2

    only = {s.strip() for s in args.only.split(",") if s.strip()}
    selected = [m for m in MUTATIONS if not only or m["id"] in only]

    # A baseline first: an unmutated tree must be GREEN, or nothing below is interpretable.
    code, out = run_guard()
    if code != 0 or "PASS" not in out:
        print("BASELINE FAILED — the guard does not pass on an unmutated tree; every result "
              "below would be meaningless.\n" + out, file=sys.stderr)
        return 2
    print("BASELINE  green (unmutated tree)\n")

    results = []
    for mut in selected:
        touched = sorted({p for p, _, _ in mut["edits"]})
        before = {p: sha(p) for p in touched}
        originals = {p: read(p) for p in touched}
        try:
            for path, old, new in mut["edits"]:
                text = read(path)
                if text.count(old) != 1:
                    raise SystemExit("%s: anchor for %s matched %d times, expected 1 — the "
                                     "harness is stale against the tree it is measuring"
                                     % (mut["id"], path, text.count(old)))
                write(path, text.replace(old, new, 1))
            code, out = run_guard()
            verdict, fired, note = classify(code, out, mut)
        finally:
            for path, text in originals.items():
                write(path, text)
        after = {p: sha(p) for p in touched}
        for p in touched:
            if before[p] != after[p]:
                raise SystemExit("%s: %s was NOT restored (sha256 %s → %s)"
                                 % (mut["id"], p, before[p][:12], after[p][:12]))
        results.append((mut["id"], verdict, sorted(fired), note))
        print("%-3s %-8s fired=%-34s %s" % (mut["id"], verdict, ",".join(sorted(fired)) or "-", note))
        print("      %s\n" % mut["why"])

    bad = [r for r in results if r[1] != "CATCH"]
    print("%d/%d as predicted" % (len(results) - len(bad), len(results)))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
