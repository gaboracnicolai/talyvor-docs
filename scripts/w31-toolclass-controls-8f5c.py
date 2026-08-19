#!/usr/bin/env python3
"""Positive controls for the MCP tool-classification guards (tab-8f5c, W3.1).

The guards PASSED ON THEIR FIRST RUN — the three sets are correct today — so every claim they
make rests on these mutations. Each one runs the FULL Go suite against a real Postgres, records
the named assertions that caught it, and restores every touched file in a `finally`, verified by
sha256 rather than by an exit code.

PREDICTIONS ARE WRITTEN DOWN BEFORE THE RUN (see PREDICTED) so an unpredicted catcher shows up as
a wrong prediction rather than being absorbed as a pass.

  T0  no mutation                                      -> GREEN
  T1  a new MUTATING tool served by callTool, in NO
      set and not advertised (the measured defect)     -> ACCESS + SPEND + UNADVERTISED
  T2  T1 + spend-exempted + advertised                 -> TOOL-ACCESS-UNCLASSIFIED only
  T3  T1 + access-exempted + advertised                -> TOOL-SPEND-UNCLASSIFIED only
  T4  T1 + BOTH exempt lists, still not advertised     -> TOOL-UNADVERTISED only
  T5  a tool advertised by tools/list and served by
      nothing                                          -> TOOL-UNDISPATCHED only
  T6  T1 with the POPULATION BLINDED — read from the
      classification sets instead of the dispatch
      switch                                           -> GREEN. The measured blindness: a guard
                                                          that iterates the sets is satisfied by
                                                          construction and would watch T1 go in
  T7  the population read from a function that does
      not exist                                        -> TOOLPOP-FLOOR (an empty population must
                                                          be loud, not a clean bill of health)
"""

import hashlib
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SERVER = os.path.join(ROOT, "internal/mcp/server.go")
GUARD = os.path.join(ROOT, "internal/mcp/toolclassification_test.go")
DSN = os.environ.get(
    "DOCS_TEST_DATABASE_URL",
    "postgres://postgres:postgres@127.0.0.1:55445/postgres?sslmode=disable",
)

DISPATCH_ANCHOR = """	case "get_space_tree":
		return s.toolGetSpaceTree(ctx, args)
	}"""
DISPATCH_ADD = """	case "get_space_tree":
		return s.toolGetSpaceTree(ctx, args)
	case "archive_page":
		return s.toolUpdatePage(ctx, args)
	}"""

WS_ANCHOR = """	case "update_page", "verify_page", "get_page_analytics":
		return s.pageWorkspace(ctx, stringArg(args, "page_id", ""))"""
WS_ADD = """	case "update_page", "verify_page", "get_page_analytics", "archive_page":
		return s.pageWorkspace(ctx, stringArg(args, "page_id", ""))"""

LIST_ANCHOR = """func (s *Server) toolsList() []toolSpec {
	return []toolSpec{"""
LIST_ADD = """func (s *Server) toolsList() []toolSpec {
	return []toolSpec{
		{
			Name:        "archive_page",
			Description: "control-only",
			InputSchema: schema(required("page_id"), prop("page_id", "string", "id")),
		},"""

ACCESS_ANCHOR = 'var accessExemptTools = map[string]string{'
ACCESS_ADD = 'var accessExemptTools = map[string]string{\n\t"archive_page":    "control-only",'
SPEND_ANCHOR = 'var spendExemptTools = map[string]string{'
SPEND_ADD = 'var spendExemptTools = map[string]string{\n\t"archive_page":       "control-only",'

# T6 blinds the population: it stops reading the dispatch switch and returns the classification
# sets instead — the shape a guard takes when nobody asks where its population comes from.
POP_ANCHOR = '\tvar names []string\n\tast.Inspect(file, func(n ast.Node) bool {'
POP_BLIND = '''\tvar names []string
\tfor k := range writeTools {
\t\tnames = append(names, k)
\t}
\tfor k := range gatedReadTools {
\t\tnames = append(names, k)
\t}
\tfor k := range accessExemptTools {
\t\tnames = append(names, k)
\t}
\t_ = file
\tif len(names) > 0 {
\t\tsort.Strings(names)
\t\treturn names
\t}
\tast.Inspect(file, func(n ast.Node) bool {'''

FLOOR_ANCHOR = 'fn.Name.Name != "callTool"'
FLOOR_BREAK = 'fn.Name.Name != "callToolThatDoesNotExist"'

PREDICTED = {
    "T0": set(),
    "T1": {"TOOL-ACCESS-UNCLASSIFIED", "TOOL-SPEND-UNCLASSIFIED", "TOOL-UNADVERTISED"},
    "T2": {"TOOL-ACCESS-UNCLASSIFIED"},
    "T3": {"TOOL-SPEND-UNCLASSIFIED"},
    "T4": {"TOOL-UNADVERTISED"},
    "T5": {"TOOL-UNDISPATCHED"},
    "T6": set(),
    "T7": {"TOOLPOP-FLOOR"},
}

TAG_RE = re.compile(r"\[([A-Z0-9-]+)\]")


def sha(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def run_suite():
    env = dict(os.environ, DOCS_TEST_DATABASE_URL=DSN)
    proc = subprocess.run(["go", "test", "-timeout", "600s", "-race", "-count=1", "./..."],
                          cwd=ROOT, env=env, capture_output=True, text=True)
    out = proc.stdout + proc.stderr
    return proc.returncode, set(TAG_RE.findall(out)), ("build failed" in out), out


def build(server_src, guard_src, name):
    s, g = server_src, guard_src
    if name in ("T1", "T2", "T3", "T4", "T6"):
        s = s.replace(DISPATCH_ANCHOR, DISPATCH_ADD, 1).replace(WS_ANCHOR, WS_ADD, 1)
    if name in ("T2", "T3", "T5"):
        s = s.replace(LIST_ANCHOR, LIST_ADD, 1)
    if name in ("T3", "T4"):
        g = g.replace(ACCESS_ANCHOR, ACCESS_ADD, 1)
    if name in ("T2", "T4"):
        g = g.replace(SPEND_ANCHOR, SPEND_ADD, 1)
    if name == "T6":
        # ⚠ THE FIRST VERSION OF THIS CONTROL ALSO ADDED archive_page TO THE EXEMPT LISTS, AND THE
        # RUN REFUTED ITS PREDICTION. The blinded population is built FROM those lists, so the
        # addition put the fake tool back into the population and TOOL-UNADVERTISED fired — the
        # control was measuring its own fixture. Blinding alone is the mutation: the population
        # becomes the ten real tools, every one classified and advertised, and the defect is
        # invisible.
        g = g.replace(POP_ANCHOR, POP_BLIND, 1)
    if name == "T7":
        g = g.replace(FLOOR_ANCHOR, FLOOR_BREAK, 1)
    return s, g


def main():
    server_src = open(SERVER, encoding="utf-8").read()
    guard_src = open(GUARD, encoding="utf-8").read()
    base = (sha(SERVER), sha(GUARD))
    for anchor, where in ((DISPATCH_ANCHOR, SERVER), (WS_ANCHOR, SERVER), (LIST_ANCHOR, SERVER),
                          (ACCESS_ANCHOR, GUARD), (SPEND_ANCHOR, GUARD), (POP_ANCHOR, GUARD),
                          (FLOOR_ANCHOR, GUARD)):
        src = server_src if where == SERVER else guard_src
        if anchor not in src:
            sys.exit(f"ABORT: anchor missing from {os.path.basename(where)} — the controls would "
                     f"mutate nothing and score NOT CAUGHT for the wrong reason:\n{anchor[:80]}")

    results = {}
    try:
        for name in ("T0", "T1", "T2", "T3", "T4", "T5", "T6", "T7"):
            s, g = build(server_src, guard_src, name)
            open(SERVER, "w", encoding="utf-8").write(s)
            open(GUARD, "w", encoding="utf-8").write(g)
            rc, tags, broken, out = run_suite()
            if broken:
                results[name] = tags
                print(f"{name}: VOID — build failed (a compile error is not a caught mutation)")
                print(out[-1500:])
                continue
            results[name] = tags
            verdict = "as predicted" if tags == PREDICTED[name] else f"!! PREDICTED {sorted(PREDICTED[name])}"
            print(f"{name}: exit={rc} {'GREEN' if rc == 0 else 'RED'} caught={sorted(tags)} {verdict}")
    finally:
        open(SERVER, "w", encoding="utf-8").write(server_src)
        open(GUARD, "w", encoding="utf-8").write(guard_src)
        assert (sha(SERVER), sha(GUARD)) == base, "files NOT restored"
        print("restored: sha256 verified for both files")

    bad = [n for n, tags in results.items() if tags != PREDICTED[n]]
    print("\nSUMMARY:", "ALL AS PREDICTED" if not bad else f"PREDICTION WRONG FOR {bad}")
    return 0 if not bad else 1


if __name__ == "__main__":
    sys.exit(main())
