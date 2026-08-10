#!/usr/bin/env python3
"""Positive controls for internal/sharing/viewcount_realpg_test.go.

THOSE TWO TESTS HAVE NO RED-FIRST MOMENT — the behaviour they pin is already correct — so this
file is their ENTIRE justification. Every control below names its catcher BEFORE the run, and
the harness FAILS if any control produces a different failing set than predicted. A wrong
prediction is kept wrong in the record rather than retargeted.

WHAT EACH CONTROL HAS TO SHOW, one catcher per failure mode:

  EXACT   TestValidate_BumpsTheViewedLinkByExactlyOne_RealPG
  ONLYONE TestValidate_TouchesNoLinkButTheOneViewed_RealPG
  others  everything else in the package — the pgxmock suite, which is the MUST-STAY-GREEN
          COMPANION: it is blind BY CONSTRUCTION to what Postgres did with the statement, so a
          mutation that reds a guard while `others` stays green has broken the BEHAVIOUR and
          not the package.

  S1 S2 S4 S6   only EXACT reds      (value wrong on the viewed link)
  S3            only ONLYONE reds    (blast radius; EXACT is blind — its database holds one link)
  S8 S9         only ONLYONE reds, AND THROUGH ONE NAMED ASSERTION EACH
  S5            EXACT + others red   (the statement deleted: #65's ExpectationsWereMet speaks;
                                      recorded as justifying NEITHER new guard)
  S7            MUST-NOT-CATCH       (an unrelated edit in the same file: both guards stay
                                      green while the package's own mock reds)

⚠ THE VERDICT IS THE SET OF ASSERTIONS THAT FIRED, NOT PASS/FAIL. The first version of this
file predicted PASS/FAIL per test and scored 7/7 — and that 7/7 was worth less than it looked.
S3 (` OR TRUE`) fires BOTH of ONLYONE's assertions at once, so under a PASS/FAIL verdict the
same-page assertion and the cross-workspace assertion were justified by nothing individually:
either one could have been deleted, or written constant-true, and every control would still
have scored AS PREDICTED. S8 and S9 are the mutations that separate them — each reaches exactly
one bystander — and the four assertion tags below are what make "which assertion spoke" a
checkable claim rather than a story told about a red test.

Run: DOCS_TEST_DATABASE_URL=... python3 scripts/w31-sharelink-viewcount-controls.py
"""
import hashlib
import json
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = os.path.join(REPO, "internal/sharing/store.go")

EXACT = "TestValidate_BumpsTheViewedLinkByExactlyOne_RealPG"
ONLYONE = "TestValidate_TouchesNoLinkButTheOneViewed_RealPG"
GUARDS = [EXACT, ONLYONE]

# ANCHOR CENSUS, ASSERTED BEFORE EVERY WRITE. The whole statement appears EXACTLY ONCE in the
# repo's .go files. internal/sharing/store_test.go carries the shorter prefix
# `UPDATE share_links SET view_count` as its pgxmock matcher — that is the mock's regex, not a
# second product site, and every mutation here deliberately preserves that prefix so the mock
# keeps matching. (An ad-hoc anchor that matched two sites is how #66's harness nearly reported
# a green from a tree it had never mutated.)
BUMP = "`UPDATE share_links SET view_count = view_count + 1 WHERE id = $1`"

REVOKE = "`DELETE FROM share_links WHERE id = $1 AND page_id = $2`, id, pageID)"

EXEC_BLOCK = """	if _, err := s.pool.Exec(ctx,
		`UPDATE share_links SET view_count = view_count + 1 WHERE id = $1`,
		link.ID,
	); err != nil {
		// View-count bump failure shouldn't fail the read — log and
		// continue once a logger is wired. Phase 8 keeps it simple.
		_ = err
	}
"""


# The individual assertions inside each guard, keyed by a short tag and matched on a fragment
# of the message the assertion prints. A control's prediction is the SET of tags it must make
# fire — so "the guard reddened" and "the assertion I claim exists reddened" stop being the
# same claim. If an assertion's wording changes, the fragment stops matching and every control
# that names it FAILS LOUD rather than quietly passing on a tag nothing can set.
ASSERTIONS = {
    EXACT: [
        ("ONE", "after ONE public view"),
        ("TWO", "after TWO public views"),
    ],
    ONLYONE: [
        ("SIBLING", "bumped a SIBLING link"),
        ("FOREIGN", "bumped ANOTHER WORKSPACE's link"),
    ],
}


def read():
    with open(STORE, "r") as f:
        return f.read()


def write(text):
    with open(STORE, "w") as f:
        f.write(text)


def assert_anchor(text, needle, want):
    got = text.count(needle)
    if got != want:
        sys.exit(f"FATAL ANCHOR: {needle!r} appears {got}x, expected {want}")


def sub(old, new, expect=1):
    def apply():
        text = read()
        assert_anchor(text, old, expect)
        write(text.replace(old, new))
    return apply


def drop_exec():
    """Delete the whole discarded-error Exec.

    One read-modify-write, one anchor, and the block leaves no variable behind when it goes —
    `if _, err := ...; err != nil` declares nothing that outlives it, so unlike the changelog
    harness's D5 this cannot fail to compile. That is checked anyway: a build error scoring as
    a catch is the failure mode BUILD_FAILED exists for.
    """
    def apply():
        text = read()
        assert_anchor(text, EXEC_BLOCK, 1)
        write(text.replace(EXEC_BLOCK, ""))
    return apply


def run_package():
    """Run the whole sharing package once, reporting PER TEST with the ASSERTION MESSAGE.

    The message is the point. A verdict read off a list of test names cannot tell a real catch
    from a crash, nor tell WHICH assertion spoke — and both guards here carry a t.Fatalf
    precondition ahead of the assertion they exist for, so "it failed" and "it failed for the
    predicted reason" are genuinely different claims.
    """
    p = subprocess.run(
        ["go", "test", "-count=1", "-json", "./internal/sharing/"],
        cwd=REPO, env=dict(os.environ), capture_output=True, text=True, timeout=900,
    )
    out = p.stdout + p.stderr
    if "[build failed]" in out or re.search(r"^# ", out, re.M):
        first = next((ln for ln in out.split("\n") if ln.startswith("#") or ".go:" in ln), out[:200])
        return ({g: ("BUILD_FAILED", first.strip()) for g in GUARDS},
                ("BUILD_FAILED", first.strip()))

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

    def blob(name):
        """EVERY assertion line for the test, not just the first.

        Taking only the first would collapse "both bystander assertions fired" into "the
        sibling one fired", which is exactly the distinction S8 and S9 exist to make.
        """
        return " ".join(" ".join(ln.split()) for ln in output.get(name, [])
                        if ".go:" in ln and "RUN" not in ln)

    states = {}
    for g in GUARDS:
        if g not in actions:
            # A guard that did not run is not a guard that passed — a skip and a pass are the
            # same colour in a summary line, and this repo's real-PG tests fail rather than
            # skip for exactly that reason.
            states[g] = ("BUILD_FAILED", "the guard did not run", set())
        elif actions[g] == "pass":
            states[g] = ("PASS", "", set())
        else:
            text = blob(g)
            fired = {tag for tag, frag in ASSERTIONS[g] if frag in text}
            states[g] = ("FAIL", text[:300], fired)

    failed = [n for n, a in actions.items() if a == "fail" and n not in GUARDS]
    # newMockStore's ExpectationsWereMet runs in a t.Cleanup, so its failure is reported
    # against the test that registered it and lands in `failed` like any other.
    others = ("FAIL", f"{failed[0]}: {blob(failed[0])[:200]}") if failed else ("PASS", "")
    return states, others


# (id, description, mutation, {guard: set of assertion tags that MUST fire}, others)
CONTROLS = [
    ("S1", "the bump stops bumping: `+ 1` -> `+ 0`. The statement runs and the row does not "
           "move. Mock regex and bound argument untouched.",
     sub(BUMP, "`UPDATE share_links SET view_count = view_count + 0 WHERE id = $1`"),
     {EXACT: {"ONE", "TWO"}, ONLYONE: set()}, "PASS"),

    ("S2", "` AND FALSE`: the statement runs and matches NO row. There is no RowsAffected "
           "branch here and the error is discarded on purpose, so a row read is the only thing "
           "that can speak — unlike changelog.DeleteEntry, where D4 red through the branch.",
     sub(BUMP, "`UPDATE share_links SET view_count = view_count + 1 WHERE id = $1 AND FALSE`"),
     {EXACT: {"ONE", "TWO"}, ONLYONE: set()}, "PASS"),

    ("S3", "BLAST RADIUS — ` OR TRUE`: one public view bumps EVERY share link in the table, "
           "across pages and across workspaces. ONLY ONLYONE CAN SEE IT: EXACT's database holds "
           "exactly one link, so its target still reads 1 then 2. Fires BOTH bystander "
           "assertions, which is why it alone justifies neither of them — see S8/S9.",
     sub(BUMP, "`UPDATE share_links SET view_count = view_count + 1 WHERE id = $1 OR TRUE`"),
     {EXACT: set(), ONLYONE: {"SIBLING", "FOREIGN"}}, "PASS"),

    ("S8", "THE SAME-PAGE RADIUS ALONE: scope the bump to the viewed link's PAGE. $1 is still "
           "bound (the subquery uses it) and the matched prefix is unchanged, so pgxmock is "
           "blind. The sibling moves, the other workspace does not — so ONLY the SIBLING "
           "assertion may fire, and it is the mutation that earns it.",
     sub(BUMP, "`UPDATE share_links SET view_count = view_count + 1 "
               "WHERE page_id = (SELECT page_id FROM share_links WHERE id = $1)`"),
     {EXACT: set(), ONLYONE: {"SIBLING"}}, "PASS"),

    ("S9", "THE CROSS-WORKSPACE RADIUS ALONE, and the inverse of S8: the viewed link plus every "
           "link in EVERY OTHER workspace. The same-page sibling is deliberately spared, so "
           "ONLY the FOREIGN assertion may fire — an unauthenticated public read writing to "
           "another tenant's row, with nothing on the caller's own page to notice.",
     sub(BUMP, "`UPDATE share_links SET view_count = view_count + 1 WHERE id = $1 "
               "OR workspace_id <> (SELECT workspace_id FROM share_links WHERE id = $1)`"),
     {EXACT: set(), ONLYONE: {"FOREIGN"}}, "PASS"),

    ("S4", "over-count: `+ 1` -> `+ 2`. A guard asserting `> 0` would be blind; only an exact "
           "value sees it.",
     sub(BUMP, "`UPDATE share_links SET view_count = view_count + 2 WHERE id = $1`"),
     {EXACT: {"ONE", "TWO"}, ONLYONE: set()}, "PASS"),

    ("S6", "ASSIGNMENT WEARING AN INCREMENT'S CLOTHES: `= view_count + 1` -> `= 1`. Every link "
           "in the product pegs at 1 view forever. THE SOLE REASON EXACT VIEWS THE LINK TWICE — "
           "the FIRST read is satisfied by this mutation, so only the TWO assertion may fire, "
           "and this is the control that proves the second view is not decoration.",
     sub(BUMP, "`UPDATE share_links SET view_count = 1 WHERE id = $1`"),
     {EXACT: {"TWO"}, ONLYONE: set()}, "PASS"),

    ("S5", "the whole Exec deleted. CAUGHT TWICE — by EXACT and by #65's ExpectationsWereMet — "
           "and therefore RECORDED AS JUSTIFYING NEITHER NEW GUARD. It is here to prove this "
           "harness reaches the file at all and that the mock still speaks.",
     drop_exec(),
     {EXACT: {"ONE", "TWO"}, ONLYONE: set()}, "FAIL"),

    ("S7", "MUST-NOT-CATCH: an unrelated edit elsewhere in store.go — Revoke's DELETE reordered "
           "so the mock's full-statement regex stops matching. Both guards must stay GREEN "
           "while the package's own mock reds, which is what makes 'green' here mean 'this "
           "mutation was applied and my guards were right to ignore it'.",
     sub(REVOKE, "`DELETE FROM share_links WHERE page_id = $2 AND id = $1`, id, pageID)"),
     {EXACT: set(), ONLYONE: set()}, "FAIL"),
]


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        sys.exit("FATAL: DOCS_TEST_DATABASE_URL unset — these guards are real-Postgres tests")

    pristine = open(STORE, "rb").read()
    pristine_sha = hashlib.sha256(pristine).hexdigest()
    print(f"pristine store.go sha256 = {pristine_sha}\n")

    states, others = run_package()
    print(f"BASELINE  EXACT={states[EXACT][0]}  ONLYONE={states[ONLYONE][0]}  others={others[0]}")
    if any(states[g][0] != "PASS" for g in GUARDS) or others[0] != "PASS":
        sys.exit(f"FATAL: baseline is not green — every verdict below would be meaningless "
                 f"({states} others={others})")

    # Every assertion tag must be claimed by at least one control, or the tag is a fragment
    # nothing can set and its assertion is justified by nothing. This is the check that the
    # PASS/FAIL version of this harness could not express.
    claimed = {t for _, _, _, want, _ in CONTROLS for g in GUARDS for t in want[g]}
    declared = {t for g in GUARDS for t, _ in ASSERTIONS[g]}
    if declared - claimed:
        sys.exit(f"FATAL: assertion(s) {sorted(declared - claimed)} are justified by no control")
    print()

    results = []
    for cid, desc, apply, want, want_others in CONTROLS:
        # try/finally, because THIS HARNESS IS AN INSTRUMENT TOO AND ITS FIRST RUN PROVED IT:
        # a NameError inside run_package (a helper renamed and one call site missed) raised
        # BETWEEN the mutation and the restore, and left ` OR TRUE`-class SQL sitting in
        # store.go. The final sha256 check cannot help — the process is already gone. The
        # restore has to be unconditional.
        try:
            apply()
            if open(STORE, "rb").read() == pristine:
                sys.exit(f"FATAL {cid}: the mutation changed NO BYTES ON DISK — an anchor that "
                         f"matched is not a mutation that meant anything")
            states, others = run_package()
        finally:
            open(STORE, "wb").write(pristine)

        built = all(states[g][0] != "BUILD_FAILED" for g in GUARDS) and others[0] != "BUILD_FAILED"
        if not built:
            verdict = "BUILD ERROR — NOT A CATCH"
        else:
            wrong = []
            for g in GUARDS:
                fired = states[g][2]
                if fired != want[g]:
                    wrong.append(f"{g.split('_')[1]} fired {sorted(fired) or 'nothing'} "
                                 f"(predicted {sorted(want[g]) or 'nothing'})")
            if others[0] != want_others:
                wrong.append(f"others={others[0]} (predicted {want_others})")
            verdict = "AS PREDICTED" if not wrong else "PREDICTION WRONG: " + "; ".join(wrong)

        results.append((cid, verdict, {g: sorted(states[g][2]) for g in GUARDS}, others[0]))
        print(f"{cid}  {desc}")
        for g in GUARDS:
            st, m, fired = states[g]
            print(f"    {g[len('TestValidate_'):]:<40s} {st:6s} fired={sorted(fired)}")
            if m:
                print(f"      {m}")
        print(f"    {'others (the pgxmock companion)':<40s} {others[0]:6s} {others[1]}")
        print(f"    VERDICT: {verdict}\n")

    final = hashlib.sha256(open(STORE, "rb").read()).hexdigest()
    print(f"restored store.go sha256 = {final}")
    if final != pristine_sha:
        sys.exit("FATAL: tree NOT restored to pristine bytes")

    print("\nSUMMARY (the set of ASSERTIONS each mutation made fire)")
    for cid, verdict, gs, o in results:
        compact = "  ".join(f"{g[len('TestValidate_'):][:22]}={v or '-'}" for g, v in gs.items())
        print(f"  {cid}: {compact}  others={o}  {verdict}")
    wrong = [r for r in results if not r[1].startswith("AS PREDICTED")]
    if wrong:
        print(f"\n{len(wrong)} PREDICTION(S) WRONG — keep them wrong in the record, do not retarget.")
        sys.exit(1)
    print("\nALL PREDICTIONS HELD.")


if __name__ == "__main__":
    main()
