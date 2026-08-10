#!/usr/bin/env python3
"""Positive controls for the /pages/search + /pages/stale private-space gate.

WHAT THIS SCORES, AND WHY IT IS NOT PASS/FAIL.

`c70aa6e` (#69) recorded that a PASS/FAIL control harness scored 7/7 while TWO of four
assertions were justified by nothing: one mutation fired two assertions at once, so either
could have been deleted with every control still reading AS PREDICTED. Knowing WHICH TEST
went red is not enough. This harness therefore:

  * scores THE SET OF ASSERTION TAGS THAT FIRED ([A-LEAK/pages/search] and friends), read
    out of the test output, not the exit code;
  * compares that set to a prediction written per control BEFORE running it;
  * FAILS if any declared assertion is claimed by NO control (an assertion nothing justifies
    is an assertion that could be constant-true);
  * treats a BUILD FAILURE as its own verdict, so a compile error can never be scored as a
    catch (`a control that breaks the build is not a control`);
  * carries a NO-OP control that must stay GREEN, so "everything reds" cannot look like a
    working control set;
  * restores every mutated file in a `finally` and sha256-compares, so a crash between
    mutate and restore cannot leave a mutation on disk.

Run:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-pagesearch-visibility-controls.py
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HANDLER = os.path.join(REPO, "internal/page/handler.go")
SPACEAUTH = os.path.join(REPO, "internal/spaceauth/spaceauth.go")
PERMSTORE = os.path.join(REPO, "internal/permission/store.go")
TEST = "TestPageSearchAndStale_PrivateSpace_NotVisibleWithoutGrant_RealPG"

# Every assertion the guard makes, by tag. A control must claim each one.
DECLARED = [
    "A-PREMISE/pages/search", "A-PREMISE/pages/stale",
    "A-LEAK/pages/search", "A-LEAK/pages/stale",
    "A-SLOT/search",
    "A-OWNER/pages/search", "A-OWNER/pages/stale",
    "A-GRANT/pages/search", "A-GRANT/pages/stale",
    "A-GRANTPUB/pages/search", "A-GRANTPUB/pages/stale",
]

# (id, file, old, new, predicted-tags, why)
#
# PREDICTIONS ARE WRITTEN AGAINST THE FIXED TREE AND ARE MEANT TO BE FALSIFIABLE. Where a
# prediction below turned out wrong the note says so rather than being quietly edited — a
# wrong prediction is the only thing that shows the harness is reading the product and not
# my expectations of it.
CONTROLS = [
    (
        "C0-noop",
        HANDLER,
        "// visibleTo drops every row the caller may not VIEW",
        "// visibleTo drops every row the caller may not VIEW (C0: a comment-only edit)",
        [],
        "MUST STAY GREEN. A comment-only edit. Without this, a harness in which every control "
        "reds — because the tree no longer builds, or the fixture broke — is indistinguishable "
        "from a working control set.",
    ),
    (
        "C1-search-unfiltered",
        HANDLER,
        "\tout := h.visibleTo(r.Context(), rows)\n\tif limit > 0 && len(out) > limit {",
        "\tout := rows\n\tif limit > 0 && len(out) > limit {",
        ["A-LEAK/pages/search", "A-SLOT/search"],
        "The defect itself, on the search half only. /pages/stale must stay green — that is what "
        "makes this control name a catcher on THIS half rather than 'the guard noticed something'. "
        "⚠ I PREDICTED A-LEAK ALONE AND WAS WRONG: the private memo is the TOP row, so with the "
        "filter gone limit=1 returns it and A-SLOT speaks too. A-LEAK/pages/search is therefore "
        "claimed only JOINTLY; C5/C6 are what claim A-SLOT alone.",
    ),
    (
        "C2-stale-unfiltered",
        HANDLER,
        "\t// GetStalePages has no LIMIT, so unlike Search there is no truncation to race and the filter\n"
        "\t// is complete rather than a mitigation.\n\tout := h.visibleTo(r.Context(), rows)",
        "\t// GetStalePages has no LIMIT, so unlike Search there is no truncation to race and the filter\n"
        "\t// is complete rather than a mitigation.\n\tout := rows",
        ["A-LEAK/pages/stale"],
        "The defect on the stale half only. The two halves are separate call sites and a guard "
        "green at one says nothing about the other.",
    ),
    (
        "C3-canview-ignored",
        HANDLER,
        "\t\tfound, canView := h.access.AuthorizePageRead(ctx, p.ID)\n\t\tif !found || !canView {",
        "\t\tfound, _ := h.access.AuthorizePageRead(ctx, p.ID)\n\t\tif !found {",
        ["A-LEAK/pages/search", "A-LEAK/pages/stale", "A-SLOT/search"],
        "The filter runs but consults only EXISTENCE. Every page in the workspace is `found`, so "
        "this is the shape where a gate is present, is called, and decides nothing. ⚠ A-SLOT rides "
        "along for C1's reason.",
    ),
    (
        "C4-drop-everything",
        HANDLER,
        "\t\tif !found || !canView {\n\t\t\tcontinue\n\t\t}\n\t\tout = append(out, p)",
        "\t\tif !found || !canView {\n\t\t\tcontinue\n\t\t}\n\t\t_ = p",
        ["A-PREMISE/pages/search", "A-PREMISE/pages/stale",
         "A-GRANT/pages/search", "A-GRANT/pages/stale",
         "A-GRANTPUB/pages/search", "A-GRANTPUB/pages/stale"],
        "The over-correction that empties the endpoint. It must be the PREMISE assertions that "
        "speak, not the LEAK ones: an endpoint serving nothing satisfies every absence check "
        "perfectly, and that is exactly how an absence sweep goes vacuously green. ⚠ I PREDICTED THE "
        "TWO PREMISE TAGS ALONE AND WAS WRONG: PREMISE is a t.Fatalf, which aborts only its own "
        "SUBTEST — the granted-bob block runs after that loop, at function scope, so it still "
        "executes and fires four more tags. Where an assertion sits relative to a Fatalf is part "
        "of what it can say.",
    ),
    (
        "C5-no-overfetch",
        HANDLER,
        "\t\tfetchLimit = limit * searchFetchFactor",
        "\t\tfetchLimit = limit",
        ["A-SLOT/search"],
        "Removes the over-fetch alone. The private memo sorts above the public handbook, so with "
        "limit=1 the store returns the hidden row, the filter drops it, and denied bob is told "
        "NOTHING MATCHED for a document he may open. This is the ONLY control that claims A-SLOT.",
    ),
    (
        "C6-truncate-before-filter",
        HANDLER,
        "\tout := h.visibleTo(r.Context(), rows)\n\tif limit > 0 && len(out) > limit {\n\t\tout = out[:limit]\n\t}",
        "\tif limit > 0 && len(rows) > limit {\n\t\trows = rows[:limit]\n\t}\n\tout := h.visibleTo(r.Context(), rows)",
        ["A-SLOT/search"],
        "Filter and truncation in the wrong order. Same visible symptom as C5 by a different "
        "edit, which is the point: A-SLOT has two independent catchers and neither is the "
        "mutation the other tests.",
    ),
    (
        "C7-require-edit-not-view",
        SPACEAUTH,
        "\treturn true, permission.AtLeast(lvl, permission.AccessView)",
        "\treturn true, permission.AtLeast(lvl, permission.AccessEdit)",
        ["A-PREMISE/pages/search", "A-PREMISE/pages/stale",
         "A-GRANT/pages/search", "A-GRANT/pages/stale",
         "A-GRANTPUB/pages/search", "A-GRANTPUB/pages/stale"],
        "The gate asks for a level the caller was never required to hold. bob's grant is 'view', "
        "so the OVER-CORRECTION assertions must speak while alice (space creator ⇒ admin) still "
        "sees her page — which is what separates 'too strict' from 'broken'. ⚠ I PREDICTED THE GRANT "
        "TAGS ALONE AND WAS WRONG, AND THE MISS IS A FACT ABOUT THE PRODUCT: a plain member has "
        "no Edit on a PUBLIC space either (resolveAccess's public arm returns AccessView), so bob "
        "loses the public handbook too and PREMISE speaks first.",
    ),
    (
        "C8-no-owner-is-admin",
        PERMSTORE,
        "\tif memberID != \"\" && memberID == res.CreatedBy {\n\t\treturn AccessAdmin\n\t}",
        "\tif false {\n\t\treturn AccessAdmin\n\t}",
        ["A-OWNER/pages/search", "A-OWNER/pages/stale"],
        "ADDED BECAUSE THE HARNESS SAID A-OWNER WAS CLAIMED BY NOTHING. resolveAccess's first arm "
        "is what makes a space's CREATOR an admin without a grant row; without it alice falls to "
        "the private space's AccessNone and loses her own memo. bob's explicit 'view' grant is "
        "untouched, so A-GRANT must stay green — that is what makes this control name A-OWNER "
        "rather than 'the permission engine broke'.",
    ),
]


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def run_test():
    """Returns (verdict, tags, raw). verdict in {'green','red','build'}."""
    env = dict(os.environ)
    proc = subprocess.run(
        ["go", "test", "./internal/page/", "-run", TEST, "-count=1", "-v"],
        cwd=REPO, env=env, capture_output=True, text=True, timeout=600,
    )
    raw = proc.stdout + proc.stderr
    # A build/vet failure is its own verdict. `go test` prints these without ever running a
    # test, so scoring it as a catch would credit the guard for a compile error.
    if "[build failed]" in raw or "cannot use" in raw or "undefined:" in raw or "syntax error" in raw:
        return "build", set(), raw
    tags = set(re.findall(r"\[(A-[A-Z]+/[a-z/]+)\]", raw))
    if proc.returncode == 0:
        return "green", tags, raw
    return "red", tags, raw


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set — this suite needs a real Postgres.")
        return 2

    originals = {p: open(p, "rb").read() for p in (HANDLER, SPACEAUTH, PERMSTORE)}
    before = {p: sha(p) for p in originals}

    # BASELINE FIRST. If the unmutated tree is not green, every verdict below is unreadable.
    verdict, tags, raw = run_test()
    if verdict != "green":
        print(f"BASELINE NOT GREEN ({verdict}) — nothing below can be read.\n{raw[-4000:]}")
        return 1
    print("baseline: GREEN\n")

    results = []
    claimed = set()
    try:
        for cid, path, old, new, predicted, why in CONTROLS:
            src = originals[path].decode()
            if src.count(old) != 1:
                # An anchor that matches 0 or 2+ times is a dead control that would score
                # NOT CAUGHT for a reason that has nothing to do with the guard.
                results.append((cid, "ANCHOR", set(), set(predicted), why))
                print(f"{cid}: ANCHOR MISSING/AMBIGUOUS ({src.count(old)} matches) — control is dead")
                continue
            with open(path, "w") as f:
                f.write(src.replace(old, new, 1))
            verdict, tags, raw = run_test()
            # Restore immediately so a later control never stacks on this one.
            with open(path, "wb") as f:
                f.write(originals[path])
            if verdict == "build":
                results.append((cid, "BUILD", set(), set(predicted), why))
                print(f"{cid}: BUILD FAILED — not a catch")
                continue
            got = tags
            ok = (got == set(predicted)) and (verdict == ("red" if predicted else "green"))
            results.append((cid, "AS PREDICTED" if ok else "DIVERGED", got, set(predicted), why))
            claimed |= got
            mark = "AS PREDICTED" if ok else "⚠ DIVERGED"
            print(f"{cid}: {verdict.upper():5s} fired={sorted(got) or '(none)'} "
                  f"predicted={sorted(predicted) or '(none)'} -> {mark}")
    finally:
        for p, b in originals.items():
            with open(p, "wb") as f:
                f.write(b)
        for p in originals:
            assert sha(p) == before[p], f"RESTORE FAILED for {p}"
        print("\nall files restored, sha256 verified")

    unclaimed = [a for a in DECLARED if a not in claimed]
    print("\n--- verdict ---")
    for cid, status, got, pred, _ in results:
        print(f"  {cid:28s} {status}")
    if unclaimed:
        print("\n⚠ ASSERTIONS CLAIMED BY NO CONTROL (each could be constant-true and nothing "
              "here would notice):")
        for a in unclaimed:
            print(f"    {a}")
    diverged = [r for r in results if r[1] != "AS PREDICTED"]
    print(f"\n{len(results) - len(diverged)}/{len(results)} as predicted; "
          f"{len(DECLARED) - len(unclaimed)}/{len(DECLARED)} assertions claimed")
    return 1 if (diverged or unclaimed) else 0


if __name__ == "__main__":
    sys.exit(main())
