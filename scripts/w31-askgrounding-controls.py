#!/usr/bin/env python3
"""Positive controls for /ai/ask's private-space grounding gate.

WHAT THIS SCORES, AND WHY IT IS NOT PASS/FAIL.

`c70aa6e` (#69) recorded a PASS/FAIL harness scoring 7/7 while two of four assertions were
justified by nothing. Knowing WHICH TEST reddened is not enough — one mutation firing two
assertions at once leaves either deletable with every control still reading as predicted. So
this harness:

  * scores THE SET OF ASSERTION TAGS THAT FIRED ([B-LEAK-PROMPT] and friends), read out of
    `go test -v` output rather than off an exit code;
  * compares that set to a prediction written per control BEFORE the run;
  * ALSO records every OTHER test in the package that reddened, so a control caught by a
    pre-existing guard cannot be read as justifying mine (`a control two guards catch
    justifies neither`);
  * FAILS if a PRODUCT assertion is claimed by no control;
  * treats a BUILD FAILURE as its own verdict — a compile error is not a catch;
  * carries a NO-OP control that must stay GREEN, so "everything reds" cannot pass for a
    working control set;
  * restores every mutated file in a `finally` and sha256-compares, so a crash between mutate
    and restore cannot leave a mutation on disk.

⚠ ONE ASSERTION IS AN INSTRUMENT CHECK AND IS LISTED SEPARATELY RATHER THAN EXEMPTED QUIETLY.
[B-PREMISE] fires when Lens was never sent the question at all. No mutation of the PRODUCT can
reach it: the response carries the model's answer, so a 200 means the engine ran, and any
mutation that stops it running fails the status check first. It is there so that a broken
harness reports itself instead of letting the two absence-shaped LEAK assertions pass over an
empty prompt. It is printed as unclaimed-by-construction, with that reason, and does not count
against the verdict — the split is by KIND (a claim about the instrument vs a claim about the
product), not a hand-kept exemption list.

Run:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-askgrounding-controls.py
"""

import hashlib
import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HANDLER = os.path.join(REPO, "internal/ai/handler.go")
MAIN = os.path.join(REPO, "cmd/docs/main.go")
WIRETEST = os.path.join(REPO, "internal/ai/mainwiring_test.go")
PERMSTORE = os.path.join(REPO, "internal/permission/store.go")

# Claims about the PRODUCT. Each must be claimed by some control.
DECLARED = [
    "B-LEAK-PROMPT", "B-LEAK-SOURCE",
    "B-SLOT", "B-SLOTSRC",
    "B-OWNER", "B-GRANT",
    "B-NILGATE", "B-NILSTORE", "B-SIBLING",
    "B-WIRED", "B-SENTINEL", "B-STRIP",
]

# Claims about the INSTRUMENT. See the module docstring.
INSTRUMENT = ["B-PREMISE"]

# (id, file, old, new, predicted-tags, why)
CONTROLS = [
    (
        "C0-noop",
        HANDLER,
        "// visibleTo drops every page the caller may not VIEW",
        "// visibleTo drops every page the caller may not VIEW (C0: a comment-only edit)",
        [],
        "MUST STAY GREEN. A comment-only edit. Without it, a harness in which everything reds — "
        "because the tree stopped building or the fixture broke — is indistinguishable from a "
        "working control set.",
    ),
    (
        "C1-no-filter",
        HANDLER,
        "\tpages = h.visibleTo(r.Context(), pages)",
        "\t_ = h.visibleTo(r.Context(), pages)",
        ["B-LEAK-PROMPT", "B-LEAK-SOURCE", "B-SLOT", "B-SLOTSRC"],
        "THE DEFECT ITSELF. The gate is wired and called and its answer is thrown away, which is "
        "exactly the state the product shipped in (there was no call at all). All four fire "
        "together because the three private pages are the top three: they are both quoted AND "
        "occupying every grounding slot.",
    ),
    (
        "C2-canview-ignored",
        HANDLER,
        "\t\tfound, canView := h.access.AuthorizePageRead(ctx, p.ID)\n\t\tif !found || !canView {",
        "\t\tfound, _ := h.access.AuthorizePageRead(ctx, p.ID)\n\t\tif !found {",
        ["B-LEAK-PROMPT", "B-LEAK-SOURCE", "B-SLOT", "B-SLOTSRC"],
        "The filter runs and consults only EXISTENCE. Every page in the workspace is `found`, so "
        "this is the shape where a gate is present, is called, and decides nothing — the one a "
        "reader skimming for `AuthorizePageRead` would call handled.",
    ),
    (
        "C3-drop-everything",
        HANDLER,
        "\t\tout = append(out, p)\n\t}\n\treturn out\n}",
        "\t\t_ = p\n\t}\n\treturn out\n}",
        ["B-SLOT", "B-SLOTSRC", "B-OWNER", "B-GRANT"],
        "THE OVER-CORRECTION, and the reason the leak assertions cannot be trusted alone: an "
        "endpoint grounded in NOTHING satisfies every absence check perfectly. It must be the "
        "positive-shaped assertions that speak here and the LEAK tags that stay silent — if a "
        "LEAK tag ever fired on this control it would mean it was reading something other than "
        "the private bodies.",
    ),
    (
        "C4-no-overfetch",
        HANDLER,
        "\taskFetchFactor  = 4",
        "\taskFetchFactor  = 1",
        ["B-SLOT", "B-SLOTSRC"],
        "Removes the over-fetch ALONE and nothing else. The three private pages sort above the "
        "public one and Ask takes three, so a denied caller is left grounded in nothing while a "
        "document he may read waited directly behind. This is the only control that isolates the "
        "window size, and it is why the two SLOT assertions are not decoration on C1.",
    ),
    (
        "C5-refusal-deleted",
        HANDLER,
        # ⚠ THE ANCHOR WAS `if h.access == nil {` + `slog.Error(` AND NOTHING ELSE, WHICH WAS
        # UNIQUE WHEN THIS CONTROL WAS WRITTEN AND HAS NOT BEEN SINCE THE NEXT DAY.
        # 24adb1d (2026-08-10) created this control against the ONE such site — /ask's refusal.
        # d1fc206 (2026-08-11) added a second, the page_id-carrying request's refusal, and from
        # that commit this control printed "ANCHOR MISSING/AMBIGUOUS (2 matches) — control is
        # dead" on every run. Nothing runs it, so nobody read it for 18 days, and the harness's
        # own closing line said the rest: B-NILGATE was left CLAIMED BY NO CONTROL.
        #
        # ⚠⚠ THE REPAIR IS TO DISAMBIGUATE, NOT TO ACCEPT TWO MATCHES. Mutating both sites at
        # once would silently widen this control from "the /ask refusal" to "every nil-gate
        # refusal in the file", and B-NILGATE would then be justified by a mutation that also
        # breaks a sibling seam — the shape W3.64 refused for the same reason in ci.yaml.
        # The /ask message is what makes the site identifiable, so the anchor carries it.
        "\tif h.access == nil {\n\t\tslog.Error(\"ai: /ask has no page-read gate wired",
        "\tif false {\n\t\tslog.Error(\"ai: /ask has no page-read gate wired",
        ["B-NILGATE", "B-NILSTORE"],
        "The unwired-deployment path. With the refusal gone /ask runs the corpus search and "
        "answers from the empty set visibleTo fails closed to — a confident reply from zero "
        "grounding, which is #57's failure returning by a different door. Both nil tags fire "
        "together; C6 is what claims B-NILSTORE on its own.",
    ),
    (
        "C6-refusal-after-search",
        HANDLER,
        "\tif h.access == nil {\n\t\tslog.Error(\"ai: /ask has no page-read gate wired — refusing rather than answering \" +\n"
        "\t\t\t\"from an unfiltered or empty corpus (cmd/docs/main.go must call aiHandler.WithPageRead)\")\n"
        "\t\twriteJSON(w, http.StatusInternalServerError, map[string]string{\"error\": \"ask unavailable\"})\n"
        "\t\treturn\n\t}",
        "\tif h.access == nil {\n\t\tslog.Error(\"ai: /ask has no page-read gate wired\")\n"
        "\t\t_, _ = h.pages.Search(r.Context(), wsID, in.Question, askFetchRows)\n"
        "\t\twriteJSON(w, http.StatusInternalServerError, map[string]string{\"error\": \"ask unavailable\"})\n"
        "\t\treturn\n\t}",
        ["B-NILSTORE"],
        "The refusal still refuses, but AFTER reading the corpus. The caller sees the same 500, so "
        "B-NILGATE must stay silent — that is what makes B-NILSTORE a claim of its own rather than "
        "a second sentence about the status code.",
    ),
    (
        "C7-wiring-deleted",
        MAIN,
        "\taiHandler.WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))\n",
        "",
        ["B-WIRED"],
        "The gate stops being switched on in production. THE REAL-PG GUARD MUST STAY GREEN — it "
        "builds its own handler and is structurally blind to main.go, which is the entire reason "
        "the tripwire exists as a second, deliberately blind guard.",
    ),
    (
        "C8-strip-removed",
        WIRETEST,
        "\t\tif strings.HasPrefix(strings.TrimSpace(line), \"//\") {\n\t\t\tcontinue\n\t\t}",
        "\t\tif false {\n\t\t\tcontinue\n\t\t}",
        ["B-STRIP"],
        "A GUARD MUTATION, NOT A PRODUCT ONE. Without comment stripping the tripwire is satisfied "
        "by main.go merely MENTIONING the call in prose — the way it would go quietly vacuous. "
        "B-WIRED must stay silent: the call is still there.",
    ),
    (
        "C9-sentinel-removed",
        MAIN,
        "AND THE SEAM'S THIRD COPY, WHICH ANSWERS IN PROSE.",
        "AND THE SEAM'S THIRD COPY.",
        ["B-SENTINEL"],
        "A COMMENT-ONLY EDIT THAT MUST RED, which is the opposite of C0 and the point of both. The "
        "vacuity check in C8 is only meaningful if its sentinel is actually in the file; deleting "
        "the phrase makes its absence prove nothing, and B-SENTINEL is the assertion that says so. "
        "Without this control the vacuity check could itself be vacuous and nothing would notice.",
    ),
    (
        "C10-write-routed-to-ask",
        HANDLER,
        "\tr.Post(\"/workspaces/{wsID}/ai/write\", h.limited(h.Write))",
        "\tr.Post(\"/workspaces/{wsID}/ai/write\", h.limited(h.Ask))",
        ["B-SIBLING"],
        "MANUFACTURED, AND SAID SO. B-SIBLING guards against a future fix that gates the WHOLE "
        "handler, and no one-line edit produces that shape naturally, so the control puts a "
        "corpus-reading route behind a sibling's address instead. ⚠ IT IS ALSO CAUGHT BY A "
        "PRE-EXISTING TEST (TestWriteEndpoint_ReturnsGeneratedText), which the harness prints — "
        "so B-SIBLING is claimed JOINTLY here, not solely.",
    ),
    (
        "C11-no-owner-is-admin",
        PERMSTORE,
        "\tif memberID != \"\" && memberID == res.CreatedBy {\n\t\treturn AccessAdmin\n\t}",
        "\tif false {\n\t\treturn AccessAdmin\n\t}",
        ["B-OWNER"],
        "resolveAccess's first arm is what makes a space's CREATOR an admin with no grant row. "
        "Without it alice falls to her own private space's AccessNone. bob's explicit grant is "
        "untouched, so B-GRANT must stay green — that is what makes this control name B-OWNER "
        "rather than 'the permission engine broke'.",
    ),
    (
        "C12-member-grant-inert",
        PERMSTORE,
        "\t\tcase \"member\":\n\t\t\tif p.SubjectID != memberID {\n\t\t\t\treturn\n\t\t\t}",
        "\t\tcase \"member\":\n\t\t\treturn",
        ["B-GRANT"],
        "The mirror of C11: an explicit per-member grant confers nothing, while owner-is-admin and "
        "the public-space default are untouched. B-OWNER and the SLOT tags must stay green, which "
        "is what separates B-GRANT from B-OWNER instead of leaving both claimed only by C3.",
    ),
]

PKG = "./internal/ai/"
TAG_RE = re.compile(r"\[(B-[A-Z-]+)\]")
FAIL_RE = re.compile(r"^\s*--- FAIL: (\S+)", re.M)
MINE = {
    "TestAskAI_PrivateSpace_NotGroundedOrCitedWithoutGrant_RealPG",
    "TestAsk_MainWiresThePageReadGate",
    "TestAskEndpoint_RefusesWhenNoPageReadGateIsWired",
}


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def run_tests():
    """Returns (verdict, tags, failed_test_names, raw). verdict in {'green','red','build'}."""
    proc = subprocess.run(
        ["go", "test", PKG, "-count=1", "-v"],
        cwd=REPO, env=dict(os.environ), capture_output=True, text=True, timeout=900,
    )
    raw = proc.stdout + proc.stderr
    if ("[build failed]" in raw or "cannot use" in raw or "undefined:" in raw
            or "syntax error" in raw or "declared and not used" in raw):
        return "build", set(), set(), raw
    tags = set(TAG_RE.findall(raw))
    failed = set(FAIL_RE.findall(raw))
    return ("green" if proc.returncode == 0 else "red"), tags, failed, raw


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        print("DOCS_TEST_DATABASE_URL must be set — this suite needs a real Postgres.")
        return 2

    paths = {HANDLER, MAIN, WIRETEST, PERMSTORE}
    originals = {p: open(p, "rb").read() for p in paths}
    before = {p: sha(p) for p in paths}

    verdict, tags, failed, raw = run_tests()
    if verdict != "green":
        print(f"BASELINE NOT GREEN ({verdict}) — nothing below can be read.\n{raw[-4000:]}")
        return 1
    print("baseline: GREEN\n")

    results = []
    claimed = set()
    try:
        for cid, path, old, new, predicted, why in CONTROLS:
            src = originals[path].decode()
            if src.count(old) != 1:
                results.append((cid, "ANCHOR", set(), set(predicted), set()))
                print(f"{cid}: ANCHOR MISSING/AMBIGUOUS ({src.count(old)} matches) — control is dead")
                continue
            with open(path, "w") as f:
                f.write(src.replace(old, new, 1))
            verdict, tags, failed, raw = run_tests()
            with open(path, "wb") as f:
                f.write(originals[path])
            if verdict == "build":
                results.append((cid, "BUILD", set(), set(predicted), set()))
                print(f"{cid}: BUILD FAILED — not a catch")
                continue
            others = failed - MINE
            ok = (tags == set(predicted)) and (verdict == ("red" if predicted else "green"))
            results.append((cid, "AS PREDICTED" if ok else "DIVERGED", tags, set(predicted), others))
            claimed |= tags
            mark = "AS PREDICTED" if ok else "⚠ DIVERGED"
            extra = f"  also-red={sorted(others)}" if others else ""
            print(f"{cid}: {verdict.upper():5s} fired={sorted(tags) or '(none)'} "
                  f"predicted={sorted(predicted) or '(none)'} -> {mark}{extra}")
    finally:
        for p, b in originals.items():
            with open(p, "wb") as f:
                f.write(b)
        for p in paths:
            assert sha(p) == before[p], f"RESTORE FAILED for {p}"
        print("\nall files restored, sha256 verified")

    unclaimed = [a for a in DECLARED if a not in claimed]
    print("\n--- verdict ---")
    for cid, status, got, pred, others in results:
        note = f"   (also reddened {sorted(others)})" if others else ""
        print(f"  {cid:26s} {status}{note}")
    for a in INSTRUMENT:
        print(f"\n  {a}: UNCLAIMED BY CONSTRUCTION — an instrument check, not a product claim. "
              f"No product mutation can make /ask answer 200 without having sent a prompt.")
    if unclaimed:
        print("\n⚠ PRODUCT ASSERTIONS CLAIMED BY NO CONTROL (each could be constant-true and "
              "nothing here would notice):")
        for a in unclaimed:
            print(f"    {a}")
    diverged = [r for r in results if r[1] != "AS PREDICTED"]
    print(f"\n{len(results) - len(diverged)}/{len(results)} as predicted; "
          f"{len(DECLARED) - len(unclaimed)}/{len(DECLARED)} product assertions claimed")
    return 1 if (diverged or unclaimed) else 0


if __name__ == "__main__":
    sys.exit(main())
