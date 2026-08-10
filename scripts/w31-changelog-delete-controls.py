#!/usr/bin/env python3
"""W3.1 finding (11) — positive controls for the changelog DeleteEntry row guards.

WHAT IS BEING PROVEN. The three guards in
internal/changelog/delete_removes_the_row_realpg_test.go PASS ON THE UNMODIFIED TREE by
construction: the product behaviour is already correct and they pin it, so they have NO
red-first moment. Nothing about them passing says they work. THESE CONTROLS ARE THEIR ENTIRE
JUSTIFICATION.

THE POINT OF THE DESIGN IS ONE CATCHER PER FAILURE MODE, PREDICTED BEFORE THE RUN:

  · D1 `AND` -> `OR`. One deletion of one entry destroys EVERY entry in the caller's
    workspaces. THIS IS THE MEASURED ESCAPE: on the unmodified tree with this mutation applied,
    `go test ./...` on real Postgres was GREEN REPO-WIDE (451 pass / 0 fail / 0 skip, 51
    real-PG+SEC-4 tests among them). Caught by TWO of the three new guards and recorded as
    justifying neither on its own — it is here because it is the defect class, not because it
    discriminates.

  · D2 the workspace scope neutralised while the statement STILL CONTAINS `workspace_id = ANY`
    and STILL BINDS BOTH ARGUMENTS, so pgxmock's regex matches and WithArgs holds. Only the
    cross-tenant guard can see it. THE MOCK IS THE MUST-STAY-GREEN COMPANION HERE.

  · D3 a second OR-arm that deletes published entries, WITH THE WORKSPACE SCOPE HELD ON BOTH
    ARMS. Only the blast-radius guard can see it: the target is still deleted, the refusal is
    still correct, and the entry beside it is gone.

  · D4 ` AND FALSE` — the P-class mutation W3.1 finding (11) PRESCRIBED for this target. IT IS
    CAUGHT THROUGH THE `RowsAffected() == 0 => ErrNotFound` BRANCH, NOT THROUGH ANY ROW READ,
    and the printed assertion message is what shows that. The queue predicted this in advance
    ("a ` AND FALSE` there may red through the BRANCH rather than through any row assertion");
    the prediction is recorded here as CONFIRMED, because being caught and being checked are
    different things.

  · D5 the `RowsAffected() == 0 => ErrNotFound` branch deleted. W3.1 finding (10) names this
    exact edit as covered by NO control in the item ("it is one `if` away from being none:
    deleting the branch is a plausible edit and no control in this item covers it"). It is
    covered now, by the cross-tenant guard's error assertion.

  · D6 MUST-NOT-CATCH. An unrelated edit elsewhere in the same store.go. The three new guards
    must ALL stay green while the package's own mock test reds — a guard that reddens on any
    movement in the file it watches is not measuring what its name says.

Every control asserts its anchor count BEFORE writing, verifies the bytes actually moved on
disk, and distinguishes a BUILD FAILURE from a caught mutation so a compile error cannot score
as a catch. The verdict is read from the PRINTED ASSERTION MESSAGE, never from a list of test
names: a crash and a real catch look identical in a list of names.

The tree is restored from saved bytes and sha256-compared at the end.

Usage:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-changelog-delete-controls.py
"""

import hashlib
import json
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = os.path.join(REPO, "internal", "changelog", "store.go")

ROW_GONE = "TestDeleteEntry_ActuallyRemovesTheRow_RealPG"
ONLY_THAT = "TestDeleteEntry_DeletesOnlyThatEntry_RealPG"
CROSS_TENANT = "TestDeleteEntry_RefusesAnotherWorkspacesEntryAndLeavesIt_RealPG"
GUARDS = [ROW_GONE, ONLY_THAT, CROSS_TENANT]

# ⚠ THE SHORT ANCHOR IS NOT UNIQUE AND THAT IS NOT HYPOTHETICAL: `AND workspace_id = ANY($2)`,
# id, wsIDs)` appears TWICE in this file — GetEntry's SELECT at :166 and DeleteEntry's DELETE at
# :237. A first ad-hoc pass used it, the count assertion fired, and the two runs it guarded
# would otherwise have reported a repo-wide green from a tree that had not been mutated at all.
# Every anchor below carries the `DELETE FROM changelog_entries` prefix for that reason.
DELETE_WHERE = "`DELETE FROM changelog_entries WHERE id = $1 AND workspace_id = ANY($2)`, id, wsIDs)"
NOTFOUND_BRANCH = ["\tif tag.RowsAffected() == 0 {", "\t\treturn ErrNotFound", "\t}"]


def read():
    with open(STORE, encoding="utf-8") as f:
        return f.read()


def write(text):
    with open(STORE, "w", encoding="utf-8") as f:
        f.write(text)


def assert_anchor(text, needle, want):
    got = text.count(needle)
    if got != want:
        sys.exit(f"FATAL ANCHOR: {needle!r} appears {got}x, expected {want}x — refusing to write")


def sub(old, new, *, expect=1):
    def apply():
        text = read()
        assert_anchor(text, old, expect)
        write(text.replace(old, new))
    return apply


TAG_DECL = "\ttag, err := s.pool.Exec(ctx,"


def drop_notfound_branch():
    """Delete the `if tag.RowsAffected() == 0 { return ErrNotFound }` block from DeleteEntry.

    ⚠ TWO EDITS, ONE READ-MODIFY-WRITE, BOTH ANCHORS ASSERTED BEFORE THE SINGLE WRITE. Dropping
    the branch alone leaves `tag` declared and not used, and the first run of this harness
    scored D5 `BUILD ERROR — NOT A CATCH` for exactly that: a control that does not compile is
    not a control, it is a compile error wearing a verdict. `tag` must become `_` in the SAME
    pass — two sequential writes would have the second read back the first's output, which is
    the other way this goes wrong.
    """
    text = read()
    assert_anchor(text, TAG_DECL, 1)
    lines = text.split("\n")
    hits = [i for i in range(len(lines) - 2) if lines[i:i + 3] == NOTFOUND_BRANCH]
    if len(hits) != 1:
        sys.exit(f"FATAL ANCHOR: the RowsAffected branch appears {len(hits)}x, expected 1")
    del lines[hits[0]:hits[0] + 3]
    text = "\n".join(lines).replace(TAG_DECL, "\t_, err := s.pool.Exec(ctx,")
    write(text)


def run_package():
    """Run the whole changelog package once and report PER TEST, with the assertion message.

    Go's -run is RE2 — no negative lookahead — so "every test except the guards" cannot be
    expressed as a pattern. Parsing -json instead also yields the ASSERTION MESSAGE, which is
    what tells a real catch apart from a crash: a verdict read off a list of test names cannot
    make that distinction.
    """
    p = subprocess.run(
        ["go", "test", "-count=1", "-json", "./internal/changelog/"],
        cwd=REPO, env=dict(os.environ), capture_output=True, text=True, timeout=900,
    )
    out = p.stdout + p.stderr
    if "[build failed]" in out or re.search(r"^# ", out, re.M):
        first = next((ln for ln in out.split("\n") if ln.startswith("#") or ".go:" in ln), out[:200])
        return {g: ("BUILD_FAILED", first.strip()) for g in GUARDS}, ("BUILD_FAILED", first.strip())

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
        return ({g: ("BUILD_FAILED", "no test events — the package did not run") for g in GUARDS},
                ("BUILD_FAILED", ""))

    def msg(name):
        for ln in output.get(name, []):
            if ".go:" in ln and "RUN" not in ln:
                return " ".join(ln.split())[:300]
        return ""

    states = {}
    for g in GUARDS:
        if g not in actions:
            states[g] = ("BUILD_FAILED", "the guard did not run")
        elif actions[g] == "pass":
            states[g] = ("PASS", "")
        else:
            states[g] = ("FAIL", msg(g))

    failed = [n for n, a in actions.items() if a == "fail" and n not in GUARDS]
    # The package-level cleanup failure (pgxmock ExpectationsWereMet) is reported against the
    # test that registered it, so it lands in `failed` like any other.
    others = ("FAIL", f"{failed[0]}: {msg(failed[0])}") if failed else ("PASS", "")
    return states, others


# predicted state per guard, then for `others` (everything else in the package, incl. the mock)
CONTROLS = [
    ("D1", "`AND` -> `OR` between the id and the workspace scope: deleting ONE entry deletes "
           "EVERY entry in the caller's workspaces. THE MEASURED REPO-WIDE ESCAPE.",
     sub(DELETE_WHERE,
         "`DELETE FROM changelog_entries WHERE id = $1 OR workspace_id = ANY($2)`, id, wsIDs)"),
     {ROW_GONE: "PASS", ONLY_THAT: "FAIL", CROSS_TENANT: "FAIL"}, "PASS"),

    ("D2", "the workspace scope neutralised with `OR TRUE` — statement still contains "
           "`workspace_id = ANY` and still binds both args, so the mock is blind",
     sub(DELETE_WHERE,
         "`DELETE FROM changelog_entries WHERE id = $1 AND (workspace_id = ANY($2) OR TRUE)`, id, wsIDs)"),
     {ROW_GONE: "PASS", ONLY_THAT: "PASS", CROSS_TENANT: "FAIL"}, "PASS"),

    ("D3", "a second OR-arm deleting published entries, WORKSPACE SCOPE HELD ON BOTH ARMS — the "
           "target still goes, the refusal is still right, the entry beside it is gone",
     sub(DELETE_WHERE,
         "`DELETE FROM changelog_entries WHERE id = $1 AND workspace_id = ANY($2) "
         "OR published_at IS NOT NULL AND workspace_id = ANY($2)`, id, wsIDs)"),
     {ROW_GONE: "PASS", ONLY_THAT: "FAIL", CROSS_TENANT: "PASS"}, "PASS"),

    ("D4", "` AND FALSE` appended — finding (11)'s PRESCRIBED P-class mutation. Predicted to red "
           "through the ErrNotFound BRANCH, not through a row read; check the message.",
     sub(DELETE_WHERE,
         "`DELETE FROM changelog_entries WHERE id = $1 AND workspace_id = ANY($2) AND FALSE`, id, wsIDs)"),
     {ROW_GONE: "FAIL", ONLY_THAT: "FAIL", CROSS_TENANT: "PASS"}, "PASS"),

    ("D5", "the `RowsAffected() == 0 => ErrNotFound` branch deleted — the edit finding (10) names "
           "as covered by no control in this item",
     drop_notfound_branch,
     {ROW_GONE: "PASS", ONLY_THAT: "PASS", CROSS_TENANT: "FAIL"}, "PASS"),

    ("D7", "`=` -> `!=` on the id: the statement deletes everything EXCEPT the target. The "
           "inverse of D3, and the class where the two row guards fail for DIFFERENT reasons — "
           "read both messages.",
     sub(DELETE_WHERE,
         "`DELETE FROM changelog_entries WHERE id != $1 AND workspace_id = ANY($2)`, id, wsIDs)"),
     {ROW_GONE: "FAIL", ONLY_THAT: "FAIL", CROSS_TENANT: "PASS"}, "PASS"),

    ("D6", "MUST-NOT-CATCH: an unrelated edit elsewhere in store.go (ListEntries' ORDER BY). All "
           "three guards must stay GREEN while the package's own mock test reds.",
     # ListEntries spells its ORDER BY twice — once per filter branch. Both are mutated and the
     # count is asserted at 2, not 1: the same non-unique-anchor trap the DELETE_WHERE note
     # above records, found the same way.
     sub("ORDER BY published_at DESC NULLS LAST, created_at DESC",
         "ORDER BY created_at ASC", expect=2),
     {ROW_GONE: "PASS", ONLY_THAT: "PASS", CROSS_TENANT: "PASS"}, "FAIL"),
]


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        sys.exit("FATAL: DOCS_TEST_DATABASE_URL unset — these guards are real-Postgres tests")

    pristine_bytes = open(STORE, "rb").read()
    pristine_sha = hashlib.sha256(pristine_bytes).hexdigest()
    print(f"pristine store.go sha256 = {pristine_sha}\n")

    states, others = run_package()
    print("BASELINE  " + "  ".join(f"{g.split('_')[1]}={states[g][0]}" for g in GUARDS)
          + f"  others={others[0]}")
    if any(states[g][0] != "PASS" for g in GUARDS) or others[0] != "PASS":
        sys.exit(f"FATAL: baseline is not green — every verdict below would be meaningless "
                 f"({states} others={others})")
    print()

    results = []
    for cid, desc, apply, want, want_others in CONTROLS:
        apply()
        if open(STORE, "rb").read() == pristine_bytes:
            open(STORE, "wb").write(pristine_bytes)
            sys.exit(f"FATAL {cid}: the mutation changed NO BYTES ON DISK — an anchor that "
                     f"matched is not a mutation that meant anything")
        states, others = run_package()
        open(STORE, "wb").write(pristine_bytes)

        built = all(states[g][0] != "BUILD_FAILED" for g in GUARDS) and others[0] != "BUILD_FAILED"
        if not built:
            verdict = "BUILD ERROR — NOT A CATCH"
        else:
            wrong = [f"{g}={states[g][0]} (predicted {want[g]})" for g in GUARDS
                     if states[g][0] != want[g]]
            if others[0] != want_others:
                wrong.append(f"others={others[0]} (predicted {want_others})")
            verdict = "AS PREDICTED" if not wrong else "PREDICTION WRONG: " + "; ".join(wrong)

        results.append((cid, verdict, {g: states[g][0] for g in GUARDS}, others[0]))
        print(f"{cid}  {desc}")
        for g in GUARDS:
            st, m = states[g]
            print(f"    {g[len('TestDeleteEntry_'):]:<46s} {st:12s} {m}")
        print(f"    {'others (incl. the pgxmock check)':<46s} {others[0]:12s} {others[1]}")
        print(f"    VERDICT: {verdict}\n")

    final = hashlib.sha256(open(STORE, "rb").read()).hexdigest()
    print(f"restored store.go sha256 = {final}")
    if final != pristine_sha:
        sys.exit("FATAL: tree NOT restored to pristine bytes")

    print("\nSUMMARY")
    for cid, verdict, gs, o in results:
        compact = " ".join(f"{g[len('TestDeleteEntry_'):][:12]}={v}" for g, v in gs.items())
        print(f"  {cid}: {compact} others={o}  {verdict}")
    wrong = [r for r in results if not r[1].startswith("AS PREDICTED")]
    if wrong:
        print(f"\n{len(wrong)} PREDICTION(S) WRONG — keep them wrong in the record, do not retarget.")
        sys.exit(1)
    print("\nALL PREDICTIONS HELD.")


if __name__ == "__main__":
    main()
