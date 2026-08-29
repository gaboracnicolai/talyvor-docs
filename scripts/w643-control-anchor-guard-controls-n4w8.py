#!/usr/bin/env python3
"""Positive controls for scripts/check-control-anchors.py (W6.43).

THE GUARD PASSED ON ITS FIRST RUN, WHICH IS WHY THIS FILE EXISTS. Every rule
below is exercised by mutating a real file, running the guard, and reading WHICH
RULE FIRED — never merely that the exit code moved. A control that checks only
"it went red" cannot tell a rule doing its job from an unrelated rule tripping,
and this guard has six rules that can all fire at once.

Each arm additionally carries a VOID CHECK: the mutation must actually change
the bytes on disk. An arm whose `str.replace` silently no-ops because its own
anchor drifted scores as PASSED while proving nothing — the exact defect the
guard under test is built to find, and one that has already been shipped twice
in this estate.

No database, no `go test`: the guard is pure string counting over the tree, so
the whole campaign runs in about a second.
"""
import hashlib
import os
import pathlib
import re
import shutil
import signal
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
GUARD = ROOT / "scripts" / "check-control-anchors.py"

# Subjects. Chosen because each is a DIFFERENT stage of the detector.
TARGET_OK = ROOT / "internal" / "analytics" / "store.go"        # a healthy, non-allowlisted arm
HEALTHY_ANCHOR = "\tvisible := make([]ReadStats, 0, len(ranked))"
DEAD_SCRIPT = ROOT / "scripts" / "w31-version-title-controls.py"  # a KNOWN_DEAD arm
NOTANCHOR_SCRIPT = ROOT / "scripts" / "w31-classguard-blindness.py"  # a NOT_ANCHORS row
STORE_GO = ROOT / "internal" / "page" / "store.go"


def restore_on_signal(snapshot):
    """Put every snapshotted file back, then die of the signal we were sent.

    A `finally` DOES NOT RUN ON SIGTERM. This script restores in a `finally`;
    that covers exceptions and nothing else. The handler writes BYTES rather
    than shelling out to git, because a signal handler should not depend on a
    subprocess. The rule that requires this lives in
    scripts/check-restore-signal-handlers.py.
    """
    def handler(signum, _frame):
        n = 0
        for path, data in snapshot.items():
            try:
                path.write_bytes(data)
                n += 1
            except Exception:
                pass
        sys.stderr.write("\n!! signal %d — restored %d mutated file(s) before exiting\n"
                         % (signum, n))
        sys.stderr.flush()
        signal.signal(signum, signal.SIG_DFL)
        os.kill(os.getpid(), signum)

    for s in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(s, handler)


def sha(path):
    return hashlib.sha256(pathlib.Path(path).read_bytes()).hexdigest()


def run_guard():
    p = subprocess.run([sys.executable, str(GUARD), "--ci"], cwd=str(ROOT),
                       capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def rules_fired(out):
    """The set of rule ids named in the guard's own output."""
    return set(re.findall(r"^control-anchors: (R\d)", out, re.M))


# ── the arms ─────────────────────────────────────────────────────────────────
# (id, what it proves, files it edits, mutate(), expected rule ids)

def m_kill_healthy_anchor():
    """C1 — a live, non-allowlisted arm loses its anchor.

    ⚠ THE FIRST VERSION OF THIS ARM APPENDED ` // C1` TO THE ANCHOR LINE AND
    SCORED "NOT CAUGHT". The bytes moved, so the void check passed — but the
    anchor is a SUBSTRING with no trailing newline, so it still occurred once
    and the guard was right to stay green. A byte-level void check cannot see an
    arm that is void at the ANCHOR level, which is the same distinction the
    guard under test exists to make.
    """
    src = TARGET_OK.read_text()
    assert src.count(HEALTHY_ANCHOR) == 1
    out = src.replace(HEALTHY_ANCHOR, "\tvisibleC1 := make([]ReadStats, 0, len(ranked))", 1)
    assert HEALTHY_ANCHOR not in out, "C1 is void: the anchor survives its own mutation"
    TARGET_OK.write_text(out)


def m_repair_dead_arm():
    """C2 — a KNOWN_DEAD arm starts resolving again, at the SAME anchor.

    ⚠ THE FIRST VERSION REPOINTED THE SCRIPT'S ANCHOR CONSTANT AT A LIVE STRING
    AND FIRED R3, NOT R2 — because changing the anchor changes its content hash,
    so the entry stopped APPEARING rather than starting to resolve. Those are
    two different rules and the arm was proving the wrong one. Repair means the
    SAME anchor resolving, so this puts the dead text back into the target file.
    """
    STORE_GO.write_text(STORE_GO.read_text()
                        + "\n// C2 repair:\n//\t\t\tid, out.WorkspaceID, nextVer, out.Title, out.Content, updatedBy,\n")


def m_delete_dead_row():
    """C3 — a KNOWN_DEAD row stops appearing at all (stale entry, or narrowing).

    ⚠ THE FIRST VERSION DELETED THE WRONG ROW — a healthy one — and fired only
    R5. Deleting an anchor is always visible to the floor; only deleting a
    LISTED one is visible to R3, and R3 is what stops the allowlist rotting.
    """
    src = DEAD_SCRIPT.read_text()
    src = src.replace("SNAPSHOT_ARGS = ", "SNAPSHOT_ARGS_UNUSED_C3 = ", 1)
    src = src.replace("(STORE, SNAPSHOT_ARGS,", "(STORE, CLOSED_SET,", 1)
    DEAD_SCRIPT.write_text(src)


def m_delete_notanchor_row():
    """C4 — a NOT_ANCHORS row stops appearing at all."""
    src = NOTANCHOR_SCRIPT.read_text()
    NOTANCHOR_SCRIPT.write_text(src.replace('        "bf1f5fe",', '        "",', 1))


def m_blind_stage1():
    """C5 — blind the EXEC stage only: fewer scripts reached."""
    src = GUARD.read_text()
    GUARD.write_text(src.replace(
        "    tree.body = [n for n in tree.body if isinstance(",
        "    tree.body = tree.body[:1] if False else [n for n in tree.body[:6] if isinstance(", 1))


def m_blind_stage2():
    """C6 — blind the HARVEST stage only: fewer anchors, SAME scripts.

    ⚠ THIS IS THE ARM THAT MATTERS. A single floor over the union of the two
    stages is satisfied by either one alone, so a harvest that quietly halves
    can hide behind an exec that still reaches every script. talyvor-suite
    shipped exactly that trap (W6.41a) and only a per-stage control found it.
    """
    src = GUARD.read_text()
    GUARD.write_text(src.replace(
        "        out.append({\"path\": obj[i], \"old\": old, \"want\": want})",
        "        if len(out) < 3:\n            out.append({\"path\": obj[i], \"old\": old, \"want\": want})", 1))


def m_verdict_always_ok():
    """C7 — the UNDECLARED-`want` verdict is computed and then ignored.

    Proves the verdict is CONSULTED rather than merely produced — the tautology
    shape, where an instrument measures correctly and discards the result. It
    reds through R2 rather than R1, and that is the stronger catch: the arms the
    allowlist says are dead all start reporting healthy at once.
    """
    src = GUARD.read_text()
    GUARD.write_text(src.replace(
        '                    rec["verdict"] = "INERT" if n == 0 else ("ok" if n == 1 else "AMBIGUOUS")',
        '                    rec["verdict"] = "ok"', 1))


def m_verdict_always_ok_declared():
    """C7b — the DECLARED-`want` verdict is computed and then ignored.

    ⚠ THERE ARE TWO VERDICT BRANCHES AND BLINDING ONE LEAVES THE OTHER LIVE.
    C7 alone passed while five known-dead arms kept reporting correctly, because
    they declare an expected count and take the other branch. One arm per branch,
    for the same reason there is one floor per stage.
    """
    src = GUARD.read_text()
    GUARD.write_text(src.replace(
        '                    rec["verdict"] = "ok" if n == r["want"] else ("INERT" if n == 0 else "DRIFTED")',
        '                    rec["verdict"] = "ok"', 1))


def m_delete_target_file():
    """C8 — a target file is DELETED.

    The first version of this guard keyed "is this a path?" on os.path.isfile,
    so a deleted target failed the test, vanished from the census and took its
    anchors with it — the detector going quiet exactly when something big moved.
    This arm proves it now reports TARGET-MISSING instead.
    """
    STORE_GO.unlink()


def m_harmless_edit():
    """C9 — MUST STAY GREEN. An edit that touches no anchor must not red."""
    src = TARGET_OK.read_text()
    TARGET_OK.write_text(src + "\n// C9: a comment that is nobody's anchor\n")


ARMS = [
    ("C1", "a live non-allowlisted arm loses its anchor", [TARGET_OK], m_kill_healthy_anchor, {"R1"}),
    ("C2", "a KNOWN_DEAD arm resolves again — the list may only shrink", [STORE_GO], m_repair_dead_arm, {"R2"}),
    ("C3", "a KNOWN_DEAD row stops appearing", [DEAD_SCRIPT], m_delete_dead_row, {"R3"}),
    ("C4", "a NOT_ANCHORS row stops appearing", [NOTANCHOR_SCRIPT], m_delete_notanchor_row, {"R3"}),
    ("C5", "the EXEC stage is blinded", [GUARD], m_blind_stage1, {"R4"}),
    ("C6", "the HARVEST stage is blinded, exec untouched", [GUARD], m_blind_stage2, {"R5"}),
    ("C7", "the undeclared-want verdict is ignored", [GUARD], m_verdict_always_ok, {"R2"}),
    ("C7b", "the declared-want verdict is ignored", [GUARD], m_verdict_always_ok_declared, {"R2"}),
    ("C8", "a target file is deleted", [STORE_GO], m_delete_target_file, {"R1"}),
    ("C9", "MUST STAY GREEN: an edit that is nobody's anchor", [TARGET_OK], m_harmless_edit, set()),
]


def main():
    rc, out = run_guard()
    if rc != 0:
        print("PRECONDITION FAILED: the guard is not green on an unmodified tree.\n%s" % out)
        return 2
    print("precondition: the guard is green on an unmodified tree\n")

    snapshot = {p: p.read_bytes() for _, _, files, _, _ in ARMS for p in files}
    restore_on_signal(snapshot)
    tmp = tempfile.mkdtemp(prefix="w643-pristine-")

    def pristine(path):
        """⚠ KEYED ON THE RELATIVE PATH, NOT THE BASENAME. Two of this repo's
        subjects are both called `store.go` (internal/analytics and internal/page),
        so a basename-keyed pristine directory restores one from the other's bytes.
        Caught by this file's own restore assertion, not by reading it."""
        return os.path.join(tmp, str(path.relative_to(ROOT)).replace(os.sep, "__"))

    for p in snapshot:
        shutil.copyfile(p, pristine(p))

    passed = 0
    try:
        for cid, what, files, mutate, expect in ARMS:
            before = {p: sha(p) for p in files}
            mutate()

            # VOID CHECK — the mutation must actually have moved the bytes.
            void = []
            for p in files:
                if not p.exists():
                    continue  # C8 deletes on purpose
                if sha(p) == before[p]:
                    void.append(p.name)
            if void and cid != "C8":
                print("%s VOID — the mutation changed nothing in %s; the arm proves nothing"
                      % (cid, ", ".join(void)))
                for p in files:
                    shutil.copyfile(pristine(p), p)
                continue

            rc, out = run_guard()
            fired = rules_fired(out)

            for p in files:
                shutil.copyfile(pristine(p), p)
                assert sha(p) == hashlib.sha256(snapshot[p]).hexdigest(), \
                    "RESTORE FAILED for %s — stop and inspect the tree" % p

            if expect == set():
                ok = (rc == 0 and not fired)
                verdict = "STAYED GREEN" if ok else "WENT RED (%s)" % (sorted(fired) or rc)
            else:
                # READ WHICH RULE FIRED. "it went red" is not the claim.
                ok = (rc != 0 and expect <= fired)
                verdict = "CAUGHT by %s" % sorted(fired) if ok else \
                          "NOT CAUGHT — expected %s, got %s (rc=%s)" % (sorted(expect), sorted(fired), rc)
            passed += ok
            print("%s %-52s %s" % (cid, what, verdict))
            if not ok:
                print(out)
    finally:
        for p in snapshot:
            src = pristine(p)
            if os.path.exists(src):
                shutil.copyfile(src, p)
        shutil.rmtree(tmp, ignore_errors=True)

    print("\nw643 anchor-guard controls: %d/%d" % (passed, len(ARMS)))
    return 0 if passed == len(ARMS) else 1


if __name__ == "__main__":
    sys.exit(main())
