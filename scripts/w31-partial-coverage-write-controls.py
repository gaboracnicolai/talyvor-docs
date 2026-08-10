#!/usr/bin/env python3
"""
W3.1 finding (10) — THE PACKAGES THAT LOOK COVERED. 55 EXPECTATIONS, EIGHT PACKAGES.

WHY THIS EXISTS AND WHY IT IS NOT A REPEAT OF THE LAST ONE. Finding (9) selected packages with
ZERO `ExpectationsWereMet` and `3c8fbb2` closed all six. The packages with SOME are the harder
half: somebody wrote the call per test, where they remembered, so the package reads as covered and
`grep -c ExpectationsWereMet` returns a reassuring number. Counting a test as covered only when it
calls the check ITSELF, or calls a constructor whose body contains it, the residue is LARGER than
the 48 that were just closed:

    pagelock 12/15 · changelog 8/9 · analytics 7/9 · approval 7/27 · sharing 7/9
    templatelib 7/9 · search 4/10 · permission 3/7        = 55 of 167 unverified

⚠ THE PREDICATE, NOT THE COUNT, IS THE THING TO GET RIGHT, AND IT CAUGHT ME ONCE ALREADY. A first
pass counted a test covered whenever it called `newMockStore` at all and reported FOUR unverified
expectations repo-wide. Seven packages have a `newMockStore` WITH NO CHECK IN IT. The constructor
is not the guard; the constructor that has the check in it is. That is the only reason the number
above is 55 and not 4.

⚠ AND THE HEURISTIC'S OWN LIMIT, STATED: it looks for `ExpectationsWereMet` anywhere in the test
body, so a call inside a branch counts as covered. The count is therefore a FLOOR.

WHAT THIS HARNESS MEASURES. One write per package, deleted from store.go, whole repo run verbosely
against real Postgres. Seven of the eight target a test the census says is UNCOVERED; E7 targets a
test the census says IS covered and was written as the PREMISE. ⚠ THE RUN CORRECTED THAT: E7 reds
through an ordered-mock query MISMATCH on the next statement, not through the check, so it proves
the harness can redden and nothing more. What carries the premise is E3 and E8, which red with
`there is a remaining expectation which was not matched` — the check itself, on a write that
simply stopped being called. Kept as written, with the correction, rather than retargeted.

`predict_target` — does the mock test whose Expect… names this write go red?
`predict_any`    — does ANYTHING in the repo go red?
Both are written before the run. Verdicts are read from failing test names AND their printed
assertion lines; a build failure is scored BROKEN, never as a catch.

Usage:
    DOCS_TEST_DATABASE_URL=... python3 scripts/w31-partial-coverage-write-controls.py
    W31_FIXED=1 DOCS_TEST_DATABASE_URL=... python3 scripts/w31-partial-coverage-write-controls.py
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def f(*parts):
    return os.path.join(REPO, *parts)


DELETIONS = [
    dict(
        name="E1  approval.PublishApproved   UPDATE pages deleted",
        file=f("internal", "approval", "store.go"), target="TestPublishApproved_SucceedsWhenApproved",
        predict_target=False, predict_any=False,
        why="its test is one of the six in this package the census marks UNCOVERED; the status "
            "SELECT above is consumed and the UPDATE expectation is left unfulfilled. No real-PG "
            "test publishes. Predicting NOTHING speaks.",
        old='''	if _, err := s.pool.Exec(ctx,
		`UPDATE pages SET doc_status = $1 WHERE id = $2 AND workspace_id = ANY($3)`,
		string(DocApproved), pageID, wsIDs,
	); err != nil {
		return err
	}
	return nil''',
        new='''	_, _ = pageID, wsIDs
	return nil''',
    ),
    dict(
        name="E2  pagelock.Unlock            UPDATE pages deleted",
        file=f("internal", "pagelock", "store.go"), target="TestUnlock_ByLockerSucceeds",
        predict_target=False, predict_any=False,
        why="12 of this package's 15 expectations are unverified and both Unlock success tests are "
            "among them. A lock that will not release is a page nobody else can edit.",
        old='''	_, err = s.pool.Exec(ctx,
		`UPDATE pages SET locked = false, locked_by = NULL, locked_at = NULL
        WHERE id = $1`,
		pageID,
	)
	return err''',
        new='''	_ = pageID
	return err''',
    ),
    dict(
        name="E3  permission.Grant           INSERT deleted",
        file=f("internal", "permission", "store.go"), target="TestGrant_AcceptsEveryone",
        predict_target=False, predict_any=True,
        why="TestGrant_UpsertsPermissionRow DOES call the check, so the sibling with no check is "
            "the uncovered one. But permission grants are what the SEC-4 enforcers read, and "
            "several cross-tenant suites seed access through Grant — predicting those red on an "
            "authorization they were promised.",
        old='''	_, err := s.pool.Exec(ctx,
		`INSERT INTO permissions (resource_type, resource_id, subject_type, subject_id, access, workspace_id, granted_by)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (resource_type, resource_id, subject_type, subject_id)
        DO UPDATE SET access = EXCLUDED.access, granted_by = EXCLUDED.granted_by`,
		string(p.ResourceType), p.ResourceID, p.SubjectType, p.SubjectID, string(p.Access), p.WorkspaceID, p.GrantedBy,
	)
	if err != nil {
		return fmt.Errorf("permission: grant: %w", err)
	}
	return nil''',
        new='''	_ = p
	return nil''',
    ),
    dict(
        name="E4  templatelib.Delete         DELETE deleted",
        file=f("internal", "templatelib", "store.go"), target="TestDelete_AllowsWorkspaceTemplate",
        predict_target=False, predict_any=True,
        why="target uncovered. But SEC4_L2 calls templatelib.Delete THE DECEPTIVE SHAPE and asserts "
            "a cross-tenant delete is 404 — which comes from the RowsAffected branch this mutation "
            "removes with the write. READ THE MESSAGE: that is the tenancy half, not a row "
            "assertion.",
        old='''	tag, err := s.pool.Exec(ctx,
		`DELETE FROM library_templates WHERE id = $1 AND workspace_id = ANY($2)`,
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
        name="E5  sharing.Validate           view_count bump deleted",
        file=f("internal", "sharing", "store.go"), target="TestValidate_AcceptsCorrectPassword",
        predict_target=False, predict_any=False,
        why="the caller already DISCARDS this error on purpose (`_ = err`, a bump must not fail the "
            "read), so nothing downstream can notice either. The most inert site of the eight.",
        old='''	if _, err := s.pool.Exec(ctx,
		`UPDATE share_links SET view_count = view_count + 1 WHERE id = $1`,
		link.ID,
	); err != nil {
		// View-count bump failure shouldn't fail the read — log and
		// continue once a logger is wired. Phase 8 keeps it simple.
		_ = err
	}
''',
        new='''''',
    ),
    dict(
        name="E6  changelog.DeleteEntry      DELETE deleted",
        file=f("internal", "changelog", "store.go"), target="TestDeleteEntry_DeletesByID",
        predict_target=False, predict_any=True,
        why="8 of this package's 9 expectations are unverified, including this one. SEC4_L2 names "
            "changelog.DeleteEntry explicitly and wants 404 cross-tenant, so the removed "
            "RowsAffected branch should red there — the tenancy half again, not the row.",
        old='''	tag, err := s.pool.Exec(ctx,
		`DELETE FROM changelog_entries WHERE id = $1 AND workspace_id = ANY($2)`, id, wsIDs)
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
        name="E7  analytics.RecordView       INSERT deleted  (PREMISE)",
        file=f("internal", "analytics", "store.go"),
        target="TestRecordView_InsertsRowAndIncrementsPageCounter",
        predict_target=True, predict_any=True,
        why="THE PREMISE — AND THE RUN CORRECTED WHAT IT PROVES. This is the one target the "
            "census says IS covered. It DID red, but READ THE MESSAGE: `could not match actual "
            "sql: UPDATE pages SET view_count … with expected regexp INSERT INTO page_views`. "
            "That is an ORDERED-MOCK MISMATCH on the NEXT statement, not the check firing on an "
            "unfulfilled expectation — being caught and being checked are different things, and "
            "this control cannot tell them apart. The premise is carried instead by E3 and E8, "
            "whose failures print `there is a remaining expectation which was not matched`: THAT "
            "is ExpectationsWereMet speaking, and it is the only reason a NOTHING SEES IT "
            "elsewhere in this family is a fact about the repo rather than about the harness.",
        old='''	if _, err := s.pool.Exec(ctx,
		`INSERT INTO page_views (page_id, workspace_id, viewer_id, viewer_name, duration_sec)
        VALUES ($1, $2, $3, $4, $5)`,
		view.PageID, view.WorkspaceID, view.ViewerID, view.ViewerName, view.Duration,
	); err != nil {
		return fmt.Errorf("analytics: insert view: %w", err)
	}
''',
        new='''''',
    ),
    dict(
        name="E8  search.IndexPage           embedding upsert deleted",
        file=f("internal", "search", "semantic.go"),
        target="TestIndexPage_IdempotentOnUpsertConflict",
        predict_target=False, predict_any=True,
        why="target uncovered; but TestIndexPage_CallsLensEmbeddingsAndUpserts and the two-tenant "
            "isolation test both DO call the check and both drive this path, so a sibling should "
            "speak. If nothing does, semantic search silently stops being indexed.",
        old='''	_, err = s.pool.Exec(ctx,
		`INSERT INTO page_embeddings (page_id, embedding)
        VALUES ($1, $2::vector)
        ON CONFLICT (page_id) DO UPDATE SET
            embedding  = EXCLUDED.embedding,
            updated_at = NOW()`,
		pageID, encoded,
	)''',
        new='''	_, _ = pageID, encoded
	err = nil''',
    ),
]


def sha(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def run_all():
    proc = subprocess.run(
        ["go", "test", "-count=1", "-timeout", "600s", "-v", "./..."],
        cwd=REPO, env=dict(os.environ), capture_output=True, text=True,
    )
    out = proc.stdout + proc.stderr
    fails = sorted(set(re.findall(r"^\s*--- FAIL: (\S+)", out, re.M)))
    pkgs = sorted(set(re.findall(r"^FAIL\s+github.com/talyvor/docs/(\S+)", out, re.M)))
    broken = ("[build failed]" in out or "declared and not used" in out
              or re.search(r"^# github.com/talyvor/docs", out, re.M) is not None)
    return proc.returncode == 0, fails, pkgs, out, broken


def messages(out, tests, limit=2):
    found, cur = {}, None
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


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set.")
        return 2

    files = sorted({c["file"] for c in DELETIONS})
    saved = {p: open(p, "rb").read() for p in files}

    bad = False
    for c in DELETIONS:
        n = saved[c["file"]].decode().count(c["old"])
        if n != 1:
            print(f"ANCHOR FAIL {c['name']}: {n} occurrences, want exactly 1")
            bad = True
    if bad:
        return 2
    print(f"anchors: {len(DELETIONS)}/{len(DELETIONS)} unique across {len(files)} files")

    ok, fails, pkgs, _, _ = run_all()
    if not ok:
        print(f"BASELINE IS RED ({fails} in {pkgs}). Stop.")
        return 2
    print("baseline: whole repo GREEN on real Postgres\n")

    res = []
    for d in DELETIONS:
        path = d["file"]
        text = saved[path].decode()
        mutated = text.replace(d["old"], d["new"])
        assert mutated != text, d["name"]
        open(path, "w").write(mutated)
        on_disk = open(path).read()
        inserted = d["old"] in d["new"]
        applied = on_disk.count(d["old"]) == 0 or inserted
        ok, fails, pkgs, out, broken = run_all()
        open(path, "wb").write(saved[path])

        by_target = (not broken) and d["target"] in fails
        by_any = (not broken) and len(fails) > 0
        agree = ("as predicted"
                 if (by_target == d["predict_target"] and by_any == d["predict_any"])
                 else "⚠ PREDICTION WRONG")
        head = ("BROKEN BUILD" if broken else
                "TARGET SEES IT" if by_target else
                "ONLY OTHERS SEE IT" if by_any else "NOTHING SEES IT")
        res.append(dict(c=d, by_target=by_target, by_any=by_any, fails=fails, broken=broken,
                        agree=agree))
        print(f"{head:<19} {d['name']}   ({agree})")
        print(f"      applied-on-disk={applied}  failing={fails or '{}'}  pkgs={pkgs or '{}'}")
        print(f"      predicted target={d['predict_target']} any={d['predict_any']} — {d['why']}")
        for t, lines in messages(out, set(fails)).items():
            print(f"      ↳ {t}: {' | '.join(lines)[:200]}")
        print()

    print("── restored ──")
    clean = all(sha(p) == hashlib.sha256(saved[p]).hexdigest() for p in files)
    for p in files:
        print(f"    {os.path.relpath(p, REPO):34} {sha(p)[:12]}  "
              f"restored={sha(p) == hashlib.sha256(saved[p]).hexdigest()}")

    n_agree = sum(1 for r in res if r["agree"] == "as predicted")
    print(f"\n{sum(1 for r in res if r['by_target'])}/{len(res)} seen by their own mock test · "
          f"{sum(1 for r in res if r['by_any'])}/{len(res)} seen by anything · "
          f"{n_agree}/{len(res)} matched the prediction")
    unheld = [r["c"]["name"] for r in res if not r["by_any"]]
    if unheld:
        print("\n⚠ THE CALL CAN VANISH AND NOTHING IN THE REPO SPEAKS:")
        for i in unheld:
            print(f"    · {i}")

    if os.environ.get("W31_FIXED") == "1":
        escaped = [r["c"]["name"] for r in res if not r["by_target"]]
        if escaped:
            print(f"\nW31_FIXED: {len(escaped)} CONTROL(S) STILL ESCAPE THEIR OWN MOCK TEST:")
            for e in escaped:
                print(f"    · {e}")
            return 1
        print(f"\nW31_FIXED: all {len(res)} controls are caught by the very mock test whose "
              f"expectation names the write. Every prediction above is falsified, which is the "
              f"point.")
    return 0 if clean else 1


if __name__ == "__main__":
    sys.exit(main())
