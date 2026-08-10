#!/usr/bin/env python3
"""Positive controls for the offline read-routing guard (W3.1, the cache class).

Same protocol as scripts/w31-search-render-controls.py: mutate the PRODUCT only, assert every
anchor count BEFORE any write, verify the bytes moved ON DISK, name a must-red target AND a
must-stay-green companion, require tsc to stay clean, restore to the pristine sha256 after each.

THE COMPANION IS src/router/layout-smoke.test.tsx, and it is the right one for this seam rather
than a convenient one: it mounts the REAL chrome with `fetch` STUBBED TO REJECT — i.e. it drives
this exact offline path — and asserts only that nothing throws. So it was green through the whole
life of a cache that answered every sub-resource with a document, and it stays green through
every reintroduction. A guard's companion should be the thing that ALREADY could not see the
defect, and here that is demonstrable rather than asserted.
"""
import hashlib
import os
import shutil
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FE = os.path.join(ROOT, "frontend")
C = os.path.join(FE, "src", "api", "client.ts")
PRODUCT = [C]

T = "src/api/client.offline-routing.test.ts"
COMPANION = ("src/router/layout-smoke.test.tsx", None)

SUB_COMMENTS = (T, "offline GET /v1/spaces/s1/pages/p1/comments does not resolve")
SUB_APPROVAL = (T, "offline GET /v1/spaces/s1/pages/p1/approval does not resolve")
PAGE_OK = (T, "the page itself is still served from the cache")
PAGE_QS = (T, "the page itself is still served when the path carries a query string")
LIST_OK = (T, "the page list is still served from the cache")
SPACES_OK = (T, "the space list is still served from the cache")
MISS_OK = (T, "a path with no /pages/ segment was always a miss")
WRITE_NO = (T, "a sub-resource response is never written into the pages store")
WRITE_YES = (T, "the page response IS still written into the pages store")

DETAIL = "const PAGE_DETAIL = /^\\/v1\\/spaces\\/([^/]+)\\/pages\\/([^/?]+)(?:\\?|$)/;"
LIST = "const PAGE_LIST = /^\\/v1\\/spaces\\/([^/]+)\\/pages(?:\\?|$)/;"
SPACES = "const SPACE_LIST = /^\\/v1\\/spaces(?:\\?|$)/;"
ORDER = """  const pageMatch = path.match(PAGE_DETAIL);
  if (pageMatch) {
    const page = await getCachedPage(pageMatch[2]);
    return (page as unknown as T) ?? null;
  }
  const listMatch = path.match(PAGE_LIST);
  if (listMatch) {
    const pages = await getCachedPages(listMatch[1]);
    return (pages as unknown as T) ?? null;
  }"""
ORDER_SWAPPED = """  const listMatch = path.match(PAGE_LIST);
  if (listMatch) {
    const pages = await getCachedPages(listMatch[1]);
    return (pages as unknown as T) ?? null;
  }
  const pageMatch = path.match(PAGE_DETAIL);
  if (pageMatch) {
    const page = await getCachedPage(pageMatch[2]);
    return (page as unknown as T) ?? null;
  }"""
SHOULD = "  return PAGE_DETAIL.test(path) || SPACE_LIST.test(path);"

CONTROLS = [
    {
        "id": "H1",
        "what": "MAIN REPRODUCED: the page pattern unanchored at the end, exactly as shipped",
        "edits": [(C, DETAIL,
                   "const PAGE_DETAIL = /\\/v1\\/spaces\\/([^/]+)\\/pages\\/([^/?]+)/;", 1)],
        "must_red": SUB_COMMENTS,
        "extra_green": [PAGE_OK, LIST_OK],
    },
    {
        "id": "H2",
        "what": "anchored with a bare `$` — a query string stops identifying the resource",
        # THE MUTATION ONLY THE QUERY-STRING CASE CAN SEE. Every other case has no query, so
        # `$` and `(?:\?|$)` agree on all of them; this is what earns that case rather than
        # letting it ride along as decoration.
        "edits": [(C, DETAIL,
                   "const PAGE_DETAIL = /^\\/v1\\/spaces\\/([^/]+)\\/pages\\/([^/?]+)$/;", 1)],
        "must_red": PAGE_QS,
        "extra_green": [PAGE_OK, SUB_COMMENTS],
    },
    {
        "id": "H3",
        "what": "the LIST pattern left unanchored — the second branch answers for a sub-resource",
        # WHAT EARNS THE SECOND ANCHOR. With only the detail pattern fixed, `/pages/p1/comments`
        # simply falls through to the page-LIST branch and comes back as Page[] instead of a
        # Page — a different wrong answer to the same question, and one a fix aimed at the first
        # regex alone would ship.
        "edits": [(C, LIST, "const PAGE_LIST = /^\\/v1\\/spaces\\/([^/]+)\\/pages/;", 1)],
        "must_red": SUB_APPROVAL,
        "extra_green": [PAGE_OK, LIST_OK],
    },
    {
        "id": "H4",
        "what": "the two page branches tried in the other order — INERT, and that is the result",
        # PREDICTED CAUGHT, MEASURED NOT CAUGHT, AND THE PREDICTION WAS THE THING THAT WAS WRONG.
        # I expected ordering to be load-bearing: a detail path has a list path as its prefix, so
        # trying the list first "obviously" answers for it. It does not. PAGE_LIST ends at
        # `(?:\?|$)`, so `/pages/p1` cannot match it however early it is tried — the ANCHORS do
        # the work and the order is readability alone. Kept and scored as an EXPECTED NOT CAUGHT
        # because that is exactly what it proves, and the comment in client.ts that asserted the
        # opposite was corrected rather than this control retargeted at whatever would pass.
        "edits": [(C, ORDER, ORDER_SWAPPED, 1)],
        "must_red": PAGE_OK,
        "expect": "NOT CAUGHT",
        "extra_green": [SUB_COMMENTS, MISS_OK],
    },
    {
        "id": "H5",
        "what": "readCached always misses — the cache deleted rather than corrected",
        # ONE-DIRECTIONAL, AND WHAT EARNS EVERY POSITIVE CASE. "Never answer from the cache"
        # satisfies all seven sub-resource assertions perfectly, so without the positives the
        # repair for this defect would be removing the offline feature.
        "edits": [(C, "  const pageMatch = path.match(PAGE_DETAIL);",
                   "  if (path) return null;\n  const pageMatch = path.match(PAGE_DETAIL);", 1)],
        "must_red": PAGE_OK,
        "extra_green": [SUB_COMMENTS, SUB_APPROVAL],
    },
    {
        "id": "H6",
        "what": "the write half drifts back — BOTH of its checks widened",
        # The two halves differing by one character is the CAUSE here, not a symptom, so the
        # write half needs a control of its own. Under this, an approval response is written
        # into the `pages` object store and getCachedPage will later hand it back as a Page.
        # TWO EDITS BECAUSE ONE IS NOT ENOUGH — see H6b, which is that same mutation with only
        # the first half applied and is EXPECTED NOT CAUGHT.
        "edits": [(C, SHOULD,
                   "  return /\\/v1\\/spaces\\/[^/]+\\/pages\\/[^/]+/.test(path) || "
                   "SPACE_LIST.test(path);", 1),
                  (C, "    if (PAGE_DETAIL.test(path)) {",
                   "    if (/\\/v1\\/spaces\\/[^/]+\\/pages\\/[^/]+/.test(path)) {", 1)],
        "must_red": WRITE_NO,
        "extra_green": [WRITE_YES, SUB_COMMENTS],
    },
    {
        "id": "H6b",
        "what": "only shouldCache widened — INERT, because the write path is guarded twice",
        # PREDICTED CAUGHT, MEASURED NOT CAUGHT, AND WORTH SHIPPING AS THE MEASUREMENT IT IS.
        # Widening shouldCache alone lets a sub-resource REACH writeCache, and writeCache's own
        # PAGE_DETAIL test then refuses it, so nothing is written and WRITE_NO stays green. The
        # invariant is enforced at two points, so no ONE-LINE reintroduction can breach it —
        # which is a property of the fix worth recording, not a hole in the guard. H6 is the
        # same mutation applied at both points, and it IS caught.
        "edits": [(C, SHOULD,
                   "  return /\\/v1\\/spaces\\/[^/]+\\/pages\\/[^/]+/.test(path) || "
                   "SPACE_LIST.test(path);", 1)],
        "must_red": WRITE_NO,
        "expect": "NOT CAUGHT",
        "extra_green": [WRITE_YES, SUB_COMMENTS],
    },
    {
        "id": "H7",
        "what": "shouldCache caches nothing — the write half deleted rather than narrowed",
        # ONE-DIRECTIONAL COMPANION TO H6. Without WRITE_YES, "never cache" passes WRITE_NO.
        "edits": [(C, SHOULD, "  return false && (PAGE_DETAIL.test(path) || "
                              "SPACE_LIST.test(path));", 1)],
        "must_red": WRITE_YES,
        "extra_green": [WRITE_NO, PAGE_OK],
    },
    {
        "id": "H8",
        "what": "the SPACE pattern unanchored — a space sub-resource is answered with spaces",
        # THE THIRD PATTERN, AND WHAT TURNS THE MEASUREMENT'S POSITIVE CONTROL INTO A GUARD.
        # `/v1/spaces/s1/permissions` is the path that behaved CORRECTLY before this merge and
        # so proved the probe was reading real routing; unanchored, it now matches SPACE_LIST
        # and a permissions request is answered with the list of spaces.
        "edits": [(C, SPACES, "const SPACE_LIST = /^\\/v1\\/spaces/;", 1)],
        "must_red": MISS_OK,
        "extra_green": [SPACES_OK, PAGE_OK],
    },
]


def sha(p):
    with open(p, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def run(target):
    path, name = target
    cmd = ["npx", "vitest", "run", path]
    if name:
        cmd += ["-t", name]
    p = subprocess.run(cmd, cwd=FE, capture_output=True, text=True)
    out = p.stdout + p.stderr
    if name and ("No test found" in out or "no tests" in out.lower()):
        return None, out
    return p.returncode == 0, out.strip()


def typecheck():
    return subprocess.run(["npm", "run", "typecheck"], cwd=FE,
                          capture_output=True, text=True).returncode == 0


def main():
    tmp = tempfile.mkdtemp(prefix="w31-offline-pristine-")
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

    targets = [COMPANION, SUB_COMMENTS, SUB_APPROVAL, PAGE_OK, PAGE_QS, LIST_OK, SPACES_OK,
               MISS_OK, WRITE_NO, WRITE_YES]
    for tgt in targets:
        ok, out = run(tgt)
        if ok is None:
            print("PRECONDITION FAILED: target %r matched no test — the filter is wrong." % (tgt,))
            return 2
        if not ok:
            print("PRECONDITION FAILED: %r is not green before mutation.\n%s" % (tgt, out[-2000:]))
            return 2
    if not typecheck():
        print("PRECONDITION FAILED: the pristine tree does not typecheck.")
        return 2
    print("precondition: %d targets matched a real test, all green, tsc clean, before any "
          "control ran\n" % len(targets))

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

        for path, old, new, _ in c["edits"]:
            with open(path) as fh:
                src = fh.read()
            with open(path, "w") as fh:
                fh.write(src.replace(old, new))

        applied = all(new in open(path).read() for path, _o, new, _w in c["edits"]) and any(
            sha(p) != saved[p][1] for p in PRODUCT)
        if not applied:
            print("%s WRITE NOT APPLIED" % c["id"])
            restore()
            results.append((c["id"], "SUSPECT (write)", c["what"]))
            continue

        red_ok, red_out = run(c["must_red"])
        green_ok, green_out = run(COMPANION)
        tc_ok = typecheck()
        extra_bad = None
        for eg in c.get("extra_green", []):
            eok, _ = run(eg)
            if not eok:
                extra_bad = eg
                break

        failed = restore()
        if failed:
            print("%s RESTORE FAILED for %s — stop and inspect the tree" % (c["id"], failed))
            return 2

        if red_ok is None:
            verdict = "SUSPECT (must-red target matched no test)"
        elif not tc_ok:
            verdict = "SUSPECT (tree does not typecheck — a build break, not a behaviour)"
        elif not green_ok:
            verdict = "SUSPECT (companion red — the mutation broke the mount)"
            print("  %s companion:\n%s" % (c["id"], green_out[-600:]))
        elif extra_bad:
            verdict = "SUSPECT (a case that must be blind to this mutation reddened: %s)" % (
                extra_bad[1],)
        else:
            got = "CAUGHT" if not red_ok else "NOT CAUGHT"
            verdict = got if got == expect else "%s (EXPECTED %s)" % (got, expect)
            if not red_ok:
                hit = [l for l in red_out.splitlines()
                       if "AssertionError" in l or "Error:" in l or "TypeError" in l]
                if hit:
                    print("  %s red: %s" % (c["id"], hit[0].strip()[:200]))
        results.append((c["id"], verdict, c["what"]))

    print("\n" + "=" * 104)
    ok = True
    for cid, verdict, what in results:
        print("%-5s %-26s %s" % (cid, verdict, what))
        if "SUSPECT" in verdict or "EXPECTED" in verdict:
            ok = False
    print("=" * 104)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
