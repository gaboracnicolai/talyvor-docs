#!/usr/bin/env python3
"""
W3.1 finding (8) — DOES ANYTHING NOTICE A `Store.Delete` THAT RUNS AND REMOVES NOTHING?

`ef27e3c` (#61) measured that deleting the `DELETE FROM pages` statement from Store.Delete left
`go test ./...` GREEN across the whole repo, and closed the half a mock can close: newMockStore now
verifies its expectations, so the statement DISAPPEARING reddens TestDelete_ReparentsChildren.

THAT IS THE LIMIT OF A MOCK, BY CONSTRUCTION — pgxmock never executes SQL. A statement that is
CALLED, with exactly the arguments the mock expects, and that removes NO ROW satisfies every
expectation. This harness's whole job is to produce that mutation and show which guard sees it.

P1 AND P3 ARE THE CONTROLS THAT EARN THE NEW TESTS. Both keep the call, the query prefix and the
arguments identical and only narrow the predicate, so the mock suite is expected to stay GREEN and
the real-PG tests are expected to be the ONLY thing that reddens. A mutation two guards catch
justifies neither of them; these are the ones only the new guard can see.

P2 is the crude case #61 already holds. It is here to prove the new tests are not blind to it AND
to re-verify #61's check is still live — an expectation is that a control is caught TWICE, and
that is stated rather than read as extra credit.

P4 separates the two new tests from each other. Without it they are two spellings of one claim.

Every control PREDICTS its catcher before the run; anchors are count-asserted before any write;
bytes are verified on disk and restored from saved bytes with a sha256 comparison; a build failure
is detected explicitly so a compile error can never score as a catch.

Usage:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-delete-row-controls.py
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = os.path.join(REPO, "internal", "page", "store.go")

NEW_ROW = "TestDelete_ActuallyRemovesTheRow_RealPG"
NEW_READ = "TestDelete_TheDeletedPageIsNoLongerReadable_RealPG"
MOCK = "TestDelete_ReparentsChildren"

CONTROLS = [
    dict(
        name="P1  DELETE runs, matches, and removes nothing (`AND FALSE`)",
        why="the mutation ONLY the new guard can see: same call, same query prefix, same args, so "
            "every pgxmock expectation is satisfied and #61's ExpectationsWereMet has nothing to "
            "report. If the mock reddens here, P1 is not the control I claimed it is.",
        predict={NEW_ROW, NEW_READ},
        old='	if _, err := s.pool.Exec(ctx, `DELETE FROM pages WHERE id = $1`, id); err != nil {',
        new='	if _, err := s.pool.Exec(ctx, `DELETE FROM pages WHERE id = $1 AND FALSE`, id); err != nil {',
    ),
    dict(
        name="P3  DELETE narrowed by a wrong predicate (`AND is_template = true`)",
        why="the same class as P1 but the shape a real edit takes — somebody adds a condition. "
            "Same prediction, and it is here because `AND FALSE` is a mutation nobody would write "
            "and a control set made only of implausible mutations proves less than it looks.",
        predict={NEW_ROW, NEW_READ},
        old='	if _, err := s.pool.Exec(ctx, `DELETE FROM pages WHERE id = $1`, id); err != nil {',
        new='	if _, err := s.pool.Exec(ctx, `DELETE FROM pages WHERE id = $1 AND is_template = true`, id); err != nil {',
    ),
    dict(
        name="P2  the DELETE statement removed entirely",
        why="the crude case. CAUGHT TWICE by design — #61's mock check AND the new tests — which "
            "is exactly why it cannot justify the new tests on its own. It is here to prove the "
            "new tests are not blind to it and that #61's check has not gone quiet.",
        predict={NEW_ROW, NEW_READ, MOCK},
        old='''	if _, err := s.pool.Exec(ctx, `DELETE FROM pages WHERE id = $1`, id); err != nil {
		return fmt.Errorf("page: delete: %w", err)
	}
	return nil''',
        new='''	return nil''',
    ),
    dict(
        name="P4  GetByID narrowed, Delete untouched (SEPARATES the two new tests)",
        why="without this the two new tests are two spellings of one claim. Delete still works, so "
            "the row-count test must stay GREEN; the readability test must redden — on its own "
            "PRECONDITION, which is the assertion that says its subject was readable to begin "
            "with. That is what proves they read different things.",
        predict={NEW_READ},
        # ⚠ THE BLAST-RADIUS PREDICTION BELOW IS WRONG AND IS KEPT WRONG. Narrowing GetByID
        # reddens every test that reads a page through it — four, not one. Rewriting the
        # prediction after seeing the output would destroy the only thing a prediction is for.
        # What P4 EXISTS to establish is the `separates` pair, and that held: the row-count test
        # stayed GREEN while the readability test reddened on its own precondition.
        separates=(NEW_ROW, NEW_READ),
        old='''func (s *Store) GetByID(ctx context.Context, id string) (*model.Page, error) {
	return scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM pages WHERE id = $1`, id))''',
        new='''func (s *Store) GetByID(ctx context.Context, id string) (*model.Page, error) {
	return scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM pages WHERE id = $1 AND FALSE`, id))''',
    ),
]


def run():
    p = subprocess.run(["go", "test", "-count=1", "-timeout", "600s", "-v", "./internal/page/"],
                       cwd=REPO, capture_output=True, text=True, env=dict(os.environ))
    out = p.stdout + p.stderr
    broken = "[build failed]" in out or "declared and not used" in out
    fails = set(re.findall(r"^\s*--- FAIL: (\S+)", p.stdout, re.M))
    return p.returncode == 0, fails, broken, out


def msg_for(out, test):
    keep, lines = False, []
    for ln in out.splitlines():
        if ln.startswith("=== RUN   " + test):
            keep = True
            continue
        if keep and ln.startswith("=== RUN"):
            keep = False
        if keep and "_test.go:" in ln:
            lines.append(ln.strip())
    return " | ".join(lines[:2])


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set — the tests under control are real-PG tests.")
        return 2

    orig = open(STORE, "rb").read()
    sha0 = hashlib.sha256(orig).hexdigest()
    text = orig.decode()

    bad = False
    for c in CONTROLS:
        n = text.count(c["old"])
        if n != 1:
            print(f"ANCHOR FAIL {c['name']}: {n} occurrences, want 1")
            bad = True
    if bad:
        return 2
    print(f"anchors: {len(CONTROLS)}/{len(CONTROLS)} unique · store.go sha256 {sha0[:12]}")

    ok, fails, broken, _ = run()
    if not ok:
        print(f"BASELINE RED ({sorted(fails)}) — stop.")
        return 2
    print("baseline: internal/page GREEN\n")

    agreed = 0
    for c in CONTROLS:
        open(STORE, "w").write(text.replace(c["old"], c["new"]))
        # The OLD text being GONE is the honest test of application. Counting the NEW text
        # misreports whenever a mutation's replacement is a fragment that already occurs elsewhere
        # (P2 replaces a block with `return nil`, which the file has many of) — it reported
        # applied=False for a mutation whose effect the failing set proves.
        applied = open(STORE).read().count(c["old"]) == 0
        ok, fails, broken, out = run()
        open(STORE, "wb").write(orig)

        if broken:
            verdict, agree = "BROKEN BUILD", "⚠ NOT A CONTROL"
        else:
            agree = "as predicted" if fails == c["predict"] else "⚠ PREDICTION WRONG"
            verdict = "CAUGHT" if fails else "NOT CAUGHT"
            agreed += fails == c["predict"]

        print(f"{verdict:<12} {c['name']}   ({agree})")
        print(f"             applied={applied}  reddened={sorted(fails) or '{}'}")
        print(f"             predicted={sorted(c['predict'])}")
        if c.get("separates"):
            green, red = c["separates"]
            held = green not in fails and red in fails
            print(f"             SEPARATION (the claim this control exists for): {green} GREEN and "
                  f"{red} RED  ->  {held}")
        print(f"             {c['why']}")
        for t in sorted(fails):
            m = msg_for(out, t)
            if m:
                print(f"             {t}: {m[:170]}")
        print()

    now = hashlib.sha256(open(STORE, "rb").read()).hexdigest()
    print(f"restored store.go {now[:12]} == pristine {sha0[:12]}: {now == sha0}")
    print(f"{agreed}/{len(CONTROLS)} controls reddened EXACTLY the predicted set")
    return 0 if agreed == len(CONTROLS) else 1


if __name__ == "__main__":
    sys.exit(main())
