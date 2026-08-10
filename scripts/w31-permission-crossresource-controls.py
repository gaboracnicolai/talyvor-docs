#!/usr/bin/env python3
"""Positive-control harness for the permission by-id resource-scope guard (W3.1, finding (18a)).

Same shape as scripts/w31-changelog-crosspage-controls.py, and the same reasons: the verdict is
THE SET OF ASSERTION TAGS THAT FIRED plus THE SET OF OTHER TESTS THAT REDDENED, tags scraped
anchored to the `file.go:NN: ` prefix, anchors counted before any write, each file written once,
bytes verified on disk, restore in a `finally`, closing sha256 check.

C1 restores the three files to origin/main — the defect exactly as it shipped — because that is
the only form in which "nothing else in the repository notices" is a claim about the product
rather than about the mock expectations this change itself moved.

C3–C6 exist because C1 and a joint over-correction each fire two or three assertions at once.
Every declared assertion has at least one control that fires it ALONE.
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

STORE = "internal/permission/store.go"
HANDLER = "internal/permission/handler.go"
STORETEST = "internal/permission/store_test.go"
GUARD = "TestPermissionRevoke_MustNameTheResourceTheRouteAuthorized_RealPG"

DECLARED = [
    "P-PREMISE", "P-HONEST", "P-HONEST-DELETE",
    "P-LEAK-SPACE", "P-LEAK-PAGE", "P-TYPE",
    "P-OWN-SPACE", "P-OWN-PAGE",
]

TYPE_PRED = "resource_type = $2"
ID_PRED = "resource_id = $3"
WS_PRED = "workspace_id = ANY($4)"
LIST_ROUTE = 'r.With(h.spaceEnf.Require(AccessAdmin)).Method(http.MethodGet, "/spaces/{spaceID}/permissions", http.HandlerFunc(h.listSpace))'
DEL_ROUTE = 'r.With(h.spaceEnf.Require(AccessAdmin)).Method(http.MethodDelete, "/spaces/{spaceID}/permissions/{permID}", http.HandlerFunc(h.deleteSpace))'

CONTROLS = [
    dict(id="C1", kind="restore-main",
         what="the defect exactly as it shipped: store.go/handler.go and the mock test restored to "
              "origin/main, only the new guard added",
         expect_tags={"P-LEAK-SPACE", "P-LEAK-PAGE", "P-TYPE"},
         expect_others="NONE — no pre-existing test in the repository sees any of the three, "
                       "including internal/database/sec4_l2_test.go (b), which drives THIS route"),
    dict(id="C2", kind="edit",
         edits=[(STORE, TYPE_PRED, "($2 <> '' OR TRUE)")],
         what="the resource TYPE dropped from the scope, resource_id kept. ⚠ THIS CONTROL FIRED "
              "NOTHING ON ITS FIRST RUN and the miss was a property of the FIXTURE, not of the "
              "product: P-TYPE's subject was an ordinary page grant, whose resource_id can never "
              "equal a space id, so the id half discriminated alone and the type half was "
              "unfalsifiable. The fixture now manufactures the collision the schema permits "
              "(UNIQUE (resource_type, resource_id, …), no FK on resource_id).",
         expect_tags={"P-TYPE"},
         expect_others="the two RevokeByID mock tests — their regex pins the statement text"),
    dict(id="C3", kind="edit",
         edits=[(STORE, ID_PRED, "(resource_id = $3 OR $2 = 'space')")],
         what="the id scope made inert FOR THE SPACE ROUTE ONLY — the mutation only P-LEAK-SPACE sees",
         expect_tags={"P-LEAK-SPACE"},
         expect_others="the two RevokeByID mock tests — their regex pins the statement text"),
    dict(id="C4", kind="edit",
         edits=[(STORE, ID_PRED, "(resource_id = $3 OR $2 = 'page')")],
         what="the id scope made inert FOR THE PAGE ROUTE ONLY — the mutation only P-LEAK-PAGE sees",
         expect_tags={"P-LEAK-PAGE"},
         expect_others="the two RevokeByID mock tests — their regex pins the statement text"),
    dict(id="C5", kind="edit",
         edits=[(STORE, ID_PRED, "(resource_id = $3 AND $2 <> 'space')")],
         what="OVER-CORRECTION on the space route only — the mutation only P-OWN-SPACE sees",
         expect_tags={"P-OWN-SPACE"},
         expect_others="the two mock tests + TestSEC4_L2_SecondaryCrossTenant, whose own positive "
                       "control is Bob revoking his own grant through this route"),
    dict(id="C6", kind="edit",
         edits=[(STORE, ID_PRED, "(resource_id = $3 AND $2 <> 'page')")],
         what="OVER-CORRECTION on the page route only — the mutation only P-OWN-PAGE sees",
         expect_tags={"P-OWN-PAGE"},
         expect_others="the two RevokeByID mock tests — their regex pins the statement text"),
    dict(id="C7", kind="edit",
         edits=[(STORE, WS_PRED, "(workspace_id = ANY($4) OR TRUE)")],
         what="the WORKSPACE scope removed while type+id stay — resource scope must NOT be allowed "
              "to read as workspace scope",
         expect_tags=set(),
         expect_others="TestRevokeByID_ForeignWorkspace (+ the sibling mock test, whose regex pins "
                       "the text). ⚠ internal/database/sec4_l2_test.go (b) does NOT red: its "
                       "cross-tenant DELETE names spaceB, and the resource scope this change adds "
                       "refuses it on resource_id before the workspace predicate is reached — "
                       "MY GUARD MUST STAY GREEN: it is blind to the cross-tenant ring by "
                       "construction, which is exactly how that ring's guard was blind to this one"),
    dict(id="C8", kind="edit",
         edits=[(HANDLER, DEL_ROUTE,
                 'r.Method(http.MethodDelete, "/spaces/{spaceID}/permissions/{permID}", http.HandlerFunc(h.deleteSpace))')],
         what="the Admin ENFORCER removed from the space DELETE route — the gate, not the scope",
         expect_tags={"P-HONEST-DELETE"}, expect_others="none"),
    dict(id="C9", kind="edit",
         edits=[(HANDLER, LIST_ROUTE,
                 'r.Method(http.MethodGet, "/spaces/{spaceID}/permissions", http.HandlerFunc(h.listSpace))')],
         what="the Admin enforcer removed from the space LIST route — the read half of the same gate",
         expect_tags={"P-HONEST"}, expect_others="none"),
    dict(id="C10", kind="edit",
         edits=[(HANDLER, LIST_ROUTE, LIST_ROUTE.replace("h.spaceEnf", "h.pageEnf"))],
         what="the LIST route gated by the PAGE enforcer, whose {pageID} resolver finds nothing on "
              "that address — the mutation only P-PREMISE sees (and it is a t.Fatalf, so it "
              "shadows the rest by design)",
         expect_tags={"P-PREMISE"}, expect_others="none"),
    dict(id="C11", kind="edit",
         edits=[(STORE, "string(resourceType), resourceID, subjectType, subjectID,",
                 "string(resourceType), resourceID, subjectID, subjectType,")],
         what="SCOPE CONTROL: an unrelated argument swap in Revoke (the by-subject sibling) in the "
              "same file",
         expect_tags=set(),
         expect_others="TestRevoke_DeletesByResourceAndSubject — proves the run is live"),
]

TAG_RE = re.compile(r"[\w./-]+\.go:\d+: \[(P-[A-Z0-9-]+)\]")
FAIL_RE = re.compile(r"^\s*--- FAIL: (\S+)")


def sha(p):
    with open(os.path.join(REPO, p), "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def read(p):
    with open(os.path.join(REPO, p)) as f:
        return f.read()


def write(p, t):
    with open(os.path.join(REPO, p), "w") as f:
        f.write(t)


PRISTINE = {p: (read(p), sha(p)) for p in (STORE, HANDLER, STORETEST)}


def restore():
    for p, (t, _) in PRISTINE.items():
        write(p, t)


def run_suite():
    r = subprocess.run(["go", "test", "-timeout", "300s", "-count=1", "-v", "./..."],
                       cwd=REPO, capture_output=True, text=True,
                       env={**os.environ, "DOCS_TEST_DATABASE_URL": DSN})
    out = r.stdout + r.stderr
    if "[build failed]" in out:
        return None, None
    tags = set(TAG_RE.findall(out))
    fails = {m.group(1) for m in map(FAIL_RE.match, out.splitlines()) if m}
    if "panic:" in out:
        fails.add("<PANIC>")
    return tags, fails


def apply_control(c):
    if c["kind"] == "restore-main":
        for p in PRISTINE:
            blob = subprocess.run(["git", "show", f"origin/main:{p}"], cwd=REPO,
                                  capture_output=True, text=True, check=True).stdout
            if blob == PRISTINE[p][0]:
                sys.exit(f"{c['id']}: {p} already equals origin/main — the control edits no bytes")
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
    results, claimed = [], {}
    try:
        for c in CONTROLS:
            apply_control(c)
            for p in ({e[0] for e in c.get("edits", [])} or set(PRISTINE)):
                if sha(p) == PRISTINE[p][1]:
                    sys.exit(f"{c['id']}: {p} byte-identical after the edit — nothing applied")
            tags, fails = run_suite()
            restore()
            if tags is None:
                print(f"{c['id']}  BUILD FAILED — not a catch")
                results.append((c, "BUILD FAILED"))
                continue
            others = {f for f in fails if not f.startswith(GUARD)}
            verdict = "AS PREDICTED" if tags == c["expect_tags"] else "DIVERGED"
            results.append((c, verdict))
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
        print(f"  {tag:<18} {', '.join(who) if who else 'CLAIMED BY NOTHING'}")
        if not who:
            unclaimed.append(tag)
    div = [c["id"] for c, v in results if v != "AS PREDICTED"]
    print(f"\n{len(results) - len(div)}/{len(results)} controls as predicted"
          f"{'' if not div else '  DIVERGED: ' + ', '.join(div)}")
    if unclaimed:
        print(f"UNCLAIMED ASSERTIONS: {unclaimed} — delete them or earn them")
    sys.exit(1 if (bad or div or unclaimed) else 0)


if __name__ == "__main__":
    main()
