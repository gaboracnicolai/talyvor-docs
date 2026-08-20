#!/usr/bin/env python3
"""w31-tenancypredicate-census-5r8k — WHICH TENANCY PREDICATES CAN ANY TEST SEE?

Every production SQL statement in this repo that scopes a row to a workspace does it with one
of two shapes: `workspace_id = $n` (single workspace) or `workspace_id = ANY($n)` (the caller's
workspace set). This harness NEUTERS one of them at a time — the predicate stays in the
statement, keeps referencing its placeholder so the bind still type-checks, and becomes
`(<predicate> OR TRUE)`, which is the INERT-FILTER shape this fleet has already found once in
production — and runs the WHOLE Go suite against a from-zero real Postgres.

Three outcomes per site, and only the first is a guard:

  BEHAVIOURAL  at least one failing test failed for a reason OTHER than a pgxmock query-text
               mismatch. Something executed the neutered statement and disagreed with the rows
               it got back.
  TEXT-ONLY    every failing test failed with a pgxmock "call to Query/Exec … was not expected"
               or "does not match" diagnostic. THE ONLY THING THAT NOTICED IS A REGEX OVER THE
               SQL STRING. Such a test reds if you reformat the statement and stays green if you
               change what it means, because pgxmock never asks Postgres to run it.
  SILENT       no test failed at all. The predicate is load-bearing for tenancy and nothing in
               the repo can tell whether it is there.

⚠ THE HARNESS MUST NOT BE TRUSTED BECAUSE IT PRINTS WORDS. Its own controls, all required:
  · BASELINE. The unmutated suite is run first and must be GREEN. Without it every "the suite
    reddened" verdict is true for free against an already-red suite.
  · MUTATION APPLIED. The rewritten file is re-read and the mutated text asserted present. A
    site whose anchor no longer matches prints MUTATION NOT APPLIED and is scored INVALID, never
    SILENT — the two look identical from the outside and mean opposite things.
  · RESTORE. Every file is restored in a `finally` and its sha256 compared to the pre-run digest.
  · COMPILE. A mutation that does not build is INVALID, not CAUGHT. `[build failed]` and
    `# github.com/...` compile diagnostics are detected explicitly.
  · PACKAGE NAMES ARE PARSED FROM `^FAIL\\s+(github\\.com/\\S+)`, never from a bare `FAIL` —
    tab-8v3r's probe captured Go's bare summary line as a package name and reported "1 package"
    for a mutation that reddened three. A wrong count is worse than no count.
  · BOTH ANSWERS, FROM THE SAME RUN. The census is its own positive control in both directions
    ONLY IF it produces BEHAVIOURAL and SILENT on different sites in one run — a scorer that can
    only ever print one verdict is not measuring, it is asserting. The run prints the counts; if
    either is 0, say so out loud rather than reporting the other as a result.
  · --only NAMES ARE CHECKED. An ident that matches no site is an error, not a smaller census.
    A typo'd site list would otherwise report "0 SILENT" over an empty population.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
DSN_ENV = "DOCS_TEST_DATABASE_URL"

# The two tenancy shapes, optionally table-qualified (`p.workspace_id`, `pv.workspace_id`).
PREDICATE_RE = re.compile(r"(?:[A-Za-z_][A-Za-z0-9_]*\.)?workspace_id\s*=\s*(?:ANY\(\$\d+\)|\$\d+)")

# pgxmock's diagnostics when the SQL TEXT no longer matches the expectation. These are the
# strings that mean "a regex over the statement noticed", not "a row came back wrong".
#
# ⚠ THE FIRST VERSION OF THIS TUPLE WAS MISSING `could not match actual sql` — pgxmock's ACTUAL
# message, and the one 18 of the 64 runs produced. Every site whose only red was that message was
# scored BEHAVIOURAL, i.e. the harness reported a predicate as behaviourally guarded when the
# only thing that had noticed was a regex over the statement text. THAT IS THE DANGEROUS
# DIRECTION: a census that overstates coverage retires a guard nobody has. It was caught by
# reading the one-line note the runner prints beside each verdict — the note said
# "could not match actual sql" while the verdict said BEHAVIOURAL — and NOT by the harness, which
# had no way to disagree with itself. --reclassify re-scores the saved logs so the correction is
# a re-read of the same evidence rather than a second, differently-seeded run.
PGXMOCK_TEXT_MARKERS = (
    "could not match actual sql",
    "was not expected",
    "does not match expected",
    "which was not expected",
    "expected query to match regex",
    "there is a remaining expectation",
    "all expectations were already fulfilled",
    "ExpectedQuery",
    "ExpectedExec",
)

FAIL_PKG_RE = re.compile(r"^FAIL\s+(github\.com/\S+)", re.M)
OK_PKG_RE = re.compile(r"^ok\s+(github\.com/\S+)", re.M)
FAIL_TEST_RE = re.compile(r"^\s*--- FAIL: (\S+)", re.M)
BUILD_FAIL_RE = re.compile(r"\[build failed\]|^# github\.com/\S+", re.M)


@dataclass
class Site:
    ident: str
    path: Path
    line: int
    col: int
    text: str
    context: str


@dataclass
class Result:
    site: Site
    verdict: str
    fail_pkgs: list = field(default_factory=list)
    fail_tests: list = field(default_factory=list)
    note: str = ""


def sha256(p: Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def strip_go_comment(line: str) -> str:
    """Return the code part of a Go line — everything before a // that is not inside a string.

    Crude on purpose: it exists only to drop predicate matches that live in prose. A predicate
    inside a raw string literal spanning lines is never preceded by // on its own line.
    """
    in_raw = False
    in_str = False
    i = 0
    while i < len(line) - 1:
        c, nxt = line[i], line[i + 1]
        if not in_str and c == "`":
            in_raw = not in_raw
        elif not in_raw and c == '"' and (i == 0 or line[i - 1] != "\\"):
            in_str = not in_str
        elif not in_raw and not in_str and c == "/" and nxt == "/":
            return line[:i]
        i += 1
    return line


def discover(only: list | None = None) -> list:
    sites = []
    for path in sorted(REPO.glob("internal/**/*.go")) + sorted(REPO.glob("cmd/**/*.go")):
        if path.name.endswith("_test.go"):
            continue
        rel = path.relative_to(REPO).as_posix()
        for n, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
            code = strip_go_comment(raw)
            for m in PREDICATE_RE.finditer(code):
                ident = f"{rel}:{n}:{m.start()}"
                if only and ident not in only:
                    continue
                sites.append(
                    Site(ident=ident, path=path, line=n, col=m.start(), text=m.group(0),
                         context=raw.strip()[:120])
                )
    return sites


def apply_mutation(site: Site) -> str:
    """Rewrite the one occurrence at (line, col). Returns the mutated line for verification."""
    lines = site.path.read_text(encoding="utf-8").splitlines(keepends=True)
    original = lines[site.line - 1]
    before = original[: site.col]
    target = original[site.col : site.col + len(site.text)]
    if target != site.text:
        raise RuntimeError(f"anchor drift at {site.ident}: found {target!r}, expected {site.text!r}")
    after = original[site.col + len(site.text) :]
    mutated = f"{before}({site.text} OR TRUE){after}"
    lines[site.line - 1] = mutated
    site.path.write_text("".join(lines), encoding="utf-8")
    return mutated


def run_suite(dsn: str, timeout: int = 900) -> tuple:
    env = dict(os.environ)
    env[DSN_ENV] = dsn
    proc = subprocess.run(
        ["go", "test", "-timeout", "600s", "-count=1", "./..."],
        cwd=REPO, env=env, capture_output=True, text=True, timeout=timeout,
    )
    return proc.returncode, proc.stdout + proc.stderr


def classify(out: str) -> tuple:
    if BUILD_FAIL_RE.search(out):
        return "INVALID", [], [], "mutation did not compile"
    pkgs = sorted(set(FAIL_PKG_RE.findall(out)))
    tests = sorted(set(FAIL_TEST_RE.findall(out)))
    if not pkgs and not tests:
        return "SILENT", [], [], ""
    # A red is TEXT-ONLY when every failure diagnostic is a pgxmock statement-text complaint.
    lowered = out.lower()
    has_text_marker = any(m.lower() in lowered for m in PGXMOCK_TEXT_MARKERS)
    # Strip the pgxmock lines and see whether any failure text survives that is not one of them.
    residue_lines = []
    for ln in out.splitlines():
        if ln.startswith("ok ") or ln.startswith("---") or ln.startswith("FAIL") or ln.startswith("?   "):
            continue
        if any(m.lower() in ln.lower() for m in PGXMOCK_TEXT_MARKERS):
            continue
        if re.search(r"\.go:\d+:", ln) or "Error:" in ln or "panic" in ln.lower():
            residue_lines.append(ln.strip()[:200])
    if has_text_marker and not residue_lines:
        return "TEXT-ONLY", pkgs, tests, "every failure is a pgxmock statement-text mismatch"
    return "BEHAVIOURAL", pkgs, tests, residue_lines[0] if residue_lines else ""


def score_site(site: Site, dsn: str, outdir: Path) -> Result:
    digest = sha256(site.path)
    try:
        mutated_line = apply_mutation(site)
        reread = site.path.read_text(encoding="utf-8").splitlines(keepends=True)[site.line - 1]
        if f"({site.text} OR TRUE)" not in reread:
            return Result(site, "INVALID", note="MUTATION NOT APPLIED")
        code, out = run_suite(dsn)
        (outdir / (site.ident.replace("/", "_").replace(":", "-") + ".log")).write_text(out, encoding="utf-8")
        verdict, pkgs, tests, note = classify(out)
        if verdict == "SILENT" and code != 0:
            verdict, note = "INVALID", f"suite exit {code} with no parsed failure — read the log"
        return Result(site, verdict, pkgs, tests, note)
    finally:
        subprocess.run(["git", "checkout", "--", str(site.path.relative_to(REPO))], cwd=REPO, check=True)
        restored = sha256(site.path)
        if restored != digest:
            raise RuntimeError(f"RESTORE FAILED for {site.path}: {digest} -> {restored}")


def report(results: list, json_path: str = "") -> None:
    print("\n─── CENSUS ───")
    for v in ("SILENT", "TEXT-ONLY", "BEHAVIOURAL", "INVALID"):
        group = [r for r in results if r.verdict == v]
        print(f"{v}: {len(group)}")
        for r in group:
            print(f"    {r.site.ident}  {r.site.text}   {r.site.context[:90]}")

    if json_path:
        Path(json_path).write_text(json.dumps([
            {"ident": r.site.ident, "verdict": r.verdict, "text": r.site.text,
             "context": r.site.context, "fail_pkgs": r.fail_pkgs,
             "fail_tests": r.fail_tests, "note": r.note}
            for r in results], indent=2), encoding="utf-8")

    silent = [r for r in results if r.verdict == "SILENT"]
    textonly = [r for r in results if r.verdict == "TEXT-ONLY"]
    behavioural = [r for r in results if r.verdict == "BEHAVIOURAL"]
    print(f"\n{len(silent)} SILENT + {len(textonly)} TEXT-ONLY of {len(results)} tenancy predicates")
    if not behavioural or not silent:
        print("⚠ THIS RUN PRODUCED ONLY ONE VERDICT CLASS — it has not shown it can produce the "
              "other, so read every number above as unconfirmed by its own control.")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--dsn", default=os.environ.get(DSN_ENV, ""))
    ap.add_argument("--only", nargs="*", default=None, help="site idents (file:line:col)")
    ap.add_argument("--list", action="store_true")
    ap.add_argument("--outdir", default="/tmp/w31-tenancy-5r8k")
    ap.add_argument("--json", default="")
    ap.add_argument("--reclassify", action="store_true",
                    help="re-score the saved per-site logs in --outdir without re-running the "
                         "suite. The classifier is the only thing that changes; the evidence is "
                         "the same bytes the mutated run produced.")
    args = ap.parse_args()

    sites = discover(args.only)
    if args.only:
        missing = sorted(set(args.only) - {s.ident for s in sites})
        if missing:
            print("REFUSING TO RUN: --only named sites that do not exist (run --list):")
            for m in missing:
                print(f"    {m}")
            return 2
    if args.list:
        for s in sites:
            print(f"{s.ident}\t{s.text}\t{s.context}")
        print(f"\n{len(sites)} sites")
        return 0

    outdir = Path(args.outdir)

    if args.reclassify:
        results = []
        missing = []
        for s in sites:
            log = outdir / (s.ident.replace("/", "_").replace(":", "-") + ".log")
            if not log.exists():
                missing.append(s.ident)
                continue
            verdict, pkgs, tests, note = classify(log.read_text(encoding="utf-8"))
            results.append(Result(s, verdict, pkgs, tests, note))
        if missing:
            # A site with no log is NOT a silent site. Saying so is the whole point.
            print(f"⚠ {len(missing)} sites have no saved log and are ABSENT from the counts below, "
                  f"not scored: {', '.join(missing)}")
        report(results, args.json)
        return 0

    if not args.dsn:
        print(f"{DSN_ENV} (or --dsn) is required — this census is meaningless without a real Postgres")
        return 2

    outdir.mkdir(parents=True, exist_ok=True)

    # TRACKED modifications only. `git status --porcelain` would also list this script while it is
    # untracked, and refusing on that would make the harness unrunnable on the branch that adds it.
    # What the restore check needs is that no tracked file is already modified.
    dirty = subprocess.run(["git", "diff", "--name-only"], cwd=REPO, capture_output=True, text=True).stdout
    if dirty.strip():
        print("REFUSING TO RUN: tracked files are already modified — a restore would not be "
              "distinguishable from your edit")
        print(dirty)
        return 2

    print("BASELINE (unmutated suite must be GREEN or every verdict below is free) …", flush=True)
    t0 = time.time()
    code, out = run_suite(args.dsn)
    (outdir / "baseline.log").write_text(out, encoding="utf-8")
    if code != 0:
        print(f"BASELINE RED (exit {code}) — ABORTING. {outdir/'baseline.log'}")
        return 1
    print(f"  baseline green in {time.time()-t0:.0f}s, {len(OK_PKG_RE.findall(out))} packages\n", flush=True)

    results = []
    for i, s in enumerate(sites, start=1):
        t = time.time()
        r = score_site(s, args.dsn, outdir)
        results.append(r)
        print(f"[{i:>3}/{len(sites)}] {r.verdict:<12} {s.ident}  ({time.time()-t:.0f}s) "
              f"{len(r.fail_pkgs)} pkg {len(r.fail_tests)} test  {r.note[:80]}", flush=True)

    report(results, args.json)
    return 0


if __name__ == "__main__":
    sys.exit(main())
