import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

// A TRIPWIRE ON `Page`'s REQUIRED-KEY SET. Adding a required field must RED and force a choice
// about the records already sitting in a reader's IndexedDB.
//
// THE CLASS. `api/client.ts` persists successful page GETs to IndexedDB (`writeCache` →
// `cachePage(data as Page)`) and serves them back when fetch REJECTS. A record written months
// ago is a real `Page` value this code will be handed, and it carries the fields the API sent
// THEN — not the ones the type declares NOW. The cast is the whole safety story: `readCached`
// returns `value as unknown as T`, so a stale record with a missing key type-checks perfectly
// and crashes at the first property access that assumes it.
//
// ⚠ THIS IS A TRIPWIRE, NOT A TOLERANCE TEST, AND THAT IS THE DESIGN DECISION. "Tolerate a
// missing field" here means defaulting it — and the two fields that actually crash are
// `content_text` (`.split`) and `ai_cost_usd` (`.toFixed`). Defaulting a MONEY field to 0 is
// precisely the fabricated-zero defect findings (1)–(5) of W3.1 exist because of: a document
// that cost $12.34 rendering byte-identical to one that cost nothing. The right answer when a
// required field is added is a DELIBERATE choice — migrate the store, version the records, or
// accept the crash — and this test exists to make somebody make it.
//
// ⚠ RE-MEASURED AT c70aa6e RATHER THAN INHERITED. #59 scoped this class at cf57f83 and
// concluded it was LATENT: every field `Page` declares required today was already required at
// a8d711c, the commit the offline cache shipped. That claim is nine merges old, so it was
// measured again here across all three commits — required sets IDENTICAL (20 keys) at a8d711c,
// cf57f83 and HEAD; the fields added since (own_ai_cost_usd, total_ai_cost_usd, page_type,
// linked_issues) are ALL optional. So version skew exposes nothing TODAY. This test is what
// makes the next required field loud on the day it lands instead of nine merges later.
//
// ⚠ THE PINNED LIST BELOW IS A HARDCODED LITERAL, NOT A VALUE READ BACK FROM THE SAME PARSE.
// A source-derived guard's failure mode is a regex that quietly matches nothing, leaving two
// empty sets comparing equal — a green run over a question never asked. Comparing the parse to
// a literal makes an instrument that has stopped reading fail HERE. It also makes REMOVAL
// visible: a parse-the-source guard cannot otherwise see the source shrink.

const here = path.dirname(fileURLToPath(import.meta.url));

// EVERY REQUIRED KEY ON `Page`, PINNED. Measured at c70aa6e, and identical at a8d711c (the
// commit the IndexedDB cache shipped). Changing this list is the deliberate act the tripwire
// exists to require — and if you are ADDING to it, say in the commit message what happens to a
// cached record that predates the field.
const PINNED_REQUIRED = [
  "ai_cost_usd",
  "content",
  "content_text",
  "cover_url",
  "created_at",
  "created_by",
  "depth",
  "icon",
  "id",
  "is_template",
  "locked",
  "position",
  "slug",
  "space_id",
  "stale_after_days",
  "title",
  "updated_at",
  "updated_by",
  "view_count",
  "workspace_id",
];

type Split = { required: string[]; optional: string[] };

function pageKeys(): Split {
  const src = readFileSync(path.join(here, "types.ts"), "utf8");
  const m = src.match(/export interface Page \{([\s\S]*?)\n\}/);
  if (!m) {
    throw new Error(
      "could not find `export interface Page` in api/types.ts — the parser found NOTHING, " +
        "which is the one way this guard fails silently. Fix the parser, do not delete the test.",
    );
  }
  // Strip line comments first: this interface carries several, and they contain colons.
  const body = m[1].replace(/\/\/[^\n]*/g, "");
  const required: string[] = [];
  const optional: string[] = [];
  for (const line of body.split("\n")) {
    const mm = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)(\??):/);
    if (!mm) continue;
    (mm[2] === "?" ? optional : required).push(mm[1]);
  }
  return { required, optional };
}

describe("Page's required-key set vs the IndexedDB records already on disk", () => {
  it("has not grown a required field without a decision about pre-existing cached records", () => {
    const { required } = pageKeys();
    const got = [...new Set(required)].sort();
    const want = [...PINNED_REQUIRED].sort();

    const added = got.filter((k) => !want.includes(k));
    const removed = want.filter((k) => !got.includes(k));

    // Two separate expectations, deliberately: an ADDED required key and a REMOVED one are
    // different events with different consequences, and a single set-equality assertion would
    // report them with one indistinguishable message.
    expect(
      added,
      `required key(s) added to Page: [${added.join(", ")}]. A record cached before this field ` +
        `shipped will not have it, and api/client.ts will serve that record on the offline ` +
        `path. Decide what happens to it — migrate, version, or accept — then add the name to ` +
        `PINNED_REQUIRED. Do NOT default a money field to 0.`,
    ).toEqual([]);

    expect(
      removed,
      `required key(s) missing from Page: [${removed.join(", ")}]. Either the field became ` +
        `optional or it is gone; both are fine deliberately and neither is fine by accident, ` +
        `and a parse-the-source guard cannot see a shrinking source any other way.`,
    ).toEqual([]);
  });

  it("reads the `?` — an OPTIONAL field is not a required one", () => {
    // The predicate this guard turns on. Without it the test above would red on EVERY new field
    // and would be deleted within a merge or two as noise. The optional set is asserted
    // non-empty rather than pinned: pinning it would make exactly the safe change — adding an
    // optional field — fail, which is the behaviour this case exists to rule out.
    const { required, optional } = pageKeys();
    expect(optional.length, "no optional keys parsed — the `?` branch matched nothing").toBeGreaterThan(0);
    expect(
      optional.filter((k) => required.includes(k)),
      "a key was counted as BOTH required and optional — the predicate is not reading the `?`",
    ).toEqual([]);
    // The three cost/type fields added since the cache shipped are the worked example: all
    // optional, which is why the class is latent rather than live.
    for (const k of ["own_ai_cost_usd", "total_ai_cost_usd", "page_type"]) {
      expect(optional, `${k} must stay OPTIONAL — a cached pre-0018 record does not have it`).toContain(k);
    }
  });
});
