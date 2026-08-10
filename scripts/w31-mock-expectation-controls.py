#!/usr/bin/env python3
"""
W3.1 finding (7) — WHICH pgxmock ExpectExec ARGUMENT ASSERTIONS IN internal/page CAN ACTUALLY FAIL.

THE QUESTION, NOT THE FIX. `f53a118` (#60) found by positive control that
`TestUpdate_AppendsNewVersionOnContentChange`'s `WithArgs(...)` could not fail: `Update` writes the
version snapshot with `_, _ = s.pool.Exec(...)`, so a pgxmock argument MISMATCH is swallowed by the
PRODUCTION code and the test exits 0. It made live ONLY the one expectation its own claim depended
on. `internal/page` has SIX `ExpectExec` expectations; five were left unmeasured, and the queue's
scoping is explicit that the answer is NOT "add ExpectationsWereMet everywhere" — at an ExpectExec
whose caller CHECKS the Exec error, a mismatch already surfaces, and a second check there is
redundant enforcement no one-line control can breach.

So this harness asks, per site: IS THE ARGUMENT ASSERTION LOAD-BEARING?

METHOD. Each control mutates exactly ONE argument of one `WithArgs` to a value the shipped code
provably does not pass, then runs the WHOLE package verbosely and reads WHICH tests failed and with
WHAT MESSAGE. The mutation is in the TEST, not the product — that is the right unit here, because
the claim under test is "this assertion notices a wrong argument", and the only way to ask it is to
supply one.

CATCHER PREDICTED BEFORE THE RUN for every control (see `predict` below); a control whose verdict
disagrees with its prediction is a finding about my model, not a pass.

MUST-STAY-GREEN COMPANION IS BUILT IN AND IT IS THE WHOLE REST OF THE PACKAGE: a control is CAUGHT
only if the failing set is EXACTLY the predicted test. A mutation that reddens something else has
broken the suite rather than exercised the assertion, and #59's harness recorded that a compile
error scores as a catch if you only read the exit code.

Bytes are saved before the first write and restored from those bytes; sha256 compared at the end.

EVERY `predict_*` FIELD BELOW DESCRIBES THE TREE AT `f53a118` — BEFORE the fix this harness
justifies. That is the only state in which they are claims about a defect. Run it on `f53a118` and
the predictions are the verdicts; run it on the fix and every control must be CAUGHT, which is the
red→green record and is what `W31_FIXED=1` asserts. A control set whose predictions still "match"
after the fix would mean the fix changed nothing.

Usage:
    DOCS_TEST_DATABASE_URL=... python3 scripts/w31-mock-expectation-controls.py
    W31_FIXED=1 DOCS_TEST_DATABASE_URL=... python3 scripts/w31-mock-expectation-controls.py
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TEST_FILE = os.path.join(REPO, "internal", "page", "store_test.go")
PKG = "./internal/page/"

# ─── The five unmeasured ExpectExec sites, plus the one f53a118 already made live ───────────
#
# `kind` is the PREDICTION about the production caller, made by reading store.go BEFORE running
# anything, and it is what `verdict` falsifies:
#   discarded → the caller is `_, _ = s.pool.Exec(...)`; a mismatch cannot reach the test  ⇒ NOT CAUGHT
#   checked   → the caller returns/wraps the Exec error; the test's own t.Fatalf sees it    ⇒ CAUGHT
CONTROLS = [
    dict(
        name="C1  Create v1 snapshot        (store.go:314  `_, _ = s.pool.Exec`)",
        kind="discarded",
        predict_caught=False,
        predict_test=None,
        predict_msg=None,
        old='''	pool.ExpectExec(`INSERT INTO page_versions \\(page_id, workspace_id, version`).
		WithArgs("pg-1", "ws-1", 1, "My New Page", "{}", "creator").''',
        new='''	pool.ExpectExec(`INSERT INTO page_versions \\(page_id, workspace_id, version`).
		WithArgs("pg-1", "ws-1", 1, "CONTROL-WRONG-TITLE", "{}", "creator").''',
        target="TestCreate_AutoSlugAndDepthAndVersion",
    ),
    dict(
        name="C2  Delete reparent children  (store.go:575  `if _, err := ...; err != nil`)",
        kind="checked",
        predict_caught=True,
        predict_test="TestDelete_ReparentsChildren",
        predict_msg="page: reparent:",
        old='''	pool.ExpectExec(`UPDATE pages SET parent_id`).
		WithArgs(ptrStr("pg-root"), "pg-mid").''',
        new='''	pool.ExpectExec(`UPDATE pages SET parent_id`).
		WithArgs(ptrStr("CONTROL-WRONG-PARENT"), "pg-mid").''',
        target="TestDelete_ReparentsChildren",
    ),
    dict(
        name="C3  Delete the page row       (store.go:582  `if _, err := ...; err != nil`)",
        kind="checked",
        predict_caught=True,
        predict_test="TestDelete_ReparentsChildren",
        predict_msg="page: delete:",
        old='''	pool.ExpectExec(`DELETE FROM pages WHERE id`).
		WithArgs("pg-mid").''',
        new='''	pool.ExpectExec(`DELETE FROM pages WHERE id`).
		WithArgs("CONTROL-WRONG-ID").''',
        target="TestDelete_ReparentsChildren",
    ),
    dict(
        name="C4  RecordView                (store.go:599  `_, err := ...; return err`)",
        kind="checked",
        predict_caught=True,
        predict_test="TestRecordView_IncrementsCount",
        predict_msg="RecordView:",
        old='''	pool.ExpectExec(`UPDATE pages SET view_count`).
		WithArgs("pg-1").''',
        new='''	pool.ExpectExec(`UPDATE pages SET view_count`).
		WithArgs("CONTROL-WRONG-ID").''',
        target="TestRecordView_IncrementsCount",
    ),
    dict(
        name="C5  Verify                    (store.go:616  `_, err := ...; return err`)",
        kind="checked",
        predict_caught=True,
        predict_test="TestVerify_SetsTimestampAndOwner",
        predict_msg="Verify:",
        old='''	pool.ExpectExec(`UPDATE pages SET last_verified_at`).
		WithArgs("verifier-1", "pg-1").''',
        new='''	pool.ExpectExec(`UPDATE pages SET last_verified_at`).
		WithArgs("CONTROL-WRONG-VERIFIER", "pg-1").''',
        target="TestVerify_SetsTimestampAndOwner",
    ),
    # C6 IS THE PREMISE CONTROL, NOT A SIXTH SITE. It replays f53a118's V4 against the expectation
    # that merge made live. Without it, "C1 is NOT CAUGHT" is indistinguishable from "this harness
    # cannot redden a discarded-Exec site at all" — the instrument would be vouching for itself.
    # Same caller shape as C1 (`_, _ = s.pool.Exec`, store.go:517); the ONLY difference is that its
    # test calls pool.ExpectationsWereMet(). If C6 is CAUGHT and C1 is not, the difference is the
    # check and nothing else.
    dict(
        name="C6  Update snapshot           (store.go:517  `_, _ =` + ExpectationsWereMet — PREMISE)",
        kind="discarded+checked",
        predict_caught=True,
        predict_test="TestUpdate_AppendsNewVersionOnContentChange",
        predict_msg="unmet or mismatched pgxmock expectations",
        old='''	pool.ExpectExec(`INSERT INTO page_versions \\(page_id, workspace_id, version`).
		WithArgs("pg-1", "ws-1", 4, "New", "{}", "editor").''',
        new='''	pool.ExpectExec(`INSERT INTO page_versions \\(page_id, workspace_id, version`).
		WithArgs("pg-1", "ws-1", 4, "CONTROL-WRONG-TITLE", "{}", "editor").''',
        target="TestUpdate_AppendsNewVersionOnContentChange",
    ),
]


STORE_FILE = os.path.join(REPO, "internal", "page", "store.go")

# ─── FAMILY D — THE WRITE ITSELF DELETED FROM THE PRODUCT ───────────────────────────────────
#
# Family A asks "does this assertion notice a WRONG argument". That is not the only way a mock
# expectation goes inert, and it is not the most consequential one. An `ExpectExec` that is never
# CALLED is, without `ExpectationsWereMet`, silently ignored — so a test can assert in detail about
# a write that no longer happens. Family A cannot see this: at a checked caller there is no error
# to return when there is no call.
#
# So each D control DELETES the write from store.go and asks TWO questions the A family conflates:
#   · does the TARGET mock test — the one whose expectation names this write — go red?
#   · does ANYTHING in the package go red, i.e. is the BEHAVIOUR protected at all?
# Both are predicted below. Every mutation is written to COMPILE (`_ = x` where removing the call
# would orphan a local); a build failure is detected explicitly and scored as BROKEN, never as a
# catch — #59's harness recorded that trap and the verdict logic here is the instrument too.
DELETIONS = [
    dict(
        name="D1  Create v1 snapshot        write deleted",
        predict_target=False, predict_any=True,
        why="ordered mock: it is the LAST expectation, so nothing absorbs it and nothing checks "
            "it. The RealPG version tests renumber (MAX+1) and should speak instead.",
        target="TestCreate_AutoSlugAndDepthAndVersion",
        old='''	_, _ = s.pool.Exec(ctx,
		`INSERT INTO page_versions (page_id, workspace_id, version, title, content, created_by)
        VALUES ($1, $2, $3, $4, $5, $6)`,
		out.ID, out.WorkspaceID, 1, out.Title, out.Content, p.CreatedBy,
	)
	return out, nil''',
        new='''	return out, nil''',
    ),
    dict(
        name="D2  Delete reparent children  write deleted",
        predict_target=True, predict_any=True,
        why="ordered mock: the surviving DELETE call is matched against the REPARENT expectation, "
            "mismatches on the query, and Delete CHECKS that error.",
        target="TestDelete_ReparentsChildren",
        old='''	if _, err := s.pool.Exec(ctx,
		`UPDATE pages SET parent_id = $1, updated_at = NOW() WHERE parent_id = $2`,
		parent, id,
	); err != nil {
		return fmt.Errorf("page: reparent: %w", err)
	}''',
        new='''	_ = parent''',
    ),
    dict(
        name="D3  Delete the page row       write deleted",
        predict_target=False, predict_any=True,
        why="the reparent expectation is consumed correctly and the DELETE expectation is simply "
            "left unfulfilled. The SEC-4 real-PG by-id tests should speak instead.",
        target="TestDelete_ReparentsChildren",
        old='''	if _, err := s.pool.Exec(ctx, `DELETE FROM pages WHERE id = $1`, id); err != nil {
		return fmt.Errorf("page: delete: %w", err)
	}
	return nil''',
        new='''	return nil''',
    ),
    dict(
        name="D4  RecordView                write deleted",
        predict_target=False, predict_any=False,
        why="one expectation, left unfulfilled, and no real-PG test in this package reads "
            "view_count. Predicting NOTHING speaks.",
        target="TestRecordView_IncrementsCount",
        old='''	_, err := s.pool.Exec(ctx,
		`UPDATE pages SET view_count = view_count + 1,
            last_viewed_at = NOW(), updated_at = NOW()
        WHERE id = $1`, pageID,
	)
	return err
}''',
        new='''	_ = pageID
	return nil
}''',
    ),
    dict(
        name="D5  Verify                    write deleted",
        predict_target=False, predict_any=False,
        why="same shape as D4; the stale-pages test is itself a mock. Predicting NOTHING speaks.",
        target="TestVerify_SetsTimestampAndOwner",
        old='''	_, err := s.pool.Exec(ctx,
		`UPDATE pages SET last_verified_at = NOW(), verified_by = $1,
            updated_at = NOW()
        WHERE id = $2`, verifierID, pageID,
	)
	return err
}''',
        new='''	_ = verifierID
	_ = pageID
	return nil
}''',
    ),
    dict(
        name="D6  Update snapshot           write deleted (PREMISE)",
        predict_target=True, predict_any=True,
        why="the premise again: same discarded-Exec caller as D1, and the ONLY difference is that "
            "its test calls ExpectationsWereMet. If D6 reddens its target and D1 does not, the "
            "check is the whole difference.",
        target="TestUpdate_AppendsNewVersionOnContentChange",
        old='''		_, _ = s.pool.Exec(ctx,
			`INSERT INTO page_versions (page_id, workspace_id, version, title, content, created_by)
            VALUES ($1, $2, $3, $4, $5, $6)`,
			id, out.WorkspaceID, nextVer, out.Title, out.Content, updatedBy,
		)''',
        new='''		_ = nextVer
		_ = updatedBy''',
    ),
]


def sha(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def run_pkg():
    """Run the whole package verbosely; return (ok, failing test names, output)."""
    env = dict(os.environ)
    proc = subprocess.run(
        ["go", "test", "-count=1", "-timeout", "300s", "-v", PKG],
        cwd=REPO, env=env, capture_output=True, text=True,
    )
    fails = sorted(set(re.findall(r"^\s*--- FAIL: (\S+)", proc.stdout, re.M)))
    out = proc.stdout + proc.stderr
    # A COMPILE ERROR IS NOT A CATCH. Without this the exit code alone scores a broken build as a
    # working guard, which is the failure mode #59's harness recorded and the reason the verdict
    # logic is treated as an instrument rather than as arithmetic.
    broken = ("[build failed]" in out or "build failed" in out
              or "declared and not used" in out or "# github.com/talyvor/docs" in out)
    return proc.returncode == 0, fails, out, broken


def message_for(out, test):
    """The assertion text the target test printed — a verdict read from a name is not a verdict."""
    lines = []
    grab = False
    for ln in out.splitlines():
        if ln.startswith("=== RUN   " + test):
            grab = True
            continue
        if grab and ln.startswith("=== RUN"):
            grab = False
        if grab and ("store_test.go:" in ln or ".go:" in ln and ":" in ln):
            lines.append(ln.strip())
    return " | ".join(lines[:6])


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set — this package's real-PG tests FAIL rather "
              "than skip without it, and a harness that ran half the suite is not a harness.")
        return 2

    with open(TEST_FILE, "rb") as fh:
        pristine = fh.read()
    pristine_sha = hashlib.sha256(pristine).hexdigest()
    text = pristine.decode()

    # ── ASSERT EVERY ANCHOR BEFORE ANY WRITE ────────────────────────────────────────────────
    # A half-applied harness silently retargets its own controls. Nothing is written until all
    # six anchors are proven to occur EXACTLY once.
    bad = False
    for c in CONTROLS:
        n = text.count(c["old"])
        if n != 1:
            print(f"ANCHOR FAIL {c['name']}: {n} occurrences, want exactly 1")
            bad = True
    if bad:
        return 2
    print(f"anchors: {len(CONTROLS)}/{len(CONTROLS)} unique  ·  {TEST_FILE} sha256 {pristine_sha[:12]}")

    with open(STORE_FILE, "rb") as fh:
        store_pristine = fh.read()
    store_sha = hashlib.sha256(store_pristine).hexdigest()
    store_text = store_pristine.decode()
    for d in DELETIONS:
        n = store_text.count(d["old"])
        if n != 1:
            print(f"ANCHOR FAIL {d['name']}: {n} occurrences in store.go, want exactly 1")
            bad = True
    if bad:
        return 2
    print(f"anchors: {len(DELETIONS)}/{len(DELETIONS)} unique  ·  store.go sha256 {store_sha[:12]}")

    ok, fails, _, _ = run_pkg()
    if not ok:
        print(f"BASELINE IS RED ({fails}) — every verdict below would be unreadable. Stop.")
        return 2
    print("baseline: package GREEN\n")

    print("═══ FAMILY A — a WRONG ARGUMENT supplied to an existing write ═══\n")
    results = []
    for c in CONTROLS:
        mutated = text.replace(c["old"], c["new"])
        assert mutated != text
        with open(TEST_FILE, "w") as fh:
            fh.write(mutated)
        # Prove the mutation is ON DISK and is the one intended — an anchor assertion proves
        # application, not meaning, so check the new bytes are readable back.
        with open(TEST_FILE) as fh:
            on_disk = fh.read()
        applied = on_disk.count(c["new"]) == 1 and on_disk.count(c["old"]) == 0

        ok, fails, out, broken = run_pkg()
        with open(TEST_FILE, "wb") as fh:
            fh.write(pristine)

        caught = (not ok) and (not broken) and fails == [c["target"]]
        msg = message_for(out, c["target"]) if not ok else ""
        results.append(dict(c=c, applied=applied, caught=caught, fails=fails, msg=msg,
                            broken=broken))

        verdict = "BROKEN    " if broken else ("CAUGHT    " if caught else "NOT CAUGHT")
        agree = "as predicted" if caught == c["predict_caught"] else "⚠ PREDICTION WRONG"
        print(f"{verdict}  {c['name']}")
        print(f"            applied-on-disk={applied}  failing-set={fails or '{}'}  ({agree})")
        if msg:
            print(f"            {msg[:200]}")
        if caught and c["predict_msg"] and c["predict_msg"] not in msg:
            print(f"            ⚠ CAUGHT BY THE WRONG BRANCH: expected message to contain "
                  f"{c['predict_msg']!r}")
        print()

    print("═══ FAMILY D — the WRITE ITSELF deleted from store.go ═══\n")
    dresults = []
    for d in DELETIONS:
        mutated = store_text.replace(d["old"], d["new"])
        assert mutated != store_text
        with open(STORE_FILE, "w") as fh:
            fh.write(mutated)
        with open(STORE_FILE) as fh:
            on_disk = fh.read()
        applied = on_disk.count(d["new"]) >= 1 and on_disk.count(d["old"]) == 0

        ok, fails, out, broken = run_pkg()
        with open(STORE_FILE, "wb") as fh:
            fh.write(store_pristine)

        by_target = (not broken) and d["target"] in fails
        by_any = (not broken) and len(fails) > 0
        msg = message_for(out, d["target"]) if by_target else ""
        dresults.append(dict(d=d, applied=applied, by_target=by_target, by_any=by_any,
                             fails=fails, broken=broken))

        agree = ("as predicted"
                 if (by_target == d["predict_target"] and by_any == d["predict_any"])
                 else "⚠ PREDICTION WRONG")
        head = "BROKEN BUILD" if broken else (
            "TARGET SEES IT " if by_target else
            ("ONLY OTHERS SEE IT" if by_any else "NOTHING SEES IT"))
        print(f"{head:<19} {d['name']}   ({agree})")
        print(f"            applied-on-disk={applied}  failing-set={fails or '{}'}")
        print(f"            predicted: target={d['predict_target']} any={d['predict_any']} — "
              f"{d['why']}")
        if msg:
            print(f"            {msg[:200]}")
        print()

    restored, restored_store = sha(TEST_FILE), sha(STORE_FILE)
    print(f"restored store_test.go {restored[:12]} == {pristine_sha[:12]}: "
          f"{restored == pristine_sha}   ·   store.go {restored_store[:12]} == "
          f"{store_sha[:12]}: {restored_store == store_sha}")

    n_caught = sum(1 for r in results if r["caught"])
    n_agree = sum(1 for r in results if r["caught"] == r["c"]["predict_caught"])
    d_agree = sum(1 for r in dresults
                  if r["by_target"] == r["d"]["predict_target"]
                  and r["by_any"] == r["d"]["predict_any"])
    print(f"\nFAMILY A: {n_caught}/{len(results)} CAUGHT · {n_agree}/{len(results)} matched "
          f"the catcher predicted before the run")
    print(f"FAMILY D: {sum(1 for r in dresults if r['by_target'])}/{len(dresults)} seen by their "
          f"own target · {d_agree}/{len(dresults)} matched the prediction")
    inert = [r["c"]["name"] for r in results if not r["caught"]]
    if inert:
        print("\nINERT TO A WRONG ARGUMENT (the assertion cannot fail):")
        for i in inert:
            print(f"  · {i}")
    blind = [r["d"]["name"] for r in dresults if not r["by_target"]]
    if blind:
        print("BLIND TO THE WRITE DISAPPEARING (the expectation is silently unfulfilled):")
        for i in blind:
            print(f"  · {i}")
    unheld = [r["d"]["name"] for r in dresults if not r["by_any"]]
    if unheld:
        print("⚠ NOT HELD BY ANY TEST IN THE PACKAGE — deleting the write is entirely silent:")
        for i in unheld:
            print(f"  · {i}")

    if os.environ.get("W31_FIXED") == "1":
        # The red→green half. On the fixed tree the predictions above are all supposed to be
        # WRONG — that is what the fix means — so the only assertion that carries information
        # here is that NOTHING escapes.
        escaped = ([r["c"]["name"] for r in results if not r["caught"]]
                   + [r["d"]["name"] for r in dresults if not r["by_target"]])
        if escaped:
            print(f"\nW31_FIXED: {len(escaped)} CONTROL(S) STILL ESCAPE — the fix is incomplete:")
            for e in escaped:
                print(f"  · {e}")
            return 1
        print(f"\nW31_FIXED: all {len(results) + len(dresults)} controls CAUGHT by their own "
              f"target test. Every prediction above is now falsified, which is the point.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
