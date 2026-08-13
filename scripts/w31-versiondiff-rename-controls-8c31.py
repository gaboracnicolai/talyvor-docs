#!/usr/bin/env python3
"""Positive controls for the version-diff rename fix (W3.1, tab-8c31, talyvor-docs).

THE FINDING. `page_versions` has exactly two content-bearing columns (title, content) and
RestoreVersion writes BOTH back onto the live page, so a version IS the pair. The SPA's diff
panel diffed `content` alone, so the pair a title-only save produces — identical content,
different title, the exact state `TestTitleOnlySave_AppendsARestorePoint_RealPG` pins — rendered
as an all-`same` panel. The screen's answer for the revision that renamed the document was that
the revision changed nothing.

Each control mutates ONE thing, runs the guard, and reports the SET of [TAG]s that fired. Every
mutation is restored in a `finally` and the restore is verified by sha256 against the
pre-mutation bytes — a control that silently failed to restore would score every later control
against a different tree.

THE CATCHER IS PREDICTED BEFORE THE RUN (`expect` below). A control whose actual catcher differs
from its prediction is a finding about the guard, not a pass.

⚠ TAGS ARE SCRAPED AS THE LEADING TAG OF A FAILURE LINE ONLY (`Error: [TAG] …`). vitest echoes
the source of the throwing statement into its code frame, so every tag in this file appears in
the output of ANY failure in it; scraping every [TAG] on every line would score a control against
its own neighbours' prose. This queue has already shipped one harness that did exactly that.

Run:  python3 scripts/w31-versiondiff-rename-controls-8c31.py
"""
import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FRONTEND = os.path.join(REPO, "frontend")
COMP = os.path.join(FRONTEND, "src/components/VersionHistory.tsx")
GUARD = "src/components/VersionHistory.rename.test.tsx"
GUARD_FILE = os.path.join(FRONTEND, GUARD)

TAGS = ["RENAME-VISIBLE", "NO-PHANTOM-RENAME", "CONTENT-STILL-DIFFED"]

# ── anchors, verbatim ───────────────────────────────────────────────────────────────────
DIFF_MEMO = """    return lineDiff(versionText(diff.data.from), versionText(diff.data.to));"""

VERSION_TEXT_BODY = """  return `title: ${v.title}\\n\\n${prettyContent(v.content)}`;"""

HEADING = """            Diff v{from} → v{to}"""

MARKERS = """                {l.type === "add" ? "+ " : l.type === "remove" ? "- " : "  "}"""

# The defect exactly as it stood at 7bfa1cf — content alone, both sides.
DEFECT = """    return lineDiff(prettyContent(diff.data.from.content), prettyContent(diff.data.to.content));"""

# The rejected alternative: the title printed OUTSIDE the comparison, as a strip that always
# reports a rename. Satisfies "the titles are on screen and marked" and is a lie on every
# ordinary body edit.
BYPASS = """    return [
      { type: "remove" as const, text: `title: ${diff.data.from.title}` },
      { type: "add" as const, text: `title: ${diff.data.to.title}` },
      ...lineDiff(prettyContent(diff.data.from.content), prettyContent(diff.data.to.content)),
    ];"""

# Title INSTEAD of content — the half-fix that trades one blind spot for the other.
TITLE_ONLY = """  return `title: ${v.title}`;"""


def sha(path):
    return hashlib.sha256(open(path, "rb").read()).hexdigest()


def run_guard(only=GUARD):
    """Run the guard; return (set of LEADING tags on failure lines, exit code, output)."""
    p = subprocess.run(
        ["npx", "vitest", "run", only],
        cwd=FRONTEND, env=dict(os.environ), capture_output=True, text=True, timeout=900,
    )
    out = p.stdout + p.stderr
    if "Failed to parse" in out or "Transform failed" in out or "esbuild" in out.lower():
        return {"BUILD-FAILED"}, p.returncode, out
    fired = set()
    for line in out.split("\n"):
        m = re.match(r"\s*(?:\S+\s+)?Error: \[([A-Z][A-Z-]+)\]", line)
        if m and m.group(1) in TAGS:
            fired.add(m.group(1))
    return fired, p.returncode, out


def mutate(src, kind):
    """Return mutated source, or None when the anchor did not match exactly once."""
    if kind == "C0":
        if src.count(DIFF_MEMO) != 1:
            return None
        return src.replace(DIFF_MEMO, "    // control C0: an inert comment\n" + DIFF_MEMO)

    if kind == "C1":  # the defect exactly as measured at 7bfa1cf
        if src.count(DIFF_MEMO) != 1:
            return None
        return src.replace(DIFF_MEMO, DEFECT)

    if kind == "C2":  # the title printed OUTSIDE the diff — the rejected alternative
        if src.count(DIFF_MEMO) != 1:
            return None
        return src.replace(DIFF_MEMO, BYPASS)

    if kind == "C3":  # the title diffed INSTEAD of the content
        if src.count(VERSION_TEXT_BODY) != 1:
            return None
        return src.replace(VERSION_TEXT_BODY, TITLE_ONLY)

    if kind == "C5":  # the panel heading reworded — must stay green
        if src.count(HEADING) != 1:
            return None
        return src.replace(HEADING, "            Comparing v{from} → v{to}")

    if kind == "C6":  # the +/- marking dropped from the rendered rows
        if src.count(MARKERS) != 1:
            return None
        return src.replace(MARKERS, '                {""}')

    raise AssertionError(kind)


CONTROLS = [
    ("C0", "an inert comment above the diff composition",
     set(),
     "nothing may be caught by a comment"),
    ("C1", "THE DEFECT EXACTLY AS MEASURED AT 7bfa1cf — content diffed, title dropped",
     {"RENAME-VISIBLE", "CONTENT-STILL-DIFFED"},
     "the red-first proof. [NO-PHANTOM-RENAME] must stay GREEN: the pre-fix panel never "
     "reported a rename, it reported nothing, and a control that fired all three would mean "
     "the tags do not separate the two failure directions"),
    ("C2", "the title printed OUTSIDE the diff (an always-added/removed strip)",
     {"NO-PHANTOM-RENAME"},
     "ALONE, and it is the whole reason that assertion exists: this mutation puts both titles "
     "on screen correctly marked, satisfies [RENAME-VISIBLE] and [CONTENT-STILL-DIFFED], and "
     "tells the reader of every ordinary body edit that the document was also renamed"),
    ("C3", "the title diffed INSTEAD of the content",
     {"CONTENT-STILL-DIFFED", "NO-PHANTOM-RENAME"},
     "trading one blind spot for the other must not read as a fix. [NO-PHANTOM-RENAME] fires "
     "through its ANCHOR — a body-only edit now produces NO diff at all, which is exactly the "
     "vacuous state that would let its main assertion pass for free"),
    ("C6", "the +/- marking dropped from the rendered rows",
     {"RENAME-VISIBLE", "NO-PHANTOM-RENAME", "CONTENT-STILL-DIFFED"},
     "proves the guard reads the RENDERED MARKING and not mere text presence: with the markers "
     "gone both titles are still on screen, as unchanged lines, and a presence-only assertion "
     "would call that a pass"),
    ("C5", "the panel heading reworded to 'Comparing v1 → v2' — MUST STAY GREEN",
     set(),
     "the guard finds the panel structurally (the one <pre>) rather than by its heading text. "
     "A guard that reds on prose teaches its readers it is about wording, and the cheapest way "
     "to quiet it is to change the sentence"),
]


def main():
    original = open(COMP, encoding="utf-8").read()
    before = sha(COMP)

    print("BASELINE: the unmutated tree must be GREEN, or every verdict below is noise.")
    fired, code, out = run_guard()
    if fired or code != 0:
        print("  BASELINE IS NOT CLEAN — tags:", sorted(fired), "exit:", code)
        print(out[-4000:])
        return 1
    print("  baseline clean (no tags, exit 0).\n")

    ok = True
    for kind, what, expect, why in CONTROLS:
        mutated = mutate(original, kind)
        if mutated is None or mutated == original:
            print(f"{kind}: ANCHOR DID NOT MATCH — control not run (a failure, not a skip)")
            ok = False
            continue
        try:
            open(COMP, "w", encoding="utf-8").write(mutated)
            fired, code, out = run_guard()
        finally:
            open(COMP, "w", encoding="utf-8").write(original)
            assert sha(COMP) == before, "RESTORE FAILED — the tree is not as it was"
        verdict = "AS PREDICTED" if fired == expect else "*** DIFFERS FROM PREDICTION ***"
        if fired != expect:
            ok = False
        print(f"{kind}: {what}")
        print(f"     predicted {sorted(expect) or 'NOT CAUGHT'}")
        print(f"     actual    {sorted(fired) or 'NOT CAUGHT'}   {verdict}")
        print(f"     why: {why}\n")

    # ── C4 — THE VACUITY CONTROL ────────────────────────────────────────────────────────
    # The defect restored AND this guard file deleted: the WHOLE frontend suite must be green.
    # That is the state main was in at 7bfa1cf, and it is the only thing that establishes this
    # file is load-bearing rather than one more green test beside a live defect.
    print("C4: the defect restored with the guard file DELETED — the whole frontend suite")
    guard_src = open(GUARD_FILE, encoding="utf-8").read()
    guard_before = sha(GUARD_FILE)
    mutated = mutate(original, "C1")
    try:
        open(COMP, "w", encoding="utf-8").write(mutated)
        os.remove(GUARD_FILE)
        p = subprocess.run(["npx", "vitest", "run"], cwd=FRONTEND, env=dict(os.environ),
                           capture_output=True, text=True, timeout=1800)
        vac_code, vac_out = p.returncode, p.stdout + p.stderr
    finally:
        open(GUARD_FILE, "w", encoding="utf-8").write(guard_src)
        open(COMP, "w", encoding="utf-8").write(original)
        assert sha(COMP) == before, "RESTORE FAILED — the component is not as it was"
        assert sha(GUARD_FILE) == guard_before, "RESTORE FAILED — the guard is not as it was"
    good = vac_code == 0
    print(f"     predicted NOT CAUGHT (exit 0) — nothing else in the repo can see this")
    print(f"     actual    exit={vac_code}   {'AS PREDICTED' if good else '*** DIFFERS ***'}")
    if not good:
        ok = False
        print(vac_out[-3000:])
    print("     why: the vacuity floor. If the suite goes red here, something else already "
          "covered the defect and this file is not the guard it claims to be.\n")

    print("ALL CONTROLS AS PREDICTED" if ok else "AT LEAST ONE CONTROL DIFFERED — read it")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
