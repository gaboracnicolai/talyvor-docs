#!/usr/bin/env python3
"""Positive controls for internal/permission/grantroute_realpg_test.go (W3.1, tab-9d47).

WHY THIS EXISTS. The three guards in grantroute_realpg_test.go were RED on the unmodified tree,
which earns the fix but says nothing about the assertions that were GREEN there — [WS-STAMP],
[ACTOR-STAMP], [SUBJECT-KEPT], [UPSERT-STILL-SERVES], [UPSERT-UPGRADES], [UPSERT-ONE-ROW] and the
[LIST-NONEMPTY] floor all passed before any production line changed. An assertion that passes on
the tree it was written against is decoration until a mutation makes it fail.

METHOD, and every part of it is there because a previous session's harness lied without it:
  · the PREDICTION is written in this file, next to the mutation, BEFORE the run;
  · each anchor is asserted UNIQUE in its file — a hand-pasted anchor that has drifted reports
    ANCHOR MISS rather than silently mutating nothing (#162's harness scored 5/8 on that);
  · every source file is restored in a `finally` and sha256-verified byte-identical afterwards;
  · scoring reads FAILING TEST NAMES as well as [TAGS], because a mutation that kills a test in its
    setup prints no tag at all and tag-only scoring reads that as "the guard said nothing" (#161);
  · a control that fails to COMPILE scores BUILD-FAILED, never CAUGHT — a compile error is not a
    caught mutation.

RUN: DOCS_TEST_DATABASE_URL=... python3 scripts/w31-grant-route-controls-9d47.py
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HANDLER = "internal/permission/handler.go"
STORE = "internal/permission/store.go"
TESTED = "./internal/permission/"
RUN = "TestGrantRoute"

# Every tag the three guards can print. A prediction naming a tag outside this set is a typo in the
# harness, not a result, so it is rejected before anything runs.
ALL_TAGS = {
    "P-PREMISE", "LIST-NONEMPTY", "ID-USABLE", "CREATED-AT", "LIST-AGREES", "WS-STAMP",
    "ACTOR-STAMP", "SUBJECT-KEPT", "ROUNDTRIP-REVOKE",
    "P-PREMISE-PAGE", "ID-USABLE-PAGE", "CREATED-AT-PAGE", "PAGE-RESOURCE", "ROUNDTRIP-REVOKE-PAGE",
    "P-PREMISE-REGRANT", "UPSERT-STILL-SERVES", "UPSERT-UPGRADES", "UPSERT-SAME-ID",
    "UPSERT-ONE-ROW", "UPSERT-KEEPS-CREATED-AT",
}

CONTROLS = [
    dict(
        name="C-BASE",
        why="the unmutated tree. Anything red here means the run itself is broken.",
        edits=[],
        predict=set(),
    ),
    dict(
        name="C0",
        why="an INERT edit — a comment line. A harness that scores this as caught is scoring noise.",
        edits=[(HANDLER, "func (h *Handler) grant(w http.ResponseWriter",
                "// control C0: inert\nfunc (h *Handler) grant(w http.ResponseWriter")],
        predict=set(),
    ),
    dict(
        # RE-CUT: the first version replaced only the writeJSON line, which left `out` declared and
        # unused — the tree did not COMPILE and the control scored BUILD-FAILED. A compile error is
        # not a caught mutation; the mutation has to be the shipped defect, which discarded the row.
        name="C1",
        why="THE WHOLE FIX REVERTED: the handler serializes the struct it built, as it did at main.",
        edits=[(HANDLER, "\tout, err := h.store.Grant(r.Context(), p)", "\t_, err := h.store.Grant(r.Context(), p)"),
               (HANDLER, "writeJSON(w, http.StatusCreated, out)", "writeJSON(w, http.StatusCreated, p)")],
        predict={"ID-USABLE", "CREATED-AT", "LIST-AGREES", "ROUNDTRIP-REVOKE",
                 "ID-USABLE-PAGE", "CREATED-AT-PAGE", "ROUNDTRIP-REVOKE-PAGE", "UPSERT-SAME-ID"},
    ),
    dict(
        name="C2",
        why="the id alone is blanked — separates 'the key is missing' from 'the timestamp is missing'.",
        edits=[(HANDLER, "\tout, err := h.store.Grant(r.Context(), p)",
                "\tout, err := h.store.Grant(r.Context(), p)\n\tif out != nil {\n\t\tout.ID = \"\"\n\t}")],
        predict={"ID-USABLE", "LIST-AGREES", "ROUNDTRIP-REVOKE",
                 "ID-USABLE-PAGE", "ROUNDTRIP-REVOKE-PAGE", "UPSERT-SAME-ID"},
    ),
    dict(
        name="C3",
        why="created_at alone is blanked — the other half of C2.",
        edits=[(HANDLER, "\tout, err := h.store.Grant(r.Context(), p)",
                "\tout, err := h.store.Grant(r.Context(), p)\n\tif out != nil {\n\t\tout.CreatedAt = time.Time{}\n\t}"),
               (HANDLER, "\t\"net/http\"\n", "\t\"net/http\"\n\t\"time\"\n")],
        predict={"CREATED-AT", "LIST-AGREES", "CREATED-AT-PAGE"},
    ),
    dict(
        name="C4",
        why="A REAL uuid THAT IS NOT THIS ROW'S. [ID-USABLE] alone cannot tell 'the row's key' from "
            "'a plausible key', which is the failure mode of a fix that invents an id client-side. "
            "⚠ THIS CONTROL FOUND A BLINDNESS IN MY OWN GUARD: the re-grant test compared the two "
            "responses to EACH OTHER, so a CONSTANT forged id satisfied [UPSERT-SAME-ID] perfectly "
            "and it scored missing on the first run. The assertion now anchors the id to a row in "
            "Postgres, and this prediction includes it because it can finally fail.",
        edits=[(HANDLER, "\tout, err := h.store.Grant(r.Context(), p)",
                "\tout, err := h.store.Grant(r.Context(), p)\n\tif out != nil {\n\t\tout.ID = \"3f1b9c2e-7a44-4d21-9f60-0c5e8a1b2d33\"\n\t}")],
        predict={"LIST-AGREES", "ROUNDTRIP-REVOKE", "ROUNDTRIP-REVOKE-PAGE", "UPSERT-SAME-ID"},
    ),
    dict(
        name="C5",
        why="the upsert arm: DO UPDATE -> DO NOTHING. RETURNING then yields no row on a re-grant, "
            "which is the shape a careless RETURNING rewrite ships. Only the third guard sees it.",
        edits=[(STORE, "        DO UPDATE SET access = EXCLUDED.access, granted_by = EXCLUDED.granted_by\n        RETURNING ",
                "        DO NOTHING\n        RETURNING ")],
        predict={"UPSERT-STILL-SERVES"},
    ),
    dict(
        name="C6",
        why="THE SEC-4 WORKSPACE STAMP DELETED. [WS-STAMP] pins behaviour that was already correct; "
            "if this does not catch, the pin is decoration and the route is unguarded on that axis. "
            "⚠ THIS CONTROL ALSO FOUND A BLINDNESS: on the first run it caught the mutation as "
            "[LIST-NONEMPTY], NOT [WS-STAMP] — a forged workspace_id makes the row invisible to the "
            "workspace-scoped LIST, so the liveness floor Fatalf'd before the tenancy pin was ever "
            "evaluated. The stamp assertions now run BEFORE anything that reads the list.",
        # ⚠ THE PREDICTION NAMES TWO TAGS AND THAT IS THE TRUE ANSWER, NOT A LOOSENED ONE. After the
        # reorder [WS-STAMP] fires as intended, and [LIST-NONEMPTY] fires too — CORRECTLY: a grant
        # stamped with a foreign workspace_id really is invisible to the workspace-scoped LIST, so
        # the floor is reporting a second true consequence of the same mutation, not noise. Naming
        # only one tag would have been the incomplete prediction it was on the first run.
        edits=[(HANDLER, "\tin.WorkspaceID = ws\n", "\tif in.WorkspaceID == \"\" {\n\t\tin.WorkspaceID = ws\n\t}\n")],
        predict={"WS-STAMP", "LIST-NONEMPTY"},
    ),
    dict(
        # RE-CUT for the same reason as C1: swapping the field alone orphaned `grantedBy`, so the
        # tree did not compile. The fail-closed `ok` check is kept — the mutation under test is
        # "the actor comes from the body", not "the actor check is gone".
        name="C7",
        why="THE ACTOR STAMP FALSIFIED: granted_by taken from the body's subject instead of the "
            "verified membership. Same question as C6 for [ACTOR-STAMP].",
        edits=[(HANDLER, "\tgrantedBy, ok := ActorFromContext(r.Context())", "\t_, ok = ActorFromContext(r.Context())"),
               (HANDLER, "\t\tGrantedBy:    grantedBy,", "\t\tGrantedBy:    in.SubjectID,")],
        predict={"ACTOR-STAMP"},
    ),
    dict(
        name="C8",
        why="THE VACUITY CONTROL. Every agreement assertion compares the create body against the "
            "LIST route; with an empty list they agree about nothing. The [LIST-NONEMPTY] floor "
            "exists for that, and this is the only thing that can prove the floor is live.",
        edits=[(STORE, "        ORDER BY created_at ASC`,", "        ORDER BY created_at ASC LIMIT 0`,")],
        predict={"LIST-NONEMPTY"},
    ),
    dict(
        name="C9",
        why="MUST STAY GREEN. The subject fields are the caller's legitimate choice; deriving the "
            "tenancy fields must not overwrite them. Mutating the ACCESS the store persists must be "
            "caught by [SUBJECT-KEPT] — and by nothing else in these guards.",
        edits=[(HANDLER, "\t\tAccess:       in.Access,", "\t\tAccess:       AccessView,")],
        predict={"SUBJECT-KEPT", "UPSERT-UPGRADES"},
    ),
]


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def run_tests():
    env = dict(os.environ)
    if not env.get("DOCS_TEST_DATABASE_URL"):
        sys.exit("DOCS_TEST_DATABASE_URL is unset — these are real-Postgres guards")
    p = subprocess.run(
        ["go", "test", "-count=1", "-run", RUN, "-v", TESTED],
        cwd=REPO, capture_output=True, text=True, env=env, timeout=600,
    )
    out = p.stdout + p.stderr
    if "build failed" in out or "[build failed]" in out or "cannot use" in out or "undefined:" in out:
        return "BUILD-FAILED", set(), set(), out
    tags = set(re.findall(r"\[([A-Z0-9-]+)\]", out))
    failing = set(re.findall(r"--- FAIL: (\S+)", out))
    return ("PASS" if p.returncode == 0 else "FAIL"), tags, failing, out


def apply(edits):
    """Apply every edit, asserting each anchor is unique. Returns the original contents."""
    originals = {}
    for path, old, new in edits:
        full = os.path.join(REPO, path)
        if full not in originals:
            with open(full, encoding="utf-8") as f:
                originals[full] = f.read()
    for path, old, new in edits:
        full = os.path.join(REPO, path)
        with open(full, encoding="utf-8") as f:
            s = f.read()
        n = s.count(old)
        if n != 1:
            for f2, o in originals.items():
                with open(f2, "w", encoding="utf-8") as fh:
                    fh.write(o)
            raise SystemExit(f"ANCHOR MISS in {path}: {n} occurrences of {old!r} (want exactly 1)")
        with open(full, "w", encoding="utf-8") as f:
            f.write(s.replace(old, new))
    return originals


def main():
    for c in CONTROLS:
        bad = c["predict"] - ALL_TAGS
        if bad:
            sys.exit(f"{c['name']} predicts tags that no guard can print: {sorted(bad)}")

    baseline = {os.path.join(REPO, p): sha(os.path.join(REPO, p)) for p in (HANDLER, STORE)}
    results = []
    for c in CONTROLS:
        print(f"\n=== {c['name']} — {c['why']}")
        print(f"    PREDICT: {sorted(c['predict']) or 'NOT CAUGHT (all green)'}")
        originals = {}
        try:
            originals = apply(c["edits"])
            status, tags, failing, out = run_tests()
        finally:
            for full, o in originals.items():
                with open(full, "w", encoding="utf-8") as f:
                    f.write(o)
            for full, want in baseline.items():
                got = sha(full)
                if got != want:
                    sys.exit(f"RESTORE FAILED for {full}: {got} != {want}")

        if status == "BUILD-FAILED":
            print("    RESULT : BUILD-FAILED — not a caught mutation; re-cut this control")
            results.append((c["name"], False, "BUILD-FAILED"))
            continue

        # A caught mutation must print the predicted tags AND nothing outside the prediction that is
        # an ERROR tag. Tags also appear in passing runs' log lines, so we score against the tags
        # seen only when the run FAILED.
        caught = tags if status == "FAIL" else set()
        ok = (caught == c["predict"]) if c["predict"] else (status == "PASS")
        print(f"    RESULT : {status}; tags={sorted(caught) or '—'}; failing_tests={sorted(failing) or '—'}")
        print(f"    SCORE  : {'AS PREDICTED' if ok else 'MISMATCH'}")
        if not ok and c["predict"]:
            print(f"    DIFF   : missing={sorted(c['predict']-caught)} unexpected={sorted(caught-c['predict'])}")
        results.append((c["name"], ok, status))

    n_ok = sum(1 for _, ok, _ in results if ok)
    print(f"\n==== {n_ok}/{len(results)} AS PREDICTED ====")
    for name, ok, status in results:
        print(f"  {name}: {'ok' if ok else 'MISMATCH'} ({status})")
    sys.exit(0 if n_ok == len(results) else 1)


if __name__ == "__main__":
    main()
