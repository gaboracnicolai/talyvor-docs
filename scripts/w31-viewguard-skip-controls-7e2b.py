#!/usr/bin/env python3
"""Positive controls for the self-disarming view guard (W3.1, tab-7e2b).

THE FINDING. internal/analytics/sec_viewer_test.go is the ONLY test in the repository that
drives the HTTP POST /spaces/{s}/pages/{p}/view route. It ended with a `t.Skip` whenever the
request wrote no page_views row — a reading that made sense when page.Handler ALSO registered
that route, and stopped making sense when page.Store.RecordView and its registration were
deleted. From then on `views == 0` meant only one thing (the recording path is broken) and the
test's response to it was to go quiet.

WHAT EACH CONTROL MUST PROVE:
  · that the fixed guard FAILS on the regression it exists for   (S2)
  · that the census FAILS on the defect verbatim                 (S1, S5)
  · that neither fires on an inert edit                          (S0)
  · that the census FLOOR is isolated from its predicate         (S3, S4)

Every mutation is restored in a `finally` and the restore is sha256-verified.
"""

import hashlib
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
DSN = "postgres://postgres:postgres@localhost:55471/postgres?sslmode=disable"

SEC = ROOT / "internal/analytics/sec_viewer_test.go"
HANDLER = ROOT / "internal/analytics/handler.go"
CENSUS = ROOT / "internal/testutil/skipcensus_test.go"
VICTIM = ROOT / "internal/page/viewbump_one_owner_test.go"  # an unrelated test file, for S5

CENSUS_T = "TestNoTestSkipsItselfOffTheBuild"
SEC_T = "TestSec_RecordView_ViewerIsVerifiedNotBody"

# The skip exactly as it stood at 08e0958, for replaying the defect verbatim.
ORIGINAL_SKIP = '''	if views == 0 {
		// page.RecordView won: it resolves the viewer from the verified identity, so
		// there is no forgery surface — but then analytics records nothing at all.
		t.Skip("analytics.RecordView is shadowed by page.RecordView — no page_views row written; " +
			"see the reconciliation note in BUILD_STATE")
	}
'''


def sha(p: pathlib.Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def run(test_name: str, pkg: str, full: bool = False) -> str:
    """Return combined output of a go test run."""
    cmd = ["go", "test", "-count=1", "-timeout", "300s"]
    if full:
        cmd += ["./..."]
    else:
        cmd += ["-v", "-run", test_name, pkg]
    r = subprocess.run(cmd, cwd=ROOT, capture_output=True, text=True,
                       env={**__import__("os").environ, "DOCS_TEST_DATABASE_URL": DSN})
    return r.stdout + r.stderr


def marks(out: str) -> set:
    found = set()
    for tag in ("[NO-SKIP]", "[FLOOR]", "[VIEW-RECORDED]"):
        # [FLOOR] is also logged on success; only count it when the run FAILED on it.
        if tag == "[FLOOR]" and "[FLOOR] census scanned only" not in out:
            continue
        if tag in out:
            found.add(tag)
    if re.search(r"^--- SKIP", out, re.M):
        found.add("SKIP")
    if re.search(r"^(FAIL|--- FAIL)", out, re.M):
        found.add("FAIL")
    return found


class Mutation:
    def __init__(self, path, before, after):
        self.path, self.before, self.after = path, before, after
        self.orig_sha = sha(path)
        self.orig = path.read_text()

    def __enter__(self):
        s = self.path.read_text()
        assert self.before in s, f"anchor missing in {self.path}"
        self.path.write_text(s.replace(self.before, self.after, 1))
        return self

    def __exit__(self, *a):
        self.path.write_text(self.orig)
        assert sha(self.path) == self.orig_sha, f"RESTORE FAILED for {self.path}"
        return False


def report(name, desc, found, predicted):
    ok = found == predicted
    print(f"{'PASS' if ok else '**MISMATCH**'}  {name}  {desc}")
    print(f"        predicted={sorted(predicted) or ['NOT CAUGHT']}  actual={sorted(found) or ['NOT CAUGHT']}")
    return ok


def main():
    results = []

    # ── S0: an inert comment. Nothing may fire. ────────────────────────────────────────
    with Mutation(HANDLER, "func (h *Handler) RecordView(",
                  "// control S0: inert\nfunc (h *Handler) RecordView("):
        out = run(SEC_T, "./internal/analytics/...") + run(CENSUS_T, "./internal/testutil/...")
        results.append(report("S0", "inert comment in the handler", marks(out), set()))

    # ── S1: THE DEFECT VERBATIM — the skip restored. The census must fire. ─────────────
    with Mutation(SEC, '''	if views == 0 {
		t.Fatalf("[VIEW-RECORDED] POST''', ORIGINAL_SKIP + '''	if false {
		t.Fatalf("[VIEW-RECORDED] POST'''):
        out = run(CENSUS_T, "./internal/testutil/...")
        results.append(report("S1", "the t.Skip restored verbatim -> census", marks(out),
                              {"[NO-SKIP]", "FAIL"}))

    # ── S2: THE REGRESSION THE GUARD EXISTS FOR — handler stops forwarding the duration,
    #        so every view goes unrecorded while the route still answers 200.
    #        BEFORE the fix this ran green with a SKIP; it must now FAIL.
    with Mutation(HANDLER, "\t\tDuration:    in.Duration,\n", "\t\tDuration:    0,\n"):
        out = run(SEC_T, "./internal/analytics/...")
        results.append(report("S2", "handler drops duration -> sec guard", marks(out),
                              {"[VIEW-RECORDED]", "FAIL"}))

    # ── S3: FLOOR ISOLATION — mis-root the walk so it reads almost no test files.
    #        The floor must fire even though there is no skip to find.
    with Mutation(CENSUS, 'filepath.Join("..", "..")', 'filepath.Join("..", "..", "internal", "testutil")'):
        out = run(CENSUS_T, "./internal/testutil/...")
        results.append(report("S3", "census mis-rooted -> floor", marks(out), {"[FLOOR]", "FAIL"}))

    # ── S4: PREDICATE BLINDED, defect present. Proves the predicate is what catches S1
    #        and that a blinded census reports clean — the reason the floor exists.
    with Mutation(CENSUS, 'var skipCalls = []string{"t.Skip(",',
                  'var skipCalls = []string{"zzz-no-such-call(",'):
        with Mutation(SEC, '''	if views == 0 {
		t.Fatalf("[VIEW-RECORDED] POST''', ORIGINAL_SKIP + '''	if false {
		t.Fatalf("[VIEW-RECORDED] POST'''):
            out = run(CENSUS_T, "./internal/testutil/...")
            results.append(report("S4", "predicate blinded + defect present -> census goes quiet",
                                  marks(out), set()))

    # ── S5: A SKIP SOMEWHERE ELSE ENTIRELY. The census population is the whole tree,
    #        not the one file the finding came from.
    with Mutation(VICTIM, "func TestViewCountBump_HasExactlyOneWriter",
                  "func TestS5Injected(t *testing.T) { t.Skip(\"injected\") }\n\n"
                  "func TestViewCountBump_HasExactlyOneWriter"):
        out = run(CENSUS_T, "./internal/testutil/...")
        results.append(report("S5", "skip injected into an unrelated test file", marks(out),
                              {"[NO-SKIP]", "FAIL"}))

    print()
    print(f"{sum(results)}/{len(results)} controls as predicted")
    # Restores are asserted per-mutation; re-assert the tree is pristine.
    for p in (SEC, HANDLER, CENSUS, VICTIM):
        print(f"  {sha(p)}  {p.relative_to(ROOT)}")
    return 0 if all(results) else 1


if __name__ == "__main__":
    sys.exit(main())
