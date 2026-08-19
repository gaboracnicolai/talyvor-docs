#!/usr/bin/env python3
"""Mount/read agreement for chi path params: WHICH INSTRUMENT OWNS IT, measured (W3.1, tab-8v3r).

THIS SCRIPT EXISTS TO STOP A GUARD FROM BEING BUILT.

After #183 closed the literal blind spot in `.semgrep/operate-by-id-tenancy.yml`, the handover on
this thread named a next merge: a census over the ROUTE TREE asserting that the only path param a
workspace-scoped mount introduces is `wsID`, so the sibling rule's single literal would be true by
measurement rather than by convention. It also promised to catch the class underneath it — a route
that MOUNTS `{pageID}` while its handler READS "pageId", where `chi.URLParam` quietly returns "" and
a store op runs with an empty id.

BOTH JUSTIFICATIONS WERE MEASURED AND BOTH FAILED, so no such guard was written:

  · THE WORKSPACE-NAME HALF IS ALREADY CLOSED BY #183 ITSELF. A new mount of /orgs/{orgID} or
    /workspaces/{workspaceID} is only dangerous once a handler READS it, and the moment it does —
    by literal or through a function argument — the name is outside the census and
    docs-no-indirect-url-param-scope fires. A route census would red one step earlier on a param
    that, until it is read, does nothing.

  · THE MISMATCH HALF IS ALREADY COVERED, and not thinly. Probed twice below, on the two shapes that
    could plausibly differ — a route behind an authz enforcer, and an ungated public one. Both went
    red, and semgrep was blind to both (correctly: it is not a property of any one line).

WHAT THE PROBES ALSO MEASURED, AND IT IS THE PART WORTH KEEPING. The two shapes are NOT equally
held:

  A. GATED ROUTE (permission, {permID} -> {permIdentifier}): FOUR instruments red, including
     internal/routeguard's pinned in-class route set — a STRUCTURAL guard that fires on any change
     to the gated-route census, whether or not a behavioural test happens to drive that route.

  B. UNGATED PUBLIC ROUTE (customdomain, {slug} -> {slugName}): ONE test red, and it is a
     behavioural one — TestCustomDomain_SpaceMadePrivateStopsBeingServed_RealPG, which drives the
     public route for an unrelated reason (a private-flip regression) and notices the 404 in
     passing. Nothing structural covers ungated mounts. Delete or narrow that one test and this
     class goes silent for every public route.

So the residual is not "build a census"; it is "B rests on a single test that is not about this".
Re-run this script to find out whether that is still true — that is the whole point of writing the
measurement down as something executable rather than as a sentence in a comment.

RUN: python3 scripts/w31-mountread-probe-8v3r.py   (repo root; needs docker + DOCS_TEST_DATABASE_URL)
Each probe restores its file in a finally block. Takes a few minutes: each one runs the whole suite,
because "does ANYTHING catch it" is the question and a selected package cannot answer it.
"""

import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SEMGREP_IMAGE = "semgrep/semgrep:1.165.0"  # pinned to .github/workflows/ci.yaml

DSN = os.environ.get("DOCS_TEST_DATABASE_URL")

PROBES = [
    {
        "name": "A  GATED ROUTE — permission DELETE /spaces/{spaceID}/permissions/{permID}",
        "path": os.path.join(ROOT, "internal", "permission", "handler.go"),
        "old": '"/spaces/{spaceID}/permissions/{permID}"',
        "new": '"/spaces/{spaceID}/permissions/{permIdentifier}"',
        "reads": 'chi.URLParam(r, "permID")',
        "expect": "several instruments red, one of them STRUCTURAL (routeguard's in-class route set)",
    },
    {
        "name": "B  UNGATED PUBLIC ROUTE — customdomain GET /{slug}",
        "path": os.path.join(ROOT, "internal", "customdomain", "handler.go"),
        "old": 'r.Get("/{slug}", h.publicPage)',
        "new": 'r.Get("/{slugName}", h.publicPage)',
        "reads": 'chi.URLParam(r, "slug")',
        "expect": "red, but from BEHAVIOURAL tests only — count them, that count is the finding",
    },
]


def run(cmd, env=None):
    e = dict(os.environ)
    if env:
        e.update(env)
    return subprocess.run(cmd, cwd=ROOT, capture_output=True, text=True, env=e)


def suite():
    """The whole Go suite. -> (failed_packages, failed_tests)."""
    p = run(["go", "test", "-timeout", "300s", "-count=1", "./..."],
            env={"DOCS_TEST_DATABASE_URL": DSN})
    out = p.stdout + p.stderr
    # `[ \t]+`, NOT `\s+`. Go prints a bare "FAIL" line as well as "FAIL\t<pkg>\t<time>", and `\s`
    # matches the newline — so `^FAIL\s+(\S+)` read the bare line, swallowed the newline and
    # captured the literal string "FAIL" as a package name. Measured: it reported "1 package" for a
    # probe that reddened three. A harness that prints a wrong count is worse than one that prints
    # none, because the number looks measured.
    pkgs = sorted(set(re.findall(r"^FAIL[ \t]+(\S+)", out, re.M)))
    tests = sorted(set(re.findall(r"^--- FAIL: (\S+)", out, re.M)))
    return pkgs, tests


def semgrep_blind():
    p = run(["docker", "run", "--rm", "-v", f"{ROOT}:/src", "-w", "/src",
             SEMGREP_IMAGE, "semgrep", "--config", ".semgrep/", "--error", "--quiet"])
    return p.returncode == 0


def main():
    if not DSN:
        print("DOCS_TEST_DATABASE_URL is unset. This probe asks whether the REAL-POSTGRES suite\n"
              "catches a mount/read mismatch; without a database it would answer 'no' for the wrong\n"
              "reason, which is the exact green-by-absence trap this repo removed from CI.")
        return 2

    print("Mount/read agreement — which instrument owns it (tab-8v3r)\n")

    # BASELINE, AND IT IS NOT CEREMONY. Every verdict below is "the suite went red when I broke
    # something". Against an already-red suite that sentence is true no matter what the mutation
    # did, and the script would print CAUGHT for a probe that proved nothing — the same shape as a
    # control asserting a failure that was already there.
    print("─── BASELINE — the suite must be GREEN before any verdict below means anything")
    base_pkgs, base_tests = suite()
    if base_tests or base_pkgs:
        print(f"    ABORTING: the suite is ALREADY RED at HEAD — {len(base_tests)} test(s) in "
              f"{len(base_pkgs)} package(s):")
        for t in base_tests:
            print(f"      | {t}")
        print("    Every probe would report CAUGHT for free. Fix the tree, then re-run.")
        return 2
    print("    clean\n")

    verdicts = []
    for pr in PROBES:
        print(f"─── {pr['name']}")
        print(f"    mount renamed, handler still reads {pr['reads']} -> chi.URLParam returns \"\"")
        print(f"    expect: {pr['expect']}")
        original = open(pr["path"], encoding="utf-8").read()
        n = original.count(pr["old"])
        if n != 1:
            print(f"    MUTATION NOT APPLIED: {n} copies of the anchor, expected 1. "
                  f"This probe measured NOTHING — the route was moved or renamed; fix the anchor.")
            verdicts.append((pr["name"], False, 0))
            continue
        try:
            open(pr["path"], "w", encoding="utf-8").write(original.replace(pr["old"], pr["new"]))
            pkgs, tests = suite()
            blind = semgrep_blind()
        finally:
            open(pr["path"], "w", encoding="utf-8").write(original)
        caught = len(tests) > 0
        print(f"    {'CAUGHT' if caught else 'NOT CAUGHT — SILENT'} by {len(tests)} test(s) "
              f"in {len(pkgs)} package(s)")
        for t in tests:
            print(f"      | {t}")
        print(f"      | semgrep exit 0 (blind): {blind} — expected True; it is not a property of one line")
        verdicts.append((pr["name"], caught, len(tests)))

    print("\n" + "=" * 78)
    for name, caught, n in verdicts:
        print(f"  {'CAUGHT' if caught else 'SILENT'}  ({n} test(s))  {name}")
    print("=" * 78)
    silent = [n for n, c, _ in verdicts if not c]
    if silent:
        print("\nA MOUNT/READ MISMATCH IS NOW SILENT SOMEWHERE. The conclusion this script was written\n"
              "to record — that no route-param census is needed — NO LONGER HOLDS. Build it.")
        return 1
    thin = [n for n, _, count in verdicts if count <= 1]
    if thin:
        print("\nStill covered, but at least one shape rests on a SINGLE test — see probe B's note in\n"
              "the header. That is a fact about coverage, not a failure of this run.")
    print("\nBoth shapes still caught. No route-param census is warranted.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
