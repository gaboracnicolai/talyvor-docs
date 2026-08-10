#!/usr/bin/env python3
"""Positive-control harness for the changelog by-id page-scope guard (W3.1).

WHAT IT SCORES, AND WHY IT IS NOT PASS/FAIL. #69 recorded that a PASS/FAIL verdict scored 7/7
while two of four assertions were justified by nothing: one mutation fired two assertions at
once, so either could have been deleted with every control still reading "as predicted". The
verdict here is therefore THE SET OF ASSERTION TAGS THAT FIRED, plus THE SET OF OTHER TESTS
THAT REDDENED (#79's column — a control a pre-existing guard also catches justifies nothing on
its own). At the end the harness FAILS if any declared assertion was claimed by no control.

TAG SCRAPING IS ANCHORED TO THE `file.go:NN: ` PREFIX. #81's harness used a bare `\\[(M-[A-Z-]+)\\]`
over `go test` output and reported a tag that never fired, because one assertion's MESSAGE named
another's tag. An instrument that reads its subject's output cannot tell a tag that fired from a
tag that was quoted.

C1 IS NOT A MUTATION. It restores the four files to origin/main — the defect exactly as it
shipped — with only the new guard added, which is the only form in which "nothing else in the
repository notices" is a claim about the product rather than about expectations this change
itself introduced.

Every control asserts its anchor count BEFORE any write, writes each file ONCE (a control that
applies half of itself erases its own first edit), verifies the bytes moved on disk, and restores
in a finally — a crash between mutate and restore must not leave a mutated tree. The closing
sha256 check is a check, not the restore mechanism.
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DSN = os.environ.get("DOCS_TEST_DATABASE_URL")
if not DSN:
    sys.exit("DOCS_TEST_DATABASE_URL must be set — these controls need the real-Postgres suite")

STORE = "internal/changelog/store.go"
HANDLER = "internal/changelog/handler.go"
DELTEST = "internal/changelog/delete_removes_the_row_realpg_test.go"
STORETEST = "internal/changelog/store_test.go"
GUARD = "TestChangelogByIDRoutes_MustBelongToTheAuthorizedPage_RealPG"

# Every assertion tag the guard declares. X-PREMISE is an instrument check; it is listed because
# C6 does in fact claim it, and if it ever stops being claimed the harness must say so.
DECLARED = [
    "X-PREMISE", "X-HONEST", "X-LEAK-GET", "X-LIST",
    "X-OWN-PATCH", "X-LEAK-PATCH", "X-OWN-PUBLISH", "X-LEAK-PUBLISH",
    "X-LEAK-DELETE", "X-OWN-DELETE", "X-GRANT",
]

# ── the four scoped statements, as anchors ────────────────────────────────────
GET_SQL = "`SELECT `+cols+` FROM changelog_entries WHERE id = $1 AND page_id = $2 AND workspace_id = ANY($3)`"
UPD_SQL = "WHERE id = $%d AND page_id = $%d AND workspace_id = ANY($%d) RETURNING %s"
PUB_SQL = "WHERE id = $1 AND page_id = $2 AND workspace_id = ANY($3) RETURNING `+cols"
DEL_SQL = "`DELETE FROM changelog_entries WHERE id = $1 AND page_id = $2 AND workspace_id = ANY($3)`"


def inert(sql):
    """Make the page predicate inert while keeping the parameter bound and the arity unchanged."""
    return sql.replace("page_id = $2", "(page_id = $2 OR TRUE)").replace(
        "page_id = $%d", "(page_id = $%d OR TRUE)")


def never(sql):
    """Make the page predicate match nothing — the over-correction direction."""
    return sql.replace("page_id = $2", "page_id = $2 || 'x'").replace(
        "page_id = $%d", "page_id = $%d || 'x'")


CONTROLS = [
    dict(
        id="C1", kind="restore-main",
        what="the defect exactly as it shipped: store.go/handler.go and the two call-site test "
             "files restored to origin/main, only the new guard added",
        expect_tags={"X-LEAK-GET", "X-LEAK-PATCH", "X-LEAK-PUBLISH", "X-LEAK-DELETE"},
        expect_others="NONE — no pre-existing test in the repository sees any of the four",
    ),
    dict(
        id="C2", kind="edit", edits=[(STORE, GET_SQL, inert(GET_SQL))],
        what="GetEntry's page scope alone made inert",
        expect_tags={"X-LEAK-GET"},
        expect_others="none",
    ),
    dict(
        id="C3", kind="edit", edits=[(STORE, UPD_SQL, inert(UPD_SQL))],
        what="UpdateEntry's page scope alone made inert",
        expect_tags={"X-LEAK-PATCH"},
        expect_others="none",
    ),
    dict(
        id="C4", kind="edit", edits=[(STORE, PUB_SQL, inert(PUB_SQL))],
        what="PublishEntry's page scope alone made inert",
        expect_tags={"X-LEAK-PUBLISH"},
        expect_others="TestPublishEntry_SetsTimestamp — its mock regex pins the statement text",
    ),
    dict(
        id="C5", kind="edit", edits=[(STORE, DEL_SQL, inert(DEL_SQL))],
        what="DeleteEntry's page scope alone made inert",
        expect_tags={"X-LEAK-DELETE"},
        expect_others="TestDeleteEntry_DeletesByID — its mock regex pins the statement text",
    ),
    dict(
        id="C6", kind="edit",
        edits=[(STORE, GET_SQL, never(GET_SQL)), (STORE, UPD_SQL, never(UPD_SQL)),
               (STORE, PUB_SQL, never(PUB_SQL)), (STORE, DEL_SQL, never(DEL_SQL))],
        what="OVER-CORRECTION: the page predicate matches nothing, so the honest pair is refused too",
        # X-PREMISE is a t.Fatalf, so the read subtest aborts and X-HONEST/X-LEAK-GET/X-LIST are
        # never evaluated. Stated here rather than discovered: a Fatalf that fires is an assertion
        # firing, and the assertions it shadows are simply not part of this control's verdict.
        expect_tags={"X-PREMISE", "X-OWN-PATCH", "X-OWN-PUBLISH", "X-OWN-DELETE", "X-GRANT"},
        expect_others="the two mock tests (statement text moved)",
    ),
    dict(
        id="C7", kind="edit",
        edits=[(STORE, DEL_SQL, DEL_SQL.replace("workspace_id = ANY($3)", "(workspace_id = ANY($3) OR TRUE)"))],
        what="the WORKSPACE scope removed from DeleteEntry while the PAGE scope stays — page scope "
             "must NOT be allowed to read as workspace scope",
        expect_tags=set(),
        expect_others="TestDeleteEntry_RefusesAnotherWorkspacesEntryAndLeavesIt_RealPG (and the "
                      "mock test) — MY GUARD MUST STAY GREEN: it is blind to the cross-tenant ring "
                      "by construction, which is what makes the two guards two guards",
    ),
    dict(
        id="C8", kind="edit",
        edits=[(HANDLER,
                'r.With(h.pageEnf.Require(permission.AccessView)).Get("/spaces/{spaceID}/pages/{pageID}/changelog/entries/{id}", h.Get)',
                'r.Get("/spaces/{spaceID}/pages/{pageID}/changelog/entries/{id}", h.Get)')],
        what="the page ENFORCER removed from the GET route — the gate, not the scope",
        expect_tags={"X-HONEST"},
        expect_others="none",
    ),
    dict(
        id="C9", kind="edit",
        edits=[(STORE,
                "WHERE page_id = $1 AND workspace_id = ANY($2)",
                "WHERE (page_id = $1 OR TRUE) AND workspace_id = ANY($2)")],
        what="ListEntries' page scope made inert — the page-scoped sibling that is the guard's "
             "own positive control",
        expect_tags={"X-LIST"},
        expect_others="TestListEntries_OrdersByPublishedDescThenCreated — its mock regex pins the text",
    ),
    dict(
        id="C10", kind="edit",
        edits=[(STORE, GET_SQL,
                GET_SQL[:-1] + " AND NOT (SELECT sp.private FROM pages p JOIN spaces sp ON sp.id = p.space_id WHERE p.id = $2)`")],
        what="THE WRONG FIX: refuse whenever the page is in a private space, instead of tying the "
             "id to the authorized page",
        expect_tags={"X-GRANT"},
        expect_others="none",
    ),
    # C12–C15 EXIST BECAUSE OF THE FIRST RUN, NOT BEFORE IT. With C1–C11 alone, X-OWN-PATCH,
    # X-OWN-PUBLISH and X-OWN-DELETE were claimed ONLY by C6, which fires all three at once —
    # so any one of them could have been deleted, or made constant-true, with every control
    # still reading AS PREDICTED. #69's lesson, one level down: the harness printing "claimed
    # by C6, C6, C6" is what made it visible. One mutation per assertion.
    dict(
        id="C12", kind="edit", edits=[(STORE, UPD_SQL, never(UPD_SQL))],
        what="the over-correction on UpdateEntry ALONE — the mutation only X-OWN-PATCH sees",
        expect_tags={"X-OWN-PATCH"},
        expect_others="none",
    ),
    dict(
        id="C13", kind="edit", edits=[(STORE, PUB_SQL, never(PUB_SQL))],
        what="the over-correction on PublishEntry ALONE — the mutation only X-OWN-PUBLISH sees",
        expect_tags={"X-OWN-PUBLISH"},
        expect_others="TestPublishEntry_SetsTimestamp (statement text moved)",
    ),
    dict(
        id="C14", kind="edit", edits=[(STORE, DEL_SQL, never(DEL_SQL))],
        what="the over-correction on DeleteEntry ALONE — the mutation only X-OWN-DELETE sees",
        expect_tags={"X-OWN-DELETE"},
        expect_others="the three delete tests (statement text moved / the row survives)",
    ),
    dict(
        id="C15", kind="edit", edits=[(STORE, GET_SQL, never(GET_SQL))],
        what="the over-correction on GetEntry ALONE. ⚠ X-PREMISE IS NOT ISOLABLE BY KIND, and "
             "that is recorded rather than worked around: anything that stops bob reading his "
             "own page's entry also stops the GRANTED read at the private address, so no product "
             "mutation can fire X-PREMISE without firing X-GRANT. X-GRANT is isolated by C10.",
        expect_tags={"X-PREMISE", "X-GRANT"},
        expect_others="none",
    ),
    dict(
        id="C11", kind="edit",
        edits=[(STORE, "AND published_at IS NOT NULL", "AND published_at IS NULL")],
        what="SCOPE CONTROL: an unrelated one-line edit in the same file (the public feed's "
             "published predicate)",
        expect_tags=set(),
        expect_others="TestGetPublicFeed_OnlyReturnsPublished — proves the run is live and the "
                      "guard is not merely insensitive to everything",
    ),
]

TAG_RE = re.compile(r"[\w./-]+\.go:\d+: \[(X-[A-Z0-9-]+)\]")
FAIL_RE = re.compile(r"^\s*--- FAIL: (\S+)")


def sha(path):
    with open(os.path.join(REPO, path), "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def read(path):
    with open(os.path.join(REPO, path), "r") as f:
        return f.read()


def write(path, text):
    with open(os.path.join(REPO, path), "w") as f:
        f.write(text)


def run_suite():
    p = subprocess.run(
        ["go", "test", "-timeout", "300s", "-count=1", "-v", "./..."],
        cwd=REPO, capture_output=True, text=True,
        env={**os.environ, "DOCS_TEST_DATABASE_URL": DSN},
    )
    out = p.stdout + p.stderr
    if "[build failed]" in out or "build constraints" in out:
        return None, None, out
    tags = set(TAG_RE.findall(out))
    fails = set()
    for line in out.splitlines():
        m = FAIL_RE.match(line)
        if m:
            fails.add(m.group(1))
    if "panic:" in out:
        fails.add("<PANIC>")
    return tags, fails, out


PRISTINE = {p: (read(p), sha(p)) for p in (STORE, HANDLER, DELTEST, STORETEST)}


def restore():
    for p, (text, _) in PRISTINE.items():
        write(p, text)


def apply_control(c):
    """Assert every anchor, then write each file exactly once."""
    if c["kind"] == "restore-main":
        for p in (STORE, HANDLER, DELTEST, STORETEST):
            blob = subprocess.run(["git", "show", f"origin/main:{p}"], cwd=REPO,
                                  capture_output=True, text=True, check=True).stdout
            if blob == PRISTINE[p][0]:
                sys.exit(f"{c['id']}: {p} is already identical to origin/main — the control "
                         f"would edit no bytes and prove nothing")
            write(p, blob)
        return
    per_file = {}
    for path, old, new in c["edits"]:
        text = per_file.get(path, PRISTINE[path][0])
        n = text.count(old)
        if n != 1:
            sys.exit(f"{c['id']}: anchor count {n} (want 1) in {path} for:\n  {old[:110]}")
        per_file[path] = text.replace(old, new)
    for path, text in per_file.items():
        write(path, text)


def main():
    print(f"pristine: " + ", ".join(f"{p.split('/')[-1]}={s[:12]}" for p, (_, s) in PRISTINE.items()))
    results = []
    claimed = {}
    try:
        for c in CONTROLS:
            apply_control(c)
            for p in ({e[0] for e in c.get("edits", [])} or {STORE, HANDLER, DELTEST, STORETEST}):
                if sha(p) == PRISTINE[p][1]:
                    sys.exit(f"{c['id']}: {p} is byte-identical after the edit — nothing was applied")
            tags, fails, out = run_suite()
            restore()
            if tags is None:
                results.append((c, "BUILD FAILED — not a catch", set(), set()))
                print(f"{c['id']}  BUILD FAILED — not a catch")
                continue
            others = {f for f in fails if not f.startswith(GUARD)}
            verdict = "AS PREDICTED" if tags == c["expect_tags"] else "DIVERGED"
            results.append((c, verdict, tags, others))
            for t in tags:
                claimed.setdefault(t, []).append(c["id"])
            print(f"{c['id']}  {verdict}")
            print(f"      what     : {c['what']}")
            print(f"      predicted: {sorted(c['expect_tags']) or '(no tag of mine)'}")
            print(f"      fired    : {sorted(tags) or '(none)'}")
            print(f"      others   : {sorted(others) or '(none)'}   [predicted: {c['expect_others']}]")
    finally:
        restore()

    bad = [p for p, (_, s) in PRISTINE.items() if sha(p) != s]
    print("\ntree restored to pristine sha256: " + ("YES" if not bad else f"NO — {bad}"))

    print("\nassertion → controls that claim it")
    unclaimed = []
    for tag in DECLARED:
        who = claimed.get(tag, [])
        print(f"  {tag:<16} {', '.join(who) if who else 'CLAIMED BY NOTHING'}")
        if not who:
            unclaimed.append(tag)

    diverged = [c["id"] for c, v, _, _ in results if v != "AS PREDICTED"]
    print(f"\n{len(results) - len(diverged)}/{len(results)} controls as predicted"
          f"{'' if not diverged else '  DIVERGED: ' + ', '.join(diverged)}")
    if unclaimed:
        print(f"UNCLAIMED ASSERTIONS: {unclaimed} — delete them or earn them")
    sys.exit(1 if (bad or diverged or unclaimed) else 0)


if __name__ == "__main__":
    main()
