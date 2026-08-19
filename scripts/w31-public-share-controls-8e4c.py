#!/usr/bin/env python3
"""Positive controls for internal/sharing/publiclane_realpg_test.go (tab-8e4c, W3.1).

WHY THIS EXISTS. Every assertion in publiclane_realpg_test.go passes on the unmodified tree —
the public share lane was measured end to end and is correct — so not one of them is earned by a
red-first failure. A guard that has never been observed failing is a claim, not a guard. Each
control below breaks ONE shipped behaviour and must turn ONE named test red.

SCORING, and why it is not tag-only. A test can die at a t.Fatalf in its SETUP and print no tag
at all, which a tag-only harness reports as "silent" for a mutation it actually caught. So each
control predicts BOTH the failing test names and the tags, and the report shows both. A control
that fails to COMPILE scores BUILD-FAILED and is not a catch — it must be re-cut.

Run:  DOCS_TEST_DATABASE_URL=... python3 scripts/w31-public-share-controls-8e4c.py
"""

import hashlib
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

EXEMPT = "internal/gatewayauth/exempt.go"
STORE = "internal/sharing/store.go"
HANDLER = "internal/sharing/handler.go"

# (name, file, old, new, predicted failing tests, predicted tags, note)
CONTROLS = [
    (
        "C1 the public exemption is removed entirely",
        EXEMPT,
        'return strings.HasPrefix(path, "/v1/public/")',
        "return false",
        ["TestPublicLane_StrangerCanOpenALiveLink_RealPG"],
        ["[STRANGER-CAN-OPEN]"],
        "the stranger lane is the assertion; the other tests die in setup, which the report shows",
    ),
    (
        "C2 the exemption is widened to the whole of /v1",
        EXEMPT,
        'return strings.HasPrefix(path, "/v1/public/")',
        'return strings.HasPrefix(path, "/v1/")',
        ["TestPublicLane_ExemptionIsNarrow_RealPG"],
        ["[EXEMPTION-IS-NARROW]"],
        "THE ONE THAT MATTERS: StrangerCanOpen must stay GREEN here, or it was the whole guard",
    ),
    (
        "C3 expiry is recorded but not enforced on the read",
        STORE,
        "\tif link.ExpiresAt != nil && time.Now().UTC().After(*link.ExpiresAt) {\n"
        '\t\treturn nil, errors.New("sharing: link expired")\n\t}\n',
        "",
        ["TestPublicLane_ExpiredLinkIsGone_RealPG"],
        ["[EXPIRED-IS-GONE]"],
        "",
    ),
    (
        "C4 a missing password skips the password check",
        STORE,
        'if link.PasswordHash != nil && *link.PasswordHash != "" {',
        'if link.PasswordHash != nil && *link.PasswordHash != "" && password != "" {',
        ["TestPublicLane_PasswordIsEnforced_RealPG"],
        ["[PASSWORD-REQUIRED]"],
        "PASSWORD-RIGHT must stay green inside the same test",
    ),
    (
        "C5 a wrong password is accepted",
        STORE,
        "if bcrypt.CompareHashAndPassword([]byte(*link.PasswordHash), []byte(password)) != nil {",
        "if false {",
        ["TestPublicLane_PasswordIsEnforced_RealPG"],
        ["[PASSWORD-WRONG]"],
        "PASSWORD-RIGHT must stay green — a guard that refuses everything is not a guard",
    ),
    (
        "C6 revoke reports success and does not revoke",
        STORE,
        "tag, err := s.pool.Exec(ctx, `DELETE FROM share_links WHERE id = $1 AND page_id = $2`, id, pageID)",
        "tag, err := s.pool.Exec(ctx, `UPDATE share_links SET view_count = view_count WHERE id = $1 AND page_id = $2`, id, pageID)",
        ["TestPublicLane_RevokeActuallyRevokes_RealPG"],
        ["[REVOKE-REVOKES]"],
        "RowsAffected stays 1, so the route still answers ok:true — the lie this guard exists for",
    ),
    (
        "C7 revoke is not scoped to its page (the ce8bfe3 regression)",
        STORE,
        "`DELETE FROM share_links WHERE id = $1 AND page_id = $2`, id, pageID)",
        "`DELETE FROM share_links WHERE id = $1 AND ($2::text IS NOT NULL)`, id, pageID)",
        ["TestPublicLane_RevokeIsScopedToItsPage_RealPG"],
        ["[REVOKE-SCOPED]"],
        "RevokeActuallyRevokes must stay green: an unscoped delete still revokes its own link",
    ),
    (
        "C8 the public projection grows an edit-metadata field",
        HANDLER,
        '\tUpdatedAt   string `json:"updated_at"`\n',
        '\tUpdatedAt   string `json:"updated_at"`\n\tViewCount   int    `json:"view_count"`\n',
        ["TestPublicLane_PayloadCarriesNoEditMetadata_RealPG"],
        ["[LEAN-PROJECTION]"],
        "",
    ),
    (
        "C9 the X-Powered-By header is blanked, body untouched",
        HANDLER,
        'w.Header().Set("X-Powered-By", "Talyvor Docs")',
        'w.Header().Set("X-Powered-By", "")',
        ["TestPublicLane_StrangerCanOpenALiveLink_RealPG"],
        ["[POWERED-BY-HEADER]"],
        "SCORED NOT CAUGHT ON ITS FIRST RUN — the guard asserted the powered_by BODY field and "
        "never the header, which is a different string on a different line. The assertion this "
        "now catches was added because of this control, not because anyone read for it.",
    ),
]

TAG_RE = re.compile(r"\[[A-Z][A-Z-]+\]")


def run_tests():
    env = dict(os.environ)
    p = subprocess.run(
        ["go", "test", "-timeout", "300s", "-count=1", "-v", "-run", "TestPublicLane", "./internal/sharing/"],
        cwd=ROOT, env=env, capture_output=True, text=True,
    )
    out = p.stdout + p.stderr
    if "build failed" in out or "[build failed]" in out or "cannot use" in out or "undefined:" in out:
        return "BUILD-FAILED", set(), set(), out
    failing = set(re.findall(r"^\s*--- FAIL: (\S+)", out, re.M))
    tags = set()
    for line in out.splitlines():
        if ".go:" in line and ("Error" not in line[:0]):
            for m in TAG_RE.findall(line):
                tags.add(m)
    # Only tags printed on FAILING output matter; the file prints tags exclusively in t.Errorf/
    # t.Fatalf, so any tag in the output is a failure line.
    return ("FAIL" if failing else "PASS"), failing, tags, out


def apply(path, old, new):
    full = os.path.join(ROOT, path)
    with open(full, "r") as f:
        src = f.read()
    if src.count(old) != 1:
        raise SystemExit(f"ANCHOR MATCHED {src.count(old)} SITES in {path} — re-cut the control:\n{old!r}")
    with open(full, "w") as f:
        f.write(src.replace(old, new, 1))
    return src


def restore(path, src):
    with open(os.path.join(ROOT, path), "w") as f:
        f.write(src)


def sha(path):
    with open(os.path.join(ROOT, path), "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def main():
    if not os.environ.get("DOCS_TEST_DATABASE_URL"):
        raise SystemExit("DOCS_TEST_DATABASE_URL must be set — these are real-Postgres guards")

    base_status, base_fail, _, base_out = run_tests()
    if base_status != "PASS":
        print(base_out[-4000:])
        raise SystemExit(f"BASELINE IS NOT GREEN ({base_status}, {sorted(base_fail)}) — stop")
    print("BASELINE: all TestPublicLane guards PASS on the unmodified tree\n")

    results = []
    for name, path, old, new, want_tests, want_tags, note in CONTROLS:
        before = sha(path)
        src = apply(path, old, new)
        try:
            status, failing, tags, out = run_tests()
        finally:
            restore(path, src)
        after = sha(path)
        if after != before:
            raise SystemExit(f"RESTORE FAILED for {path} — tree is dirty, stop")

        if status == "BUILD-FAILED":
            verdict = "BUILD-FAILED (not a catch — re-cut)"
        elif not want_tests:
            verdict = "NOT CAUGHT (predicted)" if status == "PASS" else f"CAUGHT UNEXPECTEDLY by {sorted(failing)}"
        elif set(want_tests).issubset(failing) and set(want_tags).issubset(tags):
            extra = failing - set(want_tests)
            verdict = "CAUGHT as predicted" + (f" (+setup deaths: {sorted(extra)})" if extra else "")
        else:
            verdict = f"MISMATCH — failing={sorted(failing)} tags={sorted(tags)}"

        results.append((name, verdict, note))
        print(f"{name}\n    predicted: tests={want_tests} tags={want_tags}\n    -> {verdict}")
        if note:
            print(f"    note: {note}")
        print()

    bad = [r for r in results if not (r[1].startswith("CAUGHT as predicted") or r[1].startswith("NOT CAUGHT (predicted)"))]
    print(f"{len(results) - len(bad)}/{len(results)} controls as predicted")
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
