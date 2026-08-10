#!/usr/bin/env python3
"""
Positive controls for FINDING (19) — the routeguard class guard — and FINDING (20), the approval
cross-page defect it found.

Two guards ship together here and they are NOT redundant, which is the thing these controls have
to demonstrate rather than assert:

  * internal/routeguard  — a handler gated on {P} must READ {P}. Generalises to the NEXT copy.
  * internal/approval/crosspage_realpg_test.go — a mismatched (page, request) pair must 404,
    driven through the real routes on real Postgres. Proves THIS copy is closed.

So the control set contains one mutation only the GUARD can see (C8: a brand-new gated route with
an unread param — no real-PG test exists for it) and one only the REAL-PG TEST can see (C7 and C6:
the param is read and then dropped / the SQL predicate removed — the AST is unchanged). A control
that both catch would justify neither.

Every verdict is the SET of test names that failed plus, for the guard, the assertion tag in the
message — predicted before the run. A build failure is detected explicitly and scored as BUILD,
never as a catch: a compile error that reddens the suite looks exactly like a working guard.

Restores are `cp` from bytes saved before the run, in a `finally`, sha256-compared at the end.
"""

import hashlib
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile

REPO = pathlib.Path(__file__).resolve().parent.parent
DSN = "postgres://postgres:postgres@localhost:55437/postgres?sslmode=disable"

STORE = "internal/approval/store.go"
HANDLER = "internal/approval/handler.go"
STORE_TEST = "internal/approval/store_test.go"
GUARD = "internal/routeguard/enforcer_param_test.go"
FILES = [STORE, HANDLER, STORE_TEST, GUARD]

GUARD_PKG = "./internal/routeguard/"
APPROVAL_PKG = "./internal/approval/"


def sh(args, **kw):
    import os
    env = dict(os.environ, DOCS_TEST_DATABASE_URL=DSN)
    return subprocess.run(args, cwd=REPO, capture_output=True, text=True, env=env, **kw)


def sha256(p):
    return hashlib.sha256((REPO / p).read_bytes()).hexdigest()


def run_tests():
    """Return (failed_test_names, build_broke, raw)."""
    r = sh(["go", "test", "-timeout", "300s", "-count=1", GUARD_PKG, APPROVAL_PKG])
    out = r.stdout + r.stderr
    if "build failed" in out or "[build failed]" in out or re.search(r"^\S+\.go:\d+:\d+: ", out, re.M):
        return set(), True, out
    failed = set(re.findall(r"^--- FAIL: (\S+)", out, re.M))
    tags = set(re.findall(r"\[([A-Z0-9-]+)\]", out))
    return failed | {"tag:" + t for t in tags}, False, out


def sub(path, old, new, count=1):
    p = REPO / path
    s = p.read_text()
    if s.count(old) < 1:
        raise SystemExit(f"ANCHOR MISSING in {path}: {old[:80]!r} — the control would have been "
                         f"applied to nothing and its silence would have read as a result")
    p.write_text(s.replace(old, new, count))


# Each control: (label, apply_fn, prediction-set, why)
def c1_restore(_):
    """The defect exactly as shipped — a RESTORE, not a mutation I invented."""
    for f in (STORE, HANDLER, STORE_TEST):
        src = sh(["git", "show", f"origin/main:{f}"])
        if src.returncode != 0:
            raise SystemExit(f"cannot read origin/main:{f}")
        (REPO / f).write_text(src.stdout)


def c2_blind_readsparam(_):
    sub(GUARD, "func readsParam(fd *ast.FuncDecl, name string) bool {\n\tfound := false",
        "func readsParam(fd *ast.FuncDecl, name string) bool {\n\tif true {\n\t\treturn true\n\t}\n\tfound := false")
    c1_restore(None)


def c3_wrong_param(_):
    sub(GUARD, '"pageEnf":  "pageID",', '"pageEnf":  "spaceID",')


def c4_unpin_route(_):
    sub(GUARD, '\t"pagelink DELETE /pages/{pageID}/links/{issueID}",\n', "")


def c5_unpin_enforcer(_):
    sub(GUARD, '\t"blockEnf": "blockID",\n', "")


def c6_drop_sql_predicate(_):
    sub(STORE, "WHERE id = $1 AND page_id = $2 AND workspace_id = ANY($3))`,\n\t\trequestID, pageID, wsIDs,",
        "WHERE id = $1 AND workspace_id = ANY($2))`,\n\t\trequestID, wsIDs,")
    # Keep the mock test honest about the arity the gate now expects. The four Decide scope-gate
    # expectations are the ONLY ones that carry ("req-1", "pg-1", wsIDs) — GetDecisions' similar
    # `WithArgs("req-1", []string{"ws-1"})` belongs to a different statement and must NOT be
    # rewritten. An earlier draft of this control tried to and `sub`'s anchor assertion stopped
    # the run; that is what the assertion is for.
    sub(STORE_TEST, '\\s+WHERE id = \\$1 AND page_id = \\$2 AND workspace_id = ANY`',
        ' WHERE id.*workspace_id = ANY`', count=99)
    sub(STORE_TEST, 'WithArgs("req-1", "pg-1", wsIDs)', 'WithArgs("req-1", wsIDs)', count=99)
    # pageID is now unused by the gate but still used by the page flip, so this still compiles.


def c7_read_then_drop(_):
    sub(HANDLER, 'h.store.Decide(r.Context(), requestID, chi.URLParam(r, "pageID"), reviewerID,',
        'h.store.Decide(r.Context(), requestID, func() string { _ = chi.URLParam(r, "pageID"); return "" }(), reviewerID,')


def c8_new_unread_route(_):
    sub(HANDLER,
        '\tr.With(h.pageEnf.Require(permission.AccessEdit)).Post("/spaces/{spaceID}/pages/{pageID}/publish", h.Publish)',
        '\tr.With(h.pageEnf.Require(permission.AccessEdit)).Post("/spaces/{spaceID}/pages/{pageID}/publish", h.Publish)\n'
        '\tr.With(h.pageEnf.Require(permission.AccessEdit)).Post("/spaces/{spaceID}/pages/{pageID}/approval/{requestID}/nudge", h.ControlNudge)')
    p = REPO / HANDLER
    p.write_text(p.read_text() + '''
func (h *Handler) ControlNudge(w http.ResponseWriter, r *http.Request) {
	_ = chi.URLParam(r, "requestID")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
''')
    # The new route is in-class and unread → the guard must fire twice: the per-route assertion
    # AND the pinned-set comparison.


CONTROLS = [
    ("C1-restore-the-shipped-defect", c1_restore,
     {"TestGatedRouteHandlerReadsItsEnforcersParam", "TestApprovalDecide_MustBelongToTheAuthorizedPage_RealPG",
      "tag:A-LEAK-DECIDE", "tag:A-LEAK-DECISION-ROW", "tag:A-LEAK-PAGE-STATUS"},
     "the defect as shipped must red BOTH guards, and exactly the three leak tags"),
    ("C2-blind-readsParam-with-defect-restored", c2_blind_readsparam,
     {"TestApprovalDecide_MustBelongToTheAuthorizedPage_RealPG",
      "tag:A-LEAK-DECIDE", "tag:A-LEAK-DECISION-ROW", "tag:A-LEAK-PAGE-STATUS"},
     "readsParam is the guard's live predicate: constant-true and the guard goes silent on a "
     "defect the real-PG test still sees"),
    ("C3-enforcer-mapped-to-the-wrong-param", c3_wrong_param,
     {"TestGatedRouteHandlerReadsItsEnforcersParam", "TestEnforcerParamCensus"},
     "the enforcerParams map is load-bearing, not documentation. ⚠ MY PREDICTION WAS WRONG AND "
     "IS CORRECTED HERE RATHER THAN QUIETLY WIDENED: I named only the route test, forgetting "
     "that the census compares the SAME map against main.go in both directions, so a wrong pin "
     "reds there first. Two catchers, and the census is the better one — it names the cause"),
    ("C4-unpin-one-in-class-route", c4_unpin_route,
     {"TestGatedRouteHandlerReadsItsEnforcersParam"},
     "the pinned route set is compared, so a route leaving the guard's reach is visible"),
    ("C5-unpin-blockEnf", c5_unpin_enforcer,
     {"TestEnforcerParamCensus", "TestGatedRouteHandlerReadsItsEnforcersParam"},
     "an enforcer the guard does not know about fails the census instead of silently skipping "
     "every route it gates"),
    ("C6-drop-the-page_id-predicate", c6_drop_sql_predicate,
     {"TestApprovalDecide_MustBelongToTheAuthorizedPage_RealPG",
      "tag:A-LEAK-DECIDE", "tag:A-LEAK-DECISION-ROW", "tag:A-LEAK-PAGE-STATUS"},
     "ONLY THE REAL-PG TEST CAN SEE THIS. The AST is unchanged — the handler still reads "
     "{pageID} — so the class guard is green by construction. This is also the control the "
     "store.go comment cites as earning the deleted page_id lookup"),
    ("C7-read-the-param-then-drop-it", c7_read_then_drop,
     {"TestApprovalDecide_MustBelongToTheAuthorizedPage_RealPG",
      "TestDecide_WithAnEmptySpaceSegment_StillReachesTheHandler_RealPG", "tag:A-PREMISE"},
     "⚠⚠ THIS CONTROL JUSTIFIES NOTHING AND IS KEPT AS THE RECORD OF WHY. I wrote it to earn the "
     "three A-LEAK-* assertions — 'reading the param is not passing it' — and predicted they "
     "would fire. THEY NEVER RAN. Dropping the param makes bob's HONEST decide on his OWN page "
     "fail too, so A-PREMISE's t.Fatalf aborts the test before any leak assertion is reached. "
     "The premise and the leak share the fix's code path, so no mutation of that path can "
     "isolate the leak assertions: C7 is INERT BY KIND, not by accident. What actually earns "
     "A-LEAK-* is C1 (the shipped defect restored) and C6 (the SQL predicate removed) — both "
     "leave the premise green and fire all three tags. A third test reddens here as well, which "
     "is the same fact from another angle"),
    ("C8-new-gated-route-that-ignores-its-param", c8_new_unread_route,
     {"TestGatedRouteHandlerReadsItsEnforcersParam"},
     "ONLY THE CLASS GUARD CAN SEE THIS. A fresh copy of the class on a route no real-PG test "
     "has ever heard of — which is the entire reason the guard exists"),
]


def main():
    dirty = [l for l in sh(["git", "status", "--porcelain"]).stdout.splitlines()
             if pathlib.Path(__file__).name not in l and "crosspage_realpg_test.go" not in l
             and "enforcer_param_test.go" not in l and "store.go" not in l
             and "handler.go" not in l and "store_test.go" not in l]
    before = {f: sha256(f) for f in FILES}
    backup = pathlib.Path(tempfile.mkdtemp(prefix="w31-approval-controls-"))
    for f in FILES:
        shutil.copy2(REPO / f, backup / f.replace("/", "__"))

    def restore():
        for f in FILES:
            shutil.copy2(backup / f.replace("/", "__"), REPO / f)

    rows = []
    try:
        # C0 — MUST STAY GREEN. Without it, every "CAUGHT" below could be a suite that was
        # already red for an unrelated reason.
        failed, broke, out = run_tests()
        rows.append(("C0-pristine-must-stay-green", set(), failed, broke,
                     "the tree under test is green before any mutation"))

        for label, apply, predicted, why in CONTROLS:
            restore()
            apply(None)
            failed, broke, out = run_tests()
            rows.append((label, predicted, failed, broke, why))
            restore()
    finally:
        restore()
        after = {f: sha256(f) for f in FILES}
        bad = [f for f in FILES if before[f] != after[f]]
        if bad:
            print("!! RESTORE FAILED, TREE IS DIRTY:", bad)
            return 3
        shutil.rmtree(backup, ignore_errors=True)

    ok = 0
    for label, predicted, observed, broke, why in rows:
        if broke:
            verdict = "!! BUILD BROKE — scored as NOT a catch"
            passed = False
        else:
            passed = observed == predicted
            verdict = "AS PREDICTED" if passed else "!! NOT AS PREDICTED"
        ok += bool(passed)
        print(f"{verdict:<34} {label}")
        print(f"{'':34}   why: {why}")
        if not passed:
            print(f"{'':34}   predicted: {sorted(predicted)}")
            print(f"{'':34}   observed:  {sorted(observed)}")
    print(f"\n{ok}/{len(rows)} as predicted")
    if dirty:
        print("NOTE — unrelated dirty paths present at start:", dirty)
    return 0 if ok == len(rows) else 1


if __name__ == "__main__":
    sys.exit(main())
