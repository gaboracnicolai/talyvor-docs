#!/usr/bin/env python3
"""
W3.1 finding (9) — CAN A WRITE STOP HAPPENING IN THE SIX PACKAGES `9c00a99` DID NOT COVER?

WHY THIS EXISTS. `ef27e3c` (#61) and `9c00a99` (#62) measured `internal/page` and found that
`Store.Delete` could stop deleting with `go test ./...` green across the whole repo. Both merges
left the same question open for the six packages whose test files call `pgxmock` and NEVER call
`ExpectationsWereMet` — internal/database (12 expectations), comment (11), customdomain/store_test.go
(9), pagelink (8), space (5), block (3) = 48. The queue's finding (9) says, in as many words:
their argument assertions are live because their callers check the Exec error, WHICH IS EXACTLY THE
REASONING THAT WAS TRUE AND INCOMPLETE IN #61. DO NOT INHERIT IT — RE-RUN THE D FAMILY PER PACKAGE.

THE COUNT WAS RE-DERIVED, NOT INHERITED. `ExpectExec` alone gives database 1 / comment 3 /
customdomain 2 / pagelink 5 / space 1 / block 1. The queue's numbers are ALL pgxmock expectations
(Exec + Query + tx) per file, and under that shape they reproduce exactly: 12/11/9/8/5/3 = 48.
An inherited count carries the query shape of the instrument that produced it; this one checks out.

TWO FAMILIES, BECAUSE "THE WRITE STOPPED HAPPENING" HAS TWO SHAPES AND THEY ARE SEEN BY DIFFERENT
THINGS:

  FAMILY D — THE CALL IS DELETED FROM store.go. The `Expect…` that named it is left unfulfilled.
    Without `ExpectationsWereMet` pgxmock ignores that silently, so this is the family that measures
    what the missing check costs. At a CHECKED caller there is no error to return when there is no
    call, which is why "the caller checks the error" does not cover it.

  FAMILY P — THE STATEMENT STILL RUNS AND CHANGES NOTHING (` AND FALSE` appended to its WHERE).
    A MOCK CANNOT SEE THIS BY CONSTRUCTION: pgxmock never executes SQL and hands back the
    RowsAffected the fixture wrote, so every expectation still matches and every count is whatever
    the test said. Only a real-Postgres test that reads the row afterwards — or a caller that turns
    "0 rows" into an error — can. This is `9c00a99`'s P1 mutation applied to nine more sites.

WHAT THE TWO FAMILIES TOGETHER SEPARATE, and the reason P is here at all: three of the ten sites
CHECK `tag.RowsAffected() == 0` and return ErrNotFound. At those, a write that does nothing is
reported to the caller as a 404 — it CANNOT fail silently, and any real-PG test that drives the
success path holds it without asserting a single row. At the other seven the caller is
`_, err := s.pool.Exec(...); return err`, and zero rows affected is indistinguishable from success.
Predicting that difference and being told which way each site actually goes is the whole result.

METHOD. One control mutates exactly one site in one file, `go test -count=1 -v ./...` runs the WHOLE
repo against real Postgres, and the verdict is read from the FAILING TEST NAMES and their PRINTED
ASSERTION LINES — never from an exit code. A build failure is detected explicitly and scored BROKEN,
because a compile error otherwise reads as a caught mutation. Every anchor is asserted UNIQUE in its
file BEFORE any write; files are restored from bytes saved at start and sha256-compared at the end.

CATCHER PREDICTED BEFORE THE RUN for every control. A verdict that disagrees with its prediction is
a finding about my model of this repo, and is printed as one rather than quietly accepted.

Usage:
    DOCS_TEST_DATABASE_URL=... python3 scripts/w31-cross-package-write-controls.py
    W31_FIXED=1 DOCS_TEST_DATABASE_URL=... python3 scripts/w31-cross-package-write-controls.py
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def f(*parts):
    return os.path.join(REPO, *parts)


DB = f("internal", "database", "store.go")
CM = f("internal", "comment", "store.go")
CD = f("internal", "customdomain", "store.go")
PL = f("internal", "pagelink", "store.go")
SP = f("internal", "space", "store.go")
BL = f("internal", "block", "store.go")

# ─── FAMILY D — the call deleted from the product ───────────────────────────────────────────
#
# `predict_target` — does the mock test whose Expect… names this write go red?
# `predict_any`    — does ANYTHING in the repo go red, i.e. is the behaviour protected at all?
DELETIONS = [
    dict(
        name="D1  database.DeleteRow        DELETE deleted",
        file=DB, pkg="internal/database", target="TestDeleteRow_DeletesByID",
        predict_target=False, predict_any=True,
        why="the lone ExpectExec is left unfulfilled and nothing asks ⇒ target blind. But this "
            "mutation ALSO removes the RowsAffected⇒ErrNotFound branch, so SEC4_L2's cross-tenant "
            "DELETE (wants 404) should red. READ THE MESSAGE: a catch there is the 404 half, not "
            "an assertion that the row is gone.",
        old='''	tag, err := s.pool.Exec(ctx,
		`DELETE FROM database_rows WHERE id = $1
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($2))`, id, wsIDs)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil''',
        new='''	_, _ = id, wsIDs
	return nil''',
    ),
    dict(
        name="D2  comment.Resolve           UPDATE deleted",
        file=CM, pkg="internal/comment", target="TestResolve_AppliesToEntireThread",
        predict_target=False, predict_any=False,
        why="one expectation, silently unfulfilled; no real-PG test in this repo resolves a "
            "comment and reads it back. Predicting NOTHING speaks.",
        old='''	_, err := s.pool.Exec(ctx,
		`UPDATE page_comments
        SET resolved = true, resolved_by = $1, resolved_at = NOW(), updated_at = NOW()
        WHERE thread_id = (SELECT thread_id FROM page_comments WHERE id = $2)`,
		resolvedBy, commentID,
	)
	return err''',
        new='''	_, _ = resolvedBy, commentID
	return nil''',
    ),
    dict(
        name="D3  comment.Unresolve         UPDATE deleted",
        file=CM, pkg="internal/comment", target="TestUnresolve_ClearsThreadFields",
        predict_target=False, predict_any=False,
        why="same shape as D2.",
        old='''	_, err := s.pool.Exec(ctx,
		`UPDATE page_comments
        SET resolved = false, resolved_by = NULL, resolved_at = NULL, updated_at = NOW()
        WHERE thread_id = (SELECT thread_id FROM page_comments WHERE id = $1)`,
		commentID,
	)
	return err''',
        new='''	_ = commentID
	return nil''',
    ),
    dict(
        name="D4  comment.Delete            DELETE deleted",
        file=CM, pkg="internal/comment", target="TestDelete_AuthorOnly_Succeeds",
        predict_target=False, predict_any=False,
        why="the author SELECT is consumed first and the DELETE expectation is simply left "
            "unfulfilled. sec_root_actor / sec_crosspage assert the STATUS CODE of the delete "
            "route, which does not change. Predicting NOTHING speaks.",
        old='''	_, err := s.pool.Exec(ctx, `DELETE FROM page_comments WHERE id = $1`, commentID)
	return err''',
        new='''	_ = commentID
	return nil''',
    ),
    dict(
        name="D5  customdomain.Verify       UPDATE deleted",
        file=CD, pkg="internal/customdomain", target="TestVerify_TxtMatch_FlipsVerifiedAndSSL",
        predict_target=False, predict_any=True,
        why="target blind for the usual reason; but the removed RowsAffected branch is what makes "
            "SEC4_L2's cross-tenant Verify a 404, so that test should red. Message attribution "
            "again: that is the tenancy half, not the write half.",
        old='''			tag, err := s.pool.Exec(ctx,
				`UPDATE custom_domains
                SET verified = true, ssl_status = 'active', updated_at = NOW()
                WHERE id = $1 AND workspace_id = ANY($2)`,
				id, wsIDs,
			)
			if err != nil {
				return false, fmt.Errorf("customdomain: mark verified: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return false, ErrNotFound
			}
			return true, nil''',
        new='''			_, _ = id, wsIDs
			return true, nil''',
    ),
    dict(
        name="D6  customdomain.Delete       DELETE deleted",
        file=CD, pkg="internal/customdomain", target="TestDelete_RemovesByIDWithinWorkspace",
        predict_target=False, predict_any=False,
        why="no cross-tenant route test names customdomain Delete (SEC4_L2 covers Verify), so "
            "even the ErrNotFound half has no reader. Predicting NOTHING speaks.",
        old='''	tag, err := s.pool.Exec(ctx,
		`DELETE FROM custom_domains WHERE id = $1 AND workspace_id = ANY($2)`,
		id, wsIDs,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil''',
        new='''	_, _ = id, wsIDs
	return nil''',
    ),
    dict(
        name="D7  pagelink.Upsert           INSERT deleted",
        file=PL, pkg="internal/pagelink", target="TestUpsert_InsertsLink",
        predict_target=False, predict_any=True,
        why="ORDERED MOCK KNOCK-ON: in TestSyncLinks_AddsAndRemovesEmbeds the surviving DELETE call "
            "is matched against the INSERT expectation, mismatches on the query text, and Delete "
            "returns that error. So a NEIGHBOUR test speaks, by accident of ordering, not because "
            "anything verifies the insert.",
        old='''	_, err := s.pool.Exec(ctx,
		`INSERT INTO page_links (page_id, workspace_id, issue_id, link_type, created_by)
        VALUES ($1, $2, $3, $4, $5)
        ON CONFLICT (page_id, issue_id, link_type) DO NOTHING`,
		l.PageID, l.WorkspaceID, l.IssueID, l.LinkType, l.CreatedBy,
	)
	if err != nil {
		return fmt.Errorf("pagelink: upsert: %w", err)
	}
	return nil''',
        new='''	_ = l
	return nil''',
    ),
    dict(
        name="D8  pagelink.Delete           DELETE deleted",
        file=PL, pkg="internal/pagelink", target="TestDelete_RemovesLink",
        predict_target=False, predict_any=False,
        why="the mirror of D7 and the reason ordering is not protection: here the INSERT is "
            "consumed correctly and the DELETE expectation is the one left unfulfilled, so the "
            "knock-on that saved D7 does not happen.",
        old='''	_, err := s.pool.Exec(ctx,
		`DELETE FROM page_links WHERE page_id = $1 AND issue_id = $2`,
		pageID, issueID,
	)
	return err''',
        new='''	_, _ = pageID, issueID
	return nil''',
    ),
    dict(
        name="D9  space.Delete              DELETE deleted",
        file=SP, pkg="internal/space", target="TestDelete_RemovesSpace",
        predict_target=False, predict_any=False,
        why="one expectation, silently unfulfilled; the SEC4 space route tests assert status codes "
            "on list/get/update, not that a deleted space is gone. Predicting NOTHING speaks.",
        old='''	_, err := s.pool.Exec(ctx, `DELETE FROM spaces WHERE id = $1`, id)
	return err''',
        new='''	_ = id
	return nil''',
    ),
    dict(
        name="D10 block.Delete              DELETE deleted",
        file=BL, pkg="internal/block", target="TestDelete_RemovesBlock",
        predict_target=False, predict_any=True,
        why="target blind, but failclosed_gate_test.go DOES read the row back — "
            '`if exists() { t.Fatal("owner DeleteInWorkspaces did not delete") }`. This is the '
            "control that proves the harness can redden a delete at all, so it is the premise for "
            "every NOTHING SEES IT above.",
        old='''	_, err := s.pool.Exec(ctx, `DELETE FROM blocks WHERE id = $1`, id)
	return err''',
        new='''	_ = id
	return nil''',
    ),
]

# ─── FAMILY P — the statement runs and matches no row ───────────────────────────────────────
#
# Appending ` AND FALSE` keeps the SQL valid, keeps every pgxmock regex matching (none is anchored
# at the end), and makes Postgres change nothing. `predict_any` is the only question: is there any
# test in the repo that can tell a write that happened from a write that did not?
NOOPS = [
    dict(
        name="P1  database.DeleteRow        WHERE … AND FALSE",
        file=DB, pkg="internal/database", predict_any=True,
        why="RowsAffected 0 ⇒ ErrNotFound ⇒ 404, and SEC4_L2 asserts Bob deleting his OWN row gets "
            "200. The check converts a silent no-op into a visible error.",
        old='''AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($2))`, id, wsIDs)''',
        new='''AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($2)) AND FALSE`, id, wsIDs)''',
        fixed_tests=['TestSEC4_L2_SecondaryCrossTenant'],
    ),
    dict(
        name="P2  comment.Resolve           WHERE … AND FALSE",
        file=CM, pkg="internal/comment", predict_any=False,
        why="`_, err := Exec` with zero rows returns nil. Nothing reads resolved back.",
        old='''        WHERE thread_id = (SELECT thread_id FROM page_comments WHERE id = $2)`,
		resolvedBy, commentID,''',
        new='''        WHERE thread_id = (SELECT thread_id FROM page_comments WHERE id = $2) AND FALSE`,
		resolvedBy, commentID,''',
        fixed_tests=['TestResolve_ActuallyMarksTheRowResolved_RealPG', 'TestUnresolve_ActuallyClearsTheRow_RealPG'],
    ),
    dict(
        name="P3  comment.Unresolve         WHERE … AND FALSE",
        file=CM, pkg="internal/comment", predict_any=False,
        why="same shape as P2.",
        old='''        WHERE thread_id = (SELECT thread_id FROM page_comments WHERE id = $1)`,
		commentID,''',
        new='''        WHERE thread_id = (SELECT thread_id FROM page_comments WHERE id = $1) AND FALSE`,
		commentID,''',
        fixed_tests=['TestUnresolve_ActuallyClearsTheRow_RealPG'],
    ),
    dict(
        name="P4  comment.Delete            WHERE … AND FALSE",
        file=CM, pkg="internal/comment", predict_any=False,
        why="the delete route answers 200 either way; no test reads the comment back.",
        old='''	_, err := s.pool.Exec(ctx, `DELETE FROM page_comments WHERE id = $1`, commentID)''',
        new='''	_, err := s.pool.Exec(ctx, `DELETE FROM page_comments WHERE id = $1 AND FALSE`, commentID)''',
        fixed_tests=['TestDelete_ActuallyRemovesTheComment_RealPG'],
    ),
    dict(
        name="P5  customdomain.Verify       WHERE … AND FALSE",
        file=CD, pkg="internal/customdomain", predict_any=False,
        why="RowsAffected 0 ⇒ ErrNotFound, which is what SEC4_L2's CROSS-TENANT case already "
            "expects; the OWNER success path for Verify has no real-PG test, so the flip is "
            "invisible. If this reds, my model of that test is wrong.",
        old='''                WHERE id = $1 AND workspace_id = ANY($2)`,
				id, wsIDs,''',
        new='''                WHERE id = $1 AND workspace_id = ANY($2) AND FALSE`,
				id, wsIDs,''',
        fixed_tests=['TestVerify_ActuallyPersistsTheVerifiedFlag_RealPG'],
    ),
    dict(
        name="P6  customdomain.Delete       WHERE … AND FALSE",
        file=CD, pkg="internal/customdomain", predict_any=False,
        why="same reasoning as D6 — no route test drives the owner delete on real Postgres.",
        old='''		`DELETE FROM custom_domains WHERE id = $1 AND workspace_id = ANY($2)`,
		id, wsIDs,''',
        new='''		`DELETE FROM custom_domains WHERE id = $1 AND workspace_id = ANY($2) AND FALSE`,
		id, wsIDs,''',
        fixed_tests=['TestDelete_ActuallyRemovesTheDomain_RealPG', 'TestDelete_TheDeletedDomainNoLongerResolves_RealPG'],
    ),
    dict(
        name="P7  pagelink.Delete           WHERE … AND FALSE",
        file=PL, pkg="internal/pagelink", predict_any=False,
        why="`return err` with zero rows is nil; SyncLinks' own test is a mock.",
        old='''		`DELETE FROM page_links WHERE page_id = $1 AND issue_id = $2`,''',
        new='''		`DELETE FROM page_links WHERE page_id = $1 AND issue_id = $2 AND FALSE`,''',
        fixed_tests=['TestSyncPageCosts_APageWithNoLinksIsWrittenAsZero'],
    ),
    dict(
        name="P8  space.Delete              WHERE … AND FALSE",
        file=SP, pkg="internal/space", predict_any=False,
        why="same as D9 — nothing reads a deleted space back.",
        old='''	_, err := s.pool.Exec(ctx, `DELETE FROM spaces WHERE id = $1`, id)''',
        new='''	_, err := s.pool.Exec(ctx, `DELETE FROM spaces WHERE id = $1 AND FALSE`, id)''',
        fixed_tests=['TestDelete_ActuallyRemovesTheSpace_RealPG', 'TestDelete_TheDeletedSpaceIsNoLongerListed_RealPG'],
    ),
    dict(
        name="P10 customdomain.Verify       SET verified = false (row IS matched)",
        file=CD, pkg="internal/customdomain", predict_any=False,
        why="P5's ` AND FALSE` is caught through the RowsAffected⇒ErrNotFound branch, so it "
            "never reaches the assertion that READS the flag back. This one matches the row, "
            "returns RowsAffected 1 and no error, and does not set verified — so it DOES reach "
            "that assertion. ⚠ IT IS CAUGHT TWICE AND THEREFORE JUSTIFIES NEITHER CATCHER ON ITS "
            "OWN: the mock's expectation regex requires the literal `SET verified = true`, and "
            "every mutation that stops setting the flag must delete that literal. The honest "
            "reading is that the mock holds WHAT THE STATEMENT SAYS and only P5 — caught by this "
            "merge's test alone — holds WHETHER POSTGRES DID IT. Kept because separating those "
            "two is the point, and because it shows the query-text assertion is live.",
        old="                SET verified = true, ssl_status = 'active', updated_at = NOW()",
        new="                SET verified = false, ssl_status = 'active', updated_at = NOW()",
        fixed_tests=["TestVerify_ActuallyPersistsTheVerifiedFlag_RealPG",
                     "TestVerify_TxtMatch_FlipsVerifiedAndSSL"],
    ),
    dict(
        name="P9  block.Delete              WHERE … AND FALSE",
        file=BL, pkg="internal/block", predict_any=True,
        why="THE PREMISE OF THE WHOLE P FAMILY. failclosed_gate_test.go reads the row back after "
            "the owner delete, so it must red — and it is the only reason a NOTHING SEES IT above "
            "is a fact about the repo rather than about this harness.",
        old='''	_, err := s.pool.Exec(ctx, `DELETE FROM blocks WHERE id = $1`, id)''',
        new='''	_, err := s.pool.Exec(ctx, `DELETE FROM blocks WHERE id = $1 AND FALSE`, id)''',
        fixed_tests=['TestBlock_InWorkspaces_CrossTenant_GateHoldsWithoutEnforcer_RealPG'],
    ),
]


# ─── FAMILY M — did MOVING customdomain's four per-test checks shrink coverage? ─────────────
#
# space_mapping_test.go used to end each of its four tests with its own `ExpectationsWereMet`.
# Those calls are now on `newMockStore`, so the same invariant is not held twice — but "moved" is a
# claim, and the way to check it is to breach the invariant in each of the four tests and see the
# constructor speak. Each control registers ONE extra expectation LAST, so every real call still
# matches an earlier one and the stray is left unconsumed: the ONLY thing that can notice it is the
# check under test. A red here must carry the constructor's own message, not a product error.
MOVED = [
    dict(
        name="M1  RefusesForeignWorkspaceSpace  stray expectation",
        file=f("internal", "customdomain", "space_mapping_test.go"),
        target="TestCreate_RefusesForeignWorkspaceSpace",
        old='''	_, err := st.Create(context.Background(), "ws-attacker", "docs.attacker.example", "m-1", ptrStr("sp-victim"))''',
    ),
    dict(
        name="M2  RefusesPrivateSpace           stray expectation",
        file=f("internal", "customdomain", "space_mapping_test.go"),
        target="TestCreate_RefusesPrivateSpace",
        old='''	_, err := st.Create(context.Background(), "ws-1", "docs.example.com", "m-1", ptrStr("sp-private"))''',
    ),
    dict(
        name="M3  AllowsPublicSpaceInOwnWs      stray expectation",
        file=f("internal", "customdomain", "space_mapping_test.go"),
        target="TestCreate_AllowsPublicSpaceInOwnWorkspace",
        old='''	cd, err := st.Create(context.Background(), "ws-1", "docs.example.com", "m-1", ptrStr("sp-public"))''',
    ),
    dict(
        name="M4  AllowsNilSpace                stray expectation",
        file=f("internal", "customdomain", "space_mapping_test.go"),
        target="TestCreate_AllowsNilSpace",
        old='''	if _, err := st.Create(context.Background(), "ws-1", "docs.example.com", "m-1", nil); err != nil {''',
    ),
]
STRAY = ('\tpool.ExpectExec(`DELETE FROM custom_domains`).'
         'WillReturnResult(pgxmock.NewResult("DELETE", 1))\n')
for _m in MOVED:
    _m["new"] = STRAY + _m["old"]
    _m["msg"] = "unmet or mismatched pgxmock expectations"


def sha(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def run_all():
    """Whole repo, verbose, real Postgres. Returns (ok, failing tests, failing pkgs, out, broken)."""
    proc = subprocess.run(
        ["go", "test", "-count=1", "-timeout", "600s", "-v", "./..."],
        cwd=REPO, env=dict(os.environ), capture_output=True, text=True,
    )
    out = proc.stdout + proc.stderr
    fails = sorted(set(re.findall(r"^\s*--- FAIL: (\S+)", out, re.M)))
    pkgs = sorted(set(re.findall(r"^FAIL\s+github.com/talyvor/docs/(\S+)", out, re.M)))
    # A COMPILE ERROR IS NOT A CATCH.
    broken = ("[build failed]" in out or "declared and not used" in out
              or re.search(r"^# github.com/talyvor/docs", out, re.M) is not None)
    return proc.returncode == 0, fails, pkgs, out, broken


def messages(out, tests, limit=3):
    """The printed assertion lines for each failing test — a verdict read from a name is not a
    verdict, and two very different failures share one name in a list."""
    found = {}
    cur = None
    for ln in out.splitlines():
        m = re.match(r"^=== RUN\s+(\S+)", ln)
        if m:
            cur = m.group(1)
            continue
        if cur in tests and re.search(r"\.go:\d+:", ln):
            found.setdefault(cur, [])
            if len(found[cur]) < limit:
                found[cur].append(ln.strip())
    return found


def apply_and_run(spec, saved, key_old, key_new):
    path = spec["file"]
    text = saved[path].decode()
    mutated = text.replace(spec[key_old], spec[key_new])
    assert mutated != text, spec["name"]
    with open(path, "w") as fh:
        fh.write(mutated)
    with open(path) as fh:
        on_disk = fh.read()
    # AN ANCHOR ASSERTION PROVES APPLICATION, NOT MEANING — so re-read the bytes. The second
    # clause is skipped for INSERTION-style mutations (family M builds `new` as STRAY + `old`, so
    # `old` is still present by construction); the first draft of this line demanded
    # count(old) == 0 unconditionally and reported applied=False for four controls that had
    # applied perfectly and caught what they were aimed at. A harness that reports a working
    # control as unapplied is the same failure as one that reports a broken control as working.
    inserted = spec[key_old] in spec[key_new]
    applied = on_disk.count(spec[key_new]) == 1 and (inserted or on_disk.count(spec[key_old]) == 0)
    ok, fails, pkgs, out, broken = run_all()
    with open(path, "wb") as fh:
        fh.write(saved[path])
    return applied, ok, fails, pkgs, out, broken


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set — testutil.New(t) FAILS rather than skips "
              "without it, and a harness that ran half the suite is not a harness.")
        return 2

    files = sorted({c["file"] for c in DELETIONS + NOOPS + MOVED})
    saved = {}
    for p in files:
        with open(p, "rb") as fh:
            saved[p] = fh.read()

    # ── ASSERT EVERY ANCHOR BEFORE ANY WRITE ────────────────────────────────────────────────
    bad = False
    for c in DELETIONS + NOOPS + MOVED:
        n = saved[c["file"]].decode().count(c["old"])
        if n != 1:
            print(f"ANCHOR FAIL {c['name']}: {n} occurrences in "
                  f"{os.path.relpath(c['file'], REPO)}, want exactly 1")
            bad = True
    if bad:
        return 2
    n_anchor = len(DELETIONS) + len(NOOPS) + len(MOVED)
    print(f"anchors: {n_anchor}/{n_anchor} unique across {len(files)} files")
    for p in files:
        print(f"    {os.path.relpath(p, REPO):34} sha256 {sha(p)[:12]}")

    ok, fails, pkgs, _, _ = run_all()
    if not ok:
        print(f"\nBASELINE IS RED ({fails} in {pkgs}) — every verdict below would be unreadable.")
        return 2
    print("\nbaseline: whole repo GREEN on real Postgres\n")

    dres, pres = [], []

    print("═══ FAMILY D — the CALL deleted from store.go ═══\n")
    for d in DELETIONS:
        applied, ok, fails, pkgs, out, broken = apply_and_run(d, saved, "old", "new")
        by_target = (not broken) and d["target"] in fails
        by_any = (not broken) and len(fails) > 0
        agree = ("as predicted"
                 if (by_target == d["predict_target"] and by_any == d["predict_any"])
                 else "⚠ PREDICTION WRONG")
        head = ("BROKEN BUILD" if broken else
                "TARGET SEES IT" if by_target else
                "ONLY OTHERS SEE IT" if by_any else "NOTHING SEES IT")
        dres.append(dict(c=d, by_target=by_target, by_any=by_any, fails=fails, broken=broken,
                         applied=applied, agree=agree))
        print(f"{head:<19} {d['name']}   ({agree})")
        print(f"      applied-on-disk={applied}  failing={fails or '{}'}  pkgs={pkgs or '{}'}")
        print(f"      predicted target={d['predict_target']} any={d['predict_any']} — {d['why']}")
        for t, lines in messages(out, set(fails)).items():
            print(f"      ↳ {t}: {' | '.join(lines)[:220]}")
        print()

    print("═══ FAMILY P — the STATEMENT runs and matches no row ═══\n")
    for p in NOOPS:
        applied, ok, fails, pkgs, out, broken = apply_and_run(p, saved, "old", "new")
        by_any = (not broken) and len(fails) > 0
        agree = "as predicted" if by_any == p["predict_any"] else "⚠ PREDICTION WRONG"
        exact = fails == sorted(p["fixed_tests"])
        head = ("BROKEN BUILD" if broken else
                "SOMETHING SEES IT" if by_any else "NOTHING SEES IT")
        pres.append(dict(c=p, by_any=by_any, fails=fails, broken=broken, applied=applied,
                         agree=agree, exact=exact))
        print(f"{head:<19} {p['name']}   ({agree})")
        print(f"      applied-on-disk={applied}  failing={fails or '{}'}  pkgs={pkgs or '{}'}")
        print(f"      predicted any={p['predict_any']} — {p['why']}")
        print(f"      after the product-half merge, EXACTLY: {sorted(p['fixed_tests'])} — "
              f"match={exact}")
        for t, lines in messages(out, set(fails)).items():
            print(f"      ↳ {t}: {' | '.join(lines)[:220]}")
        print()

    print("═══ FAMILY M — did MOVING customdomain's four per-test checks shrink coverage? ═══\n")
    mres = []
    for m in MOVED:
        applied, ok, fails, pkgs, out, broken = apply_and_run(m, saved, "old", "new")
        msgs = messages(out, set(fails))
        by_target = (not broken) and fails == [m["target"]]
        right_branch = m["msg"] in " ".join(msgs.get(m["target"], []))
        agree = "as predicted" if (by_target and right_branch) else "⚠ PREDICTION WRONG"
        head = ("BROKEN BUILD" if broken else
                "CAUGHT" if by_target and right_branch else
                "CAUGHT, WRONG BRANCH" if by_target else "NOT CAUGHT")
        mres.append(dict(c=m, by_target=by_target, right_branch=right_branch, fails=fails,
                         broken=broken, agree=agree))
        print(f"{head:<22} {m['name']}   ({agree})")
        print(f"      applied-on-disk={applied}  failing={fails or '{}'}  pkgs={pkgs or '{}'}")
        print(f"      predicted: EXACTLY {m['target']}, message containing {m['msg']!r}")
        for t, lines in msgs.items():
            print(f"      ↳ {t}: {' | '.join(lines)[:220]}")
        print()

    print("── restored ──")
    clean = True
    for p in files:
        same = sha(p) == hashlib.sha256(saved[p]).hexdigest()
        clean = clean and same
        print(f"    {os.path.relpath(p, REPO):34} {sha(p)[:12]}  restored={same}")

    n_dagree = sum(1 for r in dres if r["agree"] == "as predicted")
    n_pagree = sum(1 for r in pres if r["agree"] == "as predicted")
    print(f"\nFAMILY D: {sum(1 for r in dres if r['by_target'])}/{len(dres)} seen by their own "
          f"mock test · {sum(1 for r in dres if r['by_any'])}/{len(dres)} seen by anything · "
          f"{n_dagree}/{len(dres)} matched the prediction")
    print(f"FAMILY P: {sum(1 for r in pres if r['by_any'])}/{len(pres)} seen by anything · "
          f"{n_pagree}/{len(pres)} matched the prediction")
    print(f"FAMILY M: {sum(1 for r in mres if r['by_target'] and r['right_branch'])}/{len(mres)} "
          f"caught by exactly their own test, through the constructor's own assertion — the four "
          f"moved checks did not shrink")

    unheld_d = [r["c"]["name"] for r in dres if not r["by_any"]]
    if unheld_d:
        print("\n⚠ THE CALL CAN VANISH AND NOTHING IN THE REPO SPEAKS:")
        for i in unheld_d:
            print(f"    · {i}")
    unheld_p = [r["c"]["name"] for r in pres if not r["by_any"]]
    if unheld_p:
        print("⚠⚠ THE STATEMENT CAN RUN AND CHANGE NOTHING AND NOTHING IN THE REPO SPEAKS:")
        for i in unheld_p:
            print(f"    · {i}")

    if os.environ.get("W31_FIXED") == "1":
        # The red→green half. The fix in this merge is the MOCK check, so it is FAMILY D that must
        # become fully visible; FAMILY P is a real-Postgres question a mock cannot answer and its
        # escapes are REPORTED, not asserted away.
        # W31_FIXED now means BOTH merges are applied: `3c8fbb2`'s mock check (family D and M)
        # and the product-level real-PG assertions (family P). Before the second merge every
        # `fixed_tests` list below was empty in practice and this block failed — that is the
        # red-first record for the product half.
        escaped = [r["c"]["name"] for r in dres if not r["by_target"]]
        drifted = [f'{r["c"]["name"]}  got {r["fails"]} want {sorted(r["c"]["fixed_tests"])}'
                   for r in pres if not r["exact"]]
        if escaped or drifted:
            if escaped:
                print(f"\nW31_FIXED: {len(escaped)} FAMILY-D CONTROL(S) STILL ESCAPE THEIR OWN "
                      f"MOCK TEST — the mock half is incomplete:")
                for e in escaped:
                    print(f"    · {e}")
            if drifted:
                print(f"\nW31_FIXED: {len(drifted)} FAMILY-P CONTROL(S) DID NOT PRODUCE THE "
                      f"PREDICTED FAILING SET — a catch by the wrong test is not this merge's "
                      f"claim:")
                for e in drifted:
                    print(f"    · {e}")
            return 1
        print(f"\nW31_FIXED: all {len(dres)} FAMILY-D controls are caught by the very mock test "
              f"whose expectation names the write, and all {len(pres)} FAMILY-P controls produce "
              f"EXACTLY the failing set predicted for them. Every D and P prediction recorded "
              f"above describes the tree BEFORE these merges and is now falsified, which is the "
              f"point.")
    return 0 if clean else 1


if __name__ == "__main__":
    sys.exit(main())
