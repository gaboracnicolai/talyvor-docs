#!/usr/bin/env python3
"""Positive controls for the inline-database by-id scope fix (finding (18), third copy).

WHAT A CONTROL IS SCORED ON HERE. Not "did the suite go red" — a build error, a panic or an
unrelated flake all do that. The verdict is a PAIR:

  · the SET OF ASSERTION TAGS that fired in crossdatabase_realpg_test.go, scraped anchored to the
    `crossdatabase_realpg_test.go:NN: [TAG]` prefix, and
  · the SET OF OTHER TESTS in the repository that reddened.

Both are PREDICTED IN THIS FILE BEFORE THE RUN and compared exactly. A control that reds "the
right test" through the wrong assertion is not a catch, and a control whose blast radius is wider
than predicted is a fact about the repository worth printing rather than rounding off.

BUILD FAILURES ARE DETECTED EXPLICITLY. A mutation that does not compile reds everything and would
otherwise score as the strongest catch in the set.

C0 IS THE MUST-STAY-GREEN: no mutation, and the whole suite must come back with an empty tag set
and an empty failure set. Without it, a harness whose verdict-reader is stuck saying FAIL scores
13 catches and means nothing.

C1 IS NOT A MUTATION. It restores the three touched files to origin/main — the defect exactly as
it shipped — and its prediction is that it reds EXACTLY the five leak assertions AND NOT ONE OTHER
TEST IN THE REPOSITORY. That is the only form in which "nothing else noticed this hole" is a claim
about the product rather than about expectations this change itself introduced.

Usage:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-database-crossdb-controls.py [--only C5]
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = "internal/database/store.go"
HANDLER = "internal/database/handler.go"
STORE_TEST = "internal/database/store_test.go"
GUARD = "internal/database/crossdatabase_realpg_test.go"

TEST_CMD = ["go", "test", "-timeout", "300s", "-count=1", "./..."]
# -race is dropped for the control sweep only (14 full-suite runs). The gauntlet run in the fix
# report is the CI command verbatim, -race included. Stated because a control set that quietly
# runs a weaker command than the gauntlet is a different measurement wearing its name.

MY_TEST = "TestDatabaseByID_MustNameTheDatabaseTheRouteAuthorized_RealPG"

# ── the mutations ────────────────────────────────────────────────────────────────────────────
# Each edit is (file, old, new). Every anchor's count is asserted BEFORE any write, and each file
# is written exactly ONCE per control — two edits to one file are applied to the same buffer, so
# the second write cannot erase the first.

ROW_SELECT_SCOPED = """		`SELECT values FROM database_rows WHERE id = $1 AND database_id = $2
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($3))`, id, databaseID, wsIDs,"""
ROW_SELECT_UNSCOPED = """		`SELECT values FROM database_rows WHERE id = $1
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($2))`, id, wsIDs,"""
ROW_SELECT_NOWS = """		`SELECT values FROM database_rows WHERE id = $1 AND database_id = $2`, id, databaseID,"""

ROW_UPDATE_SCOPED = """		`UPDATE database_rows SET values = $1, updated_at = NOW() WHERE id = $2 AND database_id = $3
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($4)) RETURNING `+rowCols,
		encoded, id, databaseID, wsIDs,"""
ROW_UPDATE_UNSCOPED = """		`UPDATE database_rows SET values = $1, updated_at = NOW() WHERE id = $2
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($3)) RETURNING `+rowCols,
		encoded, id, wsIDs,"""
ROW_UPDATE_NOWS = """		`UPDATE database_rows SET values = $1, updated_at = NOW() WHERE id = $2 AND database_id = $3 RETURNING `+rowCols,
		encoded, id, databaseID,"""

DEL_SCOPED = """		`DELETE FROM database_rows WHERE id = $1 AND database_id = $2
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($3))`, id, databaseID, wsIDs)"""
DEL_UNSCOPED = """		`DELETE FROM database_rows WHERE id = $1
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($2))`, id, wsIDs)"""
DEL_NOWS = """		`DELETE FROM database_rows WHERE id = $1 AND database_id = $2`, id, databaseID)"""

VIEW_SCOPED = """	args = append(args, databaseID)
	dbPos := idx
	idx++
	args = append(args, wsIDs)
	row := s.pool.QueryRow(ctx,
		fmt.Sprintf(`UPDATE database_views SET %s WHERE id = $%d AND database_id = $%d
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($%d)) RETURNING %s`,
			strings.Join(setParts, ", "), idPos, dbPos, idx, viewSelectCols),"""
VIEW_UNSCOPED = """	args = append(args, wsIDs)
	row := s.pool.QueryRow(ctx,
		fmt.Sprintf(`UPDATE database_views SET %s WHERE id = $%d
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($%d)) RETURNING %s`,
			strings.Join(setParts, ", "), idPos, idx, viewSelectCols),"""
VIEW_NOWS = """	args = append(args, databaseID)
	dbPos := idx
	idx++
	row := s.pool.QueryRow(ctx,
		fmt.Sprintf(`UPDATE database_views SET %s WHERE id = $%d AND database_id = $%d RETURNING %s`,
			strings.Join(setParts, ", "), idPos, dbPos, viewSelectCols),"""

MERGE_LIVE = """	for k, v := range patch {
		merged[k] = v
	}"""
MERGE_DEAD = """	for k, v := range patch {
		_, _ = k, v
	}"""

H_UPDATEROW = """	row, err := h.store.UpdateRow(r.Context(), chi.URLParam(r, "dbID"), chi.URLParam(r, "rowID"), in.Values, wsIDs)"""
H_UPDATEROW_BAD = """	row, err := h.store.UpdateRow(r.Context(), chi.URLParam(r, "rowID"), chi.URLParam(r, "rowID"), in.Values, wsIDs)"""
H_DELETEROW = """	if err := h.store.DeleteRow(r.Context(), chi.URLParam(r, "dbID"), chi.URLParam(r, "rowID"), wsIDs); err != nil {"""
H_DELETEROW_BAD = """	if err := h.store.DeleteRow(r.Context(), chi.URLParam(r, "rowID"), chi.URLParam(r, "rowID"), wsIDs); err != nil {"""
H_UPDATEVIEW = """	v, err := h.store.UpdateView(r.Context(), chi.URLParam(r, "dbID"), chi.URLParam(r, "viewID"), updates, wsIDs)"""
H_UPDATEVIEW_BAD = """	v, err := h.store.UpdateView(r.Context(), chi.URLParam(r, "viewID"), chi.URLParam(r, "viewID"), updates, wsIDs)"""

H_GET_ROWS = """	r.With(h.dbEnf.Require(permission.AccessView)).Get("/databases/{dbID}/rows", h.ListRows)"""
H_GET_ROWS_UNGATED = """	r.Get("/databases/{dbID}/rows", h.ListRows)"""
H_PATCH_ROW = """	r.With(h.dbEnf.Require(permission.AccessEdit)).Patch("/databases/{dbID}/rows/{rowID}", h.UpdateRow)"""
H_PATCH_ROW_UNGATED = """	r.Patch("/databases/{dbID}/rows/{rowID}", h.UpdateRow)"""

LEAKS = {"X-LEAK-ROW-WRITE", "X-LEAK-ROW-READ", "X-LEAK-ROW-DELETE", "X-LEAK-VIEW-WRITE", "X-LEAK-VIEW-READ"}

CONTROLS = [
    dict(
        id="C0", kind="must-stay-green", edits=[],
        why="No mutation. The fixed tree must come back with an empty tag set and an empty failure "
            "set, or every verdict below is a reading of a broken instrument.",
        tags=set(), others=set(),
    ),
    dict(
        id="C1", kind="restore-to-origin/main", restore_from_main=[STORE, HANDLER, STORE_TEST],
        edits=[],
        why="THE DEFECT AS IT SHIPPED. Not a mutation — the three touched files are checked out "
            "from origin/main, so the only thing new in the tree is the guard itself. Predicts the "
            "five leak assertions and NOT ONE other test in the repository: that is what makes "
            "'no existing guard could see this' a claim about the product.",
        tags=set(LEAKS), others=set(),
    ),
    dict(
        id="C2", kind="mutation", edits=[(STORE, ROW_SELECT_SCOPED, ROW_SELECT_UNSCOPED)],
        why="THE WRITE-SHAPED FIX. Scope the UPDATE and leave UpdateRow's read-modify-write SELECT "
            "unscoped. The queue's handed-down claim is that this leaves the disclosure half open. "
            "PREDICTION: it does NOT — the merged row is only ever returned by the UPDATE's "
            "RETURNING, so a scoped UPDATE answers 404 and no cell reaches the caller. No product "
            "assertion fires; the mock test that pins the statement does.",
        tags=set(), others={"TestUpdateRow_MergesValues"},
    ),
    dict(
        id="C3", kind="mutation", edits=[(STORE, ROW_UPDATE_SCOPED, ROW_UPDATE_UNSCOPED)],
        why="The mirror: scope the SELECT, leave the UPDATE unscoped. PREDICTION: also NOT caught "
            "at the product level — the SELECT refuses first and returns ErrNotFound. C2+C3 "
            "together are the honest record that the two predicates in UpdateRow are individually "
            "redundant for this observable, and neither is justified alone by a product test.",
        tags=set(), others={"TestUpdateRow_MergesValues"},
    ),
    dict(
        id="C4", kind="mutation",
        edits=[(STORE, ROW_SELECT_SCOPED, ROW_SELECT_UNSCOPED), (STORE, ROW_UPDATE_SCOPED, ROW_UPDATE_UNSCOPED)],
        why="UpdateRow's database scope removed from BOTH statements — the whole of that half of "
            "the fix. This is the control that justifies it. Both row-patch assertions fire, and "
            "the read one only ever fires with the write one (see C2/C3): recorded, not implied.",
        tags={"X-LEAK-ROW-WRITE", "X-LEAK-ROW-READ"}, others={"TestUpdateRow_MergesValues"},
    ),
    dict(
        id="C5", kind="mutation", edits=[(STORE, DEL_SCOPED, DEL_UNSCOPED)],
        why="DeleteRow's database scope removed. One statement, one assertion — the cleanest "
            "isolation in the set.",
        tags={"X-LEAK-ROW-DELETE"}, others={"TestDeleteRow_DeletesByID"},
    ),
    dict(
        id="C6", kind="mutation", edits=[(STORE, VIEW_SCOPED, VIEW_UNSCOPED)],
        why="UpdateView's database scope removed. NO mock test pins this statement, so the whole "
            "of `others` is expected empty — the view half of this fix is held by the new guard "
            "and by nothing else in the repository.",
        tags={"X-LEAK-VIEW-WRITE", "X-LEAK-VIEW-READ"}, others=set(),
    ),
    dict(
        id="C7", kind="mutation", edits=[(HANDLER, H_UPDATEROW, H_UPDATEROW_BAD)],
        why="The handler passes {rowID} where {dbID} belongs, so the new predicate can never match "
            "— the over-correction direction. X-PREMISE and X-OWN-ROW-PATCH are the SAME operation "
            "on different rows and cannot be separated by any product mutation; both fire, and "
            "that is recorded rather than papered over with a control that does not isolate.",
        tags={"X-PREMISE", "X-OWN-ROW-PATCH"}, others=set(),
    ),
    dict(
        id="C8", kind="mutation", edits=[(HANDLER, H_DELETEROW, H_DELETEROW_BAD)],
        why="The same substitution on the delete route. Predicts the pre-existing SEC-4 L2 test "
            "too — its last line is 'Bob DELETE own db row = 200', which is exactly this "
            "over-correction seen from the outer ring.",
        tags={"X-OWN-ROW-DELETE"}, others={"TestSEC4_L2_SecondaryCrossTenant"},
    ),
    dict(
        id="C9", kind="mutation", edits=[(HANDLER, H_UPDATEVIEW, H_UPDATEVIEW_BAD)],
        why="The same substitution on the view route. Nothing else in the repository patches a "
            "view through the routes, so `others` is empty.",
        tags={"X-OWN-VIEW-PATCH"}, others=set(),
    ),
    dict(
        id="C10", kind="mutation", edits=[(STORE, MERGE_LIVE, MERGE_DEAD)],
        why="The patch stops being applied: 200 with the row written back unchanged. Isolates "
            "X-OWN-ROW-PATCH's SECOND clause (the cell actually changed) from its status check, "
            "which C7 cannot. PREDICTION: the mock test does NOT see it — its UPDATE expectation "
            "takes AnyArg for the encoded blob and returns a canned row, so a merge that stops "
            "merging satisfies it exactly.",
        tags={"X-OWN-ROW-PATCH"}, others=set(),
    ),
    dict(
        id="C11", kind="mutation",
        edits=[(STORE, ROW_SELECT_SCOPED, ROW_SELECT_NOWS), (STORE, ROW_UPDATE_SCOPED, ROW_UPDATE_NOWS),
               (STORE, DEL_SCOPED, DEL_NOWS), (STORE, VIEW_SCOPED, VIEW_NOWS)],
        why="THE OTHER DIRECTION: the SEC-4 L2 workspace predicate removed from all four "
            "statements, the new database predicate kept. PREDICTION: NOT CAUGHT at the product "
            "level. dbEnf's resolver looks the page up scoped to the caller's verified workspaces, "
            "so a foreign {dbID} never reaches the store at all — the workspace predicate on these "
            "four statements is redundant UNDER THE GATE. It is kept anyway (the store is exported "
            "and every by-id op in this repo carries it), and this control is the honest record "
            "that no product test can see it go. Only the two mock tests, which pin the SQL, red.",
        tags=set(), others={"TestUpdateRow_MergesValues", "TestDeleteRow_DeletesByID"},
    ),
    dict(
        id="C12", kind="mutation", edits=[(HANDLER, H_GET_ROWS, H_GET_ROWS_UNGATED)],
        why="The View gate removed from GET /databases/{dbID}/rows. Isolates X-HONEST-READ — the "
            "assertion that the product already refuses bob at the honest address, which is what "
            "makes the leak a statement about the PAIR and not about bob's access.",
        tags={"X-HONEST-READ"}, others=set(),
    ),
    dict(
        id="C13", kind="mutation", edits=[(HANDLER, H_PATCH_ROW, H_PATCH_ROW_UNGATED)],
        why="The Edit gate removed from PATCH /databases/{dbID}/rows/{rowID}. Isolates "
            "X-HONEST-WRITE, which nothing else can see: the store scope is satisfied by the "
            "honest URL, so only the gate refuses there. Predicts the pre-existing tier test, "
            "whose first row is 'viewer UpdateRow = 403'.",
        tags={"X-HONEST-WRITE"}, others={"TestA3_DatabaseTierEnforcement"},
    ),
]

TAG_RE = re.compile(r"crossdatabase_realpg_test\.go:\d+: \[([A-Z0-9-]+)\]")
FAIL_RE = re.compile(r"^\s*--- FAIL: (\S+)", re.M)
BUILD_RE = re.compile(r"\[build failed\]|^# \S+", re.M)


def sh(cmd, **kw):
    return subprocess.run(cmd, cwd=REPO, capture_output=True, text=True, **kw)


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
    env = dict(os.environ)
    p = subprocess.run(TEST_CMD, cwd=REPO, capture_output=True, text=True, env=env)
    out = p.stdout + p.stderr
    build = bool(BUILD_RE.search(out))
    tags = set(TAG_RE.findall(out))
    others = {n for n in FAIL_RE.findall(out) if n != MY_TEST and "/" not in n}
    return tags, others, build, out


def apply_control(c, originals):
    """Assert EVERY anchor first, then write each file exactly once."""
    for path in c.get("restore_from_main", []):
        p = sh(["git", "checkout", "origin/main", "--", path])
        if p.returncode != 0:
            raise SystemExit(f"{c['id']}: git checkout origin/main -- {path}: {p.stderr}")
    if not c["edits"]:
        return
    buffers = {}
    for path, old, new in c["edits"]:
        buf = buffers.get(path, originals[path])
        n = buf.count(old)
        if n != 1:
            raise SystemExit(f"{c['id']}: anchor count {n} (want 1) in {path}:\n{old[:120]}…")
        buffers[path] = buf.replace(old, new, 1)
    for path, buf in buffers.items():
        write(path, buf)
        if read(path) != buf:
            raise SystemExit(f"{c['id']}: {path} did not land on disk")


def main():
    only = None
    if "--only" in sys.argv:
        only = sys.argv[sys.argv.index("--only") + 1]
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        raise SystemExit("DOCS_TEST_DATABASE_URL must be set — these controls need real Postgres")

    touched = [STORE, HANDLER, STORE_TEST]
    originals = {p: read(p) for p in touched}
    before = {p: sha(p) for p in touched}
    guard_before = sha(GUARD)

    results = []
    try:
        for c in CONTROLS:
            if only and c["id"] != only:
                continue
            for p in touched:  # every control starts from the fixed tree
                write(p, originals[p])
            apply_control(c, originals)
            tags, others, build, out = run_suite()
            ok = (tags == c["tags"] and others == c["others"] and not build)
            results.append((c, tags, others, build, ok))
            print(f"\n=== {c['id']} [{c['kind']}] {'AS PREDICTED' if ok else '*** NOT AS PREDICTED ***'}")
            print(f"    why:   {c['why']}")
            print(f"    tags:  want {sorted(c['tags'])}")
            print(f"           got  {sorted(tags)}")
            print(f"    others:want {sorted(c['others'])}")
            print(f"           got  {sorted(others)}")
            if build:
                print("    !! BUILD FAILURE — this control compiled nothing, its red is meaningless")
                print("\n".join(l for l in out.splitlines() if BUILD_RE.search(l))[:800])
    finally:
        # RESTORE IN A finally, not after the run: a crash between mutate and restore would
        # otherwise leave an unscoped statement on disk with the sha check never reached.
        for p in touched:
            write(p, originals[p])
        after = {p: sha(p) for p in touched}
        bad = [p for p in touched if before[p] != after[p]] + ([GUARD] if sha(GUARD) != guard_before else [])
        print("\n" + "=" * 100)
        if bad:
            print(f"!! RESTORE FAILED for {bad} — the working tree is NOT as it was. Fix before committing.")
        else:
            print("restore verified: sha256 of every touched file matches the pre-run bytes")

    total = len(results)
    good = sum(1 for r in results if r[4])
    print(f"\n{good}/{total} AS PREDICTED")
    for c, tags, others, build, ok in results:
        if not ok:
            print(f"  {c['id']}: tags {sorted(tags)} vs {sorted(c['tags'])} | "
                  f"others {sorted(others)} vs {sorted(c['others'])} | build_failed={build}")
    return 0 if good == total else 1


if __name__ == "__main__":
    sys.exit(main())
