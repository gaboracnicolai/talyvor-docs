#!/usr/bin/env python3
"""Positive controls for the ask_docs error-propagation guard (W3.1, finding 2).

Same protocol as scripts/w31-mcp-cost-controls.py: mutate the PRODUCT only, assert every anchor
count BEFORE any write, verify the bytes actually moved ON DISK, name a must-red target AND a
must-stay-green companion for every control, restore the tree to its pristine sha256 after each,
and read every red BY ASSERTION TEXT rather than by exit code.

THE COMPANION IS CHOSEN PER CONTROL, AND THE DEFAULT ONE IS THE POINT. `TestAskDocs_AnswersWithSources`
(server_test.go) drives ask_docs through the same dispatch on the happy path — and its fakes CANNOT
fail: every fakePages and fakeAI method in this package returns a nil error unconditionally. It was
green through this defect's entire life and stays green through every reintroduction of it below.
That is what makes the catcher prediction falsifiable rather than decorative: if a control reds the
blind test too, the mutation broke something other than what it was aimed at.

⚠ C3 CANNOT USE THAT COMPANION AND SAYING SO IS PART OF THE RESULT. It makes the search arm error
UNCONDITIONALLY, which reds the happy-path test correctly — an over-block is exactly what that test
should catch. Its companion is the cross-tenant suite, which never calls ask_docs, so it still
answers "did the build survive".

C6 and C7 are expected NOT CAUGHT, one-directional BY DESIGN, and each names a real limit of the
guard rather than a hole in it. See their notes.
"""
import hashlib
import os
import shutil
import subprocess
import sys
import tempfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
S = os.path.join(REPO, "internal", "mcp", "server.go")
PRODUCT = [S]

PKG = "./internal/mcp/"
ASK_TEST = (PKG, "TestMCPAskDocsReportsFailureRatherThanAnsweringUngrounded_RealPG")
BLIND = (PKG, "TestAskDocs_AnswersWithSources")           # fakes that cannot fail
CROSS = (PKG, "TestSEC4_MCP_ArgTrust_CrossTenant")        # never calls ask_docs
LIMIT = (PKG, "TestSecMCP_AskDocs_RateLimitedPerVerifiedWorkspace")

SEARCH_CHECK = '''	hits, err := s.deps.pages.SearchWithRank(ctx, wsID, question, nil, 3, 0)
	if err != nil {
		slog.Error("mcp: ask_docs search failed — reporting the failure rather than answering ungrounded",
			slog.String("workspace_id", wsID), slog.Any("err", err))
		return nil, &rpcError{Code: errInternal, Message: "search failed"}
	}'''

SEARCH_SWALLOWED = '''	hits, _ := s.deps.pages.SearchWithRank(ctx, wsID, question, nil, 3, 0)'''

AI_CHECK = '''		ans, err := s.deps.ai.AskDocs(ctx, wsID, question, pages)
		if err != nil {
			slog.Error("mcp: ask_docs AI call failed — reporting the failure rather than an empty answer",
				slog.String("workspace_id", wsID), slog.Any("err", err))
			if errors.Is(err, ai.ErrUnavailable) {
				return nil, &rpcError{Code: errInternal, Message: "the AI service is not available"}
			}
			return nil, &rpcError{Code: errInternal, Message: "the AI service failed to answer"}
		}
		answer = ans'''

AI_SWALLOWED = '''		ans, err := s.deps.ai.AskDocs(ctx, wsID, question, pages)
		if err == nil {
			answer = ans
		}'''

AI_UNAVAIL_BRANCH = '''			if errors.Is(err, ai.ErrUnavailable) {
				return nil, &rpcError{Code: errInternal, Message: "the AI service is not available"}
			}
'''

CONTROLS = [
    {
        "id": "C1",
        "what": "MAIN REPRODUCED (search half): `hits, _ :=`, exactly as shipped at 198636a",
        "edits": [(S, SEARCH_CHECK, SEARCH_SWALLOWED, 1)],
        "must_red": ASK_TEST,
        "companion": BLIND,
    },
    {
        "id": "C2",
        "what": "MAIN REPRODUCED (AI half): `if err == nil { answer = ans }`, as shipped",
        # NEITHER C1 NOR C2 SUBSUMES THE OTHER, and that is what earns two error checks rather
        # than one: C1 is invisible to the "ai fails" case (the AI is healthy there) and C2 is
        # invisible to the "search fails" case (it returns before the AI is reached).
        "edits": [(S, AI_CHECK, AI_SWALLOWED, 1)],
        "must_red": ASK_TEST,
        "companion": BLIND,
    },
    {
        "id": "C3",
        "what": "OVER-BLOCK: the search arm errors unconditionally, so an empty corpus reads as a failure",
        # THIS IS WHAT EARNS THE SECOND DIRECTION. Without the "search legitimately matches
        # nothing" and "everything healthy" cases, C1's assertion is satisfiable by a change
        # that simply errors on everything, and the guard would be indistinguishable from one
        # that had banned the tool.
        "edits": [(S, "	if err != nil {\n		slog.Error(\"mcp: ask_docs search failed",
                   "	if true {\n		slog.Error(\"mcp: ask_docs search failed", 1)],
        "must_red": ASK_TEST,
        "companion": CROSS,   # BLIND correctly reds here — an over-block IS its business
    },
    {
        "id": "C4",
        "what": "THE STORE'S OWN WORDS on the wire: Message: err.Error() instead of the fixed string",
        # The mutation ONLY the leak assertion can see: every other case still observes an rpc
        # error and passes. Without it, nothing would stop the fix from shipping SQLSTATE text
        # and relation names to an MCP client.
        "edits": [(S, 'return nil, &rpcError{Code: errInternal, Message: "search failed"}',
                   'return nil, &rpcError{Code: errInternal, Message: err.Error()}', 1)],
        "must_red": ASK_TEST,
        "companion": BLIND,
    },
    {
        "id": "C5",
        "what": "SOURCES DROPPED on the healthy path (sources built from a zero-length slice)",
        # Earns the healthy-path content assertions: a change that reports failures correctly
        # but quietly stops grounding its answers would pass every error case above.
        "edits": [(S, "	sources := make([]askSource, 0, len(hits))",
                   "	sources := make([]askSource, 0)\n	hits = nil", 1)],
        "must_red": ASK_TEST,
        "companion": CROSS,   # BLIND asserts sources too, so it reds correctly — not a companion here
    },
    {
        "id": "C6",
        "what": "THE FIXED MESSAGE REWORDED: \"search failed\" -> \"could not search\"",
        # EXPECTED NOT CAUGHT, ONE-DIRECTIONAL BY DESIGN. The guard asserts that an error is
        # REPORTED and that it does not carry the database's internals — never its exact
        # wording. A guard pinned to the string would redden on every copy edit while proving
        # nothing about behaviour. Recorded so the limit is stated rather than implied.
        "edits": [(S, 'Message: "search failed"', 'Message: "could not search"', 1)],
        "must_red": ASK_TEST,
        "companion": BLIND,
        "expect": "NOT CAUGHT",
    },
    {
        "id": "C7",
        "what": "THE unavailable/failed DISTINCTION COLLAPSED: drop the errors.Is(ErrUnavailable) arm",
        # EXPECTED NOT CAUGHT, AND IT NAMES THE HONEST LIMIT. The REST sibling separates these
        # two as 503 AI_UNAVAILABLE vs 502 AI_FAILED. Here both carry errInternal and differ
        # only in a human-readable message, so an MCP client cannot tell them apart
        # programmatically and this guard does not pretend it can. Closing that would mean
        # choosing a JSON-RPC code for "unavailable", which is a protocol decision, not a fix.
        "edits": [(S, AI_UNAVAIL_BRANCH, "", 1)],
        "must_red": ASK_TEST,
        "companion": BLIND,
        "expect": "NOT CAUGHT",
    },
]


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def run_test(pkg, name):
    p = subprocess.run(["go", "test", "-count=1", "-run", "^" + name + "$", pkg],
                       cwd=REPO, capture_output=True, text=True)
    return p.returncode == 0, (p.stdout + p.stderr).strip()


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set — these controls need a real Postgres.")
        return 2

    tmp = tempfile.mkdtemp(prefix="w31ask-pristine-")
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

    for pkg, name in [ASK_TEST, BLIND, CROSS, LIMIT]:
        ok, out = run_test(pkg, name)
        if not ok:
            print("PRECONDITION FAILED: %s is not green before mutation.\n%s" % (name, out))
            return 2
    print("precondition: every target is green before any control ran\n")

    results = []
    for c in CONTROLS:
        expect = c.get("expect", "CAUGHT")
        companion = c.get("companion", BLIND)

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

        # Re-read per edit so two edits to one file compose instead of the second erasing the first.
        for path, old, new, _ in c["edits"]:
            with open(path) as fh:
                src = fh.read()
            with open(path, "w") as fh:
                fh.write(src.replace(old, new))

        applied = all(new == "" or new in open(p).read() for p, _o, new, _w in c["edits"])
        touched = any(sha(p) != saved[p][1] for p in PRODUCT)
        if not applied or not touched:
            print("%s WRITE NOT APPLIED" % c["id"])
            restore()
            results.append((c["id"], "SUSPECT (write)", c["what"]))
            continue

        red_ok, red_out = run_test(*c["must_red"])
        green_ok, green_out = run_test(*companion)

        failed = restore()
        if failed:
            print("%s RESTORE FAILED for %s — stop and inspect the tree" % (c["id"], failed))
            return 2

        if not green_ok:
            verdict = "SUSPECT (companion red — the mutation broke the build)"
            print("  %s companion %s:\n%s" % (c["id"], companion[1], green_out[:400]))
        else:
            got = "CAUGHT" if not red_ok else "NOT CAUGHT"
            verdict = got if got == expect else "%s (EXPECTED %s)" % (got, expect)
            if not red_ok:
                # BY ASSERTION TEXT, not by exit code — a CAUGHT can name a test that fataled
                # before the assertion the control exists for was ever reached.
                lines = [l for l in red_out.splitlines() if "_test.go:" in l]
                for l in lines[:2]:
                    print("  %s red: %s" % (c["id"], l.strip()[:170]))
        results.append((c["id"], verdict, c["what"]))

    print("\n" + "=" * 104)
    ok = True
    for cid, verdict, what in results:
        print("%-4s %-24s %s" % (cid, verdict, what))
        if "SUSPECT" in verdict or "EXPECTED" in verdict:
            ok = False
    print("=" * 104)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
