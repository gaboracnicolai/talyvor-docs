#!/usr/bin/env python3
"""w31-setup-claims-controls.py — positive controls for internal/config/setupclaims_test.go.

WHY THIS EXISTS

The five assertions in setupclaims_test.go RED on the tree they were written against (the
README quick start named neither required secret, the migration paragraph described a mount
that is not there, the local-dev block told you to copy a file nothing reads, it sent you to
a port nothing binds, and BUILD_STATE's migration high-water was four behind). So there IS a
red-first moment and it is recorded in the item. This harness answers the OTHER question:
does each assertion still fire for the reason it exists, and is any of them carried by a
neighbour?

THE VERDICT IS THE SET OF ASSERTIONS THAT FIRED, NOT PASS/FAIL. A control that reds "the
guard" says nothing about WHICH claim spoke — #69 in this repo scored 7/7 under a PASS/FAIL
verdict while two of four assertions were justified by nothing. Every control below declares
the exact tag set it expects, the run FAILS on any difference, and it FAILS AGAIN at the end
if any declared assertion is never the sole speaker for some control.

A compile/build failure is detected explicitly and scored BUILD ERROR, never as a catch, and
every control carries a MUST-STAY-GREEN companion (the pre-existing tests in the same
package) so "it red" cannot mean "everything red".

Every mutation asserts its anchor's OCCURRENCE COUNT before writing, and every file is
restored in a `finally` and sha256-compared — an exception between mutate and restore once
left `OR TRUE` SQL on disk in this repo.

Run from the repo root:  python3 scripts/w31-setup-claims-controls.py
"""

import hashlib
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

GUARD_TESTS = (
    "TestQuickStartNamesEverySecretTheOperatorMustSupply|"
    "TestReadmeDoesNotClaimAnInitDbHookThatComposeDoesNotMount|"
    "TestLocalDevDoesNotTellYouToCopyAnEnvFileNothingReads|"
    "TestLocalDevOnlyNamesPortsSomethingActuallyBinds|"
    "TestBuildStateMigrationHighWaterMatchesTheMigrationsDirectory"
)
# The pre-existing tests of the same package. They know nothing about README prose, so they
# are an independent must-stay-green companion for every control here.
COMPANION_TESTS = (
    "TestLoad_RejectsPublishedSecret|TestLoad_StillAcceptsRealSecretsAndRejectsWeakOnes|"
    "TestComposeDoesNotSupplyAGatewaySecret|TestEnvExampleShipsNoUsableSecret|"
    "TestSlogLevel_HonoursDocsLogLevel|TestLoad_GatewayAuthSecret_BootFailsClosed"
)

# Every assertion the guard can make. Each must be the SOLE speaker for at least one control.
DECLARED = {"A1", "A1-FLOOR", "A2", "A3", "A4", "A5"}

TAG_RE = re.compile(r"\b(A\d(?:-FLOOR)?):")
BUILD_FAIL_RE = re.compile(r"\[build failed\]|cannot find|undefined:|syntax error")

README = "README.md"
COMPOSE = "docker-compose.yaml"
VITE = "frontend/vite.config.ts"
BUILD_STATE = "BUILD_STATE.md"
MAIN_GO = "cmd/docs/main.go"

NEW_MIGRATION = "migrations/0019_w31_control_only.sql"

INITDB_SENTENCE = "applied by the docs"


def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def edit(rel, old, new, want_count):
    return (rel, old, new, want_count)


CONTROLS = [
    dict(
        id="C1",
        why="the quick start stops naming one of the two secrets the operator must generate",
        edits=[edit(README, "POSTGRES_PASSWORD", "PGPASS_RENAMED", 2)],
        expect={"A1"},
    ),
    dict(
        id="C2",
        why="compose stops REQUIRING one of them (`:?` weakened to an empty default), so the "
        "derived population halves — the shape a broken parse also takes",
        edits=[
            edit(
                COMPOSE,
                "${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD — openssl rand -hex 16}",
                "${POSTGRES_PASSWORD:-}",
                2,
            )
        ],
        expect={"A1-FLOOR"},
    ),
    dict(
        id="C3",
        why="a NEW required-and-blank secret is added to compose and the quick start is not "
        "updated — the tripwire's whole purpose, and the inverse of C1",
        edits=[
            edit(
                COMPOSE,
                "      - DOCS_LOG_LEVEL=${DOCS_LOG_LEVEL:-info}",
                "      - DOCS_LOG_LEVEL=${DOCS_LOG_LEVEL:-info}\n"
                "      - NEW_API_SECRET=${NEW_API_SECRET:?set NEW_API_SECRET}",
                1,
            )
        ],
        expect={"A1"},
    ),
    dict(
        id="C4",
        why="MUST-NOT-CATCH — the README claim comes back AND compose actually mounts the "
        "hook again, so the sentence is true. Proves A2 reads compose, not only prose",
        edits=[
            edit(README, INITDB_SENTENCE, "mounted into the container's init-db hook and " + INITDB_SENTENCE, 1),
            edit(
                COMPOSE,
                "      - pgdata:/var/lib/postgresql/data",
                "      - pgdata:/var/lib/postgresql/data\n      - ./migrations:/docker-entrypoint-initdb.d",
                1,
            ),
        ],
        expect=set(),
    ),
    dict(
        id="C5",
        why="the init-db-hook claim comes back with compose unchanged — the exact sentence "
        "that shipped",
        edits=[edit(README, INITDB_SENTENCE, "mounted into the container's init-db hook and " + INITDB_SENTENCE, 1)],
        expect={"A2"},
    ),
    dict(
        id="C5b",
        why="RECORDED INERT, ONE-DIRECTIONAL BY DESIGN — the same false claim REWORDED past "
        "the token list. A2 is a token check and this is the escape it cannot see",
        edits=[
            edit(
                README,
                INITDB_SENTENCE,
                "mounted into the container's initialisation hook and " + INITDB_SENTENCE,
                1,
            )
        ],
        expect=set(),
        inert=True,
    ),
    dict(
        id="C6",
        why="the local-dev block tells you to copy .env again, while nothing reads it",
        edits=[
            edit(
                README,
                "go run ./cmd/docs           # API on :4000",
                "cp .env.example .env\ngo run ./cmd/docs           # API on :4000",
                1,
            )
        ],
        expect={"A3"},
    ),
    dict(
        id="C7",
        why="MUST-NOT-CATCH — C6's edit PLUS a real .env read in cmd/docs, so copying it is a "
        "real step. Proves A3 reads the Go tree, not only the prose",
        edits=[
            edit(
                README,
                "go run ./cmd/docs           # API on :4000",
                "cp .env.example .env\ngo run ./cmd/docs           # API on :4000",
                1,
            ),
            edit(
                MAIN_GO,
                "\tslog.SetDefault(logger)",
                '\tslog.SetDefault(logger)\n\tif b, err := os.ReadFile(".env"); err == nil {\n\t\t_ = b\n\t}',
                1,
            ),
        ],
        expect=set(),
    ),
    dict(
        id="C8",
        why="the Vite server moves to a port the README does not name — the defect that "
        "shipped, in the direction a real edit takes",
        edits=[edit(VITE, "port: 5174,", "port: 5999,", 1)],
        expect={"A4"},
    ),
    dict(
        id="C9",
        why="a migration is added and BUILD_STATE's high-water is not touched — a prose count "
        "decaying while the thing it counts grows",
        edits=[],
        new_files={NEW_MIGRATION: "-- control only; never applied.\nSELECT 1;\n"},
        expect={"A5"},
    ),
    dict(
        id="C10",
        why="MUST-NOT-CATCH — an unrelated README edit outside both sections",
        edits=[edit(README, "Real-time collaboration", "Realtime collaboration", 1)],
        expect=set(),
    ),
]


def go_test(pattern):
    p = subprocess.run(
        ["go", "test", "-count=1", "-run", pattern, "./internal/config/"],
        cwd=ROOT,
        capture_output=True,
        text=True,
    )
    return p.returncode, p.stdout + p.stderr


def run_control(c):
    touched = [Path(ROOT, rel) for rel, _, _, _ in c.get("edits", [])]
    originals = {p: p.read_bytes() for p in touched}
    hashes = {p: sha(p) for p in touched}
    created = [Path(ROOT, rel) for rel in c.get("new_files", {})]

    try:
        # Assert EVERY anchor count before ANY write: a control that applies half of itself
        # reports a working guard as blind.
        for rel, old, _, want in c.get("edits", []):
            got = Path(ROOT, rel).read_text().count(old)
            if got != want:
                return "ANCHOR", f"{rel}: anchor {old!r} appears {got}x, expected {want}x"
        for rel, old, new, _ in c.get("edits", []):
            p = Path(ROOT, rel)
            p.write_text(p.read_text().replace(old, new))
        for rel, body in c.get("new_files", {}).items():
            Path(ROOT, rel).write_text(body)

        # Confirm each edit actually landed, IN FULL. Checking only the first line is not
        # enough: an inserted line whose anchor is reproduced above it would go unverified,
        # and a control that applies half of itself reports a working guard as blind.
        for rel, _, new, _ in c.get("edits", []):
            if new not in Path(ROOT, rel).read_text():
                return "APPLY", f"{rel}: edit did not land in full"
        for rel in c.get("new_files", {}):
            if not Path(ROOT, rel).exists():
                return "APPLY", f"{rel}: new file was not created"

        rc_c, out_c = go_test(COMPANION_TESTS)
        rc, out = go_test(GUARD_TESTS)
        if BUILD_FAIL_RE.search(out) or BUILD_FAIL_RE.search(out_c):
            return "BUILD", out.strip().splitlines()[:4]
        if rc_c != 0:
            return "COMPANION", "must-stay-green companion RED under this control:\n" + out_c
        fired = set(TAG_RE.findall(out))
        return "OK", fired
    finally:
        for p, b in originals.items():
            p.write_bytes(b)
        for p in created:
            p.unlink(missing_ok=True)
        for p, h in hashes.items():
            if sha(p) != h:
                print(f"!! RESTORE FAILED for {p}", file=sys.stderr)
                sys.exit(2)


def main():
    print("=" * 78)
    print("w31 setup-claims controls — verdict is the SET OF ASSERTIONS THAT FIRED")
    print("=" * 78)

    rc, out = go_test(GUARD_TESTS)
    if rc != 0:
        print("PREMISE FAILED: the guard is not green on the unmodified tree.\n" + out)
        return 2
    print("premise: all five assertions green on the unmodified tree\n")

    ok = True
    sole_speaker = {}
    for c in CONTROLS:
        kind, res = run_control(c)
        if kind != "OK":
            print(f"{c['id']:5} {kind:9} {res}")
            ok = False
            continue
        fired = res
        verdict = "AS PREDICTED" if fired == c["expect"] else "MISMATCH"
        if fired != c["expect"]:
            ok = False
        label = "NOT CAUGHT (inert, by design)" if c.get("inert") else (
            "NOT CAUGHT (must-not-catch)" if not c["expect"] else "fired " + ",".join(sorted(fired))
        )
        if c["expect"] and fired == c["expect"]:
            label = "fired " + ",".join(sorted(fired))
        print(f"{c['id']:5} {verdict:13} {label:34} — {c['why']}")
        if len(fired) == 1:
            sole_speaker.setdefault(next(iter(fired)), []).append(c["id"])

    print()
    unjustified = sorted(DECLARED - set(sole_speaker))
    for a in sorted(DECLARED):
        print(f"  {a:9} earned by: {', '.join(sole_speaker.get(a, [])) or 'NOTHING'}")
    if unjustified:
        print(f"\nUNJUSTIFIED ASSERTIONS (no control makes them the sole speaker): {unjustified}")
        print("Either they are carried by a neighbour or they cannot fire. Do not claim them.")
        ok = False

    print("\n" + ("ALL CONTROLS AS PREDICTED" if ok else "CAMPAIGN FAILED — read above"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
