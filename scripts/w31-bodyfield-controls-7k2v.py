#!/usr/bin/env python3
"""Positive controls for internal/bodyfieldcensus — the decoded-but-unused authority census.

WHY THIS FILE EXISTS. The census went GREEN on its first run after the one live field was
deleted, which is precisely the state a guard that cannot fail is in. Three sessions in this
queue have shipped a guard that could not fail; each was caught only by a control. So every
claim the census makes is armed here as a real mutation of the tree and the PREDICTED CATCHER is
named BEFORE the run, so a control that fires for the wrong reason is a failed control and not a
pass.

METHOD, per control:
  · name the predicted catcher first (test name, or "NOT CAUGHT" for a blindness control)
  · sha256 every file before touching it
  · mutate, run the FULL Go suite (go test -count=1 ./...) against real Postgres
  · restore in a `finally` and sha256-compare — a restore that does not match is a hard error
  · compare the observed failing tests against the prediction

  ⚠ -race IS NOT USED IN THE CONTROLS AND IS NOT NEEDED: every claim here is about WHICH test
  catches a STATIC defect, which is not a scheduling question. The final tree is verified
  separately with `-race`, as CI runs it.

USAGE:
  DOCS_TEST_DATABASE_URL=postgres://postgres:postgres@localhost:55817/postgres?sslmode=disable \
      python3 scripts/w31-bodyfield-controls-7k2v.py
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

CENSUS = "internal/bodyfieldcensus/census_test.go"
COMMENT = "internal/comment/handler.go"
APPROVAL = "internal/approval/handler.go"
AI = "internal/ai/handler.go"
MODEL_SPACE = "internal/model/model.go"

CENSUS_TEST = "TestNoDecodedButUnusedIdentityField"
STALE_TEST = "TestExemptListHasNoStaleEntries"
RESOLVE_TEST = "TestBodyDecodeTargetsAllResolve"


def sha(path):
    with open(os.path.join(REPO, path), "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def read(path):
    with open(os.path.join(REPO, path), encoding="utf-8") as f:
        return f.read()


def write(path, text):
    with open(os.path.join(REPO, path), "w", encoding="utf-8") as f:
        f.write(text)


def run_suite():
    """Full Go suite. Returns (ok, set_of_failing_tests, set_of_failing_packages)."""
    p = subprocess.run(
        ["go", "test", "-timeout", "600s", "-count=1", "./..."],
        cwd=REPO, capture_output=True, text=True,
    )
    out = p.stdout + p.stderr
    tests = set(re.findall(r"^\s*--- FAIL: (\S+)", out, re.M))
    pkgs = set(re.findall(r"^FAIL\s+(github\.com/\S+)", out, re.M))
    return p.returncode == 0, tests, pkgs


class Control:
    def __init__(self, name, predicted, edits, note):
        self.name = name
        self.predicted = predicted  # set of test names, or empty set for "NOT CAUGHT"
        self.edits = edits          # list of (path, transform)
        self.note = note


def apply(edits):
    """Apply each transform; hard-fail if a transform is a no-op (the fixture rotted)."""
    for path, fn in edits:
        before = read(path)
        after = fn(before)
        if after == before:
            raise SystemExit(f"FIXTURE ROT: transform on {path} changed nothing — "
                             f"the anchor text is gone, so this control was measuring nothing.")
        write(path, after)


def controls():
    def restore_replybody(s):
        return s.replace(
            '\tContent    string `json:"content"`\n\tAuthorName string `json:"author_name"`',
            '\tContent    string `json:"content"`\n\tAuthorID   string `json:"author_id"`\n'
            '\tAuthorName string `json:"author_name"`',
        )

    def fake_field_on_approval(s):
        # decideBody in a DIFFERENT package, never mentioned by Decide.
        return s.replace(
            '\tDecision string `json:"decision"`',
            '\tDecision string `json:"decision"`\n\tDecidedBy string `json:"decided_by"`',
        )

    def fake_field_on_anon(s):
        # The Ask handler decodes an ANONYMOUS struct literal. If the census cannot resolve those,
        # eleven of the thirty-three sites are silently zero-field.
        return s.replace(
            '\t\tQuestion string `json:"question"`',
            '\t\tQuestion string `json:"question"`\n\t\tViewerID string `json:"viewer_id"`',
        )

    def fake_field_on_model_space(s):
        # A CROSS-PACKAGE named type, resolved through internal/space's import table. Anchored on
        # Space's `private` line, which occurs once — `created_by` occurs three times in model.go
        # and would have armed three structs at once, blurring which path the census used.
        anchor = '\tPrivate     bool      `json:"private"      db:"private"`\n'
        return s.replace(
            anchor,
            anchor + '\tOwnerID     string    `json:"owner_id"    db:"-"`\n',
            1,
        )

    def non_identity_field_on_replybody(s):
        return s.replace(
            '\tContent    string `json:"content"`\n\tAuthorName string `json:"author_name"`',
            '\tContent    string `json:"content"`\n\tColour     string `json:"colour"`\n'
            '\tAuthorName string `json:"author_name"`',
        )

    def blind_the_matcher(s):
        # Narrow the decode matcher so it matches nothing — the quiet way a census dies.
        return s.replace(
            'if !ok || sel.Sel.Name != "Decode" || len(call.Args) != 1 {',
            'if !ok || sel.Sel.Name != "Decode" || len(call.Args) != 99 {',
        )

    def bogus_exemption(s):
        return s.replace(
            '\t"internal/model.Page.UpdatedBy":',
            '\t"internal/nowhere.Ghost.MemberID": "a field that does not exist",\n'
            '\t"internal/model.Page.UpdatedBy":',
        )

    def exempt_the_replybody_field(s):
        return s.replace(
            '\t"internal/model.Page.UpdatedBy":',
            '\t"internal/comment.replyBody.AuthorID": "deliberately exempted by control B9",\n'
            '\t"internal/model.Page.UpdatedBy":',
        )

    def delete_census(s):
        return "package bodyfieldcensus\n"

    return [
        Control("B0 pristine", set(), [], "the tree as it will be merged"),
        Control("B1 THE DEFECT: replyBody.AuthorID restored", {CENSUS_TEST},
                [(COMMENT, restore_replybody)],
                "the live finding, put back. Must be caught by the NEW test ALONE — if any "
                "pre-existing test also reds, this census is not what closed the hole."),
        Control("B2 fake identity field on a DIFFERENT body struct in a DIFFERENT package",
                {CENSUS_TEST}, [(APPROVAL, fake_field_on_approval)],
                "the control the handover named: without it the census would be one hardcoded "
                "field's worth of guard."),
        Control("B3 fake identity field on an ANONYMOUS body struct", {CENSUS_TEST},
                [(AI, fake_field_on_anon)],
                "eleven of the thirty-three decode targets are anonymous struct literals; if "
                "resolution of those is dead code the census is a third blind and still green."),
        Control("B4 fake identity field on a CROSS-PACKAGE named type", {CENSUS_TEST},
                [(MODEL_SPACE, fake_field_on_model_space)],
                "model.Space is resolved through internal/space's import table, not by package "
                "base name; this is the path that would silently return nil."),
        Control("B5 MUST STAY GREEN: a NON-identity unmentioned field", set(),
                [(COMMENT, non_identity_field_on_replybody)],
                "stops CAUGHT from meaning 'a field was added'. An unused colour field is not an "
                "authority claim and must not red CI."),
        Control("B6 THE BLINDNESS: the defect with the census file GUTTED", set(),
                [(COMMENT, restore_replybody), (CENSUS, delete_census)],
                "this is the state main was in at b98933b. The whole suite must be GREEN — that "
                "is the measurement that says nothing else in the repository could see it."),
        Control("B7 the matcher narrowed to match nothing, defect armed",
                {RESOLVE_TEST, STALE_TEST},
                [(COMMENT, restore_replybody), (CENSUS, blind_the_matcher)],
                "a census whose population quietly goes to zero must RED on the instrument checks, "
                "not pass. Predicted catchers are the FLOOR and the anti-rot test — and NOT the "
                "census rule itself, which sees an empty population and is happy. That is the "
                "whole reason those two exist."),
        Control("B8 a stale exemption naming a field that does not exist", {STALE_TEST},
                [(CENSUS, bogus_exemption)],
                "an exempt list that is never checked rots into an unfalsifiable claim and "
                "silently pre-approves any field that later takes the name."),
        Control("B9 the escape hatch works: defect armed AND exempted", set(),
                [(COMMENT, restore_replybody), (CENSUS, exempt_the_replybody_field)],
                "the exemption must actually suppress, or the only way past the census would be "
                "to weaken it."),
    ]


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        raise SystemExit("DOCS_TEST_DATABASE_URL is not set — the real-PG half would fail for "
                         "the wrong reason and every control would read as CAUGHT.")

    results = []
    for c in controls():
        paths = sorted({p for p, _ in c.edits})
        before = {p: sha(p) for p in paths}
        originals = {p: read(p) for p in paths}
        predicted = ", ".join(sorted(c.predicted)) if c.predicted else "NOT CAUGHT (green)"
        print(f"\n=== {c.name}\n    PREDICTED CATCHER: {predicted}\n    {c.note}", flush=True)
        try:
            apply(c.edits)
            ok, tests, pkgs = run_suite()
        finally:
            for p, text in originals.items():
                write(p, text)
            for p in paths:
                if sha(p) != before[p]:
                    raise SystemExit(f"RESTORE FAILED for {p} — the tree is dirty, stop here.")

        observed = tests
        match = observed == c.predicted
        # A control predicting green must also have an all-green suite, not merely the absence of
        # the predicted names.
        if not c.predicted and not ok:
            match = False
        print(f"    OBSERVED : {', '.join(sorted(observed)) if observed else 'nothing (suite green)'}"
              f"   suite_ok={ok}   failing_pkgs={sorted(pkgs)}")
        print(f"    -> {'MATCH' if match else 'MISMATCH'}")
        results.append((c.name, predicted, sorted(observed), match))

    print("\n================ SUMMARY ================")
    for name, predicted, observed, match in results:
        print(f"{'PASS' if match else 'FAIL'}  {name}\n      predicted={predicted}\n      observed={observed}")
    bad = [r for r in results if not r[3]]
    print(f"\n{len(results) - len(bad)}/{len(results)} controls matched their prediction")
    if bad:
        sys.exit(1)


if __name__ == "__main__":
    main()
