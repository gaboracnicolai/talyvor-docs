#!/usr/bin/env python3
"""w31-tenancyguard-controls-5r8k — CAN THE TWO NEW GUARDS FAIL, AND ONLY FOR THEIR OWN REASON?

Both guards added in this merge PASSED ON THEIR FIRST RUN, because the production predicate they
protect is correct today. A test that has only ever been green is indistinguishable from a test
that cannot go red, so every control below mutates something REAL, carries a verdict written
down BEFORE the run, restores in a `finally` and verifies the restore by sha256.

The verdicts are per-TEST, not per-suite. Scoring "the suite reddened" cannot express a
must-stay-green control and cannot tell "my guard caught it" from "something else did" — the
defect tab-7k2m recorded in its own harness on this same thread.

  C1 NEUTER      `workspace_id = $1` → `(workspace_id = $1 OR TRUE)` in WorkspacePageIDs.
                 The exact mutation the census scored SILENT at this site.
                 PREDICT: both guards RED. Everything else in the repo GREEN — that is the
                 census result restated as an assertion, and it is what makes these two tests
                 the ONLY thing standing here.
  C2 DELETE      the tenancy meaning removed while the placeholder stays bound
                 (`WHERE workspace_id = $1` → `WHERE $1::text IS NOT NULL`).
                 PREDICT: both guards RED, on the SCOPE tags. A guard keyed on the literal
                 text rather than on the behaviour would pass C1 and fail this.

                 ⚠ THE FIRST C2 WAS AN INVALID CONTROL AND IT SCORED "RED/RED", WHICH IS THE
                 ANSWER IT WAS LOOKING FOR. It rewrote the statement to `WHERE TRUE`, which
                 drops `$1` from the SQL while the Go call still passes workspaceID — Postgres
                 refuses the bind ("bind message supplies 1 parameters, but prepared statement
                 requires 0"), the query ERRORS, and both guards went red on a failure that has
                 nothing to do with tenancy: the page guard on its bare `t.Fatalf` (NO tag at
                 all) and the track guard on its [OWN-PRICED] PREMISE. Reading only the pass/fail
                 bit, that control was indistinguishable from a real catch. It was the TAG
                 assertion — added for C3 — that exposed it. A mutation must stay executable to
                 mean anything; "the suite went red" is not the same claim as "the guard saw it".

                 ⚠ AND THE FIRST FIX WAS ALSO INVALID, THE SAME WAY, FOR A DIFFERENT REASON.
                 `WHERE $1 IS NOT NULL` keeps the placeholder but gives Postgres nothing to
                 infer its type from: `could not determine data type of parameter $1`
                 (SQLSTATE 42P18). Red again, tags empty again. Two invalid controls in a row
                 both scored the predicted RED — which is precisely why the prediction is
                 written per-TAG and not per-test.
  C3 EMPTY       `WHERE workspace_id = $1 AND FALSE` — the enumerator returns nothing.
                 PREDICT: both guards RED **on their PREMISE assertions** ([ENUMERATES],
                 [OWN-PRICED]), never on the scope assertions. A guard whose premise is
                 decorative would report [SCOPED]/[FOREIGN-UNPRICED] here — i.e. it would call
                 a dead enumerator "correctly scoped", which is the vacuity these tests exist
                 to refuse.
  M1 ELSEWHERE   an UNRELATED tenancy predicate neutered (analytics WorkspaceStats, itself
                 SILENT). PREDICT: both guards GREEN. Without this, "RED under C1" could mean
                 the guards red on any change at all.
  M2 UNMUTATED   no mutation. PREDICT: both guards GREEN. The baseline that makes every RED
                 above meaningful rather than free.
"""

from __future__ import annotations

import hashlib
import os
import signal
import re
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
DSN_ENV = "DOCS_TEST_DATABASE_URL"

PAGE_STORE = REPO / "internal/page/store.go"
ANALYTICS_STORE = REPO / "internal/analytics/store.go"

# The exact source line WorkspacePageIDs runs. Anchored on the whole statement so a mutation that
# no longer matches is LOUD (MUTATION NOT APPLIED) rather than silently absent.
WPI_ANCHOR = "`SELECT id FROM pages WHERE workspace_id = $1`"
ANALYTICS_ANCHOR = "WHERE pv.workspace_id = $1"

GUARDS = {
    "page": ("./internal/page/", "TestWorkspacePageIDs_DoesNotEnumerateAnotherWorkspacesPages"),
    "track": ("./internal/trackintegration/", "TestSyncPageCosts_DoesNotPriceAnotherWorkspacesPages"),
}

# Which assertion inside each guard fired. The tags are the [BRACKETED] labels in the failure
# messages; C3 is the control that distinguishes premise from scope, so the tags must be read,
# not just the pass/fail bit.
SCOPE_TAGS = ("[SCOPED]", "[FOREIGN-UNPRICED]")
PREMISE_TAGS = ("[ENUMERATES]", "[OWN-PRICED]", "[FOREIGN-LINKED]")


def sha256(p: Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def mutate(path: Path, old: str, new: str) -> None:
    src = path.read_text(encoding="utf-8")
    if src.count(old) != 1:
        raise RuntimeError(f"MUTATION NOT APPLIED: {old!r} appears {src.count(old)}× in {path.name}")
    path.write_text(src.replace(old, new, 1), encoding="utf-8")
    if new not in path.read_text(encoding="utf-8"):
        raise RuntimeError(f"MUTATION NOT APPLIED: {new!r} absent after write to {path.name}")


def run_guard(key: str, dsn: str) -> tuple:
    pkg, test = GUARDS[key]
    env = dict(os.environ)
    env[DSN_ENV] = dsn
    proc = subprocess.run(
        ["go", "test", "-count=1", "-run", f"^{test}$", "-v", pkg],
        cwd=REPO, env=env, capture_output=True, text=True, timeout=600,
    )
    out = proc.stdout + proc.stderr
    if re.search(r"\[build failed\]|^# github\.com/\S+", out, re.M):
        return "BUILD-FAILED", out
    if re.search(rf"^--- PASS: {re.escape(test)}", out, re.M):
        return "GREEN", out
    if re.search(rf"^--- FAIL: {re.escape(test)}", out, re.M):
        return "RED", out
    return f"NOT-RUN (exit {proc.returncode})", out


def tags_in(out: str) -> list:
    return sorted({t for t in SCOPE_TAGS + PREMISE_TAGS if t in out})


def whole_suite(dsn: str) -> tuple:
    env = dict(os.environ)
    env[DSN_ENV] = dsn
    proc = subprocess.run(["go", "test", "-timeout", "600s", "-count=1", "./..."],
                          cwd=REPO, env=env, capture_output=True, text=True, timeout=1800)
    out = proc.stdout + proc.stderr
    return sorted(set(re.findall(r"^FAIL\s+(github\.com/\S+)", out, re.M))), out



def restore_on_signal(snapshot: dict) -> None:
    """Put every snapshotted file back, then die of the signal we were sent.

    A `finally` DOES NOT RUN ON SIGTERM. This script already restores in a `finally` — with
    `git checkout --` — which makes it EXCEPTION-safe and not SIGNAL-safe; those are different
    properties and only the first was covered.

    Measured in talyvor-suite (W1.7, 78c69c8): a 2-minute timeout killed a control mid-mutation
    and left a GATE REMOVED in the tree, with a green suite and a `git status` showing only files
    the session had edited on purpose. Reproduced on demand in 5de27e3, ffe9063 and 5f01947.

    The handler writes BYTES rather than shelling out to git: a signal handler should not depend
    on a subprocess starting. Re-raising with SIG_DFL keeps the exit status honest; SIGKILL still
    strands. Self-contained rather than an import, so the next script is a paste — the population
    and the rule live in scripts/check-restore-signal-handlers.py.
    """
    def handler(signum, _frame):
        for path, blob in snapshot.items():
            try:
                path.write_bytes(blob)
            except OSError:
                pass
        sys.stderr.write("\n!! signal %d — restored %d mutated file(s) before exiting\n"
                         % (signum, len(snapshot)))
        signal.signal(signum, signal.SIG_DFL)
        os.kill(os.getpid(), signum)

    for s in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(s, handler)


def control(name: str, muts: list, dsn: str, predict: dict, want_tags: str = "") -> bool:
    digests = {p: sha256(p) for p, _, _ in muts}
    # Installed AFTER the snapshot and re-installed per control, because `muts` names a different
    # file set each time. The `finally` below is the normal path; this is the one a SIGTERM takes,
    # and `git checkout` in a `finally` cannot cover it.
    restore_on_signal({p: p.read_bytes() for p, _, _ in muts})
    try:
        for path, old, new in muts:
            mutate(path, old, new)
        ok = True
        for key in GUARDS:
            verdict, out = run_guard(key, dsn)
            got_tags = tags_in(out)
            hit = verdict == predict[key]
            tag_ok = True
            if want_tags == "premise" and verdict == "RED":
                tag_ok = any(t in got_tags for t in PREMISE_TAGS) and not any(
                    t in got_tags for t in SCOPE_TAGS)
            if want_tags == "scope" and verdict == "RED":
                tag_ok = any(t in got_tags for t in SCOPE_TAGS)
            ok = ok and hit and tag_ok
            print(f"  {name:<12} {key:<6} predicted {predict[key]:<5} got {verdict:<12} "
                  f"tags={got_tags} -> {'OK' if hit and tag_ok else 'MISMATCH'}")
        return ok
    finally:
        for path in digests:
            subprocess.run(["git", "checkout", "--", str(path.relative_to(REPO))], cwd=REPO, check=True)
            if sha256(path) != digests[path]:
                raise RuntimeError(f"RESTORE FAILED for {path}")


def main() -> int:
    dsn = os.environ.get(DSN_ENV, "")
    if not dsn:
        print(f"{DSN_ENV} is required")
        return 2
    if subprocess.run(["git", "diff", "--name-only"], cwd=REPO,
                      capture_output=True, text=True).stdout.strip():
        print("REFUSING TO RUN: tracked files are already modified")
        return 2

    results = {}

    print("M2 UNMUTATED — predicted GREEN/GREEN")
    results["M2"] = control("M2", [], dsn, {"page": "GREEN", "track": "GREEN"})

    print("C1 NEUTER — predicted RED/RED (the census mutation, scored SILENT before these guards)")
    results["C1"] = control(
        "C1", [(PAGE_STORE, WPI_ANCHOR, "`SELECT id FROM pages WHERE (workspace_id = $1 OR TRUE)`")],
        dsn, {"page": "RED", "track": "RED"}, want_tags="scope")

    print("C2 DELETE — predicted RED/RED on the SCOPE tags (placeholder stays bound)")
    results["C2"] = control(
        "C2", [(PAGE_STORE, WPI_ANCHOR, "`SELECT id FROM pages WHERE $1::text IS NOT NULL`")],
        dsn, {"page": "RED", "track": "RED"}, want_tags="scope")

    print("C3 EMPTY — predicted RED/RED, and on the PREMISE tags only")
    results["C3"] = control(
        "C3", [(PAGE_STORE, WPI_ANCHOR, "`SELECT id FROM pages WHERE workspace_id = $1 AND FALSE`")],
        dsn, {"page": "RED", "track": "RED"}, want_tags="premise")

    print("M1 ELSEWHERE — predicted GREEN/GREEN (an unrelated SILENT tenancy predicate)")
    results["M1"] = control(
        "M1", [(ANALYTICS_STORE, ANALYTICS_ANCHOR, "WHERE (pv.workspace_id = $1 OR TRUE)")],
        dsn, {"page": "GREEN", "track": "GREEN"})

    # C1's second half: with the predicate neutered, is anything ELSE in the repository red?
    # The census said no. Restating it as an assertion is what makes "these two tests are the
    # only thing standing here" a measurement rather than a recollection.
    print("C1b WHOLE SUITE under C1 — predicted exactly internal/page + internal/trackintegration")
    digest = sha256(PAGE_STORE)
    try:
        mutate(PAGE_STORE, WPI_ANCHOR, "`SELECT id FROM pages WHERE (workspace_id = $1 OR TRUE)`")
        pkgs, _ = whole_suite(dsn)
        want = ["github.com/talyvor/docs/internal/page",
                "github.com/talyvor/docs/internal/trackintegration"]
        results["C1b"] = pkgs == want
        print(f"  C1b          red packages = {pkgs}")
        print(f"               want         = {want} -> {'OK' if pkgs == want else 'MISMATCH'}")
    finally:
        subprocess.run(["git", "checkout", "--", str(PAGE_STORE.relative_to(REPO))], cwd=REPO, check=True)
        if sha256(PAGE_STORE) != digest:
            raise RuntimeError("RESTORE FAILED for page/store.go")

    print("\n─── CONTROLS ───")
    for k, v in results.items():
        print(f"  {k}: {'OK' if v else 'MISMATCH'}")
    failed = [k for k, v in results.items() if not v]
    if failed:
        print(f"\n{len(failed)} CONTROL(S) DID NOT MATCH THEIR PREDICTION: {failed}")
        return 1
    print("\nall controls matched their written-down predictions")
    return 0


if __name__ == "__main__":
    sys.exit(main())
