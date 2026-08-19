#!/usr/bin/env python3
"""Positive controls for get_space_tree's page-read error path (tab-8f5c, W3.1).

Every control MUTATES the product, runs the FULL Go suite against a real Postgres, records
which named assertion caught it, and RESTORES the tree in a finally — the restore is verified
by sha256, never by an exit code.

THE PREDICTION IS WRITTEN DOWN BEFORE THE RUN (see PREDICTED below) so an unpredicted catcher
is visible as a wrong prediction rather than absorbed as a pass.

  C0  no mutation                          -> suite GREEN
  C1  restore `pages, _ :=` (the defect)   -> TREE-UNREADABLE only
  C2  drop the failing space and carry on  -> TREE-UNREADABLE only (a PARTIAL tree presented
                                              as complete is the same false claim, one level up)
  C3  treat an EMPTY page list as an error -> [WIRED-TREE] (nilstore_chokepoint_realpg_test.go,
                                              REAL Postgres). PREDICTED WRONG: the first run of
                                              this harness predicted a companion test of my own
                                              and under-listed WIRED-TREE, which already covers it
  C5  drop spaces that have no pages       -> [WIRED-TREE] again. C3+C5 are why the companion was
                                              DELETED rather than shipped: no mutation catches it
                                              that WIRED-TREE does not catch first
  C4  C1 with THIS GUARD FILE DELETED      -> suite GREEN: the measured blindness — nothing
                                              else in the repository can see this defect

Run from the repo root:  python3 scripts/w31-treeread-controls-8f5c.py
"""

import hashlib
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SERVER = os.path.join(ROOT, "internal/mcp/server.go")
GUARD = os.path.join(ROOT, "internal/mcp/spacetree_pageread_test.go")
DSN = os.environ.get(
    "DOCS_TEST_DATABASE_URL",
    "postgres://postgres:postgres@127.0.0.1:55444/postgres?sslmode=disable",
)

# The shipped block this file mutates. Kept as an exact string so a drifted source is a loud
# failure here rather than a control that silently mutated nothing and scored NOT CAUGHT.
FIXED = """		pages, err := s.deps.pages.List(ctx, page.PageFilter{SpaceID: sp.ID, Limit: 200})
		if err != nil {
			slog.Error("mcp: get_space_tree page read failed — reporting the failure rather than an empty space",
				slog.String("workspace_id", wsID), slog.String("space_id", sp.ID), slog.Any("err", err))
			return nil, &rpcError{Code: errInternal, Message: "the page read failed; the space tree is unavailable"}
		}
"""

C1_SWALLOW = """		pages, _ := s.deps.pages.List(ctx, page.PageFilter{SpaceID: sp.ID, Limit: 200})
"""

C2_CONTINUE = """		pages, err := s.deps.pages.List(ctx, page.PageFilter{SpaceID: sp.ID, Limit: 200})
		if err != nil {
			continue
		}
"""

C5_DROP_EMPTY = """		pages, err := s.deps.pages.List(ctx, page.PageFilter{SpaceID: sp.ID, Limit: 200})
		if err != nil {
			slog.Error("mcp: get_space_tree page read failed — reporting the failure rather than an empty space",
				slog.String("workspace_id", wsID), slog.String("space_id", sp.ID), slog.Any("err", err))
			return nil, &rpcError{Code: errInternal, Message: "the page read failed; the space tree is unavailable"}
		}
		if len(pages) == 0 {
			continue
		}
"""

C3_EMPTY_IS_ERROR = """		pages, err := s.deps.pages.List(ctx, page.PageFilter{SpaceID: sp.ID, Limit: 200})
		if err != nil || len(pages) == 0 {
			return nil, &rpcError{Code: errInternal, Message: "the page read failed; the space tree is unavailable"}
		}
"""

PREDICTED = {
    "C0": set(),
    "C1": {"TREE-UNREADABLE"},
    "C2": {"TREE-UNREADABLE"},
    "C3": {"WIRED-TREE"},
    "C4": set(),
    "C5": {"WIRED-TREE"},
}

TAG_RE = re.compile(r"\[([A-Z0-9-]+)\]")


def sha(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def run_suite():
    env = dict(os.environ, DOCS_TEST_DATABASE_URL=DSN)
    proc = subprocess.run(
        ["go", "test", "-timeout", "600s", "-race", "-count=1", "./..."],
        cwd=ROOT, env=env, capture_output=True, text=True,
    )
    out = proc.stdout + proc.stderr
    tags = set(TAG_RE.findall(out))
    failed_pkgs = [ln for ln in out.splitlines() if ln.startswith("FAIL")]
    build_broken = "build failed" in out or "[build failed]" in out
    return proc.returncode, tags, failed_pkgs, build_broken, out


def main():
    baseline_server, baseline_guard = sha(SERVER), sha(GUARD)
    original = open(SERVER, encoding="utf-8").read()
    guard_src = open(GUARD, encoding="utf-8").read()
    if FIXED not in original:
        sys.exit("ABORT: the shipped block is not in server.go verbatim — the controls would "
                 "mutate nothing and score NOT CAUGHT for the wrong reason.")

    results = {}
    try:
        for name, mutation, drop_guard in (
            ("C0", None, False),
            ("C1", C1_SWALLOW, False),
            ("C2", C2_CONTINUE, False),
            ("C3", C3_EMPTY_IS_ERROR, False),
            ("C4", C1_SWALLOW, True),
            ("C5", C5_DROP_EMPTY, False),
        ):
            if mutation is not None:
                open(SERVER, "w", encoding="utf-8").write(original.replace(FIXED, mutation))
            else:
                open(SERVER, "w", encoding="utf-8").write(original)
            if drop_guard:
                os.remove(GUARD)
            elif not os.path.exists(GUARD):
                open(GUARD, "w", encoding="utf-8").write(guard_src)

            rc, tags, failed, build_broken, out = run_suite()
            if build_broken:
                results[name] = ("VOID (build failed — a compile error is not a caught mutation)", tags)
                print(f"{name}: VOID — build failed\n{out[-2000:]}")
                continue
            caught = tags & set().union(*PREDICTED.values()) if any(PREDICTED.values()) else tags
            status = "GREEN" if rc == 0 else "RED"
            results[name] = (status, tags)
            ok = "as predicted" if tags == PREDICTED[name] else f"!! PREDICTED {sorted(PREDICTED[name])}"
            print(f"{name}: exit={rc} {status} caught={sorted(tags)} {ok}")
            if failed:
                print("      failing packages: " + "; ".join(failed[:6]))
    finally:
        open(SERVER, "w", encoding="utf-8").write(original)
        if not os.path.exists(GUARD):
            open(GUARD, "w", encoding="utf-8").write(guard_src)
        assert sha(SERVER) == baseline_server, "server.go NOT restored"
        assert sha(GUARD) == baseline_guard, "guard file NOT restored"
        print("restored: sha256 verified for both files")

    bad = [n for n, (_, tags) in results.items() if tags != PREDICTED[n]]
    print("\nSUMMARY:", "ALL AS PREDICTED" if not bad else f"PREDICTION WRONG FOR {bad}")
    return 0 if not bad else 1


if __name__ == "__main__":
    sys.exit(main())
