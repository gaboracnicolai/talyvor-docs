#!/usr/bin/env python3
"""Positive controls for internal/search/crossworkspace_semantic_realpg_test.go.

Same discipline as scripts/w31-crossws-rollup-controls-4j7q.py (#187), and the same reason:
a guard that has only ever been green is not evidence that it can go red. Every control mutates
something real, carries a verdict written down BEFORE the run, is scored PER-ASSERTION-TAG rather
than per-test, restores in a `finally` and verifies the restore by sha256.

⚠ THE INFRASTRUCTURE RED IS THE ONE TO WATCH HERE. The first red-first run of this guard failed
with "dial tcp 127.0.0.1:55432: connect: connection refused" — the Postgres container had been
removed after the previous item. A harness that scores "exit != 0" as CAUGHT would have recorded
that as a catch and the guard would have been accepted on evidence it never produced. Hence
`is_infrastructure_failure`: a run that never reached an assertion is INVALID, not a red.

    python3 scripts/w31-crossws-semantic-controls-4j7q.py --dsn postgres://...
"""

from __future__ import annotations

import argparse
import hashlib
import os
import pathlib
import re
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
SEMANTIC = REPO / "internal" / "search" / "semantic.go"
HANDLER = REPO / "internal" / "search" / "handler.go"
GUARD = "TestSemanticSearch_ForeignWorkspaceIsNotSearched_RealPG"

SEARCH_SCOPE = "WHERE p.workspace_id = $2 AND p.is_template = false"
VISIBLE_FILTER = "found, canView := h.access.AuthorizePageRead(ctx, r.PageID)\n\t\tif !found || !canView {"

CONTROLS = [
    dict(
        name="M0", target=None, edits=[],
        why="unmutated tree — the guard must be GREEN before any catch it reports means anything",
        verdict="GREEN", tags=[],
    ),
    dict(
        name="C1", target="semantic",
        edits=[(SEARCH_SCOPE, "WHERE (p.workspace_id = $2 OR TRUE) AND p.is_template = false")],
        why="semantic.go:287 neutered — the census's SILENT site, in the census's own `OR TRUE` "
            "shape so the placeholder stays bound and Postgres can still infer its type",
        verdict="RED", tags=["FOREIGN-ABSENT"],
        # If either of these fires, the run is red for a reason other than tenancy.
        forbidden_tags=["OWN-PRESENT", "PREMISE-FOREIGN-READABLE"],
    ),
    dict(
        name="C2", target="handler",
        edits=[(VISIBLE_FILTER, VISIBLE_FILTER.replace("if !found", "if true || !found"))],
        why="THE PREMISE ITSELF: visibleTo made to drop every row. [PREMISE-FOREIGN-READABLE] must "
            "fire FIRST — it is a t.Fatalf — which is what proves the premise is load-bearing "
            "rather than decorative, and that an empty result cannot pass this file",
        verdict="RED", tags=["PREMISE-FOREIGN-READABLE"],
        forbidden_tags=["FOREIGN-ABSENT"],
    ),
    dict(
        name="M1", target="semantic",
        # is_template is a cohort predicate, not a tenancy one: neutering it changes which pages
        # are searchable, but neither seeded page is a template, so nothing this guard asserts
        # about can move. A guard that reddens here would be a blanket suite-reddener.
        edits=[(SEARCH_SCOPE, "WHERE p.workspace_id = $2 AND (p.is_template = false OR TRUE)")],
        why="an UNRELATED predicate in the SAME statement (is_template) neutered — the guard must "
            "stay GREEN, so it is specific to the tenancy conjunct rather than to the query",
        verdict="GREEN", tags=[],
    ),
]

TAG_RE = re.compile(r"\[([A-Z][A-Z0-9-]+)\]")
INFRA = ("connection refused", "dial error", "DOCS_TEST_DATABASE_URL is not set",
         "admin connect", "[build failed]", "build failed")


def is_infrastructure_failure(out: str) -> str | None:
    for marker in INFRA:
        if marker in out:
            return marker
    return None


def run_guard(dsn: str) -> tuple[int, str]:
    proc = subprocess.run(
        ["go", "test", "-count=1", "-run", GUARD, "./internal/search/"],
        cwd=REPO, env={**os.environ, "DOCS_TEST_DATABASE_URL": dsn},
        capture_output=True, text=True, timeout=900,
    )
    return proc.returncode, proc.stdout + proc.stderr


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--dsn", required=True)
    args = ap.parse_args()

    files = {"semantic": SEMANTIC, "handler": HANDLER}
    originals = {k: v.read_text() for k, v in files.items()}
    shas = {k: hashlib.sha256(v.encode()).hexdigest() for k, v in originals.items()}
    for k, s in shas.items():
        print(f"{files[k].name} sha256 {s}")
    print()

    results = []
    for c in CONTROLS:
        print(f"── {c['name']}: {c['why']}")
        print(f"   PREDICTED {c['verdict']}" + (f" on {c['tags']}" if c["tags"] else ""))
        target = c["target"]
        try:
            if target:
                text = originals[target]
                for old, new in c["edits"]:
                    n = text.count(old)
                    if n != 1:
                        raise SystemExit(
                            f"INVALID CONTROL {c['name']}: anchor matched {n} times, expected 1.\n"
                            f"  anchor: {old[:90]!r}\nFix the anchor; do not score this run.")
                    text = text.replace(old, new)
                files[target].write_text(text)
            code, out = run_guard(args.dsn)
        finally:
            for k, v in files.items():
                v.write_text(originals[k])
                back = hashlib.sha256(v.read_text().encode()).hexdigest()
                if back != shas[k]:
                    raise SystemExit(f"RESTORE FAILED for {c['name']} / {k}: {back} != {shas[k]}")

        infra = is_infrastructure_failure(out)
        if infra:
            print(f"   ACTUAL   INVALID — the run never reached an assertion ({infra!r}); "
                  f"NOT scored as a red\n")
            results.append((c["name"], "INVALID", False))
            continue

        failed_tags = {t for t in TAG_RE.findall(out)
                       if any(f"[{t}]" in ln for ln in out.splitlines() if "_test.go:" in ln)}
        actual = "RED" if code != 0 else "GREEN"
        ok, detail = actual == c["verdict"], ""
        if c["verdict"] == "RED":
            missing = [t for t in c["tags"] if t not in failed_tags]
            forbidden = [t for t in c.get("forbidden_tags", []) if t in failed_tags]
            if missing:
                ok, detail = False, f" MISSING TAGS {missing}"
            if forbidden:
                ok, detail = False, detail + f" UNEXPECTED TAGS {forbidden}"
        print(f"   ACTUAL   {actual} tags={sorted(failed_tags)} → "
              f"{'AS PREDICTED' if ok else 'MISMATCH' + detail}\n")
        results.append((c["name"], actual, ok))

    print("── SUMMARY")
    for name, actual, ok in results:
        print(f"   {name:4} {actual:8} {'ok' if ok else 'MISMATCH'}")
    return 0 if all(ok for _, _, ok in results) else 1


if __name__ == "__main__":
    sys.exit(main())
