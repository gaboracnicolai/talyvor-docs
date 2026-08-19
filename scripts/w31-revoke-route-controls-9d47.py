#!/usr/bin/env python3
"""Positive controls for internal/sharing/revokeroute_realpg_test.go (W3.1, tab-9d47).

⚠⚠ THIS HARNESS IS THE WHOLE VALUE OF THAT FILE, AND SAYING SO IS NOT MODESTY.

Every one of the four guards PASSED ON ITS FIRST RUN against the unmodified tree at `a641d4eb`. The
route is correct: a revoked token stops opening, a second revoke is 404 rather than a cheerful
`{"ok":true}`, a cross-page revoke is refused, a view-tier colleague is refused, a foreign tenant
gets 404 and not 403. RED-FIRST IS NOT CLAIMED and must not be implied. A guard written against a
tree it cannot fail on is decoration until a mutation makes it red, so every assertion below is
earned here or it is not earned at all.

Same method as scripts/w31-grant-route-controls-9d47.py, and each part is there for a reason a
previous harness in this repository lied without it: predictions written BEFORE the run; anchors
asserted unique (a drifted anchor reports ANCHOR MISS instead of silently mutating nothing); every
file restored in a `finally` and sha256-verified; scoring reads failing TEST NAMES as well as
[TAGS], because a mutation that kills a test in its setup prints no tag and tag-only scoring reads
that as silence; a control that fails to COMPILE scores BUILD-FAILED, never CAUGHT.

RUN: DOCS_TEST_DATABASE_URL=... python3 scripts/w31-revoke-route-controls-9d47.py
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HANDLER = "internal/sharing/handler.go"
STORE = "internal/sharing/store.go"
MIDDLEWARE = "internal/permission/middleware.go"
TESTED = "./internal/sharing/"
RUN = "TestRevokeRoute"

ALL_TAGS = {
    "LINK-LIVE-FIRST", "REVOKE-SERVES", "REVOKED-TOKEN-DEAD", "REVOKE-TWICE", "REVOKE-UNKNOWN",
    "REVOKE-SCOPED-TO-PAGE", "REVOKE-NEEDS-ADMIN", "REVOKE-CROSS-TENANT",
}

CONTROLS = [
    dict(
        name="C-BASE",
        why="the unmutated tree. Anything red here means the run itself is broken.",
        edits=[], predict=set(),
    ),
    dict(
        name="C0",
        why="an INERT edit — a comment. A harness that scores this as caught is scoring noise.",
        edits=[(HANDLER, "func (h *Handler) Revoke(w http.ResponseWriter",
                "// control C0: inert\nfunc (h *Handler) Revoke(w http.ResponseWriter")],
        predict=set(),
    ),
    dict(
        name="C1",
        why="THE revoke-that-does-not-revoke LIE: the route reports {\"ok\":true} whatever the store "
            "says. A delete that removed nothing answering success is the failure a status-code "
            "test cannot see, and it is the reason this file is more than coverage.",
        edits=[(HANDLER, "\terr := h.store.Revoke(r.Context(), chi.URLParam(r, \"id\"), chi.URLParam(r, \"pageID\"))\n\tif errors.Is(err, ErrShareLinkNotFound) {",
                "\terr := h.store.Revoke(r.Context(), chi.URLParam(r, \"id\"), chi.URLParam(r, \"pageID\"))\n\terr = nil\n\tif errors.Is(err, ErrShareLinkNotFound) {")],
        predict={"REVOKE-TWICE", "REVOKE-UNKNOWN", "REVOKE-SCOPED-TO-PAGE"},
    ),
    dict(
        name="C2",
        why="THE ce8bfe3 CROSS-PAGE FIX REMOVED at the store: `AND page_id = $2` dropped. The "
            "existing store-level guard also catches this; the question here is whether the ROUTE "
            "can see it, since {id} and {pageID} both come out of the URL.",
        edits=[(STORE, "`DELETE FROM share_links WHERE id = $1 AND page_id = $2`, id, pageID)",
                "`DELETE FROM share_links WHERE id = $1 AND ($2 = $2)`, id, pageID)")],
        predict={"REVOKE-SCOPED-TO-PAGE"},
    ),
    dict(
        name="C3",
        why="THE WRONG URL PARAM — the handler scopes the delete by {spaceID} instead of {pageID}. "
            "This is the defect shape fixed one package over (permission's `delete`, one handler "
            "mounted twice, naming a resource its enforcer had not authorized). A store-level test "
            "is structurally blind to it: the store receives whatever the handler chose.",
        edits=[(HANDLER, "chi.URLParam(r, \"id\"), chi.URLParam(r, \"pageID\"))",
                "chi.URLParam(r, \"id\"), chi.URLParam(r, \"spaceID\"))")],
        predict={"REVOKE-SERVES"},
    ),
    dict(
        name="C4",
        why="THE ADMIN GATE LOWERED to View on the revoke route. Every other test in the file calls "
            "as an admin, so without [REVOKE-NEEDS-ADMIN] this mutation ships green.",
        edits=[(HANDLER, "Delete(\"/spaces/{spaceID}/pages/{pageID}/share/{id}\", h.Revoke)",
                "Delete(\"/spaces/{spaceID}/pages/{pageID}/share/{id}\", h.Revoke) // gate lowered below"),
               (HANDLER, "r.With(h.pageEnf.Require(permission.AccessAdmin)).Delete(\"/spaces/{spaceID}/pages/{pageID}/share/{id}\"",
                "r.With(h.pageEnf.Require(permission.AccessView)).Delete(\"/spaces/{spaceID}/pages/{pageID}/share/{id}\"")],
        predict={"REVOKE-NEEDS-ADMIN"},
    ),
    dict(
        name="C5",
        why="THE NO-ORACLE CONVENTION BROKEN: a resource outside the caller's workspaces answers "
            "403 instead of 404, confirming that the page exists. 404-not-403 is a security "
            "property, not a formatting choice, and only [REVOKE-CROSS-TENANT] states it.",
        # ⚠ THE FIRST CUT OF THIS ANCHOR WAS INDENTED ONE TAB TOO DEEP AND THE HARNESS SAID SO —
        # `ANCHOR MISS ... 0 occurrences`, run aborted, nothing mutated. That is the behaviour the
        # unique-anchor check exists for: a drifted anchor that silently matches nothing turns a
        # control into a second copy of C-BASE, which is how a harness reports 8/8 while testing 7.
        edits=[(MIDDLEWARE, "\t\t\t\twriteNotFound(w)\n\t\t\t\treturn",
                "\t\t\t\twriteForbidden(w)\n\t\t\t\treturn")],
        predict={"REVOKE-CROSS-TENANT"},
    ),
    dict(
        name="C6",
        why="THE VACUITY CONTROL. Two guards assert a token is DEAD after a revoke; a token that "
            "never opened satisfies that perfectly. The [LINK-LIVE-FIRST] floor exists for exactly "
            "that, and this is the only thing that can show the floor is live: the public lane is "
            "made to 404 unconditionally, so 'dead afterwards' becomes true for the wrong reason. "
            "⚠ THIS CONTROL FOUND A MISSING FLOOR AND A MIS-SCOPED TAG. On its first run it also "
            "fired [REVOKE-NEEDS-ADMIN]: that test closes by asserting the link STILL OPENS after a "
            "refused revoke, with nothing saying it opened BEFORE — so the gate assertion went red "
            "for a reason unrelated to the gate. The admin test now carries its own "
            "[LINK-LIVE-FIRST] floor, and this prediction is a single tag again.",
        edits=[(HANDLER, "\tw.Header().Set(\"X-Powered-By\", \"Talyvor Docs\")\n\tpassword := r.URL.Query().Get(\"password\")",
                "\tw.Header().Set(\"X-Powered-By\", \"Talyvor Docs\")\n\tif true {\n\t\twriteErr(w, http.StatusNotFound, \"control C6\")\n\t\treturn\n\t}\n\tpassword := r.URL.Query().Get(\"password\")")],
        predict={"LINK-LIVE-FIRST"},
    ),
    dict(
        name="C7",
        why="MUST-STAY-GREEN COMPANION, and the reason [REVOKE-SERVES] checks the BODY and not only "
            "the status: the route answers 200 with a payload saying the opposite. A caller that "
            "reads `ok` believes the revoke failed while the row is gone.",
        edits=[(HANDLER, "writeJSON(w, http.StatusOK, map[string]bool{\"ok\": true})\n}\n\n// Public is the no-auth viewer.",
                "writeJSON(w, http.StatusOK, map[string]bool{\"ok\": false})\n}\n\n// Public is the no-auth viewer.")],
        predict={"REVOKE-SERVES"},
    ),
]


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def run_tests():
    env = dict(os.environ)
    if not env.get("DOCS_TEST_DATABASE_URL"):
        sys.exit("DOCS_TEST_DATABASE_URL is unset — these are real-Postgres guards")
    p = subprocess.run(["go", "test", "-count=1", "-run", RUN, "-v", TESTED],
                       cwd=REPO, capture_output=True, text=True, env=env, timeout=900)
    out = p.stdout + p.stderr
    if "build failed" in out or "[build failed]" in out or "cannot use" in out or "undefined:" in out:
        return "BUILD-FAILED", set(), set(), out
    return (("PASS" if p.returncode == 0 else "FAIL"),
            set(re.findall(r"\[([A-Z0-9-]+)\]", out)),
            set(re.findall(r"--- FAIL: (\S+)", out)), out)


def apply(edits):
    originals = {}
    for path, _, _ in edits:
        full = os.path.join(REPO, path)
        if full not in originals:
            originals[full] = open(full, encoding="utf-8").read()
    for path, old, new in edits:
        full = os.path.join(REPO, path)
        s = open(full, encoding="utf-8").read()
        n = s.count(old)
        if n != 1:
            for f2, o in originals.items():
                open(f2, "w", encoding="utf-8").write(o)
            raise SystemExit(f"ANCHOR MISS in {path}: {n} occurrences of {old!r} (want exactly 1)")
        open(full, "w", encoding="utf-8").write(s.replace(old, new))
    return originals


def main():
    for c in CONTROLS:
        bad = c["predict"] - ALL_TAGS
        if bad:
            sys.exit(f"{c['name']} predicts tags no guard can print: {sorted(bad)}")

    watched = (HANDLER, STORE, MIDDLEWARE)
    baseline = {os.path.join(REPO, p): sha(os.path.join(REPO, p)) for p in watched}
    results = []
    for c in CONTROLS:
        print(f"\n=== {c['name']} — {c['why']}")
        print(f"    PREDICT: {sorted(c['predict']) or 'NOT CAUGHT (all green)'}")
        originals = {}
        try:
            originals = apply(c["edits"])
            status, tags, failing, _ = run_tests()
        finally:
            for full, o in originals.items():
                open(full, "w", encoding="utf-8").write(o)
            for full, want in baseline.items():
                if sha(full) != want:
                    sys.exit(f"RESTORE FAILED for {full}")

        if status == "BUILD-FAILED":
            print("    RESULT : BUILD-FAILED — not a caught mutation; re-cut this control")
            results.append((c["name"], False, status))
            continue
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
