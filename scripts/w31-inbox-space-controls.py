#!/usr/bin/env python3
"""Positive controls for the approval-inbox space plumbing (W3.1 finding (12)(b)).

WHAT THIS PROVES, AND WHY IT IS NOT AN EXIT CODE. Each control makes ONE deliberate change to
the product and records THE SET OF ASSERTIONS THAT FIRED — the failing test names plus the
first line of each failure message. A pass/fail verdict cannot tell a caught mutation from a
file that no longer compiles, and it cannot tell WHICH assertion spoke, so it cannot show that
an assertion is earned by anything. The catcher is predicted here BEFORE the run; a control
that reds through a different assertion than predicted is a wrong prediction, and it is
recorded as one.

Every control:
  · asserts EVERY anchor's occurrence count in the target file BEFORE any write (a control
    that half-applies is a control that reports a working guard as blind);
  · applies all of its edits in ONE write;
  · restores the file in a `finally` and re-checks its sha256, so a crash between mutate and
    restore cannot leave mutated source on disk;
  · declares a MUST-STAY-GREEN set — the tests that must NOT red — so a control that merely
    breaks everything cannot score as a catch.

Run:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-inbox-space-controls.py
"""

from __future__ import annotations

import hashlib
import json
import os
import pathlib
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
FRONTEND = ROOT / "frontend"

INBOX = "frontend/src/pages/ApprovalInbox.tsx"
API = "frontend/src/api/approval.ts"
STORE = "internal/approval/store.go"
HANDLER = "internal/approval/handler.go"

# ─── the assertions this campaign claims to justify ────────────────────────────
# tag -> what it asserts. The harness FAILS if any tag is claimed by no control: an assertion
# no control can reach is an assertion nothing earns, and it could be deleted or written
# constant-true without a single control noticing.
ASSERTIONS = {
    "F1-LEAF": "Open's destination resolves to the page route, not the catch-all",
    "F1-PARAMS": "the resolved ids are the ROW's ids (a right-shaped wrong URL is not enough)",
    "F2-PATH": "the exact pathname the click produces",
    "F3-PERROW": "a second row with a different space gets ITS space (not a constant/global)",
    "B1-PRESENT": "space_id is on the WIRE (not merely on the Go struct)",
    "B1-VALUE": "space_id equals the page's own space, read with SQL against the pool",
    "B2-PERROW": "each row reports its own space (server side)",
    "B3-EMPTY": "the decide route still tolerates an empty {spaceID} segment",
    "B4-MOCK": "the statement JOINs pages and the row carries the column",
    "TYPE": "the API type boundary cannot silently drop the field",
}

# ─── controls ─────────────────────────────────────────────────────────────────
# runner: "frontend" (whole vitest suite), "backend" (whole approval package), "typecheck".
CONTROLS = [
    {
        "id": "C1",
        "why": "THE ORIGINAL DEFECT: the hardcoded empty space, put back.",
        "file": INBOX,
        "runner": "frontend",
        "edits": [(
            "onOpen={() => onOpenPage(req.space_id, req.page_id)}",
            'onOpen={() => onOpenPage("", req.page_id)}',
        )],
        "expect": {"F1-LEAF", "F2-PATH", "F3-PERROW"},
    },
    {
        "id": "C2",
        "why": ("A CONSTANT that happens to be right for the first row. C1 and C2 differ only "
                "in intent and neither is worth anything alone: a guard reddening on any edit "
                "to that line passes C1 and cannot distinguish C2, and a guard asserting only "
                "the URL's SHAPE passes C2 while the button still opens the wrong space."),
        "file": INBOX,
        "runner": "frontend",
        "edits": [(
            "onOpen={() => onOpenPage(req.space_id, req.page_id)}",
            'onOpen={() => onOpenPage("sp-42", req.page_id)}',
        )],
        "expect": {"F3-PERROW"},
    },
    {
        "id": "C11",
        "why": ("THE TWO IDS ON THE ROW SWAPPED — the request id where the page id belongs. The "
                "URL still RESOLVES to the page route, so the leaf assertion is green; only the "
                "params assertion can see it."),
        "file": INBOX,
        "runner": "frontend",
        "edits": [(
            "onOpen={() => onOpenPage(req.space_id, req.page_id)}",
            "onOpen={() => onOpenPage(req.space_id, req.id)}",
        )],
        "expect": {"F1-PARAMS", "F2-PATH", "F3-PERROW"},
    },
    {
        "id": "C6",
        "why": ("MUST-NOT-CATCH: the row's visible copy. The guard finds the row by the page id "
                "in its accessible name, NOT by the word 'Page' — a guard that reds on a copy "
                "edit is a guard someone deletes at the next merge."),
        "file": INBOX,
        "runner": "frontend",
        "edits": [(
            "<div className=\"text-sm font-medium\">Page {req.page_id.slice(0, 8)}</div>",
            "<div className=\"text-sm font-medium\">Doc {req.page_id.slice(0, 8)}</div>",
        )],
        "expect": set(),
    },
    {
        "id": "C3",
        "why": ("THE JOINED COLUMN REMOVED, both halves in one write (the SELECT and its scan "
                "destination). Half of this control would be a scan-count error rather than the "
                "case under test."),
        "file": STORE,
        "runner": "backend",
        "edits": [
            ("`SELECT `+prefixed(\"a\", requestCols)+`, p.space_id",
             "`SELECT `+prefixed(\"a\", requestCols)+`"),
            ("\t\t&it.SpaceID,\n", ""),
        ],
        "expect": {"B1-VALUE", "B2-PERROW", "B4-MOCK"},
    },
    {
        "id": "C4",
        "why": ("A NON-EMPTY WRONG VALUE: the workspace id where the space id belongs. This is "
                "what earns an EQUALITY assertion; a 'space_id is present and non-empty' check "
                "passes it."),
        "file": STORE,
        "runner": "backend",
        "edits": [(", p.space_id", ", a.workspace_id")],
        "expect": {"B1-VALUE", "B2-PERROW"},
    },
    {
        "id": "C5",
        "why": ("THE FIELD KEPT AND THE WIRE NAME CHANGED. The Go struct is still populated, so "
                "a test decoding into the typed struct would stay green; only reading the "
                "response as generic JSON can see it. Sole speaker for the PRESENCE assertion."),
        "file": STORE,
        "runner": "backend",
        "edits": [('SpaceID string `json:"space_id"`', 'SpaceID string `json:"spaceID"`')],
        "expect": {"B1-PRESENT", "B2-PERROW"},
    },
    {
        "id": "C10",
        "why": ("THE FIRST ROW'S SPACE REUSED FOR EVERY LATER ROW — a resolve-once-and-reuse "
                "slip. With ONE page in the fixture it is invisible; it is the entire reason "
                "the per-row test seeds two pages in two spaces."),
        "file": STORE,
        "runner": "backend",
        "edits": [(
            "\t\tout = append(out, *it)",
            "\t\tif len(out) > 0 {\n\t\t\tit.SpaceID = out[0].SpaceID\n\t\t}\n\t\tout = append(out, *it)",
        )],
        "expect": {"B2-PERROW"},
    },
    {
        "id": "C7",
        "why": ("MUST-NOT-CATCH: an unrelated edit in the same function (its error wrapper). "
                "These guards are about the space the row reports, not about the shape of the "
                "statement around it."),
        "file": STORE,
        "runner": "backend",
        "edits": [("approval: list pending: %w", "approval: pending list: %w")],
        "expect": set(),
    },
    {
        "id": "C9",
        "why": ("RECORDED INERT, AND THE MEASUREMENT IS THE POINT. I predicted this would be "
                "B3-EMPTY's sole speaker: bind the route param to a non-empty regex and the "
                "empty segment should stop matching. IT DOES NOT — chi matches the param node "
                "and hands the handler \"\" WITHOUT applying the constraint, measured directly "
                "on all three patterns: `{spaceID}`, `{spaceID:[^/]+}` and "
                "`{spaceID:[a-zA-Z0-9-]+}` ALL answer 200 with spaceID=\"\" on "
                "/spaces//pages/p-1/x. So a reader who 'bounds' one of these routes with a "
                "regex has changed nothing, and the wrong prediction is kept here because it "
                "is the only evidence of that."),
        "file": HANDLER,
        "runner": "backend",
        "edits": [(
            '"/spaces/{spaceID}/pages/{pageID}/approval/{requestID}/decide"',
            '"/spaces/{spaceID:[^/]+}/pages/{pageID}/approval/{requestID}/decide"',
        )],
        "expect": set(),
    },
    {
        "id": "C9b",
        "why": ("THE MUTATION B3 ACTUALLY EXISTS FOR, once C9 showed the route layer cannot "
                "produce it: a handler-level rejection of the empty segment — what a later "
                "'hardening' pass would plausibly add. Sole speaker for B3-EMPTY."),
        "file": HANDLER,
        "runner": "backend",
        "edits": [(
            '\trequestID := chi.URLParam(r, "requestID")',
            '\tif chi.URLParam(r, "spaceID") == "" {\n'
            '\t\twriteErr(w, http.StatusBadRequest, "space is required")\n'
            '\t\treturn\n'
            '\t}\n'
            '\trequestID := chi.URLParam(r, "requestID")',
        )],
        "expect": {"B3-EMPTY"},
    },
    {
        "id": "C8",
        "why": ("THE TYPE BOUNDARY REVERTED. #55 measured this exact escape in this repo: three "
                "cost fields arrived on the wire and were dropped at the TypeScript boundary, "
                "and nothing was red. It is caught by TYPECHECK, not by any test — recorded as "
                "a different channel rather than counted as a test catch."),
        "file": API,
        "runner": "typecheck",
        "edits": [("apiRequest<PendingApproval[]>", "apiRequest<ApprovalRequest[]>")],
        "expect": {"TYPE"},
    },
]

# ─── verdict extraction ───────────────────────────────────────────────────────


def classify_frontend(name: str, msg: str) -> str:
    first = msg.strip().splitlines()[0] if msg.strip() else ""
    if "has no route for" in first:
        return "F1-LEAF"
    if "route table resolves to the page route" in name:
        return "F1-PARAMS"
    if "actually mounts the page" in name:
        return "F2-PATH" if "toBe" in first or "expected" in first else "F2-MOUNT"
    if "carries the space the API reported" in name:
        return "F3-PERROW"
    return f"?FRONTEND:{name}"


def classify_backend(name: str, msg: str) -> str:
    if "space_id ABSENT" in msg:
        return "B1-PRESENT"
    if "pending row space_id =" in msg:
        return "B1-VALUE"
    if "is not per-row" in msg:
        return "B2-PERROW"
    if "EMPTY space segment" in msg:
        return "B3-EMPTY"
    if "TestListPending" in name:
        return "B4-MOCK"
    return f"?BACKEND:{name}:{msg.strip().splitlines()[0][:80] if msg.strip() else ''}"


def run_frontend() -> tuple[set[str], list[str], str | None]:
    out = pathlib.Path(tempfile.mkstemp(suffix=".json")[1])
    p = subprocess.run(
        ["npx", "vitest", "run", "--reporter=json", f"--outputFile={out}"],
        cwd=FRONTEND, capture_output=True, text=True,
    )
    if not out.exists() or not out.read_text().strip():
        return set(), [], f"vitest produced no report (rc={p.returncode})\n{p.stdout[-2000:]}{p.stderr[-2000:]}"
    report = json.loads(out.read_text())
    out.unlink(missing_ok=True)
    tags, failed = set(), []
    for suite in report.get("testResults", []):
        for a in suite.get("assertionResults", []):
            if a.get("status") == "failed":
                name = a.get("fullName", "")
                failed.append(name)
                msg = "\n".join(a.get("failureMessages", []) or [])
                tags.add(classify_frontend(name, msg))
    return tags, failed, None


def run_backend() -> tuple[set[str], list[str], str | None]:
    env = dict(os.environ)
    p = subprocess.run(
        ["go", "test", "-count=1", "-v", "./internal/approval/"],
        cwd=ROOT, capture_output=True, text=True, env=env,
    )
    # A compile error is NOT a catch. go test prints "[build failed]" and no test output at all.
    if "[build failed]" in p.stdout + p.stderr or "cannot use" in p.stderr:
        return set(), [], "BUILD ERROR — not a catch:\n" + (p.stderr or p.stdout)[-1500:]
    tags, failed, buf, cur = set(), [], [], None
    for line in p.stdout.splitlines():
        if line.startswith("=== RUN"):
            cur, buf = line.split()[-1], []
        elif line.startswith("--- FAIL:") or line.startswith("    --- FAIL:"):
            name = line.split()[2]
            failed.append(name)
            tags.add(classify_backend(name, "\n".join(buf)))
            buf = []
        elif line.startswith("--- PASS") or line.startswith("=== CONT"):
            buf = []
        elif cur:
            buf.append(line)
    return tags, failed, None


def run_typecheck() -> tuple[set[str], list[str], str | None]:
    p = subprocess.run(["npx", "tsc", "--noEmit"], cwd=FRONTEND, capture_output=True, text=True)
    if p.returncode != 0:
        return {"TYPE"}, ["tsc --noEmit"], None
    return set(), [], None


RUNNERS = {"frontend": run_frontend, "backend": run_backend, "typecheck": run_typecheck}

# ─── driver ───────────────────────────────────────────────────────────────────


def main() -> int:
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL is not set — the backend controls need a real Postgres.")
        return 2

    print("BASELINE (unmodified tree): every guard must be GREEN before any control means anything.")
    baseline_bad = []
    for runner in ("frontend", "backend", "typecheck"):
        tags, failed, err = RUNNERS[runner]()
        print(f"  {runner:<10} failing={sorted(failed) or 'none'}{'  ERR=' + err if err else ''}")
        if failed or err:
            baseline_bad.append(runner)
    if baseline_bad:
        print(f"BASELINE IS NOT GREEN ({baseline_bad}) — stopping; no control verdict would mean anything.")
        return 1

    results, claimed = [], set()
    for c in CONTROLS:
        path = ROOT / c["file"]
        original = path.read_bytes()
        digest = hashlib.sha256(original).hexdigest()
        text = original.decode()

        # EVERY anchor asserted BEFORE any write.
        anchor_err = None
        for anchor, _ in c["edits"]:
            n = text.count(anchor)
            if n != 1:
                anchor_err = f"ANCHOR {anchor!r} appears {n}x in {c['file']}, expected 1x"
                break
        if anchor_err:
            print(f"\n{c['id']}: {anchor_err}")
            results.append((c["id"], "ANCHOR", set(), c["expect"], []))
            continue

        mutated = text
        for anchor, repl in c["edits"]:
            mutated = mutated.replace(anchor, repl, 1)
        # The whole replacement must be present, not merely its first line: a control that
        # inserts under a copy of its own anchor can half-apply invisibly.
        applied = all(repl in mutated for _, repl in c["edits"] if repl)

        try:
            path.write_text(mutated)
            if not applied:
                tags, failed, err = set(), [], "CONTROL DID NOT FULLY APPLY"
            else:
                tags, failed, err = RUNNERS[c["runner"]]()
        finally:
            path.write_bytes(original)
            back = hashlib.sha256(path.read_bytes()).hexdigest()
            if back != digest:
                print(f"FATAL: {c['file']} not restored ({back} != {digest})")
                return 1

        ok = (tags == c["expect"]) and not err
        claimed |= tags
        results.append((c["id"], "OK" if ok else "MISMATCH", tags, c["expect"], failed))
        verdict = "AS PREDICTED" if ok else "*** NOT AS PREDICTED ***"
        print(f"\n{c['id']} [{c['runner']}] {verdict}")
        print(f"    why      : {c['why']}")
        print(f"    predicted: {sorted(c['expect']) or 'NOTHING (must-not-catch)'}")
        print(f"    fired    : {sorted(tags) or 'NOTHING'}")
        print(f"    tests    : {sorted(set(failed)) or 'none'}")
        if err:
            print(f"    ERROR    : {err}")

    print("\n" + "=" * 78)
    bad = [r for r in results if r[1] != "OK"]
    unclaimed = sorted(set(ASSERTIONS) - claimed)
    print(f"{len(results) - len(bad)}/{len(results)} controls as predicted")
    if unclaimed:
        print(f"*** ASSERTIONS CLAIMED BY NO CONTROL: {unclaimed} — each is earned by nothing.")
    for tag in sorted(claimed):
        speakers = [r[0] for r in results if tag in r[2]]
        note = "  (SOLE SPEAKER)" if len(speakers) == 1 else ""
        print(f"  {tag:<12} fired by {speakers}{note}")
    return 1 if (bad or unclaimed) else 0


if __name__ == "__main__":
    sys.exit(main())
