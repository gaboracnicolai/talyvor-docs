#!/usr/bin/env python3
"""Positive controls for internal/suppressionguard/authz_order_test.go (W3.1, tab-7f3b).

WHY THIS FILE IS THE WHOLE ARGUMENT. The guard it controls PASSED ON ITS FIRST RUN, because all
twenty-one premises in the tree were re-measured true before it was written. A guard that has only
ever been green over a population it already knew was correct is indistinguishable, from the
outside, from a guard that asserts nothing — and this repo has shipped three of those, each caught
only by a control. So every rule the guard states is mutated here, each control NAMES ITS PREDICTED
CATCHER BEFORE IT RUNS, and every mutation is run over the FULL Go suite on real Postgres so that
"only the new file reds" is measured rather than assumed.

THE LOAD-BEARING ONE IS A1. It does not delete an authorize call — deleting one is caught by
presence, which a far weaker guard would also catch. It MOVES the call BELOW the store op it
protects, leaving it present, spelled the same, in the same function. Nothing about the suppression
becomes false to a reader; only the ORDER its reason asserts does. If the guard cannot see A1 it is
not checking what these twenty-one suppressions actually claim.

Every mutation is restored in a `finally` and the restoration is verified by sha256 against the
bytes read before the run.
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DSN = "postgres://postgres:postgres@localhost:55437/postgres?sslmode=disable"
GUARD_PKG = "github.com/talyvor/docs/internal/suppressionguard"

SPACE = "internal/space/handler.go"
AI = "internal/ai/handler.go"
PAGE = "internal/page/handler.go"
FRESH = "internal/freshness/handler.go"
RATE = "internal/ratelimit/middleware.go"

SPACE_AUTHZ_BLOCK = """	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "not a member of this workspace")
		return
	}
	out, err := h.store.List(r.Context(), wsID)
"""

# A1: the store op runs FIRST; the gate is still present, still named, still refuses.
SPACE_ORDER_SWAPPED = """	out, err := h.store.List(r.Context(), wsID)
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "not a member of this workspace")
		return
	}
"""

# A7: positionally first, and reachable only on some requests. Predicted NOT CAUGHT.
SPACE_GATE_CONDITIONAL = """	if r.URL.Query().Get("skipauthz") == "" {
		if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
			writeErr(w, http.StatusForbidden, "FORBIDDEN", "not a member of this workspace")
			return
		}
	}
	out, err := h.store.List(r.Context(), wsID)
"""

AI_WRITE_GATE = """	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	var in struct {
		Prompt  string `json:"prompt"`
"""
AI_WRITE_GATE_GONE = """	var in struct {
		Prompt  string `json:"prompt"`
"""

PAGE_STALE_REFUSAL = """	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "not a member of this workspace")
		return
	}
	rows, err := h.store.GetStalePages(r.Context(), wsID)
"""
PAGE_STALE_REFUSAL_DROPPED = """	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "not a member of this workspace")
	}
	rows, err := h.store.GetStalePages(r.Context(), wsID)
"""

NEW_SUPPRESSION = (
    "\t// nosemgrep" + ": docs-no-url-param-workspace-scope "
    "-- authorized on the next line by AuthorizeWorkspace, before any store op\n"
)


def sh(cmd, env=None):
    e = dict(os.environ)
    e["DOCS_TEST_DATABASE_URL"] = DSN
    if env:
        e.update(env)
    return subprocess.run(cmd, shell=True, cwd=REPO, capture_output=True, text=True, env=e)


def digest(path):
    with open(os.path.join(REPO, path), "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def read(path):
    with open(os.path.join(REPO, path), "r") as f:
        return f.read()


def write(path, s):
    with open(os.path.join(REPO, path), "w") as f:
        f.write(s)


def full_suite():
    """Run the entire Go suite. Returns (ok, set of failing packages, raw tail)."""
    r = sh("go test -timeout 600s -count=1 ./...")
    failing = set()
    for line in (r.stdout + r.stderr).splitlines():
        m = re.match(r"^(?:FAIL|---\s+FAIL)\s+(\S+)", line)
        if m and m.group(1).startswith("github.com/"):
            failing.add(m.group(1))
    return r.returncode == 0, failing, (r.stdout + r.stderr)[-1500:]


def guard_only():
    """Run just the new guard, for a fast unambiguous verdict on it."""
    r = sh(f"go test -count=1 -run TestAuthzOrderPremises ./internal/suppressionguard/")
    return r.returncode == 0, (r.stdout + r.stderr)


RESULTS = []


def control(name, predicted, mutations, expect_guard_red, full=True):
    """mutations: list of (path, old, new). Applied together, restored in a finally."""
    print(f"\n=== {name} — PREDICTED: {predicted}", flush=True)
    originals = {}
    digests = {}
    try:
        for path, old, new in mutations:
            if path not in originals:
                originals[path] = read(path)
                digests[path] = digest(path)
            s = read(path)
            if old is not None and old not in s:
                print(f"  !! anchor missing in {path} — control VOID, not a pass")
                RESULTS.append((name, "VOID", "anchor missing"))
                return
            s = s.replace(old, new, 1) if old is not None else new
            write(path, s)

        g_ok, g_out = guard_only()
        verdict = "CAUGHT" if not g_ok else "NOT CAUGHT"
        line = ""
        for l in g_out.splitlines():
            if any(k in l for k in ("ORDER —", "GATE —", "REFUSAL —", "GATE-ONLY —", "not in ORDER_PREMISES", "were not found in the tree", "than pinned")):
                line = l.strip()[:170]
                break
        ok_expect = (not g_ok) == expect_guard_red
        others = set()
        if full:
            s_ok, failing, tail = full_suite()
            others = {p for p in failing if p != GUARD_PKG}
        print(f"  guard: {verdict}  {'(as predicted)' if ok_expect else '(PREDICTION WRONG)'}")
        if line:
            print(f"  msg:   {line}")
        if full:
            print(f"  other packages red: {sorted(others) if others else 'NONE'}")
        RESULTS.append((name, verdict + ("" if ok_expect else "  ⚠PREDICTION WRONG"),
                        f"others={sorted(others) if others else 'none'}"))
    finally:
        for path, s in originals.items():
            write(path, s)
            after = digest(path)
            assert after == digests[path], f"RESTORE FAILED for {path}: {after} != {digests[path]}"
        print(f"  restored + sha256-verified: {', '.join(originals)}")


def main():
    print("A0 — baseline, no mutation. Predicted: guard GREEN, suite GREEN.")
    g_ok, _ = guard_only()
    s_ok, failing, tail = full_suite()
    print(f"  guard green={g_ok}  suite green={s_ok}  failing={sorted(failing)}")
    if not (g_ok and s_ok):
        print("  BASELINE IS NOT GREEN — every verdict below would be meaningless.")
        print(tail)
        return 1
    RESULTS.append(("A0 baseline", "GREEN", "suite green"))

    # ── A1: THE ORDER CONTROL. The gate stays; only its position moves.
    control("A1 ORDER — space.List authorizes AFTER the store read",
            "CAUGHT by the new guard's ORDER rule; nothing else in the repo can see it "
            "(the route still answers 403, so no HTTP assertion changes)",
            [(SPACE, SPACE_AUTHZ_BLOCK, SPACE_ORDER_SWAPPED)], expect_guard_red=True)

    # ── A2: presence.
    control("A2 GATE — ai.Write's AuthorizeWorkspace deleted",
            "CAUGHT by GATE, and expected to red real-PG authz tests too",
            [(AI, AI_WRITE_GATE, AI_WRITE_GATE_GONE)], expect_guard_red=True)

    # ── A3: the verdict is dropped.
    control("A3 REFUSAL — page.Stale calls the gate and does not return on failure",
            "CAUGHT by REFUSAL",
            [(PAGE, PAGE_STALE_REFUSAL, PAGE_STALE_REFUSAL_DROPPED)], expect_guard_red=True)

    # ── A4: a new premise joins in an unpinned function.
    control("A4 CENSUS-JOIN — a new authorization suppression in an unpinned function",
            "CAUGHT as unpinned",
            [(SPACE, "func (h *Handler) Get(", NEW_SUPPRESSION + "func (h *Handler) Get(")],
            expect_guard_red=True)

    # ── A5: a SECOND premise joins a function that already has one.
    control("A5 CENSUS-COUNT — a second authorization suppression inside space.List",
            "CAUGHT by the per-key count rule (the hole a file#func key would otherwise leave)",
            [(SPACE, "\tout, err := h.store.List(r.Context(), wsID)",
              NEW_SUPPRESSION + "\tout, err := h.store.List(r.Context(), wsID)")],
            expect_guard_red=True)

    # ── A6: a premise leaves the census by rewording.
    # ⚠ A6's FIRST FORM WAS A BAD CONTROL AND IS KEPT AS A6a BECAUSE THE CORRECTION IS THE
    # FINDING. It reworded "authorized below by AuthorizeWorkspace" to "GATED below by
    # AuthorizeWorkspace" and predicted the row would leave the census. It did not, and the guard
    # was right: the predicate is /(?i)authoriz/ and the GATE'S OWN IDENTIFIER — AuthorizeWorkspace
    # — contains it. A reason therefore cannot leave this census while it still names its
    # authorizer, which is a stronger property than the one I set out to test, and it is only
    # visible because the prediction was written down before the run.
    control("A6a CENSUS-DROP (weak reword) — 'authorized below by' → 'gated below by'",
            "NOT CAUGHT — the retained identifier AuthorizeWorkspace still matches the predicate",
            [(FRESH, "-- authorized below by AuthorizeWorkspace",
              "-- gated below by AuthorizeWorkspace")], expect_guard_red=False)

    control("A6b CENSUS-DROP — freshness.Workspace's reason reworded past the predicate entirely",
            "CAUGHT as a pinned row not found in the tree",
            [(FRESH, "-- authorized below by AuthorizeWorkspace, before the stale report is built",
              "-- gated below by the membership check, before the stale report is built")],
            expect_guard_red=True)

    # ── A7: the measured blindness. Recorded, not hidden.
    control("A7 BLINDNESS — space.List's gate made conditional (?skipauthz bypasses it)",
            "NOT CAUGHT — ORDER is source position, not dominance. This is the residue the "
            "guard's header names, and it is measured rather than assumed",
            [(SPACE, SPACE_AUTHZ_BLOCK, SPACE_GATE_CONDITIONAL)], expect_guard_red=False)

    # ── A8: the gate-only claim.
    # ⚠ A8's FIRST FORM DID NOT COMPILE, WHICH IS A VOID CONTROL AND NOT A CATCH. Swapping
    # l.Allow(m.WorkspaceID) for l.Allow(wsID) leaves `m` declared and unused, so eleven packages
    # "red" with a build failure rather than a finding. This guard parses source and would have
    # scored CAUGHT either way — a mutation that cannot build proves nothing about the suite it
    # was supposed to be measured against, so `m` is kept live here.
    control("A8 GATE-ONLY — the rate limiter keys on the RAW param instead of the Membership",
            "CAUGHT by GATE-ONLY, and NOT caught elsewhere (AuthorizeWorkspace returns the same "
            "id it was handed, so the limiter behaves identically and no assertion moves)",
            [(RATE, "if !l.Allow(m.WorkspaceID) { // the VERIFIED id, not wsID",
              "_ = m.WorkspaceID\n\t\t\tif !l.Allow(wsID) { // MUTATION A8")],
            expect_guard_red=True)

    # ── A9: must-stay-green companion.
    control("A9 MUST-STAY-GREEN — space.List refuses with 404 instead of 403",
            "NOT CAUGHT here (this guard says nothing about status codes) and RED elsewhere, "
            "which is what keeps CAUGHT from being a catch-all",
            [(SPACE, 'writeErr(w, http.StatusForbidden, "FORBIDDEN", "not a member of this workspace")',
              'writeErr(w, http.StatusNotFound, "FORBIDDEN", "not a member of this workspace")')],
            expect_guard_red=False)

    print("\n\n================ SUMMARY ================")
    for n, v, extra in RESULTS:
        print(f"  {v:<28} {n}\n{'':<30}   {extra}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
