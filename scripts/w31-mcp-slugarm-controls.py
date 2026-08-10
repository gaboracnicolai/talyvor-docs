#!/usr/bin/env python3
"""
Positive controls for internal/mcp/getpage_slug_arm_realpg_test.go.

⚠ THE GUARD WAS GREEN THE FIRST TIME IT RAN. It closes a COVERAGE gap — of the 14 `get_page` calls
across every test in internal/mcp, ZERO supplied a slug — rather than a live defect. A guard in
that state is worth exactly what its controls prove and no more.

EACH CONTROL ALSO CARRIES A BLIND-GUARD CLAIM: the pre-existing MCP tenancy tests that must stay
GREEN. Those tests drive the same tool with the same intent and cannot see a slug-arm mutation,
because they never take that arm. A control that reddens them is not slug-exclusive, and is
recorded as justifying the new file only partially rather than being quietly counted.

Verdict = the SET of assertion tags that fired, predicted before the run. BUILD and PANIC are
scored explicitly and never as a catch. Restores are `cp` from bytes saved before the run, in a
`finally`, sha256-compared at the end.
"""

import hashlib
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile

REPO = pathlib.Path(__file__).resolve().parent.parent
DSN = os.environ.get(
    "DOCS_TEST_DATABASE_URL",
    "postgres://postgres:postgres@localhost:55435/postgres?sslmode=disable",
)
SERVER = "internal/mcp/server.go"
STORE = "internal/page/store.go"
FILES = [SERVER, STORE]
PKG = "./internal/mcp/"

# The pre-existing id-arm tenancy tests. Named, not counted.
BLIND_GUARDS = [
    "TestSEC4_MCP_ArgTrustAndCrossTenant",
    "TestSEC4_MCP_FailClosed",
    "TestMCPReadTools_PrivateSpace_NotVisibleWithoutGrant_RealPG",
]

TAG_RE = re.compile(r"\[(S-[A-Z-]+)\]")
FAIL_RE = re.compile(r"^\s*--- FAIL: (\S+)", re.M)
BUILD_RE = re.compile(r"^\S+\.go:\d+:\d+: |\[build failed\]|build failed", re.M)
PANIC_RE = re.compile(r"^panic: |^\[signal SIG", re.M)


def sh(args):
    return subprocess.run(args, cwd=REPO, capture_output=True, text=True,
                          env=dict(os.environ, DOCS_TEST_DATABASE_URL=DSN))


def sha256(p):
    return hashlib.sha256((REPO / p).read_bytes()).hexdigest()


def run_tests():
    r = sh(["go", "test", "-timeout", "300s", "-count=1", "-v", PKG])
    out = r.stdout + r.stderr
    if BUILD_RE.search(out):
        return set(), set(), "BUILD", out
    if PANIC_RE.search(out):
        return set(), set(), "PANIC", out
    return set(TAG_RE.findall(out)), set(FAIL_RE.findall(out)), None, out


def mutate(path, old, new, count=1):
    p = REPO / path
    s = p.read_text()
    if s.count(old) != count:
        raise SystemExit(f"ANCHOR MISS in {path}: wanted {count} of {old!r}, found {s.count(old)}")
    p.write_text(s.replace(old, new, count))


SLUG_BRANCH = ('\tcase "get_page":\n'
               '\t\tif pid := stringArg(args, "page_id", ""); pid != "" {\n'
               '\t\t\treturn s.pageWorkspace(ctx, pid)\n'
               '\t\t}\n'
               '\t\treturn s.spaceWorkspace(ctx, stringArg(args, "space_id", ""))\n')
SLUG_BRANCH_DENY = ('\tcase "get_page":\n'
                    '\t\tif pid := stringArg(args, "page_id", ""); pid != "" {\n'
                    '\t\t\treturn s.pageWorkspace(ctx, pid)\n'
                    '\t\t}\n'
                    '\t\treturn "", nil\n')
INTOOL_GATE = """	if !s.canViewPage(ctx, p.ID) {
		return nil, &rpcError{Code: errUnauthorized, Message: "not authorized: this action requires view access"}
	}
"""
GETBYSLUG_SQL = "`SELECT `+columns+` FROM pages WHERE space_id = $1 AND slug = $2`"
GETBYSLUG_GLOBAL = "`SELECT `+columns+` FROM pages WHERE space_id != $1 AND slug = $2`"

CONTROLS = [
    ("C0",
     "MUST STAY GREEN — no mutation.",
     lambda: None, set(), True),

    ("C1",
     "⚠ THE SLUG-EXCLUSIVE LEAK. Make page.Store.GetBySlug ignore the space it was given, so a "
     "slug resolves against a DIFFERENT space. ⚠ THE FIRST VERSION USED `($1 = $1)` — a "
     "global lookup with no ORDER BY, which may return the caller's OWN row, so the control "
     "passed nondeterministically and proved nothing. The id arm never calls GetBySlug, so no "
     "existing test can reach this line — it is the mutation this file exists for.",
     lambda: mutate(STORE, GETBYSLUG_SQL, GETBYSLUG_GLOBAL),
     # ⚠ PREDICTION INCOMPLETE, CORRECTED: I listed S-HONEST alone. S-REACHABLE fires too, and
     # should — it is the fixture guard in the second test, whose whole job is to notice when the
     # slug arm stops resolving a page only a slug can find. A control that breaks the arm must
     # trip both, and a prediction of one of them was a claim that the other was blind.
     {"S-HONEST", "S-REACHABLE"}, True),

    ("C2",
     "Neuter the chokepoint's slug branch: resolve this tool's workspace from nothing, so the "
     "slug arm denies unconditionally. An over-refusal, and the honest-path assertion is what "
     "catches it — a file of leak assertions alone would pass.",
     lambda: mutate(SERVER, SLUG_BRANCH, SLUG_BRANCH_DENY),
     {"S-HONEST", "S-REACHABLE"}, True),

    # ⚠ C3 WAS DELETED, ALONG WITH THE ASSERTION IT TARGETED. It removed the `space_id required
    # when using slug` refusal and the guard stayed GREEN: with an empty space_id the lookup simply
    # matches no row, so [S-REACHABLE-NOSPACE] passed whether that branch existed or not. The
    # assertion could not be made to fail by anything, so it was decoration and was removed rather
    # than kept with a control pointed at it. The bare-slug case that IS load-bearing is
    # [S-XWS-NOSPACE], which asserts the stronger property (must not return another workspace's
    # content) and is earned by C1.

    ("C4",
     "Remove the in-tool canViewPage gate. ⚠ NOT SLUG-EXCLUSIVE AND RECORDED AS SUCH: it is the "
     "gate BOTH arms share, so the pre-existing id-arm private-space test reddens too. This "
     "control demonstrates the new file reaches that gate; it does not justify the file on its "
     "own, and the blind-guard claim is deliberately inverted here to say so.",
     lambda: mutate(SERVER, INTOOL_GATE, ""),
     {"S-PRIV-SLUG"}, False),
]


def main():
    saved = tempfile.mkdtemp(prefix="w31-slugarm-")
    before = {f: sha256(f) for f in FILES}
    for f in FILES:
        shutil.copy2(REPO / f, pathlib.Path(saved) / pathlib.Path(f).name)

    results = []
    try:
        for cid, desc, apply_fn, want, blind_green in CONTROLS:
            for f in FILES:
                shutil.copy2(pathlib.Path(saved) / pathlib.Path(f).name, REPO / f)
            try:
                apply_fn()
            except SystemExit as e:
                results.append((cid, "ANCHOR-MISS", set(), set(), str(e)))
                continue
            tags, failed, abort, _ = run_tests()
            if abort:
                results.append((cid, abort, set(), set(), "never scored as a catch"))
                continue
            blind_hit = {t for t in failed if t in BLIND_GUARDS}
            ok = tags == want and ((not blind_hit) if blind_green else bool(blind_hit))
            verdict = "AS PREDICTED" if ok else (
                "MISMATCH (tag set)" if tags != want else "MISMATCH (blind-guard claim)")
            results.append((cid, verdict, tags, blind_hit, f"want {sorted(want)}"))
    finally:
        for f in FILES:
            shutil.copy2(pathlib.Path(saved) / pathlib.Path(f).name, REPO / f)

    restored = all(before[f] == sha256(f) for f in FILES)
    print("=" * 96)
    print("W3.1 — MCP get_page slug-arm controls")
    print("=" * 96)
    good = 0
    for cid, verdict, tags, blind_hit, note in results:
        if verdict == "AS PREDICTED":
            good += 1
        print(f"{'OK ' if verdict == 'AS PREDICTED' else '!! '}{cid}  {verdict}")
        if verdict != "AS PREDICTED":
            print(f"      got tags {sorted(tags)}  blind reddened {sorted(blind_hit)}  ({note})")
    print("-" * 96)
    print(f"{good}/{len(CONTROLS)} as predicted;  files restored byte-for-byte: {restored}")
    sys.exit(0 if (good == len(CONTROLS) and restored) else 1)


if __name__ == "__main__":
    main()
