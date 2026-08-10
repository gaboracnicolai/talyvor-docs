#!/usr/bin/env python3
"""Positive controls for the stale report's private-space gate (REST + MCP).

Same scoring discipline as scripts/w31-askgrounding-controls.py, and for the same reason (#69):
knowing WHICH TEST reddened is not enough, so this scores THE SET OF ASSERTION TAGS THAT FIRED,
compares it to a per-control prediction written BEFORE the run, records the OTHER tests that
reddened so a control a pre-existing guard also catches cannot be read as justifying mine, treats
a BUILD FAILURE as its own verdict, carries a NO-OP that must stay GREEN, and restores every
mutated file in a `finally` with a sha256 compare.

⚠ TWO PACKAGES ARE RUN, NOT ONE. The whole point of this finding is that ONE engine method feeds
TWO surfaces and a sweep by route finds only one of them, so the run target has to contain both
`./internal/freshness/` (the guard, which drives the REST route AND HandleRPC) and
`./internal/mcp/` (whose own tool tests are the pre-existing guards that must be seen when they
also redden).

⚠ ONE ASSERTION IS AN INSTRUMENT CHECK AND IS LISTED SEPARATELY RATHER THAN EXEMPTED QUIETLY.
[D-PREMISE] fires when bob cannot see the PUBLIC stale page either — the state in which every
absence assertion below is vacuous. A product mutation that reaches it (the report emptied) is
exactly C3, and C3 DOES fire it, so unlike the ask harness this one is claimable; it is listed as a
product assertion. What is listed as instrument-only here is nothing: the split is kept in the code
so the next copy of this harness does not have to re-derive it.

Run:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-stalereport-controls.py
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ENGINE = os.path.join(REPO, "internal/freshness/engine.go")
MAIN = os.path.join(REPO, "cmd/docs/main.go")
WIRETEST = os.path.join(REPO, "internal/freshness/mainwiring_test.go")
PERMSTORE = os.path.join(REPO, "internal/permission/store.go")
GUARD = os.path.join(REPO, "internal/freshness/privatespace_realpg_test.go")

DECLARED = [
    "D-PREMISE",
    "D-LEAK-REST", "D-LEAK-MCP", "D-MCPPUB",
    "D-DIGEST",
    "D-OWNER", "D-GRANT",
    "D-WIRED", "D-SENTINEL", "D-STRIP",
]
INSTRUMENT = []

CONTROLS = [
    (
        "C0-noop",
        ENGINE,
        "// staleReportAll is the SYSTEM view",
        "// staleReportAll is the SYSTEM view (C0: a comment-only edit)",
        [],
        "MUST STAY GREEN. A comment-only edit. Without it, a harness in which everything reds — "
        "because the tree stopped building — is indistinguishable from a working control set.",
    ),
    (
        "C1-no-filter",
        ENGINE,
        "\t\tfound, canView := e.access.AuthorizePageRead(ctx, r.PageID)\n\t\tif !found || !canView {",
        "\t\tfound, _ := e.access.AuthorizePageRead(ctx, r.PageID)\n\t\tif !found {",
        ["D-LEAK-REST", "D-LEAK-MCP"],
        "THE DEFECT ITSELF, in the shape that reads as handled: the gate is wired and CALLED and "
        "consults only EXISTENCE. Every page in the workspace is `found`. BOTH surfaces must speak "
        "— that is the claim this whole merge rests on, and a control that reddened only the REST "
        "half would mean the MCP assertion was decoration.",
    ),
    (
        "C2-filter-bypassed",
        ENGINE,
        "\t\tout = append(out, r)\n\t}\n\treturn out, nil\n}",
        "\t\tout = append(out, r)\n\t}\n\treturn all, nil\n}",
        ["D-LEAK-REST", "D-LEAK-MCP"],
        "The loop runs, appends, and the UNFILTERED list is returned anyway — a different way to be "
        "unfiltered from C1, and the one a careless refactor produces. ⚠ MY FIRST DRAFT REPLACED "
        "THE APPEND TOO, WHICH LEFT `out` UNUSED AND THE HARNESS SCORED IT `BUILD FAILED — not a "
        "catch` RATHER THAN CAUGHT.",
    ),
    (
        "C3-drop-everything",
        ENGINE,
        "\t\tout = append(out, r)\n\t}\n\treturn out, nil\n}",
        "\t\t_ = r\n\t}\n\treturn out, nil\n}",
        ["D-PREMISE"],
        "THE OVER-CORRECTION, and the reason no absence assertion here can be trusted alone: a "
        "report that serves NOTHING satisfies both LEAK checks perfectly. [D-PREMISE] is a "
        "t.Fatalf, so it aborts before the rest — which is exactly what it is for, and why the "
        "over-correction assertions that follow it (D-OWNER, D-GRANT) need C5/C6 to claim them "
        "rather than riding along here.",
    ),
    (
        "C4-digest-uses-gated-list",
        ENGINE,
        "\treports, err := e.staleReportAll(ctx, workspaceID)",
        "\treports, err := e.GetStaleReport(ctx, workspaceID)",
        ["D-DIGEST"],
        "THE MISTAKE THIS DESIGN EXISTS TO PREVENT, made deliberately: point the daily digest at "
        "the request-scoped list. ⚠⚠ MY PREDICTION WAS WRONG AND THE TRUTH IS QUIETER THAN THE "
        "PREDICTION. I expected an ERROR (no caller ⇒ ErrNoPageReadGate). The gate IS wired, so "
        "GetStaleReport does not error — it asks the permission engine about a context with NO "
        "memberships, every page resolves to not-found, and the digest logs `stale_pages=0`. The "
        "operator is told the workspace is clean. There is no error to notice, which is exactly "
        "why the unfiltered list had to survive rather than the digest being 'fixed' to pass a "
        "caller it does not have.",
    ),
    (
        "C5-no-owner-is-admin",
        PERMSTORE,
        "\tif memberID != \"\" && memberID == res.CreatedBy {\n\t\treturn AccessAdmin\n\t}",
        "\tif false {\n\t\treturn AccessAdmin\n\t}",
        ["D-OWNER"],
        "resolveAccess's first arm is what makes a space's CREATOR an admin with no grant row. "
        "bob's explicit grant is untouched, so D-GRANT must stay green — that is what makes this "
        "control name D-OWNER rather than 'the permission engine broke'.",
    ),
    (
        "C6-member-grant-inert",
        PERMSTORE,
        "\t\tcase \"member\":\n\t\t\tif p.SubjectID != memberID {\n\t\t\t\treturn\n\t\t\t}",
        "\t\tcase \"member\":\n\t\t\treturn",
        ["D-GRANT"],
        "The mirror of C5: an explicit per-member grant confers nothing while owner-is-admin and "
        "the public-space default are untouched. Separates D-GRANT from D-OWNER instead of leaving "
        "both claimed by one emptying mutation.",
    ),
    (
        "C7-public-spaces-denied",
        PERMSTORE,
        "\tif !res.Private {\n\t\treturn AccessView\n\t}",
        "\tif false {\n\t\treturn AccessView\n\t}",
        ["D-PREMISE"],
        "The public-space default removed: a plain member loses the PUBLIC page too. This is what "
        "claims [D-PREMISE] as a product assertion rather than an instrument check — the state it "
        "guards against is reachable, and reachable from the permission engine rather than from a "
        "broken fixture. ⚠ D-MCPPUB sits after the Fatalf and cannot speak here; C11 is what "
        "shows it is live at all.",
    ),
    (
        "C8-wiring-deleted",
        MAIN,
        "\tfreshEngine.WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))\n",
        "",
        ["D-WIRED"],
        "The gate stops being switched on in production. THE REAL-PG GUARD MUST STAY GREEN — it "
        "builds its own engine and is structurally blind to main.go, which is the entire reason "
        "the tripwire exists as a second, deliberately blind guard.",
    ),
    (
        "C9-strip-removed",
        WIRETEST,
        "\t\tif strings.HasPrefix(strings.TrimSpace(line), \"//\") {\n\t\t\tcontinue\n\t\t}",
        "\t\tif false {\n\t\t\tcontinue\n\t\t}",
        ["D-STRIP"],
        "A GUARD MUTATION. Without comment stripping the tripwire is satisfied by main.go merely "
        "MENTIONING the call in prose. D-WIRED must stay silent: the call is still there.",
    ),
    (
        "C10-sentinel-removed",
        MAIN,
        "AND THE SEAM'S FOURTH COPY, WHICH TWO SURFACES SHARE.",
        "AND THE SEAM'S FOURTH COPY.",
        ["D-SENTINEL"],
        "A COMMENT-ONLY EDIT THAT MUST RED — the opposite of C0, and the control the sibling guard "
        "in internal/search does not have. C9's vacuity check is only meaningful if its sentinel is "
        "actually in the file; delete the phrase and its absence proves nothing. MEASURED on the "
        "sibling: rewording ITS sentinel leaves TestSearch_MainWiresTheAccessGate GREEN.",
    ),
    (
        "C11-mcp-surface-blinded",
        GUARD,
        'srv := mcp.New(store, spaceStore, nil, nil, engine, "test")',
        'srv := mcp.New(store, spaceStore, nil, nil, nil, "test")',
        ["D-MCPPUB"],
        "A GUARD MUTATION, AND THE ONE THAT SHOWS [D-MCPPUB] IS NOT DECORATION: the MCP half of "
        "this guard would be vacuously green against a surface reporting nothing at all, and an "
        "empty payload satisfies [D-LEAK-MCP] perfectly. D-LEAK-MCP must stay SILENT here — that "
        "silence IS the vacuity being demonstrated. No PRODUCT mutation reaches this state without "
        "also emptying the REST report and tripping the D-PREMISE Fatalf first.\n"
        "        ⚠⚠ THIS CONTROL FOUND A CRASH ON ITS FIRST RUN AND SCORED `fired=(none)`. "
        "mcp.New takes a *FreshnessEngine and stores it in an INTERFACE field, so a nil engine is a "
        "NON-nil interface holding a nil pointer: internal/mcp/server.go:874's "
        "`if s.deps.freshness == nil` is not a nil-check, and get_stale_pages SIGSEGV'd. "
        "PRE-EXISTING — the old body dereferenced e.pages one statement later — but it is why "
        "GetStaleReport is now nil-receiver safe. THE DEAD nil GUARD IN internal/mcp IS ITS OWN "
        "FINDING AND IS NOT FIXED HERE.",
    ),
]

PKGS = ["./internal/freshness/", "./internal/mcp/"]
TAG_RE = re.compile(r"\[(D-[A-Z-]+)\]")
FAIL_RE = re.compile(r"^\s*--- FAIL: (\S+)", re.M)
MINE = {
    "TestFreshness_PrivateSpace_NotReportedWithoutGrant_RealPG",
    "TestFreshness_MainWiresThePageReadGate",
}


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def run_tests():
    proc = subprocess.run(
        ["go", "test", *PKGS, "-count=1", "-v"],
        cwd=REPO, env=dict(os.environ), capture_output=True, text=True, timeout=900,
    )
    raw = proc.stdout + proc.stderr
    if ("[build failed]" in raw or "cannot use" in raw or "undefined:" in raw
            or "syntax error" in raw or "declared and not used" in raw):
        return "build", set(), set(), raw
    return (("green" if proc.returncode == 0 else "red"),
            set(TAG_RE.findall(raw)), set(FAIL_RE.findall(raw)), raw)


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set — this suite needs a real Postgres.")
        return 2

    paths = {ENGINE, MAIN, WIRETEST, PERMSTORE, GUARD}
    originals = {p: open(p, "rb").read() for p in paths}
    before = {p: sha(p) for p in paths}

    verdict, tags, failed, raw = run_tests()
    if verdict != "green":
        print(f"BASELINE NOT GREEN ({verdict}) — nothing below can be read.\n{raw[-4000:]}")
        return 1
    print("baseline: GREEN\n")

    results, claimed = [], set()
    try:
        for cid, path, old, new, predicted, why in CONTROLS:
            src = originals[path].decode()
            if src.count(old) != 1:
                results.append((cid, "ANCHOR", set(), set(predicted), set()))
                print(f"{cid}: ANCHOR MISSING/AMBIGUOUS ({src.count(old)} matches) — control is dead")
                continue
            with open(path, "w") as f:
                f.write(src.replace(old, new, 1))
            verdict, tags, failed, raw = run_tests()
            with open(path, "wb") as f:
                f.write(originals[path])
            if verdict == "build":
                results.append((cid, "BUILD", set(), set(predicted), set()))
                print(f"{cid}: BUILD FAILED — not a catch")
                continue
            others = failed - MINE
            ok = (tags == set(predicted)) and (verdict == ("red" if predicted else "green"))
            results.append((cid, "AS PREDICTED" if ok else "DIVERGED", tags, set(predicted), others))
            claimed |= tags
            extra = f"  also-red={sorted(others)}" if others else ""
            print(f"{cid}: {verdict.upper():5s} fired={sorted(tags) or '(none)'} "
                  f"predicted={sorted(predicted) or '(none)'} -> "
                  f"{'AS PREDICTED' if ok else '⚠ DIVERGED'}{extra}")
    finally:
        for p, b in originals.items():
            with open(p, "wb") as f:
                f.write(b)
        for p in paths:
            assert sha(p) == before[p], f"RESTORE FAILED for {p}"
        print("\nall files restored, sha256 verified")

    unclaimed = [a for a in DECLARED if a not in claimed]
    print("\n--- verdict ---")
    for cid, status, got, pred, others in results:
        note = f"   (also reddened {sorted(others)})" if others else ""
        print(f"  {cid:28s} {status}{note}")
    if unclaimed:
        print("\n⚠ PRODUCT ASSERTIONS CLAIMED BY NO CONTROL (each could be constant-true and "
              "nothing here would notice):")
        for a in unclaimed:
            print(f"    {a}")
    diverged = [r for r in results if r[1] != "AS PREDICTED"]
    print(f"\n{len(results) - len(diverged)}/{len(results)} as predicted; "
          f"{len(DECLARED) - len(unclaimed)}/{len(DECLARED)} product assertions claimed")
    return 1 if (diverged or unclaimed) else 0


if __name__ == "__main__":
    sys.exit(main())
