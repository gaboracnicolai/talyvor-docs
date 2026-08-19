#!/usr/bin/env python3
"""Positive controls for the reply-to-a-reply promotion fix (ListByPage buckets by thread_id).

EVERY CONTROL NAMES ITS PREDICTED CATCHER BEFORE THE RUN, and the run either confirms the
prediction or FALSIFIES it — a falsified prediction is kept in the output, not tidied away.

Each control mutates the tree, runs the FULL Go suite for the affected packages against a real
Postgres, and restores the tree in a `finally`. Every restore is verified by sha256 against the
digest taken before the mutation, so a control that fails to restore cannot leave the next one
measuring a different tree.

Run:
  DOCS_TEST_DATABASE_URL=postgres://... python3 scripts/w31-replydepth-controls-6d4b.py
"""

import hashlib
import os
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = os.path.join(REPO, "internal/comment/store.go")
TEST = os.path.join(REPO, "internal/comment/replydepth_realpg_test.go")

DSN = os.environ.get("DOCS_TEST_DATABASE_URL", "")
if not DSN:
    sys.exit("DOCS_TEST_DATABASE_URL must be set — these controls measure against real Postgres")

# The whole Go suite. A control that only runs one package cannot tell "my new test caught it"
# from "nothing else in the repository would have".
PKGS = ["./..."]


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def read(path):
    with open(path) as f:
        return f.read()


def write(path, text):
    with open(path, "w") as f:
        f.write(text)


def run_suite():
    """Returns (ok, failing_test_names, raw_tail)."""
    env = dict(os.environ)
    p = subprocess.run(
        ["go", "test", "-timeout", "600s", "-count=1"] + PKGS,
        cwd=REPO, env=env, capture_output=True, text=True,
    )
    failed = []
    for line in p.stdout.splitlines():
        s = line.strip()
        if s.startswith("--- FAIL:"):
            failed.append(s.split()[2])
    return p.returncode == 0, sorted(set(failed)), (p.stdout + p.stderr)[-1500:]


# ─── the mutations ──────────────────────────────────────────────────────────────────────────

# C1: THE DEFECT ITSELF. Pass 2 looks a reply up by its parent pointer only — the shape that was
# on main. A depth-2 reply's parent is a reply, never a key in `threads`, so the row falls to the
# fail-safe and is promoted to a top-level thread.
C1_FROM = """		if c.ThreadID != nil {
			if head, ok := threads[*c.ThreadID]; ok {
				head.Replies = append(head.Replies, *c)
				continue
			}
		}
		if head, ok := threads[*c.ParentID]; ok {"""
C1_TO = """		if head, ok := threads[*c.ParentID]; ok && head.ParentID == nil && head.ID == *c.ParentID {"""

# C2: EVERY REPLY UNDER THE FIRST HEAD. A "fix" that files replies by page rather than by thread.
# It satisfies "the depth-2 reply is nested" while destroying the separation between conversations.
C2_FROM = """		if c.ThreadID != nil {
			if head, ok := threads[*c.ThreadID]; ok {
				head.Replies = append(head.Replies, *c)
				continue
			}
		}"""
C2_TO = """		if len(heads) > 0 {
			heads[0].Replies = append(heads[0].Replies, *c)
			continue
		}"""

# C3: REPLIES SILENTLY DROPPED. The bucketing runs and appends nothing.
C3_FROM = """			if head, ok := threads[*c.ThreadID]; ok {
				head.Replies = append(head.Replies, *c)
				continue
			}"""
C3_TO = """			if _, ok := threads[*c.ThreadID]; ok {
				continue
			}"""

# C4: THE RESOLVED PREDICATE DELETED. Nothing to do with this change — the must-stay-green
# companion that proves the pre-existing include_resolved contract is still being pinned by the
# test that was written for it and NOT by the new one.
C4_FROM = """	if !includeResolved {
		q += ` AND resolved = false`
	}"""
C4_TO = """	if !includeResolved {
		q += ` AND true`
	}"""

# C6: THE FIXTURE CONTROL. With the defect restored, the fixture's depth-2 reply is re-pointed at
# the HEAD (an ordinary depth-1 reply). If the test still reds, the depth-2 shape was not what it
# was measuring.
C6_TEST_FROM = 'r2 := post("DEPTH-2-ACCEPTED", base+"/"+r1.ID+"/reply"'
C6_TEST_TO = 'r2 := post("DEPTH-2-ACCEPTED", base+"/"+h1.ID+"/reply"'

CONTROLS = [
    dict(
        name="C1  THE DEFECT RESTORED (bucket by parent_id, as main did)",
        predict="CAUGHT by TestListByPage_ReplyToAReplyStaysInItsThread_RealPG [TWO-THREADS]. "
                "Every OTHER comment test must stay green — the promotion was invisible to all of them.",
        edits=[(STORE, C1_FROM, C1_TO)],
    ),
    dict(
        name="C2  EVERY REPLY UNDER THE FIRST HEAD (nesting without threads)",
        predict="CAUGHT by [TWO-THREADS-STAY-TWO] — the must-stay-green companion earning its "
                "place. [NESTED-DEPTH-2] alone would be satisfied by this.\n"
                "    ⚠ FALSIFIED, AND KEPT: the catcher is [NESTED], not the companion. `heads` is "
                "ordered by thread_id, so heads[0] is not reliably the thread the fixture builds "
                "first, and the head that ends up empty reds one assertion earlier. What this "
                "measures is therefore that the one-head collapse IS caught — not that the "
                "companion is what catches it. See the note in the test file: the companion's "
                "load-bearing half is that the second head survives AS a head, which [TWO-THREADS] "
                "and the byID lookup hold; its reply-set equality is redundant under every "
                "mutation that could be constructed here, and is kept as documentation of intent "
                "rather than claimed as a guard.",
        edits=[(STORE, C2_FROM, C2_TO)],
    ),
    dict(
        name="C3  REPLIES SILENTLY DROPPED",
        predict="CAUGHT by [NESTED]. Predicting at least one PRE-EXISTING test also reds — "
                "nesting is older than this change.",
        edits=[(STORE, C3_FROM, C3_TO)],
    ),
    dict(
        name="C4  THE include_resolved PREDICATE NEUTERED (unrelated to this change)",
        predict="CAUGHT by the PRE-EXISTING TestListByPage_IncludeResolvedIsInclusiveNotExclusive_RealPG "
                "[EXCLUDES-RESOLVED]. The new test must NOT be what catches it — if it is, this change "
                "quietly took over a contract it does not own.",
        edits=[(STORE, C4_FROM, C4_TO)],
    ),
    dict(
        name="C5  BLIND: the defect restored AND the new test file deleted",
        predict="NOT CAUGHT — the whole Go suite green. This is the state main was in.",
        edits=[(STORE, C1_FROM, C1_TO)],
        delete=[TEST],
    ),
    dict(
        name="C6  FIXTURE: the defect restored AND the depth-2 reply flattened to depth-1",
        predict="NOT CAUGHT — the depth-2 shape is load-bearing in the fixture, not decoration.\n"
                "    ⚠ FALSIFIED, AND KEPT: CAUGHT by [PRECONDITION], which reads parent_id "
                "straight from Postgres and refuses a fixture whose depth-2 row no longer hangs "
                "off a reply. That is a BETTER outcome than the predicted silence — a fixture that "
                "loses the shape reds instead of passing for the wrong reason — but it means this "
                "control cannot answer the question it was built for. C1 answers it instead: with "
                "the fixture intact and the defect restored, this test is the ONLY thing in the "
                "repository that reds.",
        edits=[(STORE, C1_FROM, C1_TO), (TEST, C6_TEST_FROM, C6_TEST_TO)],
    ),
]


def main():
    print("=" * 100)
    print("C0  BASELINE — the tree as it will be merged")
    ok, failed, tail = run_suite()
    print(f"    result: {'GREEN' if ok else 'RED'}  failing={failed}")
    if not ok:
        print(tail)
        sys.exit("C0 must be green before any control means anything")

    results = []
    for c in CONTROLS:
        print("=" * 100)
        print(c["name"])
        print(f"    PREDICTION: {c['predict']}")
        touched = sorted({p for p, _, _ in c["edits"]} | set(c.get("delete", [])))
        before = {p: (read(p), sha(p)) for p in touched}
        try:
            for path, frm, to in c["edits"]:
                src = read(path)
                if src.count(frm) != 1:
                    sys.exit(f"    ABORT: anchor for {os.path.basename(path)} matched "
                             f"{src.count(frm)} times, expected exactly 1 — the mutation is not "
                             f"the one described and the control would measure something else")
                write(path, src.replace(frm, to))
            for path in c.get("delete", []):
                os.remove(path)
            ok, failed, tail = run_suite()
            verdict = "NOT CAUGHT (suite GREEN)" if ok else f"CAUGHT — {failed}"
            print(f"    RESULT: {verdict}")
            if not ok:
                print("    " + tail.strip().replace("\n", "\n    ")[:1200])
            results.append((c["name"], verdict))
        finally:
            for path, (text, digest) in before.items():
                write(path, text)
                got = sha(path)
                if got != digest:
                    sys.exit(f"    RESTORE FAILED for {path}: {got} != {digest}")
            print("    restored, sha256 verified: " +
                  ", ".join(os.path.basename(p) for p in touched))

    print("=" * 100)
    print("SUMMARY")
    for name, verdict in results:
        print(f"  {name}\n      -> {verdict}")


if __name__ == "__main__":
    main()
