#!/usr/bin/env python3
"""Positive controls for the takeover-affordance guards (tab-b2e7, W3.1).

The finding spans two runtimes, so the controls do too: Go mutations are measured against the
FULL Go suite, TSX/TS mutations against the FULL vitest run. Each names its predicted catcher
BEFORE the run; every mutation is restored in a `finally` and verified by sha256.

⚠ THERE IS NO SEPARATE CLIENT BLIND CONTROL, AND THAT IS A DELIBERATE STOP, NOT AN OMISSION.
I wrote one (blind [GONE-WHEN-GRANTED], apply T3's defect on top, expect green) and could not
read its result: vitest exited 1 with ZERO failed assertions in its own JSON report, twice, and
an instrument whose exit code and whose report disagree is not evidence about the product. The
blindness claim it was meant to establish is available DIRECTLY from T3, which needs no blinding:
under T3 the ENTIRE frontend suite reds exactly ONE test — the new [GONE-WHEN-GRANTED] — so
before this commit nothing in the client could see a Takeover button rendered in the state the
route grants. Stating the weaker measurement I actually have beats reporting the stronger one I
could not.

Run:
    DOCS_TEST_DATABASE_URL=postgres://... python3 scripts/w31-takeover-affordance-controls-b2e7.py
"""

import hashlib
import os
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = os.path.join(REPO, "internal/editsession/store.go")
GO_GUARD = os.path.join(REPO, "internal/editsession/takeoverparity_realpg_test.go")
BANNER = os.path.join(REPO, "frontend/src/components/EditingBanner.tsx")
HOOK = os.path.join(REPO, "frontend/src/hooks/useEditSession.ts")
WEB_GUARD = os.path.join(REPO, "frontend/src/components/EditingBanner.affordance.test.tsx")

PARITY_LIVE = "TestEditSessionTakeover_OnALiveForeignSession_IsIndistinguishableFromAcquire_RealPG"
PARITY_EXPIRED = "TestEditSessionTakeover_OnAnExpiredSession_IsAlsoIndistinguishableFromAcquire_RealPG"
TENANCY = "TestEditSession_HTTP_CrossTenant_RealPG"
STORE_TENANCY = "TestEditSession_CrossTenant_NoAcquireObserveTakeover_RealPG"
SHOWN = "SHOWN-WHEN-REFUSED"
GONE = "GONE-WHEN-GRANTED"
ONE_PRED = "ONE-PREDICATE"
PRE_EXISTING = "TestEditSession_TakeoverOnlyWhenExpired_RealPG"

CLAIM_WHERE = """            WHERE page_edit_sessions.holder = EXCLUDED.holder
               OR page_edit_sessions.last_heartbeat <= now() - make_interval(secs => $4)"""

RELEASE_SCOPE = """	if _, err := s.pageWorkspace(ctx, pageID, wsIDs); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM page_edit_sessions WHERE page_id = $1 AND holder = $2`,"""

BANNER_GATE = "  if (!flags.heldByOther) return null;"
HELD_BY_OTHER = "  const heldByOther = live && !!holder && holder !== memberID;"

# The two sentences [PARITY-LIVE] uses to read the outcome — what the BLIND control removes.
LIVE_ASSERTIONS_A = """	if takeover.Code != http.StatusLocked {"""
LIVE_ASSERTIONS_B = """	if holder != f.alice {"""

# The WHOLE body of [GONE-WHEN-GRANTED] — blinding one line was not enough, which is itself the
# lesson: the test has THREE independent readings of the same state (the flag, the DOM, the role
# query), and a blind control that removes one of them measures nothing.
GONE_ASSERTION = """    const flags = sessionFlags({ holder: holderName, live: false } as never, me);
    expect(flags.heldByOther).toBe(false);
    const { container } = render(<EditingBanner flags={flags} onTakeover={() => {}} />);
    expect(container.firstChild).toBeNull();
    expect(screen.queryByRole("button", { name: /take over/i })).toBeNull();"""


class C:
    def __init__(self, name, runtime, why, predicted, edits, caught=True):
        self.name, self.runtime, self.why = name, runtime, why
        self.predicted, self.edits, self.caught = predicted, edits, caught


CONTROLS = [
    C("T0 baseline", "both", "A control set whose baseline is not green measures the tree.", [], [], caught=False),
    C("T1 Takeover actually STEALS a live session (the product change, made)", "go",
      "The whole point of pinning: if the affordance is ever made real, the guard must red so the "
      "decision is in the diff rather than in a behaviour change nobody reviewed.",
      [PARITY_LIVE],
      [(STORE, CLAIM_WHERE, "            WHERE true")]),
    C("T2 claim stops honouring expiry", "go",
      "The other direction — a slot that can never be reclaimed. Without this, [PARITY-EXPIRED] "
      "could be satisfied by a route that always refuses.",
      [PARITY_EXPIRED],
      [(STORE, CLAIM_WHERE, "            WHERE page_edit_sessions.holder = EXCLUDED.holder")]),
    C("T3 the banner renders regardless of heldByOther", "web",
      "The client half of the same predicate: a button that appears in the expired state would "
      "make the affordance reachable, and that is a change, not a tidy-up.",
      [GONE],
      [(BANNER, BANNER_GATE, "  if (false) return null;")]),
    C("T4 heldByOther drops its `live` conjunct", "web",
      "The single field the two predicates share. Drop it and an expired session still reads as "
      "'someone else is editing' — the state the route grants and the banner claims it does not.",
      [GONE, ONE_PRED],
      [(HOOK, HELD_BY_OTHER, "  const heldByOther = !!holder && holder !== memberID;")]),
    C("T5 the edit-session Release loses its tenancy scope", "go",
      "The must-stay-green companion: the new parity guards must NOT red for a tenancy break, or "
      "CAUGHT means nothing more specific than 'something is wrong'. ⚠ PREDICTION CORRECTED AFTER "
      "MEASURING: I first named the HTTP test. It stays GREEN — the route enforcer 404s the "
      "foreign caller before the handler, so the HTTP case measures the ENFORCER and the STORE's "
      "own gate is measured by the store-level test. Same layer confusion as the block-PATCH "
      "campaign's C5, one package over; a control is the only thing that separates them.",
      [STORE_TENANCY],
      [(STORE, RELEASE_SCOPE, """	_, err := s.pool.Exec(ctx,
		`DELETE FROM page_edit_sessions WHERE page_id = $1 AND holder = $2`,""")]),
    C("T6 the Go guard BLINDED, with T1 on top — A MEASURED NEGATIVE RESULT", "go",
      "I predicted GREEN (the usual measured-blindness control) and the suite went RED. That is "
      "the honest finding about my own instrument: the BACKEND fact was ALREADY pinned — "
      "TestEditSession_TakeoverOnlyWhenExpired_RealPG and five siblings red on a stealing "
      "takeover — so the new Go cases are NOT load-bearing for 'a live session is not stolen'. "
      "What they add is the byte-identical PARITY of the two doors, which nothing else asserts. "
      "The unguarded half of this finding is the CLIENT one (T7).",
      [PRE_EXISTING],
      [(GO_GUARD, LIVE_ASSERTIONS_A, "\tif false {"),
       (GO_GUARD, LIVE_ASSERTIONS_B, "\tif false {"),
       (STORE, CLAIM_WHERE, "            WHERE true")]),
]


def sha(p):
    return hashlib.sha256(open(p, "rb").read()).hexdigest()


def run_go():
    p = subprocess.run(["go", "test", "-count=1", "-json", "./..."], cwd=REPO,
                       capture_output=True, text=True)
    failed = {l.split('"Test":"', 1)[1].split('"', 1)[0]
              for l in p.stdout.splitlines() if '"Action":"fail"' in l and '"Test":"' in l}
    return p.returncode, failed, p.stderr


def run_web():
    fe = os.path.join(REPO, "frontend")
    p = subprocess.run(["npx", "vitest", "run", "--reporter=json", "--outputFile=/tmp/vt-b2e7.json"],
                       cwd=fe, capture_output=True, text=True)
    failed = set()
    try:
        import json
        rep = json.load(open("/tmp/vt-b2e7.json"))
        for tr in rep.get("testResults", []):
            for a in tr.get("assertionResults", []):
                if a.get("status") == "failed":
                    failed.add(a.get("title", ""))
    except Exception as e:  # a run that produced no report is a failed measurement, not a pass
        return 3, {f"<no vitest report: {e}>"}, p.stderr
    return p.returncode, failed, p.stderr


def catchers(failed, predicted):
    """A predicted catcher matches by substring — the web tests are named by their [TAG]."""
    return [p for p in predicted if not any(p in f or p == f for f in failed)]


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL unset — the real-PG half would FAIL, not skip.", file=sys.stderr)
        return 2
    paths = [STORE, GO_GUARD, BANNER, HOOK, WEB_GUARD]
    originals = {p: open(p, "rb").read() for p in paths}
    digests = {p: sha(p) for p in paths}
    results = []
    try:
        for c in CONTROLS:
            print(f"\n=== {c.name}  [{c.runtime}]")
            print(f"    why: {c.why}")
            print(f"    PREDICTION: {'CAUGHT by ' + ', '.join(c.predicted) if c.caught else 'NOT CAUGHT — green'}")
            for path, old, new in c.edits:
                src = open(path).read()
                if src.count(old) != 1:
                    print(f"    ANCHOR MISS in {os.path.basename(path)} ({src.count(old)} matches) — harness stale")
                    return 3
                open(path, "w").write(src.replace(old, new))
            failed, code = set(), 0
            if c.runtime in ("go", "both"):
                gc, gf, gerr = run_go()
                if "build failed" in gerr:
                    print(f"    BUILD FAILED (go): {gerr[:400]}")
                    return 3
                code |= gc
                failed |= gf
            if c.runtime in ("web", "both"):
                wc, wf, _ = run_web()
                code |= wc
                failed |= wf
            shown = sorted(f for f in failed if "EditSession" in f or "-WHEN-" in f or "PREDICATE" in f)
            print(f"    exit={code} failed={shown if shown else '(none)'}")
            missing = catchers(failed, c.predicted)
            ok = (code != 0 and not missing) if c.caught else (code == 0)
            print(f"    -> {'MATCHES' if ok else 'DIVERGES'}" + (f" (missing: {missing})" if missing else ""))
            results.append((c.name, ok))
            for p, b in originals.items():
                open(p, "wb").write(b)
    finally:
        for p, b in originals.items():
            open(p, "wb").write(b)
        bad = [p for p in digests if sha(p) != digests[p]]
        print("\nRESTORE: " + ("VERIFIED (sha256 unchanged)" if not bad else f"FAILED for {bad}"))
        if bad:
            return 4
    print("\n=== SUMMARY")
    for n, ok in results:
        print(f"  {'PASS' if ok else 'FAIL'}  {n}")
    return 0 if all(ok for _, ok in results) else 1


if __name__ == "__main__":
    sys.exit(main())
