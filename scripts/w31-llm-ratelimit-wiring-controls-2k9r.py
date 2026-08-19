#!/usr/bin/env python3
"""Positive controls for internal/ratelimit/mainwiring_test.go (tab-2k9r, W3.1).

THE GUARD PASSES ON A HEALTHY TREE BY CONSTRUCTION, so its whole value is in these mutations.
Every control names its PREDICTED catcher before it runs, mutates the tree, runs the FULL
gauntlet (`go test -timeout 600s -race -count=1 ./...` against a real Postgres, plus gofmt and
go vet), restores in a `finally`, and verifies the restore by sha256 of every touched file.

Running the FULL suite rather than one package is the point: "only the new guard reds" is then
MEASURED rather than assumed, and the must-stay-green companion (C7) is what keeps CAUGHT from
being a catch-all on any edit to cmd/docs/main.go.

Usage:
    DOCS_TEST_DATABASE_URL=postgres://... python3 scripts/w31-llm-ratelimit-wiring-controls-2k9r.py
"""

import hashlib
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MAIN = os.path.join(ROOT, "cmd", "docs", "main.go")
GUARD = os.path.join(ROOT, "internal", "ratelimit", "mainwiring_test.go")
EXTRA_SURFACE = os.path.join(ROOT, "internal", "export", "zz_ratelimit_control_2k9r.go")

MCP_WIRING = (
    '\tmcpServer := mcp.New(pageStore, spaceStore, analyticsStore, aiEngine, freshEngine, "0.1.0").\n'
    "\t\tWithRateLimit(aiLimiter) // ask_docs reaches Lens; an agent loop calls it far faster than a human clicks"
)
MCP_UNWIRED = (
    '\tmcpServer := mcp.New(pageStore, spaceStore, analyticsStore, aiEngine, freshEngine, "0.1.0")'
)
# C5: the call REPLACED BY A MENTION OF ITSELF in a trailing comment. The sentinel
# [A-STRIP-TRAILING] needs is deliberately preserved, so the control isolates rule B.
MCP_AS_PROSE = (
    '\tmcpServer := mcp.New(pageStore, spaceStore, analyticsStore, aiEngine, freshEngine, "0.1.0")'
    " // WithRateLimit(aiLimiter) — ask_docs reaches Lens; an agent loop calls it far faster than a human clicks"
)

AI_WIRING = "aiHandler := ai.NewHandler(aiEngine, pageStore).WithRateLimit(aiLimiter)"
AI_UNWIRED = "aiHandler := ai.NewHandler(aiEngine, pageStore)"

SEARCH_WIRING = "searchHandler := search.NewHandler(pageStore, semSearch).WithRateLimit(searchLimiter)"
SEARCH_UNWIRED = "searchHandler := search.NewHandler(pageStore, semSearch)"

# C7 must keep COMPILING and keep BEHAVING — a compile error is not a caught mutation, and a
# behaviour change would red something other than the analytics tripwire. Routing the identical
# call through a local alias removes the LITERAL and nothing else.
ANALYTICS_WIRING = (
    "\tanalyticsStore.WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))"
)
ANALYTICS_ALIASED = (
    "\tanalyticsWire2k9r := analyticsStore.WithPageRead\n"
    "\tanalyticsWire2k9r(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))"
)

# C4: a FOURTH package joins the class without being wired anywhere.
EXTRA_SURFACE_SRC = """package export

import "github.com/talyvor/docs/internal/ratelimit"

// controlSurface2k9r exists only for scripts/w31-llm-ratelimit-wiring-controls-2k9r.py.
type controlSurface2k9r struct{ limit *ratelimit.Limiter }

func (c *controlSurface2k9r) WithRateLimit(l *ratelimit.Limiter) *controlSurface2k9r {
	c.limit = l
	return c
}
"""

# C6: the stripper reduced to the SIBLINGS' shape (whole-line comments only) with the two
# vacuity assertions removed — the blinding whose purpose is to show that rule B alone cannot
# tell a call from a trailing comment naming it.
QUOTE_AWARE_STRIPPER_START = "func stripComment(line string) string {"
WHOLE_LINE_STRIPPER = """func stripComment(line string) string {
	if strings.HasPrefix(strings.TrimSpace(line), "//") {
		return ""
	}
	return line
}
"""


def sha(path):
    if not os.path.exists(path):
        return "<absent>"
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def read(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


def write(path, s):
    with open(path, "w", encoding="utf-8") as f:
        f.write(s)


def replace_once(path, old, new):
    s = read(path)
    n = s.count(old)
    if n != 1:
        raise SystemExit(f"ANCHOR NOT UNIQUE in {path}: {n} occurrences of {old[:70]!r}")
    write(path, s.replace(old, new))


def blind_stripper():
    """Replace the quote-aware stripper with the whole-line one AND drop the vacuity loop."""
    s = read(GUARD)
    start = s.index(QUOTE_AWARE_STRIPPER_START)
    end = s.index("\n}\n", start) + len("\n}\n")
    s = s[:start] + WHOLE_LINE_STRIPPER + s[end:]
    # Drop the vacuity loop so the blinding is not simply reported by [A-STRIP-*].
    vstart = s.index("\tfor _, s := range []struct{ tag, sentinel string }{")
    # Anchor on a phrase that IS in the guard's source. The first draft anchored on the tag
    # "[A-STRIP-TRAILING]" as it appears in the FAILURE OUTPUT — the guard builds that string
    # with %s, so it is nowhere in the file, and the control crashed instead of running.
    vend = s.index("\n\t}\n", s.index("would pass on prose alone", vstart)) + len("\n\t}\n")
    # `body` exists ONLY for the vacuity assertions (rule B reads `statements`), so removing the
    # loop leaves it unused and the mutation VOIDs on a compile error rather than measuring
    # anything. The harness reported that correctly; this keeps the blinding compilable.
    s = s[:vstart] + "\t_ = body\n" + s[vend:]
    write(GUARD, s)


FAIL_RE = re.compile(r"^\s*--- FAIL: (\S+)", re.M)


def gauntlet():
    """gofmt + go vet + the full real-Postgres suite. Returns (ok, failing test names, note)."""
    fmt = subprocess.run(["gofmt", "-l", "."], cwd=ROOT, capture_output=True, text=True)
    unformatted = [x for x in fmt.stdout.split() if x]
    vet = subprocess.run(["go", "vet", "./..."], cwd=ROOT, capture_output=True, text=True)
    if vet.returncode != 0:
        return False, [], "VOID: go vet failed — a compile error is not a caught mutation\n" + vet.stderr[-800:]
    run = subprocess.run(
        ["go", "test", "-timeout", "600s", "-race", "-count=1", "./..."],
        cwd=ROOT, capture_output=True, text=True,
    )
    fails = sorted(set(FAIL_RE.findall(run.stdout + run.stderr)))
    note = ""
    if unformatted:
        note = "gofmt-dirty: " + ",".join(unformatted)
    return run.returncode == 0, fails, note


CONTROLS = [
    dict(
        name="C0 no mutation",
        predict="NOTHING — the baseline must be green, or every CAUGHT below is unreadable",
        touch=[],
        apply=lambda: None,
    ),
    dict(
        name="C1 mcp wiring deleted (the defect, sharpest surface)",
        predict="CAUGHT by TestRateLimit_MainWiresEveryLLMSpendCeiling ONLY ([B-WIRED mcp])",
        touch=[MAIN],
        apply=lambda: replace_once(MAIN, MCP_WIRING, MCP_UNWIRED),
    ),
    dict(
        name="C2 search wiring deleted",
        predict="CAUGHT by TestRateLimit_MainWiresEveryLLMSpendCeiling ONLY ([B-WIRED search])",
        touch=[MAIN],
        apply=lambda: replace_once(MAIN, SEARCH_WIRING, SEARCH_UNWIRED),
    ),
    dict(
        name="C3 ai wiring deleted",
        predict="CAUGHT by TestRateLimit_MainWiresEveryLLMSpendCeiling ONLY ([B-WIRED ai])",
        touch=[MAIN],
        apply=lambda: replace_once(MAIN, AI_WIRING, AI_UNWIRED),
    ),
    dict(
        name="C4 a FOURTH package declares WithRateLimit and nothing wires it",
        predict="CAUGHT by TestRateLimit_MainWiresEveryLLMSpendCeiling ONLY ([A-POPULATION]) — "
                "rule B is structurally blind to a surface it was never told about",
        touch=[EXTRA_SURFACE],
        apply=lambda: write(EXTRA_SURFACE, EXTRA_SURFACE_SRC),
    ),
    dict(
        name="C5 mcp wiring replaced by a TRAILING COMMENT naming itself",
        predict="CAUGHT by TestRateLimit_MainWiresEveryLLMSpendCeiling ONLY ([B-WIRED mcp]) — "
                "the guard must read the code, not the documentation of the code",
        touch=[MAIN],
        apply=lambda: replace_once(MAIN, MCP_WIRING, MCP_AS_PROSE),
    ),
    dict(
        name="C6 stripper BLINDED to the siblings' whole-line shape, with C5's mutation on top",
        predict="NOT CAUGHT — the suite goes fully green with the MCP ceiling deleted. This is "
                "the measured blindness the quote-aware trailing-comment stripper exists for",
        touch=[MAIN, GUARD],
        apply=lambda: (replace_once(MAIN, MCP_WIRING, MCP_AS_PROSE), blind_stripper()),
    ),
    dict(
        name="C7 analytics WithPageRead deleted (must-stay-green companion)",
        predict="CAUGHT by TestAnalytics_MainWiresThePageReadGate and NOT by the new guard — "
                "keeps CAUGHT from being a catch-all on any edit to cmd/docs/main.go",
        touch=[MAIN],
        apply=lambda: replace_once(MAIN, ANALYTICS_WIRING, ANALYTICS_ALIASED),
    ),
]

NEW_GUARD = "TestRateLimit_MainWiresEveryLLMSpendCeiling"


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        raise SystemExit("DOCS_TEST_DATABASE_URL must be set — a skipped suite proves nothing")
    originals = {p: (read(p) if os.path.exists(p) else None) for p in (MAIN, GUARD, EXTRA_SURFACE)}
    before = {p: sha(p) for p in originals}
    results = []
    only = os.environ.get("CONTROLS_ONLY", "")
    for c in CONTROLS:
        if only and not c["name"].split()[0] in only.split(","):
            continue
        print(f"\n=== {c['name']}\n    PREDICT: {c['predict']}", flush=True)
        try:
            c["apply"]()
            ok, fails, note = gauntlet()
            caught_by_new = NEW_GUARD in fails
            others = [f for f in fails if f != NEW_GUARD]
            print(f"    RESULT : exit_green={ok} new_guard_red={caught_by_new} "
                  f"other_reds={others} {note}", flush=True)
            results.append((c["name"], c["predict"], ok, caught_by_new, others, note))
        finally:
            for p, s in originals.items():
                if s is None:
                    if os.path.exists(p):
                        os.remove(p)
                else:
                    write(p, s)
            after = {p: sha(p) for p in originals}
            if after != before:
                raise SystemExit(f"RESTORE FAILED — sha256 mismatch: {before} vs {after}")

    print("\n\n================ SUMMARY ================")
    for name, predict, ok, caught, others, note in results:
        print(f"{name}\n  predicted: {predict}\n  measured : green={ok} new_guard_red={caught} "
              f"other_reds={others} {note}\n")
    print(f"restore verified by sha256 for: {', '.join(os.path.relpath(p, ROOT) for p in before)}")


if __name__ == "__main__":
    main()
