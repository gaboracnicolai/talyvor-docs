#!/usr/bin/env python3
"""
FINDING (19): measure whether `.semgrep/operate-by-id-tenancy.yml` can see the class it is named
for.

The class is "a by-id write that does not name the object its route's enforcer authorized".
Three copies shipped and were fixed in three consecutive PRs — #82 (changelog), #83 (permission),
#84 (database rows/views). The rule ran on every one of those PRs and exited 0 every time.

This harness does not read the rule and reason about it. It RESTORES each defect exactly as it
shipped — `git show <fix>^:<path>`, the real bytes, not a mutation I invented — and runs the exact
CI command over it.

Verdict per case is the pair (exit code, finding count), predicted before the run.

The three RESTORE cases are the subject. They are paired with:

  * P0  MUST-STAY-GREEN: the pristine tree scans 0. Without it a rule file that fails to parse,
        or a semgrep that scans nothing, would make every RESTORE case read as "blind" for free.
  * P1-P3 POSITIVE CONTROLS: into each of the SAME three files, plant a by-id write with NO
        workspace predicate at all — the shape rule (a) exists for. If these do not fire, semgrep
        is not reading that file and its silence on the RESTORE case means nothing. This is the
        control that separates "the rule cannot express this defect" from "the scanner never
        looked". Note semgrep scans only files TRACKED BY GIT, so every mutation here edits a
        tracked file in place; a new file would be skipped silently.

Restores run in a `finally` and every file is sha256-compared against the bytes saved before the
run, so a crash between mutate and restore cannot leave a defect on disk.
"""

import hashlib
import pathlib
import shutil
import subprocess
import sys
import tempfile

REPO = pathlib.Path(__file__).resolve().parent.parent
SEMGREP = ["semgrep", "--config", ".semgrep/", "--error", "--quiet", "--metrics=off"]

# (label, path, commit-of-the-fix, what the statement looked like before it)
RESTORES = [
    (
        "R1-changelog",
        "internal/changelog/store.go",
        "bf1f5fe",
        "WHERE id = $1 AND workspace_id = ANY($2)  (no page_id) x4 statements",
    ),
    (
        "R2-permission",
        "internal/permission/store.go",
        "5a09c08",
        "DELETE FROM permissions WHERE id = $1 AND workspace_id = ANY($2)  (no resource_type/id)",
    ),
    (
        "R3-database",
        "internal/database/store.go",
        "13b6095",
        "database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($n))  (no database_id = $n)",
    ),
]

# The positive controls go into the same three files. A bare by-id write, no workspace predicate:
# exactly rule (a)'s subject. Appended as a real method so the file still parses as Go.
PLANT = """

// nosemgrep-none: planted by scripts/w31-classguard-blindness.py, never committed
func (s *Store) w31ControlPlantedBareByIDWrite(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM changelog_entries WHERE id = $1`, id)
	return err
}
"""


def sh(args, cwd=REPO):
    return subprocess.run(args, cwd=cwd, capture_output=True, text=True)


def sha256(p):
    return hashlib.sha256(pathlib.Path(p).read_bytes()).hexdigest()


def semgrep_verdict():
    """Return (exit_code, finding_count). --quiet prints one line per finding-free run."""
    r = sh(SEMGREP)
    # `--error` makes exit 1 on any finding. Count findings from the non-quiet run only when
    # there is something to count, so the fast path stays fast.
    if r.returncode == 0:
        return 0, 0
    j = sh(["semgrep", "--config", ".semgrep/", "--json", "--quiet", "--metrics=off"])
    import json

    try:
        n = len(json.loads(j.stdout)["results"])
    except Exception:
        n = -1
    return r.returncode, n


def main():
    paths = sorted({p for _, p, _, _ in RESTORES})

    # REFUSE only if one of THIS harness's own subject files is modified. The first version
    # refused on any dirty path at all, which sounds stricter and is worse: run from a branch
    # that touches something unrelated, it exits 2 and prints nothing about the semgrep rule —
    # so the measurement it exists for silently does not happen on the branch that most needs it.
    # What must be pristine is the three store.go files it restores; everything else is noted and
    # scanned as-is, because the scan is over the whole tree and the reader should know what was
    # in it.
    status = sh(["git", "status", "--porcelain"]).stdout.strip().splitlines()
    subject_dirty = [l for l in status if any(p in l for p in paths)]
    if subject_dirty:
        print("REFUSING TO RUN: a file this harness restores is already modified, so its "
              "'restore the shipped defect' cases would restore over your edits.\n"
              + "\n".join(subject_dirty))
        return 2
    other = [l for l in status if l not in subject_dirty]
    if other:
        print("NOTE — the scanned tree also contains these changes (none of them a subject file):")
        for l in other:
            print("   ", l)

    before = {p: sha256(REPO / p) for p in paths}
    backup_dir = pathlib.Path(tempfile.mkdtemp(prefix="w31-classguard-"))
    for p in paths:
        dst = backup_dir / p.replace("/", "__")
        shutil.copy2(REPO / p, dst)  # cp, not `git checkout` — the restore must not depend
        #                              on the index being what I think it is.

    results = []
    try:
        # ---- P0 must-stay-green ------------------------------------------------------------
        code, n = semgrep_verdict()
        results.append(("P0-pristine", "GREEN (0 findings)", f"exit={code} findings={n}",
                        code == 0 and n == 0))

        # ---- R1..R3 the defects, restored as shipped ---------------------------------------
        for label, path, fix, shape in RESTORES:
            src = sh(["git", "show", f"{fix}^:{path}"])
            if src.returncode != 0:
                results.append((label, "n/a", f"could not read {fix}^:{path}", False))
                continue
            pre = src.stdout
            if pre == (REPO / path).read_text():
                results.append((label, "n/a", "pre-fix bytes identical to HEAD — nothing restored",
                                False))
                continue
            (REPO / path).write_text(pre)
            code, n = semgrep_verdict()
            # PREDICTION: 0 findings. The rule's pattern-not excludes any statement containing
            # `workspace_id = ANY`, and every one of these defects HAS that predicate — it is the
            # defect. If this fires, the finding is wrong and I want to know.
            results.append((label, "PREDICTED BLIND (0 findings)",
                            f"exit={code} findings={n}   [{shape}]", code == 0 and n == 0))
            shutil.copy2(backup_dir / path.replace("/", "__"), REPO / path)

        # ---- P1..P3 positive controls: does semgrep read these files at all? ---------------
        for i, (label, path, _, _) in enumerate(RESTORES, start=1):
            table = {"internal/changelog/store.go": "changelog_entries",
                     "internal/permission/store.go": "permissions",
                     "internal/database/store.go": "database_rows"}[path]
            plant = PLANT.replace("changelog_entries", table).replace(
                "w31ControlPlantedBareByIDWrite", f"w31ControlPlanted{i}")
            (REPO / path).write_text((REPO / path).read_text() + plant)
            code, n = semgrep_verdict()
            # PREDICTION: fires. A by-id DELETE with no workspace predicate is rule (a)'s
            # literal subject.
            results.append((f"P{i}-plant-{path.split('/')[1]}", "PREDICTED CAUGHT (>=1 finding)",
                            f"exit={code} findings={n}", code != 0 and n >= 1))
            shutil.copy2(backup_dir / path.replace("/", "__"), REPO / path)
    finally:
        for p in paths:
            shutil.copy2(backup_dir / p.replace("/", "__"), REPO / p)
        after = {p: sha256(REPO / p) for p in paths}
        bad = [p for p in paths if before[p] != after[p]]
        if bad:
            print("!! RESTORE FAILED, TREE IS DIRTY:", bad)
            return 3
        shutil.rmtree(backup_dir, ignore_errors=True)

    width = max(len(r[0]) for r in results)
    ok = 0
    for label, predicted, observed, passed in results:
        print(f"{'AS PREDICTED' if passed else '!! NOT AS PREDICTED':<20} "
              f"{label:<{width}}  predicted={predicted:<32} observed={observed}")
        ok += bool(passed)
    print(f"\n{ok}/{len(results)} as predicted")
    return 0 if ok == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
