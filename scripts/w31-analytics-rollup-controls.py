#!/usr/bin/env python3
"""
Positive controls for the workspace analytics roll-up's private-space gate (W3.1).

WHAT THIS ANSWERS. Two new guards landed:

  internal/analytics/privatespace_realpg_test.go   the FILTER decides correctly  (real Postgres)
  internal/analytics/mainwiring_test.go            the filter is SWITCHED ON     (literal pin)

and one existing unit test grew a fail-closed case. A guard that has never failed is a claim, not
an instrument. Each control below names its PREDICTED CATCHER BEFORE the run, and names a
MUST-STAY-GREEN companion, so a verdict of CAUGHT cannot be earned by a catch-all.

HOW THE VERDICT IS READ, AND WHY NOT FROM THE EXIT CODE. The mutations live in
`internal/analytics` and so do the guards, so a package-level or exit-code verdict would be CAUGHT
for every control and would say nothing about WHICH assertion spoke. The verdict is the SET OF
ASSERTION TAGS that appear in the output — `[LEAK-LIST]`, `[SHORT-LIST]`, `[A-WIRED]`, … — plus,
for the one assertion that carries no tag, the failing test NAME. `go test` prints no PASS lines
without -v and a panic kills a package binary, so absence from a failure list is NOT green: every
run also records the package-level ok/FAIL lines.

SAFETY. Every anchor's occurrence count is asserted BEFORE any file is written; edits accumulate
PER FILE so a two-edit control cannot half-apply; the tree is restored in a `finally` and the
restore is verified against the pre-mutation sha256 of every touched file.

  DOCS_TEST_DATABASE_URL must be set — the real-PG guard FAILS (does not skip) without it, and a
  skipped tenancy suite is indistinguishable from a passing one.
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = "internal/analytics/store.go"
GUARD = "internal/analytics/privatespace_realpg_test.go"
MAIN = "cmd/docs/main.go"

# Every assertion tag either guard can emit. A control's verdict is a subset of this set.
ALL_TAGS = [
    "LEAK-LIST", "LEAK-COUNT", "LEAK-TOTAL", "VISIBLE", "OWNER", "GRANTED",
    "SHORT-LIST", "HONEST-403", "A-WIRED", "A-SENTINEL", "A-STRIP",
]
# The fail-closed case asserts through errors.Is and carries no tag, so it is named by test.
NAMED = ["TestGetWorkspaceStats_NoGate_FailsClosed", "TestGetWorkspaceStats_TopAndBottomPagesAndNeverRead"]


def sha256(path):
    with open(os.path.join(REPO, path), "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def read(path):
    with open(os.path.join(REPO, path), "r", encoding="utf-8") as fh:
        return fh.read()


def write(path, body):
    with open(os.path.join(REPO, path), "w", encoding="utf-8") as fh:
        fh.write(body)


def run_suite():
    """Run the FULL suite and return (exit, stdout+stderr)."""
    env = dict(os.environ)
    p = subprocess.run(
        ["go", "test", "-timeout", "600s", "-count=1", "./..."],
        cwd=REPO, env=env, capture_output=True, text=True,
    )
    return p.returncode, p.stdout + p.stderr


def fired(out):
    """The set of assertion tags and named tests that FAILED in this run."""
    tags = {t for t in ALL_TAGS if re.search(r"\[" + re.escape(t) + r"\]", out)}
    for name in NAMED:
        if re.search(r"--- FAIL: " + re.escape(name) + r"\b", out):
            tags.add(name)
    return tags


def failing_packages(out):
    # `[ \t]`, NOT `\s`. go test prints a bare `FAIL` line before the per-package one, and `\s+`
    # matches the newline between them — so `^FAIL\s+(\S+)` captured the literal word "FAIL" off
    # the NEXT line and every control reported failing_pkgs=['FAIL']. The harness is an instrument
    # too, and this one was reporting a constant.
    return sorted(set(re.findall(r"^FAIL[ \t]+(\S+)", out, re.M)))


# Each control: (id, what it does, [(file, old, new)], predicted-to-fire, predicted-to-stay-silent)
#
# `stay_silent` is the must-stay-green half. It is not "everything else" — it names the specific
# assertions that would make the verdict a catch-all if they also fired.
CONTROLS = [
    (
        "A1",
        "THE DEFECT: both visibility filters removed — the roll-up answers pre-fix",
        [
            (STORE,
             "\t\tfound, canView := s.access.AuthorizePageRead(ctx, r.PageID)\n\t\tif !found || !canView {\n\t\t\tcontinue\n\t\t}\n",
             "\t\t_, _ = s.access.AuthorizePageRead(ctx, r.PageID)\n"),
            (STORE,
             "\t\tif found, canView := s.access.AuthorizePageRead(ctx, id); found && canView {\n\t\t\tout.NeverRead++\n\t\t}\n",
             "\t\t_ = id\n\t\tout.NeverRead++\n"),
        ],
        {"LEAK-LIST", "LEAK-COUNT", "LEAK-TOTAL", "SHORT-LIST"},
        {"VISIBLE", "OWNER", "GRANTED", "HONEST-403", "A-WIRED"},
    ),
    (
        "A2",
        "THE CAP MOVED BACK IN FRONT OF THE FILTER: truncate the ranking to 10, then filter",
        [
            (STORE,
             "\tvisible := make([]ReadStats, 0, len(ranked))",
             "\tif len(ranked) > rollupCap {\n\t\tranked = ranked[:rollupCap]\n\t}\n\tvisible := make([]ReadStats, 0, len(ranked))"),
        ],
        {"SHORT-LIST"},
        {"LEAK-LIST", "LEAK-COUNT", "LEAK-TOTAL", "VISIBLE", "OWNER", "GRANTED", "A-WIRED"},
    ),
    (
        "A3",
        "ONLY the never-read filter removed — the cohort that is a COUNT rather than a row",
        [
            (STORE,
             "\t\tif found, canView := s.access.AuthorizePageRead(ctx, id); found && canView {\n\t\t\tout.NeverRead++\n\t\t}\n",
             "\t\t_ = id\n\t\tout.NeverRead++\n"),
        ],
        {"LEAK-COUNT"},
        {"LEAK-LIST", "LEAK-TOTAL", "SHORT-LIST", "VISIBLE", "OWNER", "GRANTED", "A-WIRED"},
    ),
    (
        "A4",
        "ONLY the total re-derived from the UNFILTERED ranking (the pre-fix workspace-wide count)",
        [
            (STORE,
             "\t\tvisibleIDs = append(visibleIDs, r.PageID)\n\t\tout.TotalViews += r.TotalViews\n",
             "\t\tvisibleIDs = append(visibleIDs, r.PageID)\n"),
            (STORE,
             "\t// Most read: the head of the filtered ranking.",
             "\tfor _, r := range ranked {\n\t\tout.TotalViews += r.TotalViews\n\t}\n\t// Most read: the head of the filtered ranking."),
        ],
        {"LEAK-TOTAL", "SHORT-LIST"},
        {"LEAK-LIST", "LEAK-COUNT", "VISIBLE", "OWNER", "GRANTED", "A-WIRED"},
    ),
    (
        "A5",
        "THE FAIL-CLOSED GATE DELETED: an unwired store answers with an unfiltered roll-up",
        [
            (STORE,
             "\tif s == nil || s.access == nil {\n\t\treturn nil, ErrNoPageReadGate\n\t}",
             "\tif s == nil {\n\t\treturn nil, ErrNoPageReadGate\n\t}"),
        ],
        {"TestGetWorkspaceStats_NoGate_FailsClosed"},
        {"LEAK-LIST", "LEAK-COUNT", "LEAK-TOTAL", "SHORT-LIST", "VISIBLE", "OWNER", "GRANTED", "A-WIRED"},
    ),
    (
        "A6",
        "THE PRODUCTION WIRING DELETED from cmd/docs/main.go (the gate is never switched on)",
        [
            (MAIN,
             "\tanalyticsStore.WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))",
             "\t_ = analyticsStore"),
        ],
        {"A-WIRED"},
        {"LEAK-LIST", "LEAK-COUNT", "LEAK-TOTAL", "SHORT-LIST", "VISIBLE", "OWNER", "GRANTED",
         "TestGetWorkspaceStats_NoGate_FailsClosed"},
    ),
    (
        "A7",
        "THE BLINDING CONTROL: A1's defect WITH the real-PG guard's leak assertions disabled",
        [
            (STORE,
             "\t\tfound, canView := s.access.AuthorizePageRead(ctx, r.PageID)\n\t\tif !found || !canView {\n\t\t\tcontinue\n\t\t}\n",
             "\t\t_, _ = s.access.AuthorizePageRead(ctx, r.PageID)\n"),
            (STORE,
             "\t\tif found, canView := s.access.AuthorizePageRead(ctx, id); found && canView {\n\t\t\tout.NeverRead++\n\t\t}\n",
             "\t\t_ = id\n\t\tout.NeverRead++\n"),
            (GUARD, "func TestWorkspaceAnalytics_PrivateSpace_NotRolledUpWithoutGrant_RealPG(t *testing.T) {",
             "func TestWorkspaceAnalytics_PrivateSpace_NotRolledUpWithoutGrant_RealPG(t *testing.T) {\n\tt.Skip(\"BLINDED BY CONTROL A7\")"),
            (GUARD, "func TestWorkspaceAnalytics_RankedListIsFilledAfterFiltering_RealPG(t *testing.T) {",
             "func TestWorkspaceAnalytics_RankedListIsFilledAfterFiltering_RealPG(t *testing.T) {\n\tt.Skip(\"BLINDED BY CONTROL A7\")"),
        ],
        set(),  # PREDICTED NOT CAUGHT — nothing else in this repo watches this surface
        {"LEAK-LIST", "LEAK-COUNT", "LEAK-TOTAL", "SHORT-LIST", "VISIBLE", "OWNER", "GRANTED", "A-WIRED"},
    ),
    (
        "A8",
        "MUST-STAY-GREEN COMPANION: the wiring comment reworded so the vacuity sentinel vanishes",
        [
            (MAIN, "fifth copy of the same seam", "fifth copy of that same shape"),
        ],
        {"A-SENTINEL"},
        {"LEAK-LIST", "LEAK-COUNT", "LEAK-TOTAL", "SHORT-LIST", "VISIBLE", "OWNER", "GRANTED", "A-WIRED"},
    ),
]


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL is unset — the real-PG guard would FAIL for the wrong "
              "reason and every verdict would be a lie about the instrument.", file=sys.stderr)
        return 2

    touched = sorted({f for _, _, edits, _, _ in CONTROLS for f, _, _ in edits})
    pristine = {f: (read(f), sha256(f)) for f in touched}

    print("=== A0: the unmutated tree (the floor) ===")
    code, out = run_suite()
    base_fired = fired(out)
    print(f"    exit={code} fired={sorted(base_fired) or '(none)'} failing_pkgs={failing_packages(out) or '(none)'}")
    if code != 0 or base_fired:
        print("    A0 IS NOT CLEAN — every verdict below would be unreadable. STOPPING.")
        return 1

    results = []
    try:
        for cid, desc, edits, must_fire, stay_silent in CONTROLS:
            print(f"\n=== {cid}: {desc} ===")
            print(f"    PREDICTED CATCHER: {sorted(must_fire) or 'NOT CAUGHT (nothing sees this)'}")

            # Accumulate per file, and assert EVERY anchor before writing ANYTHING.
            staged = {}
            ok = True
            for path, old, new in edits:
                body = staged.get(path, pristine[path][0])
                n = body.count(old)
                if n != 1:
                    print(f"    ANCHOR ERROR in {path}: {n} occurrences, want exactly 1 — "
                          f"the control did not apply and measures nothing.")
                    ok = False
                    break
                staged[path] = body.replace(old, new, 1)
            if not ok:
                results.append((cid, "ANCHOR-ERROR", set(), must_fire))
                continue

            for path, body in staged.items():
                write(path, body)
                assert sha256(path) != pristine[path][1], f"{path} bytes did not move"

            code, out = run_suite()
            got = fired(out)
            print(f"    exit={code} fired={sorted(got) or '(none)'} failing_pkgs={failing_packages(out) or '(none)'}")

            # restore before judging, so a judging bug cannot leave the tree dirty
            for path in staged:
                write(path, pristine[path][0])

            missing = must_fire - got
            noise = got & set(stay_silent)
            if missing:
                verdict = f"WRONG PREDICTION — expected {sorted(missing)} to fire and it did not"
            elif noise:
                verdict = f"CAUGHT BUT NOT SOLELY — {sorted(noise)} also fired (a catch-all verdict)"
            elif must_fire:
                verdict = "CAUGHT, by the predicted assertion only"
            elif got:
                verdict = f"UNEXPECTEDLY CAUGHT by {sorted(got)}"
            else:
                verdict = "NOT CAUGHT (as predicted)"
            print(f"    VERDICT: {verdict}")
            results.append((cid, verdict, got, must_fire))
    finally:
        for path, (body, digest) in pristine.items():
            write(path, body)
            now = sha256(path)
            state = "restored" if now == digest else f"⚠ RESTORE FAILED ({now} != {digest})"
            print(f"[restore] {path}: {state}")

    print("\n=== SUMMARY ===")
    for cid, verdict, got, must in results:
        print(f"  {cid}: {verdict}")
    bad = [c for c, v, _, _ in results if not (v.startswith("CAUGHT, by") or v.startswith("NOT CAUGHT"))]
    print(f"\n{len(results) - len(bad)}/{len(results)} controls as predicted.")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
