#!/usr/bin/env python3
"""Positive controls for the import-title fix (W3.1, tab-4d81).

WHY THIS EXISTS. The new tests in internal/importer/titlesource_test.go were RED before the
fix and are GREEN after it, which is necessary and not sufficient: it says the guard moved,
never that it can move for the right reasons or that it is the ONLY thing holding the
property. Each control below mutates ONE thing, names the test it predicts will catch it
BEFORE the run, and runs the FULL suite so "caught by that one" is measured rather than
assumed.

TWO of the controls must NOT be caught, deliberately:
  · C5 removes the new test file with C1's defect on top — if anything else in the repo
    could see this, C5 would still be red, and "nothing else sees it" would be a guess.
  · C6 mutates an unrelated block rule — if the new tests caught that too, CAUGHT would be
    a catch-all rather than an attribution.

Every mutation is restored in a `finally` and the restore is verified by sha256, so a crash
mid-run cannot leave a mutated tree behind pretending to be main.

Run:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-importtitle-controls-4d81.py
"""

import hashlib
import os
import pathlib
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
SRC = REPO / "internal/importer/importer.go"
TEST = REPO / "internal/importer/titlesource_test.go"

DSN = os.environ.get("DOCS_TEST_DATABASE_URL")
if not DSN:
    sys.exit("DOCS_TEST_DATABASE_URL must be set — the real-PG tests FAIL rather than skip "
             "without it, which would make every control 'caught' for the wrong reason.")


def sha(p: pathlib.Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def run_suite() -> tuple[int, list[str]]:
    """Full suite. Returns (go test's OWN exit code, failing test names).

    ⚠ THE EXIT CODE IS go test's, READ FROM Popen, NOT A PIPELINE'S. A previous session in
    this repo recorded TEST_EXIT=1 on a healthy tree because it had captured `grep`'s status
    through a pipe, and grep exits 1 exactly when it prints nothing — i.e. when everything
    passed. The filter runs on the captured text, after the code is already in hand.
    """
    proc = subprocess.run(
        ["go", "test", "-timeout", "300s", "-count=1", "./..."],
        cwd=REPO, env={**os.environ, "DOCS_TEST_DATABASE_URL": DSN},
        capture_output=True, text=True,
    )
    failed = sorted({
        line.split("--- FAIL: ")[1].split(" ")[0]
        for line in (proc.stdout + proc.stderr).splitlines()
        if "--- FAIL: " in line
    })
    return proc.returncode, failed


# (id, description, predicted catcher, file, before, after, must_be_caught)
CONTROLS = [
    (
        "C1",
        "the DEFECT ITSELF: firstTitleText skips the <head> subtree, exactly as the block "
        "walker did — a head-declared <title> becomes unreachable again",
        "TestImportTitle_HeadTitleWinsOverH1 + TestImportTitle_HeadTitleUsedWhenNoH1 "
        "(and NOT BodyDeclaredTitleAlsoCounts — the body placement never went through <head>)",
        SRC,
        "\tfor c := n.FirstChild; c != nil; c = c.NextSibling {\n\t\tif t := firstTitleText(c); t != \"\" {",
        "\tfor c := n.FirstChild; c != nil; c = c.NextSibling {\n\t\tif c.Type == html.ElementNode && c.Data == \"head\" {\n\t\t\tcontinue\n\t\t}\n\t\tif t := firstTitleText(c); t != \"\" {",
        True,
    ),
    (
        "C2",
        "PRIORITY INVERTED: the first <h1> wins over <title> — the order the tree shipped "
        "with, arrived at by accident rather than by choice",
        "TestImportTitle_HeadTitleWinsOverH1 ONLY (with no <h1> present the two orders agree, "
        "so HeadTitleUsedWhenNoH1 must stay green)",
        SRC,
        "\ttitle := firstTitleText(doc)\n\tif title == \"\" {\n\t\ttitle = w.firstH1\n\t}",
        "\ttitle := w.firstH1\n\tif title == \"\" {\n\t\ttitle = firstTitleText(doc)\n\t}",
        True,
    ),
    (
        "C3",
        "the FILENAME fallback removed — a page with neither source imports untitled",
        "TestImportTitle_FilenameWhenNeitherPresent ONLY",
        SRC,
        "\tif title == \"\" {\n\t\tbase := path.Base(filename)\n\t\ttitle = strings.TrimSuffix(base, path.Ext(base))\n\t}",
        "\tif title == \"\" && false {\n\t\tbase := path.Base(filename)\n\t\ttitle = strings.TrimSuffix(base, path.Ext(base))\n\t}",
        True,
    ),
    (
        "C4",
        "the LAST <h1> wins instead of the first",
        "TestImportTitle_FirstH1WhenNoHeadTitle ONLY",
        SRC,
        "if level == 1 && w.firstH1 == \"\" {",
        "if level == 1 {",
        True,
    ),
    (
        "C5",
        "VACUITY: C1's defect with the whole new test file DELETED — is anything ELSE in "
        "this repository able to see an unreachable title source?",
        "NOTHING. Predicted NOT CAUGHT: the suite goes green with the defect in the tree, "
        "which is the state main was in at 5591566",
        None,  # handled specially: C1's mutation + delete TEST
        None,
        None,
        False,
    ),
    (
        "C6",
        "MUST-STAY-GREEN COMPANION: <hr> stops emitting a horizontal_rule block — an "
        "unrelated block rule in the same walker",
        "NOT the title tests. Any catcher must be a pre-existing content test, never one of "
        "the six added here",
        SRC,
        "\t\tcase \"hr\":\n\t\t\tw.blocks = append(w.blocks, map[string]any{\"type\": \"horizontal_rule\"})\n\t\t\treturn",
        "\t\tcase \"hr\":\n\t\t\treturn",
        False,
    ),
]

TITLE_TESTS = {
    "TestImportTitle_HeadTitleWinsOverH1",
    "TestImportTitle_HeadTitleUsedWhenNoH1",
    "TestImportTitle_FirstH1WhenNoHeadTitle",
    "TestImportTitle_FilenameWhenNeitherPresent",
    "TestImportTitle_BodyDeclaredTitleAlsoCounts",
    "TestImportTitle_HeadContentStillNotImportedAsBlocks",
}

def main() -> int:
    src_sha, test_sha = sha(SRC), sha(TEST)

    print("C0  baseline — the tree as it will be merged, no mutation")
    code, failed = run_suite()
    print(f"    exit={code} failed={failed or 'none'}")
    if code != 0:
        print("    ⚠ BASELINE IS RED. Every control below would be 'caught' by the same "
              "failure, which measures nothing. Stopping.")
        return 1

    results = []
    for cid, what, predicted, target, before, after, must_catch in CONTROLS:
        print(f"\n{cid}  {what}")
        print(f"    PREDICTED CATCHER (named before the run): {predicted}")
        src_body, test_body = SRC.read_text(), TEST.read_text()
        try:
            if cid == "C5":
                c1 = CONTROLS[0]
                assert src_body.count(c1[4]) == 1, "C5: C1's anchor is not unique"
                SRC.write_text(src_body.replace(c1[4], c1[5]))
                TEST.unlink()
            else:
                assert src_body.count(before) == 1, f"{cid}: anchor matched {src_body.count(before)}× — not 1"
                target.write_text(src_body.replace(before, after))
            code, failed = run_suite()
        finally:
            SRC.write_text(src_body)
            if not TEST.exists():
                TEST.write_text(test_body)
            assert sha(SRC) == src_sha, f"{cid}: {SRC} NOT restored"
            assert sha(TEST) == test_sha, f"{cid}: {TEST} NOT restored"

        caught = code != 0
        by_title = sorted(set(failed) & TITLE_TESTS)
        by_other = sorted(set(failed) - TITLE_TESTS)
        verdict = "CAUGHT" if caught else "NOT CAUGHT"
        ok = caught == must_catch
        results.append((cid, verdict, ok))
        print(f"    {verdict}  exit={code}")
        print(f"    by the new tests : {by_title or 'none'}")
        print(f"    by other tests   : {by_other or 'none'}")
        print(f"    {'AS PREDICTED' if ok else '⚠ NOT AS PREDICTED — the control, or the guard, is wrong'}")

    print("\n── summary ──")
    for cid, verdict, ok in results:
        print(f"  {cid}: {verdict} {'✓' if ok else '✗ UNPREDICTED'}")
    print(f"  restore verified by sha256: {SRC.name} + {TEST.name} both match pre-run")
    return 0 if all(ok for _, _, ok in results) else 1


if __name__ == "__main__":
    sys.exit(main())
