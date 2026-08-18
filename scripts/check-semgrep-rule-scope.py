#!/usr/bin/env python3
"""Two properties of .semgrep/ that the rule fixtures CANNOT check, asserted here instead.

WHY THIS EXISTS AT ALL. `semgrep --test` applies a rule directly to its fixture and IGNORES the
rule's `paths:` filter — measured, not assumed: with `paths.include: [internal/ratelimit]` bolted
onto docs-no-indirect-url-param-scope, an unauthorized workspace read planted in
internal/freshness went UNFLAGGED by the product scan while the fixtures still reported 4/4 pass.
So the fixtures are blind to precisely the narrowing this repo has already suffered — the
paths.include allow-list that omitted four packages, one of which was shipping the exact defect
docs-no-url-param-workspace-scope exists to catch.

  (1) NO RULE MAY CARRY A `paths:` FILTER. An allow-list can only ever ratify yesterday's sweep;
      the deny-list of inline `nosemgrep` exemptions is this repo's chosen mechanism, and each of
      those is one line, states its reason, and is reviewed in the diff that adds it. If an
      exclusion is ever genuinely needed, this check is the thing that must be argued with.

  (2) THE UNFIXTURED RULES ARE NAMED, NOT MERELY ABSENT. A rule with no fixture cannot be
      red-first, so its narrowing is invisible until the defect it stopped catching ships. The
      list below is EMPTY today: every rule in .semgrep/ has a fixture case. It stays here, and
      it stays enforced in both directions, so a new rule cannot join a silent tail — adding one
      without a fixture fails here until someone writes the fixture or writes down why not.

      ⚠ IT WAS NOT EMPTY, AND EMPTYING IT IS WHERE THE LAST DEFECT CAME FROM. The four rules in
      body-supplied-authority.yml sat here with reasons that read as costs ("needs two
      near-identical handlers", "a fixture would restate the pattern rather than test it").
      Writing the fixture anyway measured that docs-no-inverted-identity-fallback could only ever
      fire against the two helpers docs-no-ambiguous-actor-helpers already rejects: six of the
      eight resolvers its regex names return (string, bool), and a two-value call cannot stand in
      the single-value assignment its pattern requires. The rule was incapable of producing a
      finding its sibling did not already produce. A declared reason is a promise that the rule
      still works, and nothing was checking it.

Run from the repo root. Exits non-zero, printing the offending rule ids, on any violation.
"""

from __future__ import annotations

import pathlib
import re
import sys

import yaml

RULES_DIR = pathlib.Path(".semgrep")

# Rules that have no fixture, and why. Keep the reason honest: "hard" is a reason, "later" is not.
#
# EMPTY, and that is a measured state rather than a default. Each of the four entries that used to
# be here named a real cost, and each was wrong about what the fixture would show: two
# near-identical handlers in one file pin both directions of a pattern-not-inside perfectly well,
# and a fixture over an "exact" rule is what stops it being narrowed later. See the module
# docstring for what writing them actually found.
DECLARED_UNFIXTURED: dict[str, str] = {}


def main() -> int:
    rule_files = sorted(RULES_DIR.glob("*.yml"))
    if not rule_files:
        print(f"error: no rule files under {RULES_DIR}/ — this check read nothing")
        return 1

    all_ids: set[str] = set()
    with_paths: list[str] = []
    for f in rule_files:
        doc = yaml.safe_load(f.read_text()) or {}
        rules = doc.get("rules") or []
        if not rules:
            print(f"error: {f} declares no rules — this check read nothing")
            return 1
        for rule in rules:
            rid = rule.get("id", "<no id>")
            all_ids.add(rid)
            if "paths" in rule:
                with_paths.append(f"{f}:{rid}")

    # Coverage is an EXPECT-A-FINDING annotation. A rule asserted only to stay silent is not
    # tested: "flags nothing" is also what a rule that matches nothing at all looks like.
    covered: set[str] = set()
    for fixture in sorted(RULES_DIR.glob("tests/*.go")):
        for m in re.finditer(r"^\s*//\s*ruleid:\s*(\S+)", fixture.read_text(), re.M):
            covered.add(m.group(1))

    failed = False

    if with_paths:
        failed = True
        print("error: a rule declares a `paths:` filter, which the fixtures cannot see:")
        for x in with_paths:
            print(f"  {x}")
        print("  An allow-list ratifies yesterday's sweep. Use an inline nosemgrep with a reason.")

    unknown = covered - all_ids
    if unknown:
        failed = True
        print(f"error: fixture expects rule ids that no rule file defines: {sorted(unknown)}")

    unfixtured = all_ids - covered
    if unfixtured != set(DECLARED_UNFIXTURED):
        failed = True
        new_ids = sorted(unfixtured - set(DECLARED_UNFIXTURED))
        gone = sorted(set(DECLARED_UNFIXTURED) - unfixtured)
        if new_ids:
            print(f"error: rule(s) with no fixture and no declared reason: {new_ids}")
            print("  Write a fixture case in .semgrep/tests/, or add the id to DECLARED_UNFIXTURED")
            print("  with the reason it is hard. A rule with no fixture cannot be red-first.")
        if gone:
            print(f"error: DECLARED_UNFIXTURED names rule(s) that are now fixtured or gone: {gone}")
            print("  Delete those entries — a stale exemption list overstates what is untested.")

    if failed:
        return 1

    print(f"ok: {len(rule_files)} rule files, {len(all_ids)} rules, "
          f"{len(covered)} fixtured, {len(unfixtured)} declared unfixtured, 0 with paths filters")
    return 0


if __name__ == "__main__":
    sys.exit(main())
