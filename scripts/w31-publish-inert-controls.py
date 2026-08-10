#!/usr/bin/env python3
"""W3.1 finding (11) — positive controls for the PublishApproved inert-write guard.

WHAT IS BEING PROVEN. `TestPublishApproved_LeavesTheRowByteIdentical_RealPG` PASSES ON THE
UNMODIFIED TREE by construction: it pins a measured no-op, so it has no red-first moment and
nothing about it passing says it works. These controls are its entire justification.

THE CLAIM UNDER TEST IS TWO-SIDED, WHICH IS WHY Q1/Q2 ARE HERE AS "MUST NOT CATCH":

  · Q1/Q2 are the mutations W3.1 finding (11) PRESCRIBED (` AND FALSE`; statement deleted).
    The guard MUST NOT catch them — not because it is weak, but because an inert write has
    nothing to stop doing. A row assertion is structurally blind to this class here. If the
    guard ever DID red under Q1, the premise (the write is inert) would be false and the
    finding would need rewriting. So Q1/Q2 are falsifiers of the FINDING, not of the guard.

  · Q3/Q4 are the mutations only this guard can see: the statement text and the bound
    arguments are untouched (so pgxmock's regex still matches and WithArgs still holds), but
    the ROW moves. Nothing in the repo except a real-Postgres row read can tell.

  · Q5 changes the written VALUE. It is CAUGHT TWICE — the mock's WithArgs("approved",...)
    mismatches and PublishApproved's caller CHECKS that Exec error — so it is recorded as
    JUSTIFYING NEITHER catcher. It is here to be counted honestly, not as evidence.

Every control asserts its anchor count BEFORE writing, verifies the bytes actually moved on
disk, and distinguishes a BUILD FAILURE from a caught mutation so a compile error cannot score
as a catch. The tree is restored from saved bytes and sha256-compared at the end.

⚠ THE ANCHOR APPEARS TWICE IN store.go — PublishApproved (the subject) and SetStatus (a real
write that must not be touched). Every mutation is applied by LINE NUMBER inside
PublishApproved's body and the count is asserted at 2, not 1.

Usage:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-publish-inert-controls.py
"""

import hashlib
import json
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = os.path.join(REPO, "internal", "approval", "store.go")

GUARD = "TestPublishApproved_LeavesTheRowByteIdentical_RealPG"


UPDATE_STMT = "`UPDATE pages SET doc_status = $1 WHERE id = $2 AND workspace_id = ANY($3)`,"
SELECT_STMT = "`SELECT doc_status FROM pages WHERE id = $1 AND workspace_id = ANY($2)`, pageID, wsIDs,"


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def read():
    with open(STORE, encoding="utf-8") as f:
        return f.read()


def write(text):
    with open(STORE, "w", encoding="utf-8") as f:
        f.write(text)


def publish_body_bounds(text):
    """Line index range [start, end) of PublishApproved's body. Everything is applied inside
    it, so SetStatus's identical statement is never the thing that moved."""
    lines = text.split("\n")
    start = None
    for i, ln in enumerate(lines):
        if ln.startswith("func (s *Store) PublishApproved("):
            start = i
            break
    if start is None:
        sys.exit("FATAL: PublishApproved not found — the harness is pointed at the wrong file")
    for j in range(start + 1, len(lines)):
        if lines[j] == "}":
            return lines, start, j + 1
    sys.exit("FATAL: PublishApproved has no closing brace")


def assert_anchor(text, needle, want):
    got = text.count(needle)
    if got != want:
        sys.exit(f"FATAL ANCHOR: {needle!r} appears {got}x, expected {want}x — refusing to write")


def mutate_in_publish(old, new, *, expect_total):
    """Replace `old` with `new` on the single line inside PublishApproved that carries it."""
    text = read()
    assert_anchor(text, old, expect_total)
    lines, lo, hi = publish_body_bounds(text)
    hits = [i for i in range(lo, hi) if old in lines[i]]
    if len(hits) != 1:
        sys.exit(f"FATAL ANCHOR: {old!r} appears {len(hits)}x INSIDE PublishApproved, expected 1")
    lines[hits[0]] = lines[hits[0]].replace(old, new)
    write("\n".join(lines))


def delete_update_stmt():
    """Remove the whole `if _, err := s.pool.Exec(...UPDATE...); err != nil { ... }` block."""
    text = read()
    assert_anchor(text, UPDATE_STMT, 2)
    lines, lo, hi = publish_body_bounds(text)
    stmt = [i for i in range(lo, hi) if UPDATE_STMT in lines[i]]
    if len(stmt) != 1:
        sys.exit(f"FATAL ANCHOR: UPDATE stmt appears {len(stmt)}x in PublishApproved, expected 1")
    i = stmt[0]
    start = next(k for k in range(i, lo - 1, -1) if lines[k].lstrip().startswith("if _, err := s.pool.Exec("))
    end = next(k for k in range(i, hi) if lines[k].strip() == "}")
    del lines[start:end + 1]
    write("\n".join(lines))


def run_package():
    """Run the whole approval package once and report PER TEST.

    Go's -run is RE2 — no negative lookahead — so "every test except the guard" cannot be
    expressed as a pattern. Parsing -json instead also gives the ASSERTION MESSAGE, which is
    what tells a real catch apart from a crash or an unrelated failure: a verdict read off a
    list of test names cannot make that distinction.

    Returns (guard_state, guard_msg, others_state, others_msg). A BUILD_FAILED anywhere is
    never a catch.
    """
    p = subprocess.run(
        ["go", "test", "-count=1", "-json", "./internal/approval/"],
        cwd=REPO, env=dict(os.environ), capture_output=True, text=True, timeout=900,
    )
    out = p.stdout + p.stderr
    if "[build failed]" in out or re.search(r"^# ", out, re.M):
        first = next((ln for ln in out.split("\n") if ln.startswith("#") or ".go:" in ln), out[:200])
        return "BUILD_FAILED", first.strip(), "BUILD_FAILED", first.strip()

    actions, output = {}, {}
    for line in p.stdout.split("\n"):
        if not line.startswith("{"):
            continue
        try:
            ev = json.loads(line)
        except json.JSONDecodeError:
            continue
        name = ev.get("Test")
        if not name:
            continue
        if ev.get("Action") in ("pass", "fail", "skip"):
            actions[name] = ev["Action"]
        elif ev.get("Action") == "output":
            output.setdefault(name, []).append(ev.get("Output", ""))

    if not actions:
        return "BUILD_FAILED", "no test events — the package did not run", "BUILD_FAILED", ""

    def msg(name):
        for ln in output.get(name, []):
            if ".go:" in ln:
                return ln.strip()[:280]
        return ""

    gstate = "PASS" if actions.get(GUARD) == "pass" else "FAIL"
    if GUARD not in actions:
        gstate = "BUILD_FAILED"
    gmsg = msg(GUARD) if gstate == "FAIL" else ""

    failed = [n for n, a in actions.items() if a == "fail" and n != GUARD]
    ostate = "FAIL" if failed else "PASS"
    omsg = f"{failed[0]}: {msg(failed[0])}" if failed else ""
    return gstate, gmsg, ostate, omsg


CONTROLS = [
    # (id, description, apply, guard_must)
    ("Q1", "` AND FALSE` appended to the UPDATE's WHERE (finding (11)'s prescribed P-class "
           "mutation: the statement runs and matches no row)",
     lambda: mutate_in_publish(
         "WHERE id = $2 AND workspace_id = ANY($3)`,",
         "WHERE id = $2 AND workspace_id = ANY($3) AND FALSE`,",
         expect_total=2),
     "PASS"),
    ("Q2", "the UPDATE statement deleted outright",
     delete_update_stmt,
     "PASS"),
    ("Q3", "the UPDATE also sets updated_at = NOW() — statement still matches the mock's "
           "regex, args unchanged, but the ROW moves",
     lambda: mutate_in_publish(
         "`UPDATE pages SET doc_status = $1 WHERE",
         "`UPDATE pages SET doc_status = $1, updated_at = NOW() WHERE",
         expect_total=4),
     "FAIL"),
    ("Q4", "the UPDATE also bumps view_count — same class as Q3 with no clock dependency",
     lambda: mutate_in_publish(
         "`UPDATE pages SET doc_status = $1 WHERE",
         "`UPDATE pages SET doc_status = $1, view_count = view_count + 1 WHERE",
         expect_total=4),
     "FAIL"),
    ("Q5", "the UPDATE writes DocDraft instead of DocApproved — CAUGHT TWICE, justifies neither",
     lambda: mutate_in_publish("string(DocApproved), pageID, wsIDs,",
                               "string(DocDraft), pageID, wsIDs,",
                               expect_total=1),
     "FAIL"),
    ("Q6", "the SELECT's status guard removed (publish accepts a draft) — the refusal half",
     lambda: mutate_in_publish('if status != string(DocApproved) {',
                               'if false {',
                               expect_total=1),
     "FAIL"),
]


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        sys.exit("FATAL: DOCS_TEST_DATABASE_URL unset — the guard is a real-Postgres test")

    pristine_bytes = open(STORE, "rb").read()
    pristine_sha = hashlib.sha256(pristine_bytes).hexdigest()
    print(f"pristine store.go sha256 = {pristine_sha}\n")

    # MUST-STAY-GREEN COMPANION, RUN FIRST. If the tree is not green before any mutation, every
    # verdict below is meaningless.
    g, d, o, od = run_package()
    print(f"BASELINE  guard={g} others={o}")
    if g != "PASS" or o != "PASS":
        sys.exit(f"FATAL: baseline is not green (guard={g} {d} | others={o} {od})")
    print()

    results = []
    for cid, desc, apply, guard_must in CONTROLS:
        apply()
        moved = open(STORE, "rb").read() != pristine_bytes
        if not moved:
            open(STORE, "wb").write(pristine_bytes)
            sys.exit(f"FATAL {cid}: the mutation changed NO BYTES ON DISK — an anchor that "
                     f"applied is not a mutation that meant anything")
        gstate, gdetail, ostate, odetail = run_package()
        open(STORE, "wb").write(pristine_bytes)

        if gstate == "BUILD_FAILED" or ostate == "BUILD_FAILED":
            verdict = "BUILD ERROR — NOT A CATCH"
        elif gstate == guard_must:
            verdict = "AS PREDICTED"
        else:
            verdict = f"PREDICTION WRONG (guard={gstate}, predicted {guard_must})"

        results.append((cid, verdict, gstate, ostate))
        print(f"{cid}  {desc}")
        print(f"    guard  : {gstate:12s} {gdetail}")
        print(f"    others : {ostate:12s} {odetail}")
        print(f"    VERDICT: {verdict}\n")

    final = hashlib.sha256(open(STORE, "rb").read()).hexdigest()
    print(f"restored store.go sha256 = {final}")
    if final != pristine_sha:
        sys.exit("FATAL: tree NOT restored to pristine bytes")

    wrong = [r for r in results if not r[1].startswith("AS PREDICTED")]
    print("\nSUMMARY")
    for cid, verdict, gs, os_ in results:
        print(f"  {cid}: guard={gs:5s} others={os_:5s}  {verdict}")
    if wrong:
        print(f"\n{len(wrong)} PREDICTION(S) WRONG — keep them wrong in the record, do not retarget.")
        sys.exit(1)
    print("\nALL PREDICTIONS HELD.")


if __name__ == "__main__":
    main()
