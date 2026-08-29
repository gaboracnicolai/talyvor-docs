#!/usr/bin/env python3
"""Every control script that mutates a tracked file and restores it in a `finally` must also
install a restoring SIGNAL HANDLER — because a `finally` does not run on SIGTERM.

WHY THIS EXISTS, MEASURED RATHER THAN SUPPOSED — AND IT WAS MEASURED IN talyvor-suite, NOT HERE,
WHICH IS THE POINT OF PORTING IT. A 2-minute command timeout SIGTERM'd a control script mid-control
(talyvor-suite W1.7, merge 78c69c8). The `finally` never ran and the working tree was left with
`deploy/decision-expiry.sh` reading `if true; then` where a gate had been. It was reproduced on
demand in talyvor-suite `5de27e3`: same kill, same file — with a handler nothing was stranded,
without one the mutated file was left in the tree.

⚠ NOTHING IN THIS REPOSITORY HAD EVER LOOKED FOR IT. At f8e7236, 43 of this repo's scripts
mutate-and-restore and ZERO installed a handler — the estate-wide count was 163 scripts and 3
protected, all three in the one repo that had been bitten.

⚠ NOTHING ABOUT THAT TREE LOOKED WRONG. `pnpm test` had passed minutes earlier, `git status`
showed only files the session had edited on purpose, and the diff was one line inside a table that
had legitimately been touched. It was found only because the NEXT harness refused to score against
a red baseline. The stranded state is the dangerous kind: a kill landing on a control whose
mutation WEAKENS a guard leaves that weakening in the tree with a green suite.

⚠⚠ AND THIS IS A GUARD RATHER THAN A BATCH OF EDITS BECAUSE A POPULATION NAMED IN PROSE DOES NOT
STOP GROWING — measured in talyvor-suite, where the fix sat in one file while the directory grew by
twelve scripts and the unprotected count went UP. The allowlist below may only SHRINK: R2 fails on
an entry that has been fixed, so the list cannot rot into a permanent excuse, and R1 fails on a NEW
unprotected mutator, so the count cannot climb.

⚠ DETECTION IS SYNTACTIC, NOT A GREP, AND THAT IS LOAD-BEARING HERE RATHER THAN STYLISTIC.
In talyvor-suite `grep -l signal scripts/*.py` reports a script as PROTECTED whose only
occurrences of the word are four sentences of English in comments. A regex reads the documentation
as the implementation; `ast` does not see comments.

⚠ AND THE TWO POPULATIONS DIFFER HERE TOO, MEASURED ON THIS TREE AT f8e7236: `grep -l finally:
scripts/*.py` counts 60, this walk counts 43. A `finally` that does not WRITE is not a restore, and
seventeen scripts in this directory have one.

WHAT COUNTS AS A MUTATOR: a script containing a `try` whose `finally` performs a write —
`.write_text` / `.write_bytes` / `.writelines` / `.write`, or `shutil`/`os` copy/move. That is the
restore, and a script that restores is a script that mutated.

WHAT COUNTS AS PROTECTED: a call to `signal.signal(...)`. This does NOT verify the handler
restores correctly or re-raises; it verifies one is installed. Converting a script needs its own
SIGTERM control — kill it mid-mutation with the handler, and again without it, and require the
second to strand what the first restores. A handler nobody has watched work is the same thing as
no handler.

THE SHAPE TO COPY IS BELOW rather than a pointer into another repository, because a cross-repo
pointer is a claim nothing in this repo can check:

    def restore_on_signal(snapshot: dict) -> None:
        def handler(signum, _frame):
            for path, blob in snapshot.items():
                try:
                    path.write_bytes(blob)
                except OSError:
                    pass
            sys.stderr.write("\n!! signal %d — restored %d file(s)\n" % (signum, len(snapshot)))
            signal.signal(signum, signal.SIG_DFL)
            os.kill(os.getpid(), signum)
        for s in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
            signal.signal(s, handler)

Install it AFTER the snapshot exists, and re-install it if the snapshot changes. Re-raising with
SIG_DFL keeps the exit status honest: a caller that killed the process still sees it die of that
signal rather than exit 0 with a tidy tree. SIGKILL still strands and nothing in Python can change
that; the defence there is a harness that refuses to score against a red baseline.

Usage:  python3 scripts/check-restore-signal-handlers.py
Exit 0 = every mutator is protected or listed; non-zero names what changed.
"""
import ast
import pathlib
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
SCRIPTS = REPO / "scripts"

# MUTATOR FLOOR. 43 scripts in this directory mutate-and-restore at f8e7236. It is a FLOOR on the
# DETECTOR, not a target for the tree: if this walk stops recognising the shape it reports a clean
# directory, which is the one failure mode a guard like this has. Deleting scripts is legitimate —
# lower it in the same diff, with the deletions visible.
MUTATOR_FLOOR = 40

# The scripts that mutate-and-restore and do NOT yet install a handler, measured at f8e7236.
# ⚠ THIS LIST MAY ONLY SHRINK. Adding to it is not a fix; R1 exists so that a new script cannot be
# written without a handler, and R2 exists so that a fixed script cannot be left listed.
UNPROTECTED = {
    "w31-ambiguousactor-controls-8f3c.py",
    "w31-askgrounding-controls.py",
    "w31-blockpatch-controls-b2e7.py",
    "w31-bodypage-attribution-controls.py",
    "w31-classguard-blindness.py",
    "w31-createhooks-controls-6b4e.py",
    "w31-crossws-rollup-controls-4j7q.py",
    "w31-crossws-semantic-controls-4j7q.py",
    "w31-frontend-manifest-controls-b9d7.py",
    "w31-grant-route-controls-9d47.py",
    "w31-importtitle-controls-4d81.py",
    "w31-inbox-space-controls.py",
    "w31-mcp-slugarm-controls.py",
    "w31-mcpreadgate-controls.py",
    "w31-mountread-controls-7k2m.py",
    "w31-mountread-probe-8v3r.py",
    "w31-nullcohorts-controls-3d8f.py",
    "w31-page-required-keys-controls.py",
    "w31-pagelock-identity-controls-9d2a.py",
    "w31-pagesearch-visibility-controls.py",
    "w31-paramliteral-controls-8v3r.py",
    "w31-perpagetitle-controls-a41f.py",
    "w31-reparent-controls-9c4e.py",
    "w31-revoke-route-controls-9d47.py",
    "w31-rollupzeros-controls-3d8f.py",
    "w31-selfidentity-controls-9d2a.py",
    "w31-setup-claims-controls.py",
    "w31-sharelink-viewcount-controls.py",
    "w31-shortflag-controls-6b4d.py",
    "w31-slug-collision-controls-7f5b.py",
    "w31-spacerollup-controls-m3r8.py",
    "w31-stalereport-controls.py",
    "w31-takeover-affordance-controls-b2e7.py",
    "w31-toolclass-controls-8f5c.py",
    "w31-treeread-controls-8f5c.py",
    "w31-verifyeditclock-controls-8f3d.py",
    "w31-versioncost-controls-9m4x.py",
    "w31-versioncostsplit-route-controls-m3r8.py",
    "w31-versiondiff-rename-controls-8c31.py",
    "w31-viewbump-owner-controls.py",
    "w31-viewcount-premeasure.py",
    "w31-viewrecord-controls-9d2a.py",
}

_WRITE_ATTRS = {"write_text", "write_bytes", "writelines", "write"}
_COPY_FUNCS = {"copy", "copy2", "copyfile", "move"}


def _writes(body) -> bool:
    """True if this statement list performs a filesystem write."""
    for n in ast.walk(ast.Module(body=body, type_ignores=[])):
        if not (isinstance(n, ast.Call) and isinstance(n.func, ast.Attribute)):
            continue
        if n.func.attr in _WRITE_ATTRS:
            return True
        if n.func.attr in _COPY_FUNCS and isinstance(n.func.value, ast.Name) \
                and n.func.value.id in ("shutil", "os"):
            return True
    return False


def _restores_in_finally(tree: ast.AST) -> bool:
    return any(isinstance(n, ast.Try) and n.finalbody and _writes(n.finalbody)
               for n in ast.walk(tree))


def _installs_handler(tree: ast.AST) -> bool:
    """A call to `signal.signal(...)`. Comments are invisible to ast, which is the point."""
    return any(
        isinstance(n, ast.Call) and isinstance(n.func, ast.Attribute)
        and n.func.attr == "signal"
        and isinstance(n.func.value, ast.Name) and n.func.value.id == "signal"
        for n in ast.walk(tree)
    )


def main() -> int:
    mutators, protected, errors = set(), set(), []
    for path in sorted(SCRIPTS.glob("*.py")):
        try:
            tree = ast.parse(path.read_text(encoding="utf-8"))
        except (SyntaxError, UnicodeDecodeError) as e:
            # R5: a file this guard cannot read is reported, never skipped. A walk that silently
            # drops what it cannot parse reports a clean directory.
            errors.append(f"R5: {path.name} could not be parsed, so it was NOT checked: {e}")
            continue
        if _restores_in_finally(tree):
            mutators.add(path.name)
            if _installs_handler(tree):
                protected.add(path.name)

    fail = list(errors)

    # R3 FLOOR FIRST, because every rule below is vacuous without a population.
    if len(mutators) < MUTATOR_FLOOR:
        fail.append(
            f"R3: found only {len(mutators)} mutate-and-restore scripts, floor is {MUTATOR_FLOOR}. "
            "A detector that stops recognising the shape reports a clean directory rather than a "
            "broken instrument. If scripts were deleted, lower the floor in the same diff.")

    # R1: a mutator with no handler that nobody has accounted for.
    for name in sorted(mutators - protected - UNPROTECTED):
        fail.append(
            f"R1: {name} mutates a tracked file and restores it in a `finally`, but installs no "
            "signal handler — a SIGTERM will strand the mutation in the working tree. The "
            "shape to copy is in this file's own docstring, and the conversion needs its own "
            "SIGTERM control.")

    # R2: an entry that has been FIXED must leave the list, or the list rots into an excuse.
    for name in sorted(UNPROTECTED & protected):
        fail.append(
            f"R2: {name} now installs a handler but is still listed in UNPROTECTED. Remove the "
            "entry — this list may only shrink.")

    # R4: an entry that is no longer a mutator (deleted, renamed, or restructured) is stale.
    for name in sorted(UNPROTECTED - mutators):
        fail.append(
            f"R4: UNPROTECTED lists {name}, which is not a mutate-and-restore script here "
            "(deleted, renamed, or no longer restoring in a `finally`). Remove the entry.")

    print(f"restore-signal-handlers: {len(mutators)} mutate-and-restore scripts, "
          f"{len(protected)} protected, {len(UNPROTECTED)} listed as not yet fixed")
    if fail:
        for line in fail:
            print(f"::error::{line}" if len(sys.argv) > 1 and sys.argv[1] == "--ci" else line)
        return 1
    print("restore-signal-handlers: ok")
    return 0


sys.exit(main())
