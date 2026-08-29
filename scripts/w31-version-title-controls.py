#!/usr/bin/env python3
"""Positive controls for the version-snapshot title/content pairing guards (W3.1).

Same protocol as scripts/w31-offline-routing-controls.py, re-implemented for a Go seam rather
than reused: assert every anchor count BEFORE any write, verify the bytes moved ON DISK, name a
must-red target AND a must-stay-green companion, restore to the pristine sha256 after each, and
read every red BY ASSERTION TEXT rather than by exit code. The catcher for each control is
PREDICTED here in `must_red` before the run.

WHY ASSERTION TEXT AND NOT EXIT CODE: these guards call t.Fatalf on setup failures. A mutation
that breaks the seed save would exit non-zero WITHOUT the assertion under test ever executing,
and a list of failing test names cannot tell that apart from a real catch. Each target therefore
names the substring its own failure must contain.

THE COMPANION IS TestVersions_OwnerCanGetAndDiff_RealPG, and it is the right one for this seam
rather than a convenient one: it drives Update twice and then the get-one and diff endpoints end
to end through the real /v1 chain — i.e. it exercises exactly the code that writes and reads
these rows — but every save it makes changes ONLY the content, so the pre-update title and the
post-update title are the same string and it was green through this defect's entire life. It
stays green through every reintroduction below. A guard's companion should be the thing that
ALREADY could not see the defect, and here that is demonstrable rather than asserted.

THE MOCK TEST IS LISTED AS A THIRD TARGET ON PURPOSE: TestUpdate_AppendsNewVersionOnContentChange
needs no Postgres, so it is the cheap catcher. It was blind before this merge because BOTH rows
it handed back were titled "Old"; it now returns "New" from the UPDATE ... RETURNING row, which
is the only source the snapshot may use.
"""
import hashlib
import os
import shutil
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE = os.path.join(ROOT, "internal", "page", "store.go")
PRODUCT = [STORE]

DSN = os.environ.get("DOCS_TEST_DATABASE_URL")

# (go -run regex, substring the red output MUST contain)
PAIRING = ("TestVersionSnapshot_PairsTitleAndContentFromTheSameSave_RealPG", "MISPAIRED SNAPSHOT")
CONTENT_ONLY = ("TestVersionSnapshot_ContentOnlySave_KeepsTheCurrentTitle_RealPG",
                "CONTENT-ONLY SAVE MISSNAPSHOTTED")
RESTORE = ("TestRestoreNewestVersion_LeavesTheLivePageUnchanged_RealPG", "DESTRUCTIVE RESTORE")
MOCK = ("TestUpdate_AppendsNewVersionOnContentChange", "")
COMPANION = ("TestVersions_OwnerCanGetAndDiff_RealPG", "")

# ⚠ THE SNAPSHOT IS NO LONGER AN INLINE INSERT IN Update, AND THE OLD ANCHOR NAMED ITS ARGUMENT
# LIST. becf0f8 ("overlapping saves no longer lose restore points", 2026-08-13) moved the write
# into Store.appendVersion and derives the version number IN SQL —
#     INSERT INTO page_versions (...) SELECT $1, $2, COALESCE(MAX(version),0)+1, $3, $4, $5
# — so `nextVer` does not occur in this file at all any more and neither does the argument list.
# THE SEAM ITSELF SURVIVED INTACT: the title and the content are still chosen at ONE call site,
# still both taken from `out` (the row the UPDATE returned), and that choice is still exactly what
# these controls mutate. The anchor is the call, and the replacements are whole call lines.
APPEND_CALL = "\t\ts.appendVersion(ctx, id, out.WorkspaceID, out.Title, out.Content, updatedBy)"
CLOSED_SET = "\t// The closed set is enforced on BOTH write paths"

# The shipped defect, verbatim: a pre-update SELECT supplies the title while the content comes
# from the row the UPDATE returned.
PRE_READ_BLOCK = """\tvar existing *model.Page
\tif contentChanged {
\t\tgot, err := s.GetByID(ctx, id)
\t\tif err != nil {
\t\t\treturn nil, fmt.Errorf("page: pre-update read: %w", err)
\t\t}
\t\texisting = got
\t}

"""

RESTORE_TITLE = '\t\t"title":      title,'

CONTROLS = [
    {
        "id": "V1",
        "what": "MAIN REPRODUCED: the title read from a pre-update SELECT, exactly as shipped",
        # Two edits, and BOTH are required for the mutation to mean anything — reinstating the
        # pre-read without consuming it does not compile (Go rejects the unused variable), and
        # changing the arg without the pre-read does not compile either. The harness asserts both
        # anchors before either write and re-reads the file between them, so the second write
        # cannot silently erase the first.
        # The pre-read still goes in ahead of the UPDATE (CLOSED_SET is at store.go:862, the
        # UPDATE ... RETURNING at :917), which is what makes this a pre-update title rather than
        # a second read of the row that was just written.
        "edits": [(STORE, CLOSED_SET, PRE_READ_BLOCK + CLOSED_SET, 1),
                  (STORE, APPEND_CALL,
                   "\t\ts.appendVersion(ctx, id, out.WorkspaceID, existing.Title, out.Content, updatedBy)", 1)],
        "must_red": PAIRING,
        "also_red": [RESTORE, MOCK],
        "extra_green": [CONTENT_ONLY],
    },
    {
        "id": "V2",
        "what": "the title read from the REQUEST MAP instead of the saved row",
        # THE MUTATION ONLY THE CONTENT-ONLY GUARD CAN SEE, and the reason that guard exists.
        # updates["title"] equals out.Title on every save that renames, so the pairing guard —
        # whose three saves all rename — stays GREEN. On a save with no "title" key the map
        # yields "" and the snapshot records an empty name. This is the shape a fix written from
        # the request rather than from the result would take, and it is invisible to the guard
        # that caught the original defect.
        # NOTE ON THE SECOND ANCHOR, KEPT BECAUSE THE HAZARD IT RECORDS IS STILL LIVE:
        # `updatedBy, _ := updates["updated_by"].(string)` occurs TWICE in this function (the
        # snapshot and the linker block), so the bare line is not a unique anchor and the harness
        # refused it as an ANCHOR MISS rather than editing the wrong one. It used to be anchored
        # to the preceding `nextVer++`, and becf0f8 deleted `nextVer` — so the declaration is now
        # folded into the ONE call-site edit below, which needs no second anchor at all.
        "edits": [(STORE, APPEND_CALL,
                   "\t\tsnapTitle, _ := updates[\"title\"].(string)\n"
                   "\t\ts.appendVersion(ctx, id, out.WorkspaceID, snapTitle, out.Content, updatedBy)", 1)],
        "must_red": CONTENT_ONLY,
        "also_red": [MOCK],
        "extra_green": [PAIRING],
    },
    {
        "id": "V3",
        "what": "RestoreVersion writes a title the version does not carry",
        # THE MUTATION ONLY THE RESTORE GUARD CAN SEE. The snapshot seam is untouched, so every
        # stored row stays a real state and the two snapshot guards stay GREEN; what breaks is
        # the write-back, which is the half a reader of page_versions cannot observe at all.
        # This is what earns a separate restore guard instead of trusting the rows.
        "edits": [(STORE, RESTORE_TITLE, '\t\t"title":      title + " (restored)",', 1)],
        "must_red": RESTORE,
        "extra_green": [PAIRING, CONTENT_ONLY],
    },
    {
        "id": "V4",
        "what": "the snapshot title filled from the page's SLUG — a real field, wrong source",
        # A correctly-shaped value from the wrong column, which is the failure mode the whole
        # W3.1 item has been chasing across five surfaces. Distinct from V1: it is not a lag, so
        # no amount of "compare against the previous row" reasoning finds it.
        "edits": [(STORE, APPEND_CALL,
                   "\t\ts.appendVersion(ctx, id, out.WorkspaceID, out.Slug, out.Content, updatedBy)", 1)],
        "must_red": PAIRING,
        "also_red": [CONTENT_ONLY, MOCK],
        "extra_green": [],
    },
    {
        "id": "V5",
        "what": "created_by upper-cased — NOT CAUGHT, one-directional by design",
        # STATED LIMIT, MEASURED RATHER THAN IMPLIED. These guards assert the title/content
        # PAIRING and the restore write-back. They say nothing about who a version says wrote it.
        # Scored NOT CAUGHT on purpose: it is what proves the predicate DISCRIMINATES instead of
        # reddening at any edit to this INSERT, and it names the coverage this merge does not add.
        # IT MUST CHANGE BEHAVIOUR, NOT BREAK THE BUILD: the first version of this control passed
        # a literal "" and left `updatedBy` declared-and-unused, which Go rejects — a compile
        # error would have been scored as a caught mutation. strings.ToUpper keeps every symbol
        # live and still writes a different author than the one the save carried.
        "edits": [(STORE, APPEND_CALL,
                   "\t\ts.appendVersion(ctx, id, out.WorkspaceID, out.Title, out.Content, "
                   "strings.ToUpper(updatedBy))", 1)],
        "must_red": PAIRING,
        "expect": "NOT CAUGHT",
        "extra_green": [CONTENT_ONLY, RESTORE],
    },
]


def sha(p):
    with open(p, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def run(target):
    """Returns (passed, output). passed is None when the filter matched no test."""
    name, _needle = target
    env = dict(os.environ)
    if DSN:
        env["DOCS_TEST_DATABASE_URL"] = DSN
    p = subprocess.run(
        ["go", "test", "-count=1", "-run", "^%s$" % name, "./internal/page/"],
        cwd=ROOT, capture_output=True, text=True, env=env)
    out = (p.stdout + p.stderr).strip()
    if "no tests to run" in out or "no test files" in out:
        return None, out
    if "build failed" in out or "[build failed]" in out or "cannot use" in out:
        return "BUILD", out
    return p.returncode == 0, out


def main():
    if not DSN:
        print("DOCS_TEST_DATABASE_URL is not set — these controls need the real Postgres the "
              "guards need. Refusing to run and report a verdict from tests that never ran.")
        return 2

    tmp = tempfile.mkdtemp(prefix="w31-version-title-pristine-")
    saved = {}
    for p in PRODUCT:
        dst = os.path.join(tmp, os.path.basename(p))
        shutil.copyfile(p, dst)
        saved[p] = (dst, sha(p))

    def restore():
        for p, (dst, want) in saved.items():
            shutil.copyfile(dst, p)
            if sha(p) != want:
                return p
        return None

    targets = [COMPANION, PAIRING, CONTENT_ONLY, RESTORE, MOCK]
    for tgt in targets:
        ok, out = run(tgt)
        if ok is None:
            print("PRECONDITION FAILED: %r matched no test — the -run filter is wrong." % (tgt[0],))
            return 2
        if ok == "BUILD" or not ok:
            print("PRECONDITION FAILED: %r is not green before mutation.\n%s"
                  % (tgt[0], out[-2000:]))
            return 2
    print("precondition: %d targets matched a real test and all are green before any control "
          "ran\n" % len(targets))

    results = []
    for c in CONTROLS:
        expect = c.get("expect", "CAUGHT")
        bad = False
        for path, old, _new, want in c["edits"]:
            with open(path) as fh:
                n = fh.read().count(old)
            if n != want:
                print("%s ANCHOR MISS in %s: %d occurrences, want %d"
                      % (c["id"], os.path.basename(path), n, want))
                bad = True
        if bad:
            results.append((c["id"], "SUSPECT (anchor)", c["what"]))
            continue

        # Re-read between edits so a second write cannot erase the first.
        for path, old, new, _ in c["edits"]:
            with open(path) as fh:
                src = fh.read()
            with open(path, "w") as fh:
                fh.write(src.replace(old, new, 1))

        applied = all(new in open(path).read() for path, _o, new, _w in c["edits"]) and any(
            sha(p) != saved[p][1] for p in PRODUCT)
        if not applied:
            print("%s WRITE NOT APPLIED" % c["id"])
            restore()
            results.append((c["id"], "SUSPECT (write)", c["what"]))
            continue

        red_ok, red_out = run(c["must_red"])
        green_ok, green_out = run(COMPANION)
        # Measured INSIDE the mutated window — these are secondary predictions about which OTHER
        # guards also see this mutation. Reported, never used to decide the verdict: a prediction
        # that misses must be visible, not silently absorbed.
        also = [(t[0], run(t)[0]) for t in c.get("also_red", [])]
        extra_bad = None
        for eg in c.get("extra_green", []):
            eok, _ = run(eg)
            if eok is not True:
                extra_bad = eg
                break

        failed = restore()
        if failed:
            print("%s RESTORE FAILED for %s — stop and inspect the tree" % (c["id"], failed))
            return 2

        needle = c["must_red"][1]
        if red_ok is None:
            verdict = "SUSPECT (must-red target matched no test)"
        elif red_ok == "BUILD" or green_ok == "BUILD":
            verdict = "SUSPECT (tree does not build — a compile error, not a behaviour)"
        elif green_ok is not True:
            verdict = "SUSPECT (companion red — the mutation broke more than the seam)"
            print("  %s companion:\n%s" % (c["id"], green_out[-800:]))
        elif extra_bad:
            verdict = "SUSPECT (a guard that must be blind to this mutation reddened: %s)" % (
                extra_bad[0],)
        elif red_ok is False and needle and needle not in red_out:
            verdict = "SUSPECT (red, but NOT by the assertion under test — no %r)" % needle
            print("  %s red-but-wrong-reason:\n%s" % (c["id"], red_out[-800:]))
        else:
            got = "CAUGHT" if red_ok is False else "NOT CAUGHT"
            verdict = got if got == expect else "%s (EXPECTED %s)" % (got, expect)
            if red_ok is False:
                hit = [l for l in red_out.splitlines() if needle and needle in l]
                if hit:
                    print("  %s red: %s" % (c["id"], hit[0].strip()[:220]))
        for name, ok_ in also:
            state = {True: "stayed GREEN", False: "also red"}.get(ok_, "inconclusive(%s)" % ok_)
            flag = "" if ok_ is False else "   <-- PREDICTED ALSO-RED AND WAS NOT"
            print("  %s also_red %s: %s%s" % (c["id"], name, state, flag))
        results.append((c["id"], verdict, c["what"]))

    print("\n" + "=" * 104)
    ok = True
    for cid, verdict, what in results:
        print("%-4s %-30s %s" % (cid, verdict, what))
        if "SUSPECT" in verdict or "EXPECTED" in verdict:
            ok = False
    print("=" * 104)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
