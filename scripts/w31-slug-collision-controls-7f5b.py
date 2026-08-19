#!/usr/bin/env python3
"""Positive controls for Create's slug disambiguation (W3.1, tab-7f5b).

Each control mutates ONE thing in the product tree, runs the FULL internal/page + internal/templatelib
suites, and reports the SET of [TAG]s that fired. Running the full suites rather than only the new
guards is what makes "nothing else in this repository can see it" a MEASUREMENT rather than a claim.

Every mutation is restored in a `finally` and the restore is verified by sha256 against the
pre-mutation bytes — a control that silently failed to restore would score every later control
against a different tree.

THE CATCHER IS PREDICTED BEFORE THE RUN (`expect` below). A control whose actual catcher differs from
its prediction is a finding about the guard, not a pass.

Run:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-slug-collision-controls-7f5b.py
"""
import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = os.path.join(REPO, "internal/page/store.go")

TAGS = [
    "SECOND-NEW-PAGE",
    "SLUGS-DIFFER",
    "FIRST-SLUG-UNCHANGED",
    "TITLE-UNTOUCHED",
    "SCOPED-TO-SPACE",
    "MANY",
    "TEMPLATE-TWICE",
]

# ─── anchors, verbatim from the product tree ────────────────────────────────────────────────

RETRY_HEAD = """	base := p.Slug
	var out *model.Page
	for attempt := 1; ; attempt++ {
		out, err = scan(s.pool.QueryRow(ctx,"""

RETRY_TAIL = """		if err == nil {
			break
		}
		if !isSlugTaken(err) || attempt >= maxSlugAttempts {
			return nil, fmt.Errorf("page: insert: %w", err)
		}
		// Re-derived from `base` every time, never from the last attempt's value, so the suffix
		// cannot compound into untitled-2-3-4 as the space fills up.
		p.Slug = fmt.Sprintf("%s-%d", base, attempt+1)
	}"""

SUFFIX_LINE = '		p.Slug = fmt.Sprintf("%s-%d", base, attempt+1)'

MAX_ATTEMPTS = "const maxSlugAttempts = 5"


def sha(path):
    return hashlib.sha256(open(path, "rb").read()).hexdigest()


def run_suites():
    """Run BOTH full packages; return the set of tags in failing output, plus raw output."""
    fired, out_all = set(), ""
    for pkg in ("./internal/page/", "./internal/templatelib/"):
        p = subprocess.run(
            ["go", "test", pkg, "-count=1", "-v"],
            cwd=REPO, env=dict(os.environ), capture_output=True, text=True, timeout=1800,
        )
        out = p.stdout + p.stderr
        out_all += out
        # A control that does not COMPILE is not a caught mutation — it is a void control.
        if re.search(r"^\S+\.go:\d+:\d+: ", out, re.M) or "build failed" in out:
            return {"BUILD-FAILED"}, out_all
        fired |= {t for t in TAGS if "[" + t + "]" in out}
    return fired, out_all


MINE = {
        "TestCreate_TwoPagesCanShareATitleInOneSpace_RealPG",
    "TestCreate_SlugSuffixRestartsPerSpace_RealPG",
    "TestCreate_ManySameTitledPagesInOneSpace_RealPG",
    "TestUseTemplate_TwiceIntoTheSameSpace_RealPG",
}


def failed_tests(out):
    """(guards of mine that FAILED, everything else that FAILED).

    ⚠ BOTH HALVES ARE REPORTED BY NAME, AND THE TAG SET ALONE IS NOT ENOUGH. C1 killed
    TestCreate_SlugSuffixRestartsPerSpace_RealPG at a t.Fatalf in its SETUP — the create it needs in
    order to reach [SCOPED-TO-SPACE] is the very thing C1 breaks — so the test was red and its tag
    never printed. Scoring on tags alone reported that guard as silent about a defect it had in fact
    caught. A guard that dies before its assertion is still a guard that fired; an instrument that
    cannot see the difference between that and a green test is the wrong instrument."""
    names = {n.split("/")[0] for n in re.findall(r"^--- FAIL: (\S+)", out, re.M)}
    return sorted(names & MINE), sorted(names - MINE)


# ─── the controls ───────────────────────────────────────────────────────────────────────────
#
# `expect` is the predicted SET of tags. `expect_other` says whether tests OUTSIDE the five new
# guards are predicted to fail — that is what keeps CAUGHT from being a catch-all.

CONTROLS = []

G_TWO = "TestCreate_TwoPagesCanShareATitleInOneSpace_RealPG"
G_SPACE = "TestCreate_SlugSuffixRestartsPerSpace_RealPG"
G_MANY = "TestCreate_ManySameTitledPagesInOneSpace_RealPG"
G_TMPL = "TestUseTemplate_TwiceIntoTheSameSpace_RealPG"


def control(name, why, expect, expect_mine, expect_other=False):
    def deco(fn):
        CONTROLS.append((name, why, expect, expect_mine, expect_other, fn))
        return fn
    return deco


@control(
    "C1 THE DEFECT — no retry at all (main's behaviour)",
    "the single INSERT that failed on the second New page click",
    # ⚠ RE-CUT. The first version deleted the suffix line, which is `base`'s only use, so the tree
    # did not COMPILE and the control scored BUILD-FAILED. A compile error is not a caught mutation.
    # Disabling the retry CONDITION instead leaves every declaration used and changes exactly one
    # thing: whether a slug collision is retried.
    # ⚠ AND THE TAG SET IS NOT THE WHOLE ANSWER — see failed_tests(). SlugSuffixRestartsPerSpace
    # dies at a t.Fatalf in its setup (the create it needs is the one C1 breaks), so it is RED with
    # no tag printed. Predicting the guard NAMES as well as the tags is what makes that visible.
    expect={"SECOND-NEW-PAGE", "SLUGS-DIFFER", "MANY", "TEMPLATE-TWICE"},
    expect_mine={G_TWO, G_SPACE, G_MANY, G_TMPL},
)
def c1(src):
    return src.replace(
        "		if !isSlugTaken(err) || attempt >= maxSlugAttempts {",
        "		if true {",
        1,
    )


@control(
    "C2 disambiguate the TITLE instead of the slug",
    "makes the INSERT succeed by renaming the user's page — every count assertion passes",
    # ⚠ RE-CUT for the same reason as C1: replacing the suffix line alone orphaned `base`.
    expect={"TITLE-UNTOUCHED"},
    expect_mine={G_TWO},
)
def c2(src):
    return src.replace("	base := p.Slug\n", "", 1).replace(
        SUFFIX_LINE,
        '		p.Title = fmt.Sprintf("%s (%d)", p.Title, attempt+1)\n'
        '		p.Slug = slugify(p.Title)',
        1,
    )


@control(
    "C3 suffix from attempt ONE — no page keeps its plain derived slug",
    "the shape a fix takes if the counter starts at 1 rather than being an exception",
    # ⚠ MY PREDICTION WAS WRONG, UNDER-LISTING. It also reds [SCOPED-TO-SPACE] and — the one that
    # matters — the PRE-EXISTING pgxmock test TestCreate_AutoSlugAndDepthAndVersion, which already
    # pins slugify(title) verbatim for a lone page. That is why the standalone real-PG plain-slug
    # test this campaign first wrote was DELETED rather than kept: it duplicated a guard the repo
    # already had. The in-collision assertion survives because the mock never sees a collision.
    expect={"FIRST-SLUG-UNCHANGED", "SCOPED-TO-SPACE"},
    expect_mine={G_TWO, G_SPACE},
    expect_other=True,
)
def c3(src):
    return src.replace(
        "	base := p.Slug\n",
        '	base := p.Slug\n	p.Slug = fmt.Sprintf("%s-1", base)\n',
        1,
    )


@control(
    "C4 bound the retry at one attempt",
    "a fix that handles the second page and not the third",
    # ⚠ MY PREDICTION WAS WRONG, UNDER-LISTING: [TEMPLATE-TWICE] drives a THIRD use of the template,
    # so it reds too. [SECOND-NEW-PAGE] stays green — it only ever needs two.
    expect={"MANY", "TEMPLATE-TWICE"},
    expect_mine={G_MANY, G_TMPL},
)
def c4(src):
    return src.replace(MAX_ATTEMPTS, "const maxSlugAttempts = 2", 1)


@control(
    "C5 disambiguate against the WORKSPACE instead of the space",
    "the plausible alternative implementation: pre-count matching slugs, scoped one level too wide",
    # ⚠ THIS CONTROL SCORED **NOT CAUGHT** AGAINST THE FIRST VERSION OF [SCOPED-TO-SPACE], AND THAT
    # IS WHY THAT TEST WAS REWRITTEN. Its first version gave each space ONE page, which never
    # collides, so the retry body — the only place this mutation lives — never executed. The guard
    # was green for a reason that had nothing to do with the product. The rewritten test makes each
    # space collide on its own, which is the only shape where per-space and per-workspace counting
    # give different answers.
    expect={"SCOPED-TO-SPACE"},
    expect_mine={G_SPACE},
)
def c5(src):
    # Replace the retry's suffix derivation with a pre-query counted over the whole workspace.
    return src.replace(
        SUFFIX_LINE,
        """		var taken int
		_ = s.pool.QueryRow(ctx,
			`SELECT count(*) FROM pages WHERE workspace_id = $1 AND slug LIKE $2 || '%'`,
			p.WorkspaceID, base).Scan(&taken)
		p.Slug = fmt.Sprintf("%s-%d", base, taken+1)""",
        1,
    )


@control(
    "C6 MUST-STAY-GREEN COMPANION — drop the page-creation metric",
    "a real defect in the same function that the new guards must NOT claim to catch",
    expect=set(),
    expect_mine=set(),
    expect_other=True,
)
def c6(src):
    return src.replace("	metrics.PagesCreated.Inc()", "	_ = metrics.PagesCreated", 1)


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        sys.exit("DOCS_TEST_DATABASE_URL is required — these are real-Postgres guards")

    original = open(STORE, "rb").read()
    baseline_sha = sha(STORE)

    print(f"baseline sha256({os.path.relpath(STORE, REPO)}) = {baseline_sha}")
    print("C0 — no mutation; both suites must be GREEN before any control means anything")
    fired, out = run_suites()
    mine, others = failed_tests(out)
    if fired or mine or others:
        sys.exit(f"C0 FAILED: tags={sorted(fired)} mine={mine} other={others}\n{out[-4000:]}")
    print("C0 ok — clean tree, both suites green\n")

    results, ok = [], True
    for name, why, expect, expect_mine, expect_other, mutate in CONTROLS:
        src = original.decode()
        mutated = mutate(src)
        if mutated == src:
            sys.exit(f"{name}: ANCHOR DID NOT MATCH — the control mutated nothing and would have "
                     f"scored NOT CAUGHT against an innocent tree")
        try:
            open(STORE, "w").write(mutated)
            fired, out = run_suites()
            mine, others = failed_tests(out)
        finally:
            open(STORE, "wb").write(original)
            if sha(STORE) != baseline_sha:
                sys.exit(f"{name}: RESTORE FAILED — tree not byte-identical; stopping")
        agree = (fired == expect) and (bool(others) == expect_other) and (mine == sorted(expect_mine))
        ok = ok and agree
        results.append((name, why, expect, fired, mine, others, agree))
        print(f"{'PASS' if agree else 'MISMATCH'}  {name}")
        print(f"        predicted tags={sorted(expect)} guards={sorted(expect_mine)}"
              f"{' +other' if expect_other else ''}")
        print(f"        actual    tags={sorted(fired)} guards={mine}"
              f"{(' +other: ' + ', '.join(others)) if others else ''}")
        if not agree:
            print(out[-3000:])
        print()

    print("=" * 78)
    for name, why, expect, fired, mine, others, agree in results:
        print(f"{'✓' if agree else '✗'} {name}\n    {why}\n    tags: {sorted(fired) or 'NONE'}"
              f"\n    guards red: {mine or 'NONE'}"
              f"{(' | other: ' + ', '.join(others)) if others else ''}")
    print("=" * 78)
    print(f"final sha256 = {sha(STORE)} ({'identical' if sha(STORE) == baseline_sha else 'DRIFTED'})")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
