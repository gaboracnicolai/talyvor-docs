#!/usr/bin/env python3
"""Every control script's anchors must still resolve in the file they name.

WHY THIS EXISTS, AND WHY THE OBVIOUS VERSION OF IT DOES NOT WORK
===============================================================
A mutate-and-restore control anchors on a literal that was unique WHEN WRITTEN.
The day the code it guards is edited again, the anchor stops resolving and the
control can no longer be APPLIED. It does not go quiet: every one of these
scripts asserts its anchor counts before writing and says so loudly. But
NOTHING IN CI RUNS THEM, so the loudness reaches nobody, and the only signal a
human ever sees is a fraction (`6/7`) that reads like a score rather than like
an instrument reporting it could not run.

W6.43 tried the obvious census — pull string literals out of `.count(`/`.replace(`
first-arguments with `ast` — and it reported ZERO because it resolved only 15
anchor-shaped literals across 79 scripts. A detector that examines 15 candidates
and reports a clean population is exactly the defect this repo keeps finding.

This one does NOT extract literals. It EXECUTES each script's module-level
definitions in a fresh namespace — imports, assignments, `def`, `class`, and
nothing else — then reads the resulting VALUES. `if __name__` blocks and bare
top-level calls are dropped, so nothing runs and nothing is written. Because
Python does the resolving, f-strings, constant composition and tuple-building
all come out exactly as the script itself sees them.

WHAT IT MEASURED WHEN IT WAS WRITTEN (main ef199ef, 80 scripts)
==============================================================
28 scripts yielded an anchor table · 204 anchors checked · 10 DEAD ARMS IN 5
SCRIPTS (nine remain; see W6.43a for what repairing one costs), none of which any instrument in this repo could see:

  w31-ask-error-controls.py        3 arms  anchor died 2026-08-10, 163 commits ago  ── REPAIRED, W6.43a
  w31-search-access-controls.py    2 arms  anchor died 2026-08-10, 168 commits ago  ── REPAIRED, W6.43a
  w31-search-offset-controls.py    2 arms  anchor died 2026-08-13,  99 commits ago  ── REPAIRED, W6.43a
  w31-version-title-controls.py    2 arms  anchor died 2026-08-13, 123 commits ago  ── REPAIRED, W6.43a
  w31-askgrounding-controls.py     1 arm   anchor MULTIPLIED 2026-08-11  ── REPAIRED, W6.43a

⚠ THE MECHANISM IS THE SAME IN ALL FIVE AND IT IS WORTH STATING, because it
predicts where the next one comes from: EVERY ONE WAS KILLED BY THE NEXT FIX TO
THE FUNCTION IT GUARDS — one day, same day, same day, three days, three days
after the control was written. A control anchored on a line INSIDE the seam it
watches is killed by precisely the event that is most likely to happen next,
because a function that just needed one fix tends to need another. Two of the
five were dead the same day they were written.

⚠⚠ AND THE GUARDS THEMSELVES ARE ALL ALIVE. All five of w31-version-title's
named targets still exist as tests. What died is the CONTROL — the thing that
proves the guard can still catch — not the guard. That is why the entries below
are not a bug list: re-pointing an anchor at moved code produces a control that
passes FOR A NEW REASON unless somebody reads the guard first and re-proves the
must-red. That work is per-script and is filed separately.

WHAT THIS PROVES AND WHAT IT DOES NOT
=====================================
It proves each control can still be APPLIED. It does NOT prove it still CATCHES:
an arm whose anchor resolves but whose mutation has become inert passes here.
Only running the campaign proves that, and the campaign needs a real Postgres
and a `go test` per arm, which is why it cannot live in this step.

POPULATION BOUNDARY — THE NUMBER THAT KEEPS THIS HONEST
=======================================================
52 of the 80 scripts are NOT reached, and that is printed on every run rather
than buried. They are not anchorless: they keep their anchors as standalone
module-level constants (`WPI_ANCHOR`, `ANCHOR_COST`, `ORIGINAL_SKIP`) while the
anchor-to-FILE binding happens inside a function or a class instance, so there
is no row for this detector to read. Measured over those 52: 14 declare exactly
one module-level path constant (an unambiguous target), 29 declare more than one
(a weaker "occurs in one of its declared targets" claim), 21 declare none. A
second detector over them is real work with a real false-positive cost and is
filed, not smuggled in here.
"""
import argparse
import ast
import contextlib
import hashlib
import io
import os
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SCRIPTS = os.path.join(REPO, "scripts")

# Source extensions a row's first element must end in to be TREATED AS A TARGET
# PATH AT ALL. Shape, not existence — see TARGET-MISSING below.
SOURCE_SUFFIXES = (".go", ".ts", ".tsx", ".js", ".jsx", ".sql", ".yaml", ".yml",
                   ".mod", ".sum", ".py", ".sh", ".json", ".css", ".html", ".md")

# ── FLOORS, ONE PER DETECTOR STAGE ───────────────────────────────────────────
# ⚠ TWO FLOORS, NOT ONE OVER THE UNION. A single floor on "anchors checked" is
# satisfied by one stage alone: blind the module-level exec so it reaches half
# the scripts, and a table that grew elsewhere holds the total up while coverage
# silently halved. talyvor-suite's W6.41a shipped exactly that trap and it took a
# dedicated control to find it. Stage 1 is "the exec reached the script"; stage 2
# is "a row was harvested from it". Each gets its own floor and its own control.
SCRIPTS_FLOOR = 28
ANCHORS_FLOOR = 204

# ── ROWS A HUMAN HAS READ AND CLASSIFIED AS NOT ANCHORS ──────────────────────
# ⚠ THIS LIST EXISTS BECAUSE THE OBVIOUS ALTERNATIVE IS A RULE THAT CANNOT FAIL.
# The tempting way to drop a false positive is "a literal that resolves 0 times
# was probably never an anchor" — which defines away every true finding this
# script exists for. So a non-anchor row is named here, by content hash, with
# the reason a human found by reading it. R3 fails if an entry stops appearing,
# so the list cannot quietly rot into decoration.
#
# Keyed (script, sha256(anchor)[:12]).
NOT_ANCHORS = {
    # RESTORES = (label, path, commit-of-the-fix, what the statement looked like
    # BEFORE it). Column 2 is a git sha and column 3 is prose, so the row is
    # provenance metadata that merely happens to carry a path. Read at ef199ef.
    ("w31-classguard-blindness.py", "988b946ede1e"): "RESTORES row R1-changelog: col 2 is a commit sha, col 3 is prose",
    ("w31-classguard-blindness.py", "6d0ec69ff2ec"): "RESTORES row R2-permission: col 2 is a commit sha, col 3 is prose",
    ("w31-classguard-blindness.py", "69fbdf401e92"): "RESTORES row R3-database: col 2 is a commit sha, col 3 is prose",
}

# ── ARMS KNOWN DEAD, EACH WITH WHAT MOVED AND WHEN ───────────────────────────
# ⚠ THIS LIST MAY ONLY SHRINK. R2 fails on an entry whose anchor resolves again,
# so a repair cannot land without deleting its line; R1 fails on any dead arm
# NOT listed, so the next one reddens the build on the commit that kills it.
#
# ⚠⚠ THESE ARE NOT BUGS TO BE PATCHED BY RE-POINTING THE ANCHOR. Every guard
# these controls exercise is still alive — all five tests w31-version-title
# names still exist. What died is the proof that the guard can still CATCH.
# Re-pointing an anchor at moved code yields a control that passes for a NEW
# REASON unless somebody reads the guard and re-proves the must-red first, which
# needs a real Postgres and a `go test` per arm. That work is filed separately.
#
# Keyed (script, sha256(anchor)[:12]).
KNOWN_DEAD = {
    # EMPTY, AND THAT IS A RESULT RATHER THAN AN ABSENCE OF LOOKING. All ten arms this
    # guard found on the commit that introduced it were repaired under W6.43a, each by
    # re-running its own campaign against a real Postgres and reading the red by assertion
    # text. R1 fails on the next arm that dies, so an empty list is a claim the guard
    # re-checks on every push rather than a decision to stop watching.
}


def module_level_only(src, path):
    """A code object holding ONLY side-effect-free module-level definitions.

    Everything that is not an import, an assignment, a `def` or a `class` is
    dropped — including `if __name__ == "__main__"` and any bare top-level call.
    Nothing in a control script therefore RUNS here, and nothing is written.
    """
    tree = ast.parse(src, filename=path)
    tree.body = [n for n in tree.body if isinstance(
        n, (ast.Import, ast.ImportFrom, ast.Assign, ast.AnnAssign,
            ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef))]
    return compile(tree, path, "exec")


def path_shaped(v):
    """Does this string CLAIM to be a source path? Shape only — never existence.

    ⚠ EXISTENCE IS DELIBERATELY NOT PART OF THIS TEST. Keying on os.path.isfile
    makes a row whose target file was DELETED fail the test, vanish from the
    census, and take its anchors with it — the detector goes quiet exactly when
    something big moved. A missing target is a FINDING (TARGET-MISSING), not a
    reason to stop looking.
    """
    if not (isinstance(v, str) and v and "\n" not in v and len(v) < 400):
        return False
    if not v.endswith(SOURCE_SUFFIXES):
        return False
    # A repo-root target is a BARE filename with no separator — `README.md`,
    # `go.mod`, `docker-compose.yaml`. Requiring a "/" dropped nine real rows in
    # w31-setup-claims-controls.py alone. A prose string that happens to end in a
    # source suffix is excluded instead by having whitespace in it.
    return "/" in v or not any(ch.isspace() for ch in v)


def anchor_sha(s):
    return hashlib.sha256(s.encode("utf-8")).hexdigest()[:12]


def harvest(obj, out, depth=0, seen=None):
    """Find (..., path, old, new, ...) rows anywhere inside a module-level value."""
    if seen is None:
        seen = set()
    if depth > 8 or id(obj) in seen:
        return
    seen.add(id(obj))
    if isinstance(obj, dict):
        for v in obj.values():
            harvest(v, out, depth + 1, seen)
        return
    if not isinstance(obj, (list, tuple)):
        return
    # The path is NOT always at index 0 — w635 puts the control NAME first and
    # the target second, and keying on index 0 left the one script whose drift
    # created this item unreachable by its own census.
    for i in range(len(obj) - 2):
        if not path_shaped(obj[i]):
            continue
        old, new = obj[i + 1], obj[i + 2]
        # ⚠ REJECT (path, path, path) ROWS. Without this the census read w635's
        # own tuple of target FILES as an anchor and called it INERT — while
        # that script's independent --check-anchors says all 8 apply. Found by
        # the positive control, not by reading.
        if not (isinstance(old, str) and old and not path_shaped(old)):
            continue
        if not (isinstance(new, str) and not path_shaped(new)):
            continue
        want = None
        if len(obj) > i + 3 and isinstance(obj[i + 3], int) and not isinstance(obj[i + 3], bool):
            want = obj[i + 3]
        out.append({"path": obj[i], "old": old, "want": want})
        return
    for v in obj:
        harvest(v, out, depth + 1, seen)


def census(scripts_dir=SCRIPTS):
    results, unreached = [], []
    names = sorted(n for n in os.listdir(scripts_dir) if n.endswith(".py"))
    for name in names:
        path = os.path.join(scripts_dir, name)
        if os.path.samefile(path, os.path.abspath(__file__)):
            continue
        ns = {"__name__": "__anchor_census__", "__file__": os.path.abspath(path)}
        try:
            code = module_level_only(open(path, encoding="utf-8").read(), path)
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(buf):
                exec(code, ns)
        except Exception as exc:
            unreached.append((name, "EXEC-FAILED: %s: %s" % (type(exc).__name__, exc)))
            continue
        rows = []
        for key, val in list(ns.items()):
            if not key.startswith("__"):
                harvest(val, rows)
        seen, uniq = set(), []
        for r in rows:
            k = (r["path"], r["old"], r["want"])
            if k not in seen:
                seen.add(k)
                uniq.append(r)
        if not uniq:
            unreached.append((name, "NO-ANCHOR-ROW-FOUND"))
            continue
        for r in uniq:
            target = r["path"] if os.path.isabs(r["path"]) else os.path.join(REPO, r["path"])
            rec = {"script": name, "path": os.path.relpath(target, REPO), "want": r["want"],
                   "old": r["old"], "sha": anchor_sha(r["old"])}
            if not os.path.isfile(target):
                rec.update(count=None, verdict="TARGET-MISSING")
            else:
                n = open(target, encoding="utf-8").read().count(r["old"])
                rec["count"] = n
                if r["want"] is None:
                    rec["verdict"] = "INERT" if n == 0 else ("ok" if n == 1 else "AMBIGUOUS")
                else:
                    rec["verdict"] = "ok" if n == r["want"] else ("INERT" if n == 0 else "DRIFTED")
            results.append(rec)
    return results, unreached, len(names) - 1  # -1: this file is not a control script


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ci", action="store_true", help="exit non-zero on any rule violation")
    ap.add_argument("--list", action="store_true", help="print every anchor, not only the failures")
    args = ap.parse_args()

    results, unreached, population = census()
    scripts_reached = len(set(r["script"] for r in results))
    keyed = {(r["script"], r["sha"]): r for r in results}

    bad = [r for r in results if r["verdict"] != "ok"]
    failures = []

    # R1 — a control arm that can no longer be APPLIED and nobody has accounted for.
    unaccounted = [r for r in bad
                   if (r["script"], r["sha"]) not in NOT_ANCHORS
                   and (r["script"], r["sha"]) not in KNOWN_DEAD]
    if unaccounted:
        failures.append("R1: %d control arm(s) can no longer be APPLIED and are not accounted for"
                        % len(unaccounted))

    # R2 — the known-dead list may only SHRINK.
    for key, why in sorted(KNOWN_DEAD.items()):
        row = keyed.get(key)
        if row is None:
            failures.append("R3: KNOWN_DEAD entry %s/%s no longer appears in the census — the "
                            "entry is stale, or the detector narrowed" % key)
        elif row["verdict"] == "ok":
            failures.append("R2: KNOWN_DEAD entry %s/%s now RESOLVES (%s) — the arm was repaired, "
                            "so delete its line; this list may only shrink" % (key[0], key[1], why))

    # R3 — an allowlist entry that has stopped appearing is stale, not satisfied.
    for key in sorted(NOT_ANCHORS):
        if key not in keyed:
            failures.append("R3: NOT_ANCHORS entry %s/%s no longer appears in the census — the "
                            "entry is stale, or the detector narrowed" % key)

    # R4/R5 — one floor per detector stage.
    if scripts_reached < SCRIPTS_FLOOR:
        failures.append("R4: the exec stage reached %d scripts, floor is %d — the detector narrowed"
                        % (scripts_reached, SCRIPTS_FLOOR))
    if len(results) < ANCHORS_FLOOR:
        failures.append("R5: the harvest stage found %d anchors, floor is %d — the detector narrowed"
                        % (len(results), ANCHORS_FLOOR))

    print("control-anchors: %d scripts in population, %d reached, %d NOT reached, "
          "%d anchors checked, %d not ok"
          % (population, scripts_reached, len(unreached), len(results), len(bad)))

    if args.list:
        for r in results:
            print("  %-8s %-44s %-34s count=%s want=%s" %
                  (r["verdict"], r["script"], r["path"], r["count"], r["want"]))

    for r in unaccounted:
        print("\n  UNACCOUNTED %s" % r["verdict"])
        print("    script : %s" % r["script"])
        print("    target : %s   count=%s want=%s" % (r["path"], r["count"], r["want"]))
        print("    anchor : %r" % (r["old"][:100],))
        print("    → the control cannot be APPLIED. Read the guard it exercises before "
              "re-pointing it: an anchor moved to new code makes a control that passes for a "
              "NEW REASON. If it is genuinely dead, add it to KNOWN_DEAD with what moved and when.")

    for f in failures:
        print("\ncontrol-anchors: %s" % f)

    if failures:
        print("\ncontrol-anchors: FAILED")
        return 1 if args.ci else 0
    print("control-anchors: ok (%d arm(s) accounted for as known-dead, %d row(s) as non-anchors)"
          % (len(KNOWN_DEAD), len(NOT_ANCHORS)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
