#!/usr/bin/env python3
"""Positive controls for internal/analytics/crossworkspace_rollup_realpg_test.go.

WHAT THIS ANSWERS. The new guard PASSED ON ITS FIRST RUN against the unmodified tree — the
predicates it is about are correct today. A guard that has only ever been green is not evidence
that it can go red, so every control below mutates something REAL and carries a verdict written
down BEFORE the run.

SCORING IS PER-ASSERTION-TAG, NOT PER-TEST, and that is deliberate. tab-5r8k's #186 handover
records two controls in a row that were INVALID (the mutation broke the bind, not the tenancy)
and BOTH scored the predicted red on a test-level check. "The suite went red" is a strictly
weaker claim than "this guard saw it". A control here is CAUGHT only if the exact tags predicted
for it appear in the failure output.

Each control restores the file in a `finally` and verifies the restore by sha256.

    python3 scripts/w31-crossws-rollup-controls-4j7q.py --dsn postgres://...
"""

from __future__ import annotations

import argparse
import hashlib
import pathlib
import re
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
STORE = REPO / "internal" / "analytics" / "store.go"
GUARD = "TestWorkspaceStats_ForeignWorkspaceIsNotRolledUp_RealPG"

# The two tenancy predicates this guard is about, verbatim from store.go.
RANKED_SCOPE = "WHERE pv.workspace_id = $1"
NEVERREAD_SCOPE = "WHERE p.workspace_id = $1 AND p.is_template = false"

# ── The controls. `verdict` is the prediction, written down first.
#
# The `OR TRUE` shape is the census's, and it is chosen over deleting the conjunct on purpose:
# the placeholder stays referenced so the bind still type-checks. #186 recorded that deleting it
# outright (`WHERE TRUE`) drops $1 from the statement, Postgres refuses the bind, and the guard
# then reds on an error that has NOTHING to do with tenancy — an invalid control that scores the
# answer it was looking for.
CONTROLS = [
    dict(
        name="M0",
        why="unmutated tree — the guard must be GREEN before any catch it reports means anything",
        edits=[],
        verdict="GREEN",
        tags=[],
    ),
    dict(
        name="C1",
        why="the ranked query's workspace scope (store.go:454) neutered — the census's SILENT site",
        edits=[(RANKED_SCOPE, "WHERE (pv.workspace_id = $1 OR TRUE)")],
        verdict="RED",
        # The foreign row enters the ranking, so the row/title/total tags move.
        #
        # ⚠ MISPREDICTED ON THE FIRST RUN, AND THE CORRECTION IS THE INTERESTING PART.
        # FOREIGN-UNVIEWERED was predicted to move with them and DID NOT: unique_visitors stayed
        # at 1. The unique-viewers query (store.go:519) is bounded by `page_id = ANY(visibleIDs)`
        # — which under C1 DOES contain the foreign page — but it also carries its own
        # `workspace_id = $1`, and the leaked page's view rows carry wsB, so they are excluded
        # there a second time. So :519 is defence-in-depth in the healthy case and LOAD-BEARING
        # in the leaked one: it is the reason a C1 leak discloses titles and totals but not
        # readers. The prediction is corrected here rather than the tag being quietly dropped.
        tags=["FOREIGN-ABSENT", "FOREIGN-TITLE-ABSENT", "FOREIGN-UNTOTALLED"],
        forbidden_tags=["FOREIGN-UNCOUNTED", "OWN-PRESENT", "PREMISE-FOREIGN-READABLE",
                        "FOREIGN-UNVIEWERED"],
    ),
    dict(
        name="C2",
        why="the never-read cohort's workspace scope (store.go:535) neutered — the other SILENT site",
        edits=[(NEVERREAD_SCOPE, "WHERE (p.workspace_id = $1 OR TRUE) AND p.is_template = false")],
        verdict="RED",
        tags=["FOREIGN-UNCOUNTED"],
        # A count-only leak must not move the ranked cohorts: this is what proves the two
        # predicates are held SEPARATELY rather than by one over-broad assertion.
        forbidden_tags=["FOREIGN-ABSENT", "FOREIGN-UNTOTALLED", "OWN-PRESENT"],
    ),
    dict(
        name="C3",
        why="BOTH scopes neutered — the whole roll-up untenanted",
        edits=[
            (RANKED_SCOPE, "WHERE (pv.workspace_id = $1 OR TRUE)"),
            (NEVERREAD_SCOPE, "WHERE (p.workspace_id = $1 OR TRUE) AND p.is_template = false"),
        ],
        verdict="RED",
        tags=["FOREIGN-ABSENT", "FOREIGN-UNCOUNTED", "FOREIGN-UNTOTALLED"],
        forbidden_tags=["OWN-PRESENT", "PREMISE-FOREIGN-READABLE"],
    ),
    dict(
        name="C4",
        why="THE PREMISE ITSELF: the page-read gate made to refuse everything. [OWN-PRESENT] must "
            "fire — this is what stops an empty roll-up from passing as a clean one",
        edits=[(
            "found, canView := s.access.AuthorizePageRead(ctx, r.PageID)\n\t\tif !found || !canView {",
            "found, canView := s.access.AuthorizePageRead(ctx, r.PageID)\n\t\tif true || !found || !canView {",
        )],
        verdict="RED",
        tags=["OWN-PRESENT"],
        forbidden_tags=["FOREIGN-ABSENT", "FOREIGN-TITLE-ABSENT"],
    ),
    dict(
        name="M1",
        why="an UNRELATED tenancy predicate — the unique-viewers query (store.go:519), which the "
            "census also flagged and which this file argues is defence-in-depth. The guard must "
            "stay GREEN: it is specific to the two load-bearing scopes, not a blanket reddener",
        edits=[(
            "WHERE workspace_id = $1\n              AND created_at > NOW() - INTERVAL '1 day' * $2\n              AND page_id = ANY($3)",
            "WHERE (workspace_id = $1 OR TRUE)\n              AND created_at > NOW() - INTERVAL '1 day' * $2\n              AND page_id = ANY($3)",
        )],
        verdict="GREEN",
        tags=[],
    ),
]

TAG_RE = re.compile(r"\[([A-Z][A-Z0-9-]+)\]")


def run_guard(dsn: str) -> tuple[int, str]:
    proc = subprocess.run(
        ["go", "test", "-count=1", "-run", GUARD, "./internal/analytics/"],
        cwd=REPO,
        env={**__import__("os").environ, "DOCS_TEST_DATABASE_URL": dsn},
        capture_output=True,
        text=True,
        timeout=900,
    )
    return proc.returncode, proc.stdout + proc.stderr


def apply(edits: list[tuple[str, str]], text: str) -> str:
    for old, new in edits:
        n = text.count(old)
        if n != 1:
            raise SystemExit(
                f"INVALID CONTROL: anchor matched {n} times, expected exactly 1.\n"
                f"  anchor: {old[:90]!r}\n"
                "A control whose anchor is stale or ambiguous mutates nothing (or too much) and "
                "scores whatever the run happened to do. Fix the anchor; do not score this run."
            )
        text = text.replace(old, new)
    return text


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--dsn", required=True)
    args = ap.parse_args()

    original = STORE.read_text()
    original_sha = hashlib.sha256(original.encode()).hexdigest()
    print(f"store.go sha256 {original_sha}\n")

    results = []
    for c in CONTROLS:
        print(f"── {c['name']}: {c['why']}")
        print(f"   PREDICTED {c['verdict']}" + (f" on {c['tags']}" if c["tags"] else ""))
        try:
            STORE.write_text(apply(c["edits"], original) if c["edits"] else original)
            code, out = run_guard(args.dsn)
        finally:
            STORE.write_text(original)
            back = hashlib.sha256(STORE.read_text().encode()).hexdigest()
            if back != original_sha:
                raise SystemExit(f"RESTORE FAILED for {c['name']}: {back} != {original_sha}")

        # A build failure is NOT a catch. #186 recorded a control whose first version left a
        # variable unused, and a compile error would have scored as a red.
        if "build failed" in out or "cannot use" in out or "[build failed]" in out:
            print(f"   ACTUAL   BUILD FAILURE — INVALID CONTROL, not scored\n{out[-800:]}\n")
            results.append((c["name"], "INVALID", False))
            continue

        seen = set(TAG_RE.findall(out))
        # Only tags on FAILING lines count; the header comment is not evidence.
        failed_tags = {t for t in seen if any(
            f"[{t}]" in line for line in out.splitlines()
            if line.strip().startswith(("---", "    ")) or "_test.go:" in line
        )}
        actual = "RED" if code != 0 else "GREEN"
        ok = actual == c["verdict"]
        detail = ""
        if c["verdict"] == "RED":
            missing = [t for t in c["tags"] if t not in failed_tags]
            forbidden = [t for t in c.get("forbidden_tags", []) if t in failed_tags]
            if missing:
                ok, detail = False, f" MISSING TAGS {missing}"
            if forbidden:
                ok, detail = False, detail + f" UNEXPECTED TAGS {forbidden}"
        print(f"   ACTUAL   {actual} tags={sorted(failed_tags)} → {'AS PREDICTED' if ok else 'MISMATCH' + detail}\n")
        results.append((c["name"], actual, ok))

    print("── SUMMARY")
    for name, actual, ok in results:
        print(f"   {name:4} {actual:7} {'ok' if ok else 'MISMATCH'}")
    return 0 if all(ok for _, _, ok in results) else 1


if __name__ == "__main__":
    sys.exit(main())
