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
NOTANCHOR_SCRIPT = ROOT / "scripts" / "w31-classguard-blindness.py"  # a NOT_ANCHORS row
STORE_GO = ROOT / "internal" / "page" / "store.go"
# ⚠ THE VERDICT ARMS INSTALL THEIR OWN SUBJECT RATHER THAN BORROWING ONE.
# C7 originally blinded the undeclared-`want` verdict branch and relied on whatever happened to
# be in the guard's KNOWN_DEAD list taking that branch. It scored CAUGHT until W6.43a repaired
# three scripts, after which every surviving entry declared a count — and the arm went VOID with
# nothing about it looking wrong. An arm whose subject is a side effect of somebody else's list
# is an arm that stops proving things silently, which is the whole subject of this campaign.
PROBE = ROOT / "scripts" / "_w643_probe_do_not_commit.py"


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


def anchor_sha(text):
    """Must stay identical to check-control-anchors.anchor_sha.

    Deliberately re-stated rather than imported: importing the guard would run
    its module-level code inside the process that is about to mutate it, and a
    key computed one way here and another way there would make the probe arms
    quietly unprovable — the failure this whole campaign is about.
    """
    return hashlib.sha256(text.encode("utf-8")).hexdigest()[:12]


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
    """C2 — a KNOWN_DEAD arm resolves again, at the SAME anchor.

    ⚠ THIS ARM HAS BEEN WRONG TWICE, BOTH TIMES FOR A REASON THIS CAMPAIGN
    EXISTS TO CATCH. First it repointed the subject script's anchor constant and
    fired R3 rather than R2 — changing an anchor changes its hash, so the entry
    stopped APPEARING instead of starting to RESOLVE, and the arm proved the
    wrong rule. Then it borrowed its subject from whatever happened to be in
    KNOWN_DEAD, and W6.43a repaired the last entry, so the list went empty and
    the arm scored NOT CAUGHT with nothing about it looking wrong.

    It now installs its own subject: a throwaway script whose anchor DOES occur
    in its target, listed as known-dead. The guard must demand the entry be
    deleted, because that list may only shrink.
    """
    _plant_probe("module github.com/talyvor/docs", declared=False, resolving=True)


def m_delete_dead_row():
    """C3 — a KNOWN_DEAD row stops appearing at all (stale entry, or narrowing).

    ⚠ THE FIRST VERSION DELETED A HEALTHY ROW AND FIRED ONLY R5; the second
    renamed a constant in a real control script and went VOID once W6.43a
    removed that constant. Deleting an anchor is always visible to the floor;
    only a LISTED one is visible to R3, and R3 is what stops the allowlist
    rotting into decoration.

    It now lists a probe it does not create — precisely the stale-entry shape R3
    is for — and depends on no other script's contents.
    """
    src = GUARD.read_text()
    GUARD.write_text(src.replace(
        "KNOWN_DEAD = {",
        "KNOWN_DEAD = {\n    (%r, \"deadbeef0000\"): \"w643 probe that was never installed\"," % PROBE.name,
        1))


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


def _plant_probe(anchor, declared, resolving=False):
    """Install a throwaway control script with ONE inert anchor, and list it as known-dead.

    The row points at go.mod with a literal that cannot occur there, so it is
    INERT by construction; listing it makes the guard green. Blinding the verdict
    branch it takes then makes it report `ok`, which R2 catches as "a known-dead
    entry now resolves". The subject belongs to the rule being exercised, rather
    than being whichever entry the allowlist happens to contain today.
    """
    # `resolving=True` means the anchor really DOES occur in go.mod, so the row is
    # healthy while still being listed — which is what R2 is for. Otherwise the
    # literal cannot occur there and the row is INERT by construction.
    if resolving:
        assert anchor in (ROOT / "go.mod").read_text(), \
            "C2 is void: its probe anchor does not occur in go.mod, so nothing can RESOLVE"
    row = ("go.mod", anchor, "irrelevant-replacement", 1) if declared else \
          ("go.mod", anchor, "irrelevant-replacement")
    PROBE.write_text("EDITS = [%r]\n" % (row,))
    src = GUARD.read_text()
    GUARD.write_text(src.replace(
        "KNOWN_DEAD = {",
        "KNOWN_DEAD = {\n    (%r, %r): \"w643 probe\"," % (PROBE.name, anchor_sha(anchor)), 1))


def m_verdict_always_ok():
    """C7 — the UNDECLARED-`want` verdict is computed and then ignored.

    Proves the verdict is CONSULTED rather than merely produced: the tautology
    shape, where an instrument measures correctly and discards the result.
    """
    _plant_probe("W643_PROBE_UNDECLARED_NEVER_OCCURS", declared=False)
    src = GUARD.read_text()
    GUARD.write_text(src.replace(
        '                    rec["verdict"] = "INERT" if n == 0 else ("ok" if n == 1 else "AMBIGUOUS")',
        '                    rec["verdict"] = "ok"', 1))


def m_verdict_always_ok_declared():
    """C7b — the DECLARED-`want` verdict is computed and then ignored.

    ⚠ THERE ARE TWO VERDICT BRANCHES AND BLINDING ONE LEAVES THE OTHER LIVE,
    for the same reason there is one floor per detector stage.
    """
    _plant_probe("W643_PROBE_DECLARED_NEVER_OCCURS", declared=True)
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


def _plant_bare_probe(role):
    """A probe with NO (path, old, new) row, so ONLY the bare-constant detector can see it.

    `role="anchor"`    — the constant sits in `.replace(NAME, …)`, so the role pass calls it an
                         anchor and the guard must report it INERT.
    `role="replacement"` — it sits in `.replace(…, NAME)`. A replacement MUST NOT occur in the
                         target, so reporting it would be a false positive; the guard must stay
                         silent unless the role pass is blinded.
    """
    lit = "W643_BARE_PROBE_%s_NEVER_OCCURS = ()" % role.upper()
    if role == "anchor":
        body = 'TARGET = "go.mod"\nANCHOR = %r\n\n\ndef apply(text):\n    return text.replace(ANCHOR, "x")\n' % lit
    else:
        body = 'TARGET = "go.mod"\nREPL = %r\n\n\ndef apply(text):\n    return text.replace("module", REPL)\n' % lit
    PROBE.write_text(body)


def m_bare_detector_finds_it():
    """C11 — the bare-constant detector reports an inert anchor-role constant.

    Detector 1 cannot see this probe at all: there is no (path, old, new) row in it.
    """
    _plant_bare_probe("anchor")


def m_role_pass_blinded():
    """C12 — the role pass is what suppresses replacements, and here is the proof.

    The probe holds a constant in REPLACEMENT position, which by construction cannot occur in the
    target. With the role pass live the guard is silent (verified in both directions). Blinding it
    — every constant treated as an anchor — turns that same probe into a finding. This is the
    46-of-49 noise the measurement in W6.43b recorded, arriving one arm at a time.
    """
    _plant_bare_probe("replacement")
    src = GUARD.read_text()
    GUARD.write_text(src.replace(
        '        if "anchor" not in roles.get(k, set()):',
        '        if False:', 1))


def m_bare_harvest_blinded():
    """C13 — the bare detector's OWN harvest stage, blinded alone."""
    src = GUARD.read_text()
    GUARD.write_text(src.replace(
        "def _anchor_shaped(v):",
        "def _anchor_shaped(v):\n    return False", 1))


def m_dataclass_probe():
    """C14 — a script using @dataclass must not vanish into the unreached bucket.

    ⚠ THIS WAS A REAL BLIND SPOT AND IT REPORTED ITSELF AS A SHAPE PROBLEM.
    `@dataclass` resolves its field types through `sys.modules[cls.__module__].__dict__`, so
    exec'ing a script into a bare dict under a synthetic __name__ raises AttributeError — and the
    guard filed the script under "unreached", indistinguishable from one that simply has no anchor
    table. w31-tenancypredicate-census-5r8k.py was invisible for exactly that reason.

    The probe carries a dataclass AND an inert row, so it can only be reported if the exec
    succeeds. Before the fix this arm scores NOT CAUGHT; after it, R1.
    """
    # ⚠ `from __future__ import annotations` IS LOAD-BEARING AND THE FIRST VERSION OF THIS ARM
    # OMITTED IT, SO IT SCORED CAUGHT WITH AND WITHOUT THE FIX. Plain annotations are real type
    # objects and dataclasses never consults sys.modules for them; PEP 563 turns every annotation
    # into a STRING, which is what forces the `sys.modules[cls.__module__].__dict__` lookup that
    # fails. The real script this reproduces, w31-tenancypredicate-census-5r8k.py, has exactly
    # this pair at lines 43 and 53. Verified in both directions: with the fix -> R1, without it
    # -> no R1.
    PROBE.write_text(
        "from __future__ import annotations\n\n"
        "from dataclasses import dataclass\n\n\n"
        "@dataclass\nclass Edit:\n    path: str\n    old: str\n\n\n"
        "EDITS = [(\"go.mod\", \"W643_DATACLASS_PROBE_NEVER_OCCURS\", \"x\", 1)]\n")


def m_harmless_edit():
    """C9 — MUST STAY GREEN. An edit that touches no anchor must not red."""
    src = TARGET_OK.read_text()
    TARGET_OK.write_text(src + "\n// C9: a comment that is nobody's anchor\n")


ARMS = [
    ("C1", "a live non-allowlisted arm loses its anchor", [TARGET_OK], m_kill_healthy_anchor, {"R1"}),
    ("C2", "a KNOWN_DEAD arm resolves again — the list may only shrink", [GUARD, PROBE], m_repair_dead_arm, {"R2"}),
    ("C3", "a KNOWN_DEAD row stops appearing", [GUARD], m_delete_dead_row, {"R3"}),
    ("C4", "a NOT_ANCHORS row stops appearing", [NOTANCHOR_SCRIPT], m_delete_notanchor_row, {"R3"}),
    ("C5", "the EXEC stage is blinded", [GUARD], m_blind_stage1, {"R4"}),
    ("C6", "the HARVEST stage is blinded, exec untouched", [GUARD], m_blind_stage2, {"R5"}),
    ("C7", "the undeclared-want verdict is ignored", [GUARD, PROBE], m_verdict_always_ok, {"R2"}),
    ("C7b", "the declared-want verdict is ignored", [GUARD, PROBE], m_verdict_always_ok_declared, {"R2"}),
    ("C8", "a target file is deleted", [STORE_GO], m_delete_target_file, {"R1"}),
    ("C11", "the bare-constant detector reports an inert anchor", [PROBE], m_bare_detector_finds_it, {"R1"}),
    ("C12", "the role pass is blinded, so a REPLACEMENT reads as an anchor", [GUARD, PROBE], m_role_pass_blinded, {"R1"}),
    ("C13", "the bare detector's harvest stage is blinded alone", [GUARD], m_bare_harvest_blinded, {"R7"}),
    ("C14", "a script using @dataclass is read, not silently skipped", [PROBE], m_dataclass_probe, {"R1"}),
    ("C9", "MUST STAY GREEN: an edit that is nobody's anchor", [TARGET_OK], m_harmless_edit, set()),
]


def main():
    rc, out = run_guard()
    if rc != 0:
        print("PRECONDITION FAILED: the guard is not green on an unmodified tree.\n%s" % out)
        return 2
    print("precondition: the guard is green on an unmodified tree\n")

    snapshot = {p: p.read_bytes() for _, _, files, _, _ in ARMS for p in files if p.exists()}
    # Files an arm CREATES have no pristine bytes; they are removed after it instead.
    created = {p for _, _, files, _, _ in ARMS for p in files if not p.exists()}
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
            before = {p: sha(p) for p in files if p.exists()}
            mutate()

            # VOID CHECK — the mutation must actually have moved the bytes.
            void = []
            for p in files:
                if not p.exists():
                    continue  # C8 deletes on purpose
                if p in before and sha(p) == before[p]:
                    void.append(p.name)
            if void and cid != "C8":
                print("%s VOID — the mutation changed nothing in %s; the arm proves nothing"
                      % (cid, ", ".join(void)))
                for p in files:
                    if p in created:
                        p.unlink(missing_ok=True)
                    else:
                        shutil.copyfile(pristine(p), p)
                continue

            rc, out = run_guard()
            fired = rules_fired(out)

            for p in files:
                if p in created:
                    p.unlink(missing_ok=True)
                    assert not p.exists(), "CLEANUP FAILED for %s" % p
                    continue
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
        for p in created:
            p.unlink(missing_ok=True)
        shutil.rmtree(tmp, ignore_errors=True)

    print("\nw643 anchor-guard controls: %d/%d" % (passed, len(ARMS)))
    return 0 if passed == len(ARMS) else 1


if __name__ == "__main__":
    sys.exit(main())
