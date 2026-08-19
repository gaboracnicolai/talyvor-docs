#!/usr/bin/env python3
"""Positive controls for the URL-param classification guard (W3.1, tab-8v3r).

WHAT SHIPPED, AND WHY IT NEEDS THIS FILE. `.semgrep/operate-by-id-tenancy.yml` classifies every
`chi.URLParam` read in the tree with two rules. The sibling owns exactly ONE literal — "wsID". The
indirect rule owned "everything that is not a literal", excluding all strings on the written
grounds that "Every literal read is the sibling rule's business, INCLUDING \"wsID\"". The sibling's
business is one literal, so a workspace path param spelled ANY other literal way — {workspaceID},
{tenantID} — was invisible to BOTH rules by construction. The fix replaces the blanket exclusion
with a CENSUS of the 15 literal names measured not to be a workspace, and internal/paramcensus
asserts that census against the tree in both directions.

BOTH NEW GUARDS PASSED ON THEIR FIRST RUN. That is expected — the census was derived from the tree
and the tree is clean — and it is exactly the condition under which a guard that cannot fail looks
identical to a guard that works. Every control below therefore MUTATES something real and asserts
the instrument goes RED, and each names which instrument, because the two halves are blind to
different things:

  · `semgrep --test` applies rules to the fixture and IGNORES the tree entirely.
  · `semgrep --config .semgrep/ --error` reads the tree and is silent about a name nobody uses.
  · internal/paramcensus is the ONLY one that can see a DEAD EXEMPTION — a name that left the code
    and kept its pass. Control C2 is the whole reason that package exists.

RUN: python3 scripts/w31-paramliteral-controls-8v3r.py   (from the repo root; needs docker)
Every control restores its file in a finally block, and the script re-asserts a clean tree at exit.
"""

import os
import shutil
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RULE = os.path.join(ROOT, ".semgrep", "operate-by-id-tenancy.yml")
FIXTURE = os.path.join(ROOT, ".semgrep", "tests", "operate-by-id-tenancy.go")
GUARD = os.path.join(ROOT, "internal", "paramcensus", "param_census_test.go")
# A real production handler, so the "plant a read" controls exercise the same population the
# guards census — not a file invented for the occasion.
VICTIM = os.path.join(ROOT, "internal", "approval", "handler.go")

SEMGREP_IMAGE = "semgrep/semgrep:1.165.0"  # pinned to .github/workflows/ci.yaml

results = []


def run(cmd, **kw):
    return subprocess.run(cmd, cwd=ROOT, capture_output=True, text=True, **kw)


def sg_fixture():
    """`semgrep --test` over the rules + fixture, exactly as CI copies them. -> (passed, tail)."""
    tmp = tempfile.mkdtemp(prefix="sg-controls-8v3r-")
    try:
        for f in os.listdir(os.path.join(ROOT, ".semgrep")):
            if f.endswith(".yml"):
                shutil.copy(os.path.join(ROOT, ".semgrep", f), tmp)
        for f in os.listdir(os.path.join(ROOT, ".semgrep", "tests")):
            if f.endswith(".go"):
                shutil.copy(os.path.join(ROOT, ".semgrep", "tests", f), tmp)
        p = run(["docker", "run", "--rm", "-v", f"{tmp}:/src", "-w", "/src",
                 SEMGREP_IMAGE, "semgrep", "--test", "/src"])
        out = p.stdout + p.stderr
        return ("All tests passed" in out), out.strip().splitlines()[-6:]
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def sg_scan():
    """The blocking product scan. -> (exit_code, tail). Non-zero means a finding in the TREE."""
    p = run(["docker", "run", "--rm", "-v", f"{ROOT}:/src", "-w", "/src",
             SEMGREP_IMAGE, "semgrep", "--config", ".semgrep/", "--error", "--quiet"])
    out = (p.stdout + p.stderr).strip()
    return p.returncode, out.splitlines()[:12]


def go_guard():
    """internal/paramcensus. -> (exit_code, tail)."""
    p = run(["go", "test", "-count=1", "./internal/paramcensus/"])
    out = (p.stdout + p.stderr).strip()
    return p.returncode, out.splitlines()[:14]


def control(name, expectation, fn):
    print(f"\n─── {name}\n    expect: {expectation}")
    ok, detail = fn()
    results.append((name, ok))
    print(f"    {'PASS — instrument went red as required' if ok else 'FAIL — INSTRUMENT STAYED GREEN'}")
    for line in detail:
        print(f"      | {line}")


def mutate(path, old, new, instrument):
    """Replace exactly one occurrence, run the instrument, restore. Fails loudly if `old` is not
    unique: a control that silently patched nothing is a control that proves nothing."""
    original = open(path, encoding="utf-8").read()
    n = original.count(old)
    if n != 1:
        return False, [f"MUTATION NOT APPLIED: {os.path.relpath(path, ROOT)} contains "
                       f"{n} copies of the anchor, expected exactly 1. This control measured NOTHING."]
    try:
        open(path, "w", encoding="utf-8").write(original.replace(old, new))
        return instrument()
    finally:
        open(path, "w", encoding="utf-8").write(original)


# ── BASELINE ────────────────────────────────────────────────────────────────────────────────────
def baseline():
    passed, tail = sg_fixture()
    rc_scan, scan_tail = sg_scan()
    rc_go, go_tail = go_guard()
    ok = passed and rc_scan == 0 and rc_go == 0
    return ok, [f"fixture passed={passed}", f"scan exit={rc_scan} (0 = tree clean)",
                f"paramcensus exit={rc_go}"] + (tail if not passed else [])


# ── S1 — the rule's blind spot, restored ────────────────────────────────────────────────────────
def s1():
    def instrument():
        passed, tail = sg_fixture()
        return (not passed), tail
    return mutate(RULE,
                  '      - pattern-not: chi.URLParam($R, "wsID")',
                  '      - pattern-not: chi.URLParam($R, "...")',
                  instrument)


# ── S2 — a NEW workspace-ish literal in real production code ────────────────────────────────────
def s2():
    def instrument():
        rc, tail = sg_scan()
        return rc != 0, tail
    return mutate(VICTIM,
                  '\trequestID := chi.URLParam(r, "requestID")',
                  '\trequestID := chi.URLParam(r, "requestID")\n\t_ = chi.URLParam(r, "orgID")',
                  instrument)


# ── S3 — the census must EXEMPT, not just flag ──────────────────────────────────────────────────
def s3():
    def instrument():
        rc, tail = sg_scan()
        # Inverted on purpose: here GREEN is the required outcome.
        return rc == 0, ([f"scan exit={rc} (0 = correctly silent on a censused name)"] + tail)
    return mutate(VICTIM,
                  '\trequestID := chi.URLParam(r, "requestID")',
                  '\trequestID := chi.URLParam(r, "requestID")\n\t_ = chi.URLParam(r, "pageID")',
                  instrument)


# ── S4 — is the census clause doing the exempting, or is it inert? ──────────────────────────────
def s4():
    def instrument():
        rc, tail = sg_scan()
        return rc != 0, tail
    return mutate(RULE, "|spaceID|", "|", instrument)


# ── C1 — paramcensus sees an unclassified read ──────────────────────────────────────────────────
def c1():
    def instrument():
        rc, tail = go_guard()
        return rc != 0, tail
    return mutate(VICTIM,
                  '\trequestID := chi.URLParam(r, "requestID")',
                  '\trequestID := chi.URLParam(r, "requestID")\n\t_ = chi.URLParam(r, "orgID")',
                  instrument)


# ── C2 — THE DEAD EXEMPTION. No scan can see this one. ──────────────────────────────────────────
def c2():
    def instrument():
        rc_go, go_tail = go_guard()
        rc_scan, _ = sg_scan()
        # The paired scan result is the FINDING, not decoration: it must stay 0 while the Go guard
        # reds, which is why the Go guard exists at all.
        return rc_go != 0 and rc_scan == 0, go_tail + [f"and the product scan stayed exit={rc_scan} — blind to it"]
    return mutate(RULE, "regex: ^\"(pageID|", "regex: ^\"(zzzRetiredName|pageID|", instrument)


# ── C3 — the guard's own vacuity: delete the clause it parses ───────────────────────────────────
def c3():
    def instrument():
        rc, tail = go_guard()
        named = any("census clause not found" in ln for ln in tail)
        return rc != 0 and named, tail
    return mutate(RULE,
                  "                regex: ^\"(pageID|",
                  "                regexDISABLED: ^\"(pageID|",
                  instrument)


# ── C4 — wsID smuggled into the census: nobody would own the workspace ──────────────────────────
def c4():
    def instrument():
        rc, tail = go_guard()
        return rc != 0, tail
    return mutate(RULE, "regex: ^\"(pageID|", "regex: ^\"(wsID|pageID|", instrument)


# ── C5 — the two rules stop agreeing which literal is the workspace ─────────────────────────────
def c5():
    def instrument():
        rc, tail = go_guard()
        return rc != 0, tail
    return mutate(RULE,
                  '      - pattern: chi.URLParam($R, "wsID")',
                  '      - pattern: chi.URLParam($R, "workspaceID")',
                  instrument)


# ── C6 — the population boundary, and the anti-vacuity floor ────────────────────────────────────
def c6():
    def instrument():
        rc, tail = go_guard()
        named = any("censused ZERO" in ln for ln in tail)
        return rc != 0 and named, tail
    return mutate(GUARD,
                  'var productionDirs = []string{"internal", "cmd"}',
                  'var productionDirs = []string{"migrations"}',
                  instrument)


# ── C7 — the fixture's deliberate violations must stay OUT of the censused population ───────────
def c7():
    def instrument():
        rc, tail = go_guard()
        named = any("workspaceID" in ln or "tenantID" in ln for ln in tail)
        return rc != 0 and named, tail
    return mutate(GUARD,
                  'var productionDirs = []string{"internal", "cmd"}',
                  'var productionDirs = []string{"internal", "cmd", ".semgrep/tests"}',
                  instrument)


def main():
    print("W3.1 tab-8v3r — positive controls for the URL-param classification guard")
    control("BASELINE — everything green before a byte is mutated",
            "fixture passes, tree scan exits 0, paramcensus passes", baseline)
    control("S1  restore the blanket `pattern-not: chi.URLParam($R, \"...\")` (the shipped blind spot)",
            "`semgrep --test` FAILS — aliasWorkspaceRead/tenantWorkspaceRead unflagged", s1)
    control("S2  plant `chi.URLParam(r, \"orgID\")` in internal/approval/handler.go",
            "product scan exits NON-ZERO — a new unclassified name is flagged in real code", s2)
    control("S3  plant `chi.URLParam(r, \"pageID\")` in the same file",
            "product scan exits ZERO — a censused resource id is NOT noise", s3)
    control("S4  delete `spaceID` from the census regex",
            "product scan exits NON-ZERO — the census clause is what exempts, it is not inert", s4)
    control("C1  plant `chi.URLParam(r, \"orgID\")` (paramcensus, direction 1: read but not exempt)",
            "internal/paramcensus FAILS", c1)
    control("C2  add `zzzRetiredName` to the census (direction 2: exempt but never read)",
            "paramcensus FAILS while the product scan stays 0 — the dead exemption no scan can see", c2)
    control("C3  rename the metavariable-regex key so the clause vanishes",
            "paramcensus FAILS naming `census clause not found` — never passes vacuously", c3)
    control("C4  smuggle `wsID` into the census",
            "paramcensus FAILS — otherwise BOTH rules exclude it and nothing owns the workspace", c4)
    control("C5  rename the sibling rule's literal to \"workspaceID\"",
            "paramcensus FAILS — the two rules must agree which literal is the workspace", c5)
    control("C6  point the census at a directory holding no Go",
            "paramcensus FAILS naming `censused ZERO` — an empty census agrees with anything", c6)
    control("C7  add .semgrep/tests to the censused population",
            "paramcensus FAILS naming workspaceID/tenantID — the fixture's violations are OUT by "
            "directory choice, which is the boundary that is actually load-bearing", c7)

    print("\n" + "=" * 78)
    bad = [n for n, ok in results if not ok]
    for n, ok in results:
        print(f"  {'PASS' if ok else 'FAIL'}  {n}")
    print("=" * 78)
    if bad:
        print(f"\n{len(bad)} CONTROL(S) DID NOT PROVE ANYTHING — the guard they name may be inert.")
        return 1
    print("\nAll controls red as required; baseline green. Every guard here can fail.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
