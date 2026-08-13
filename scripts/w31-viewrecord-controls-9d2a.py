#!/usr/bin/env python3
"""Positive controls for the dead view-record finding (W3.1, tab-9d2a).

WHAT IS BEING CONTROLLED
------------------------
Two guards land together and they answer different questions, so each is controlled against
the mutations that belong to it:

  frontend/src/pages/PageView.viewrecord.wire.test.tsx
      what the SHIPPED SCREEN puts on the wire when a document is opened and left.
      Tags: [VIEW-RECORDED-AT-ALL] [VIEW-POST-HAS-BODY] [VIEW-POST-ABOVE-MIN-DURATION]

  internal/analytics/recordview_emptybody_realpg_test.go
      what the SERVER does with each of those shapes, on real Postgres.
      Tags: [EMPTY-BODY-REJECTED] [SUB-THRESHOLD-SILENT] [THRESHOLD-VIEW-RECORDED]

The Go file PASSED ON ITS FIRST RUN — it pins the contract the frontend guard cites rather
than a defect — so C6/C7/C8 exist precisely because a guard that has never been red is a
guard nobody has shown can be.

Every mutation is applied to PRODUCT source, never to a guard (C3 is the one exception and it
DELETES a guard on purpose, which is the point of a vacuity floor). Every file is restored in
a `finally` and its sha256 compared with the pre-run digest before the script exits non-zero
on any mismatch — a harness that cannot clean up lies about the next control it runs.

Scoring is by LEADING TAG only: a control is CAUGHT if the tags it predicts appear in the
failure output and the ones it predicts must NOT appear are absent. Exit code alone is not
enough — [VIEW-POST-HAS-BODY] and [VIEW-POST-ABOVE-MIN-DURATION] are two different failure
directions of the same test and a bypass that trips the wrong one is not a pass.

Usage:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-viewrecord-controls-9d2a.py
"""

from __future__ import annotations

import hashlib
import os
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
FE = ROOT / "frontend"

PAGEVIEW = ROOT / "frontend/src/pages/PageView.tsx"
PAGESAPI = ROOT / "frontend/src/api/pages.ts"
GUARD_FE = ROOT / "frontend/src/pages/PageView.viewrecord.wire.test.tsx"
HANDLER = ROOT / "internal/analytics/handler.go"
STORE = ROOT / "internal/analytics/store.go"

TOUCHED = [PAGEVIEW, PAGESAPI, GUARD_FE, HANDLER, STORE]

# ── the mutation fragments ───────────────────────────────────────────────────────────────

RECORDVIEW_METHOD_BODYLESS = """  recordView(spaceID: string, pageID: string) {
    return apiRequest<{ ok: boolean }>(`/v1/spaces/${spaceID}/pages/${pageID}/view`, {
      method: "POST",
    });
  },
"""

RECORDVIEW_METHOD_ZERO_DURATION = """  recordView(spaceID: string, pageID: string) {
    return apiRequest<{ ok: boolean }>(`/v1/spaces/${spaceID}/pages/${pageID}/view`, {
      method: "POST",
      body: { duration_sec: 0 },
    });
  },
"""

VERIFY_ANCHOR = "  verify(spaceID: string, pageID: string) {"

MOUNT_EFFECT_ANCHOR = """  useEffect(() => {
    if (!page) return;
    pushRecentPage({"""

MOUNT_EFFECT_WITH_CALL = """  useEffect(() => {
    if (!page) return;
    void pagesApi.recordView(space.id, page.id).catch(() => undefined);
    pushRecentPage({"""

CLEANUP_ANCHOR = """    return () => {
      flush();
      window.removeEventListener("beforeunload", flush);
    };"""

CLEANUP_NO_FLUSH = """    return () => {
      window.removeEventListener("beforeunload", flush);
    };"""

DECODE_STRICT = """	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}"""

DECODE_TOLERANT = """	_ = json.NewDecoder(r.Body).Decode(&in)"""


def sub(path: pathlib.Path, old: str, new: str) -> None:
    """Exact single-occurrence replacement. Anything else is a harness bug, not a control."""
    s = path.read_text()
    n = s.count(old)
    if n != 1:
        raise SystemExit(
            f"HARNESS BUG: anchor occurs {n}x in {path.relative_to(ROOT)} (want exactly 1).\n"
            f"anchor:\n{old}"
        )
    path.write_text(s.replace(old, new))


def add_recordview(method_src: str) -> None:
    sub(PAGESAPI, VERIFY_ANCHOR, method_src + VERIFY_ANCHOR)


# ── the controls ─────────────────────────────────────────────────────────────────────────


def c0_inert() -> None:
    sub(
        PAGEVIEW,
        "  // Remember this page as a recent for the SearchModal's empty state.",
        "  // Remember this page as a recent entry for the SearchModal's empty state.",
    )


def c1_defect() -> None:
    add_recordview(RECORDVIEW_METHOD_BODYLESS)
    sub(PAGEVIEW, MOUNT_EFFECT_ANCHOR, MOUNT_EFFECT_WITH_CALL)


def c2_bypass() -> None:
    add_recordview(RECORDVIEW_METHOD_ZERO_DURATION)
    sub(PAGEVIEW, MOUNT_EFFECT_ANCHOR, MOUNT_EFFECT_WITH_CALL)


def c3_vacuity_floor() -> None:
    c1_defect()
    GUARD_FE.unlink()


def c4_no_flush() -> None:
    sub(PAGEVIEW, CLEANUP_ANCHOR, CLEANUP_NO_FLUSH)


def c5_client_threshold() -> None:
    sub(PAGEVIEW, "      if (seconds < 3) return;", "      if (seconds < 5) return;")


def c6_tolerant_decode() -> None:
    sub(HANDLER, DECODE_STRICT, DECODE_TOLERANT)


def c7_min_duration() -> None:
    sub(STORE, "const minDuration = 3", "const minDuration = 1")


def c8_duration_dropped() -> None:
    sub(HANDLER, "		Duration:    in.Duration,", "		Duration:    0,")


# ── runners ──────────────────────────────────────────────────────────────────────────────


def run_fe_guard() -> tuple[int, str]:
    p = subprocess.run(
        ["npx", "vitest", "run", "src/pages/PageView.viewrecord.wire.test.tsx"],
        cwd=FE, capture_output=True, text=True,
    )
    return p.returncode, p.stdout + p.stderr


def run_fe_suite() -> tuple[int, str]:
    p = subprocess.run(["npx", "vitest", "run"], cwd=FE, capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def run_go_guard() -> tuple[int, str]:
    p = subprocess.run(
        ["go", "test", "-timeout", "300s", "-count=1",
         "-run", "TestRecordView_EmptyBodyAndSubThresholdRecordNothing_RealPG",
         "./internal/analytics/"],
        cwd=ROOT, capture_output=True, text=True,
    )
    return p.returncode, p.stdout + p.stderr


CONTROLS = [
    # name, mutate, runner, expect_tags, forbid_tags, note
    ("C0 inert — reword a comment on the surviving effect",
     c0_inert, run_fe_guard, [], [],
     "must NOT be caught: the guard reads the wire, not the prose"),
    ("C1 the defect exactly as it shipped — bodyless mount POST restored",
     c1_defect, run_fe_guard, ["[VIEW-POST-HAS-BODY]"], ["[VIEW-RECORDED-AT-ALL]"],
     "the red-first proof"),
    ("C2 the bypass — mount POST carries a body, duration_sec 0",
     c2_bypass, run_fe_guard, ["[VIEW-POST-ABOVE-MIN-DURATION]"], ["[VIEW-POST-HAS-BODY]"],
     "proves the guard asserts the SERVER's 3s threshold, not merely 'a body was sent'"),
    ("C3 vacuity floor — the defect restored AND this guard file deleted",
     c3_vacuity_floor, run_fe_suite, [], [],
     "the whole frontend suite must stay GREEN: nothing else in it can see this"),
    ("C4 the non-vacuity anchor — unmount flush removed, no view POST at all",
     c4_no_flush, run_fe_guard, ["[VIEW-RECORDED-AT-ALL]"], ["[VIEW-POST-HAS-BODY]"],
     "proves 'every view POST is well-formed' cannot pass by there being none"),
    ("C5 the client's own threshold moved 3s -> 5s",
     c5_client_threshold, run_fe_guard, [], [],
     "must stay GREEN: the guard is pinned to the server's constant, not the client's"),
    ("C6 handler made tolerant of an empty body",
     c6_tolerant_decode, run_go_guard, ["[EMPTY-BODY-REJECTED]"], ["[THRESHOLD-VIEW-RECORDED]"],
     "proves case A is live — and shows the 'obvious' fix turns a 400 into a silent drop"),
    ("C7 minDuration 3 -> 1, so a 2s view is recorded",
     c7_min_duration, run_go_guard, ["[SUB-THRESHOLD-SILENT]"], ["[EMPTY-BODY-REJECTED]"],
     "proves case B is live and the 3s the frontend guard cites is really the server's"),
    ("C8 handler stops forwarding the duration (Duration: in.Duration -> 0)",
     c8_duration_dropped, run_go_guard, ["[THRESHOLD-VIEW-RECORDED]"], ["[EMPTY-BODY-REJECTED]"],
     "the exact mutation the repo's own notes record the whole suite missing"),
]


def main() -> int:
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set (C6/C7/C8 need real Postgres)", file=sys.stderr)
        return 2

    originals = {p: p.read_text() for p in TOUCHED}
    digests = {p: hashlib.sha256(originals[p].encode()).hexdigest() for p in TOUCHED}

    results = []
    for name, mutate, runner, expect, forbid, note in CONTROLS:
        try:
            mutate()
            code, out = runner()
        finally:
            for p in TOUCHED:
                p.write_text(originals[p])
        seen = [t for t in expect if t in out]
        bad = [t for t in forbid if t in out]
        if expect:
            ok = len(seen) == len(expect) and not bad and code != 0
            verdict = "CAUGHT" if ok else "MISSED"
        else:
            ok = code == 0 and not bad
            verdict = "NOT CAUGHT (as predicted)" if ok else "CAUGHT (UNEXPECTED)"
        results.append((ok, name, verdict, expect, seen, bad, code, note))
        print(f"[{'ok' if ok else 'FAIL'}] {name}\n"
              f"        {verdict}; exit={code}; tags seen={seen or '-'}; forbidden seen={bad or '-'}\n"
              f"        {note}")

    # A harness that cannot restore its own mutations cannot be believed about the next one.
    dirty = [p for p in TOUCHED
             if hashlib.sha256(p.read_text().encode()).hexdigest() != digests[p]]
    if dirty:
        print("\nRESTORE FAILED for: " + ", ".join(str(p.relative_to(ROOT)) for p in dirty))
        return 1

    passed = sum(1 for r in results if r[0])
    print(f"\n{passed}/{len(results)} controls behaved as predicted; "
          f"all {len(TOUCHED)} touched files restored (sha256 verified)")
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
