#!/usr/bin/env python3
"""Positive controls for the widened docs-no-ambiguous-actor-helpers rule (W3.1, tab-8f3c).

THE FINDING THIS EXISTS FOR. `.semgrep/body-supplied-authority.yml`'s rule D calls itself
"The ROOT primitive itself ... this one is exact" and its message names authz.SingleMemberID
and authz.SingleWorkspace as the thing that "created this whole bug class". Its patterns bound
neither. Both roots are exported from internal/authz and both are one `v, _ :=` away from being
the banned wrapper, so the ban was a ban on the shorter spelling.

Every control below runs the PINNED CI semgrep image (semgrep/semgrep:1.165.0) — a local
semgrep is a different instrument from the one that gates a merge. Each mutation is written,
measured, and restored in a `finally`, with a sha256 of every touched file compared before and
after so a control cannot leave a tree behind.

A1/A2/A6 carry the measurement that makes this a defect rather than a tidy-up: A6 is A1 with
the two new patterns removed — main's rule, verbatim — and it must come back GREEN.

Usage:  python3 scripts/w31-ambiguousactor-controls-8f3c.py [--only A1,A6]
"""

import argparse
import hashlib
import os
import re
import shutil
import subprocess
import sys
import tempfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RULES = os.path.join(REPO, ".semgrep", "body-supplied-authority.yml")
FIXTURE = os.path.join(REPO, ".semgrep", "tests", "body-supplied-authority.go")
CHANGELOG = os.path.join(REPO, "internal", "changelog", "handler.go")
IMAGE = "semgrep/semgrep:1.165.0"
RULE_D = "docs-no-ambiguous-actor-helpers"

# The two patterns this merge adds. A6 removes exactly these and nothing else.
NEW_PATTERNS = """          - pattern: authz.SingleMemberID(...)
          - pattern: authz.SingleWorkspace(...)
"""

# The shipped, correct attribution block in changelog.Create.
CORRECT_BLOCK = """	ws, ok := permission.WorkspaceFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the workspace for this page")
		return
	}
	actor, ok := permission.ActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the acting member for this page")
		return
	}
	in.WorkspaceID = ws
	in.CreatedBy = actor"""

# A1 — the actor resolved from the ROOT primitive with the ok discarded. WorkspaceFromContext is
# deliberately LEFT IN PLACE: rule A's sanitizer only asks that the handler consult an approved
# resolver at all, so keeping it is what makes this invisible to every rule but D.
A1_BLOCK = """	ws, ok := permission.WorkspaceFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the workspace for this page")
		return
	}
	actor, _ := authz.SingleMemberID(r.Context())
	in.WorkspaceID = ws
	in.CreatedBy = actor"""

# ⚠ A2 AND A3 ARE CUT AGAINST changelog.Feed, NOT Create, AND THE FIRST VERSION OF THIS HARNESS
# IS WHY. Both were originally the mirror of A1 at the Create site, predicted CAUGHT BY D ALONE,
# and both came back CAUGHT BY D *AND* rule A — because replacing the workspace resolution is
# exactly what empties rule A's sanitizer ("this handler never consulted an approved resolver"),
# while replacing the ACTOR (A1) leaves it satisfied. The prediction was wrong in the direction of
# under-listing, and a control caught by two rules cannot say which one saw it.
#
# Feed separates them by construction: it is a workspace-level GET that decodes NO request body,
# so rules A, B, C and C' are all structurally inapplicable and only D can speak. It is also the
# more honest site for this defect — `authz.SingleWorkspace` is documented as "the Docs common
# case (one workspace per instance)", and a workspace-level route with no resource middleware is
# precisely where that sentence is tempting.
FEED_CORRECT = """	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized on the next line by AuthorizeWorkspace, before any store read
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	entries, err := h.store.GetPublicFeed(r.Context(), []string{wsID}, 50)"""

# A2 — the tenancy root with the ok DISCARDED, on a route where nothing else can see it.
A2_BLOCK = """	ws, _ := authz.SingleWorkspace(r.Context())
	entries, err := h.store.GetPublicFeed(r.Context(), []string{ws}, 50)"""

# A3 — the ok HONOURED. The shape a careful author writes, and the one a future narrowing would
# carve out first. Still a finding: it 404s a legitimate multi-workspace member on a feed their
# membership entitles them to read.
A3_BLOCK = """	ws, ok := authz.SingleWorkspace(r.Context())
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	entries, err := h.store.GetPublicFeed(r.Context(), []string{ws}, 50)"""

# A4 — the WRAPPER, i.e. the half the rule already caught before this merge. Must still red, or
# the widening traded one blindness for another.
A4_BLOCK = """	ws, ok := permission.WorkspaceFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the workspace for this page")
		return
	}
	actor := authz.ActorOrEmpty(r.Context())
	in.WorkspaceID = ws
	in.CreatedBy = actor"""

# A5 — MUST STAY GREEN. A different correct shape from the shipped one: the workspace-level
# reference (authorize the claimed workspace, take the actor off the returned Membership), with
# no permission.* resolver anywhere. If this reds, "CAUGHT" means "mentions authz" and the three
# findings above are explained by nothing.
A5_BLOCK = """	m, ok := authz.AuthorizeWorkspace(r.Context(), in.WorkspaceID)
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the workspace for this page")
		return
	}
	in.WorkspaceID = m.WorkspaceID
	in.CreatedBy = m.MemberID"""


def sha(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def run(cmd, **kw):
    return subprocess.run(cmd, capture_output=True, text=True, **kw)


def scan():
    """Product scan with the pinned image. Returns (exit_code, findings_for_rule_D, raw)."""
    p = run(["docker", "run", "--rm", "-v", f"{REPO}:/src", "-w", "/src", IMAGE,
             "semgrep", "--config", ".semgrep/", "--error", "--quiet"])
    raw = p.stdout + p.stderr
    hits = sorted(set(re.findall(r"semgrep\.(docs-[a-z0-9-]+)", raw)))
    return p.returncode, hits, raw


def rule_test():
    """`semgrep --test` exactly as CI runs it: rules and fixtures copied into one temp dir."""
    with tempfile.TemporaryDirectory() as td:
        for src in [RULES, os.path.join(REPO, ".semgrep", "operate-by-id-tenancy.yml"),
                    FIXTURE, os.path.join(REPO, ".semgrep", "tests", "operate-by-id-tenancy.go")]:
            shutil.copy(src, td)
        p = run(["docker", "run", "--rm", "-v", f"{td}:/rt", IMAGE, "semgrep", "--test", "/rt"])
        return p.returncode, p.stdout + p.stderr


def gofmt_clean():
    p = run(["gofmt", "-l", "."], cwd=REPO)
    return p.stdout.strip() == "", p.stdout.strip()


def go_build():
    p = run(["go", "build", "./..."], cwd=REPO)
    return p.returncode == 0, (p.stdout + p.stderr)[-800:]


def scope_script():
    p = run(["python3", "scripts/check-semgrep-rule-scope.py"], cwd=REPO)
    return p.returncode == 0, (p.stdout + p.stderr).strip()[-400:]


def patch(path, old, new):
    with open(path) as fh:
        s = fh.read()
    if old not in s:
        raise SystemExit(f"ANCHOR NOT FOUND in {path} — the control is measuring nothing, refusing to run")
    with open(path, "w") as fh:
        fh.write(s.replace(old, new, 1))


CONTROLS = {}


def control(name, predicted):
    def deco(fn):
        CONTROLS[name] = (predicted, fn)
        return fn
    return deco


# ── the controls ────────────────────────────────────────────────────────────────────────────

@control("A0", "PRISTINE: product scan exit 0 / no findings; --test all pass; scope ok")
def a0():
    code, hits, raw = scan()
    ok_test, out = rule_test()
    ok_scope, scope_out = scope_script()
    passed = code == 0 and not hits and ok_test == 0 and ok_scope
    return passed, f"scan exit={code} hits={hits or 'none'} | --test exit={ok_test} " \
                   f"({'all passed' if ok_test == 0 else 'FAILED'}) | scope={'ok' if ok_scope else scope_out}"


@control("A1", f"THE DEFECT (root actor, ok discarded) -> CAUGHT by {RULE_D} ALONE")
def a1():
    patch(CHANGELOG, CORRECT_BLOCK, A1_BLOCK)
    fmt_ok, fmt_out = gofmt_clean()
    build_ok, build_out = go_build()
    code, hits, _ = scan()
    passed = build_ok and fmt_ok and code != 0 and hits == [RULE_D]
    return passed, f"gofmt={'clean' if fmt_ok else fmt_out} build={'ok' if build_ok else build_out} " \
                   f"scan exit={code} hits={hits or 'NONE'}"


@control("A2", f"tenancy root, ok discarded, on a body-less route -> CAUGHT by {RULE_D} ALONE")
def a2():
    patch(CHANGELOG, FEED_CORRECT, A2_BLOCK)
    build_ok, build_out = go_build()
    code, hits, _ = scan()
    passed = build_ok and code != 0 and hits == [RULE_D]
    return passed, f"build={'ok' if build_ok else build_out} scan exit={code} hits={hits or 'NONE'}"


@control("A3", f"the ok HONOURED -> STILL CAUGHT by {RULE_D} ALONE (the ban is outright)")
def a3():
    patch(CHANGELOG, FEED_CORRECT, A3_BLOCK)
    build_ok, build_out = go_build()
    code, hits, _ = scan()
    passed = build_ok and code != 0 and hits == [RULE_D]
    return passed, f"build={'ok' if build_ok else build_out} scan exit={code} hits={hits or 'NONE'}"


@control("A4", f"the WRAPPER (authz.ActorOrEmpty) -> still CAUGHT by {RULE_D} after the widening")
def a4():
    patch(CHANGELOG, CORRECT_BLOCK, A4_BLOCK)
    build_ok, build_out = go_build()
    code, hits, _ = scan()
    passed = build_ok and code != 0 and hits == [RULE_D]
    return passed, f"build={'ok' if build_ok else build_out} scan exit={code} hits={hits or 'NONE'}"


@control("A5", "MUST STAY GREEN: authz.AuthorizeWorkspace + Membership.MemberID -> NOT CAUGHT")
def a5():
    patch(CHANGELOG, CORRECT_BLOCK, A5_BLOCK)
    build_ok, build_out = go_build()
    code, hits, _ = scan()
    passed = build_ok and code == 0 and not hits
    return passed, f"build={'ok' if build_ok else build_out} scan exit={code} hits={hits or 'none'}"


@control("A6", "THE BLINDNESS: A1 armed + the two new patterns removed -> NOT CAUGHT (main's state)")
def a6():
    patch(CHANGELOG, CORRECT_BLOCK, A1_BLOCK)
    patch(RULES, NEW_PATTERNS, "")
    build_ok, build_out = go_build()
    code, hits, _ = scan()
    passed = build_ok and code == 0 and not hits
    return passed, f"build={'ok' if build_ok else build_out} scan exit={code} hits={hits or 'NONE — the defect is invisible'}"


@control("A7", "THE FIXTURE IS LOAD-BEARING: patterns removed -> `semgrep --test` FAILS on the new cases")
def a7():
    patch(RULES, NEW_PATTERNS, "")
    code, out = rule_test()
    # The three new expect-a-finding cases must be the ones that break.
    named = [n for n in ("rootActorOkDiscarded", "rootWorkspaceOkDiscarded", "singleHelperOkHonoured")
             if n in out]
    tail = [ln for ln in out.splitlines() if "passed" in ln or "failed" in ln or "✖" in ln]
    passed = code != 0
    return passed, f"--test exit={code} (0 would mean the fixture cases pass without the rule) | " \
                   f"lines={tail[-3:]} | named-in-output={named or 'none (semgrep reports counts, not case names)'}"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--only", default="")
    args = ap.parse_args()
    wanted = [c.strip() for c in args.only.split(",") if c.strip()] or list(CONTROLS)

    watched = [RULES, FIXTURE, CHANGELOG]
    before = {p: sha(p) for p in watched}
    backups = {p: open(p).read() for p in watched}

    results = []
    for name in wanted:
        predicted, fn = CONTROLS[name]
        print(f"\n=== {name} — PREDICTED: {predicted}", flush=True)
        try:
            passed, detail = fn()
        finally:
            for p, text in backups.items():
                with open(p, "w") as fh:
                    fh.write(text)
            after = {p: sha(p) for p in watched}
            drift = [p for p in watched if before[p] != after[p]]
            if drift:
                raise SystemExit(f"RESTORE FAILED, tree left dirty: {drift}")
        verdict = "AS PREDICTED" if passed else "*** NOT AS PREDICTED ***"
        print(f"    {verdict}: {detail}", flush=True)
        results.append((name, passed, predicted, detail))

    print("\n" + "=" * 96)
    bad = [r for r in results if not r[1]]
    for name, passed, predicted, detail in results:
        print(f"  {name}  {'ok  ' if passed else 'FAIL'}  {predicted}")
    print(f"\n{len(results) - len(bad)}/{len(results)} as predicted; every touched file sha256-identical after restore.")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
