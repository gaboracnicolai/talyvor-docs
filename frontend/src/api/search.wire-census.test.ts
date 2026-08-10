import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

// THE CENSUS: EVERY FIELD THE SEARCH WIRE EMITS MUST EXIST ON THE SPA'S SearchResult TYPE.
//
// This is the guard that would have caught W3.1 finding (5) on the day migration 0018 landed,
// rather than four merges later. `SearchModal.cost.test.tsx` next door proves the RENDERER
// shows the right number; it is blind to the failure that produced the defect in the first
// place, which was upstream of any rendering:
//
//   the Go handler grew `own_ai_cost_usd` and `total_ai_cost_usd`,
//   `frontend/src/api/search.ts` did not,
//   so both fields arrived on every full-text hit and were dropped at the TYPE boundary,
//   and NOTHING in the frontend was red, because a field the type does not mention is a
//   field no test can miss.
//
// A behavioural guard cannot see that class: it can only assert about fields somebody already
// thought to declare. This one compares the two DECLARATIONS as sets, in both directions, so a
// fourth cost column landing on the wire and missing the SPA fails the build.
//
// ⚠ BOTH DIRECTIONS, NOT A SUBSET CHECK. `TS ⊇ GO` alone passes a type that has drifted to
// declare fields the server stopped sending — a client reading `r.foo` forever undefined and
// nothing saying so. Set equality is what makes a DELETION on the wire visible too.
//
// ⚠ AND THE INSTRUMENT IS HELD TO A PINNED FLOOR. Both sides are derived by parsing source, so
// the failure mode of the census is that a regex quietly matches nothing and two empty sets
// compare equal — a green run over a question never asked. The literal expectations below are
// hardcoded names, not values read back from the same parse, so a parser that has stopped
// finding the struct fails here rather than reporting agreement.

const here = path.dirname(fileURLToPath(import.meta.url));
const repo = path.resolve(here, "..", "..", ".."); // src/api -> src -> frontend -> repo root

function goResultTags(): string[] {
  const src = readFileSync(path.join(repo, "internal", "search", "handler.go"), "utf8");
  const m = src.match(/type Result struct \{([\s\S]*?)\n\}/);
  if (!m) throw new Error("could not find `type Result struct` in internal/search/handler.go");
  return [...m[1].matchAll(/json:"([^",]+)/g)].map((x) => x[1]);
}

function tsSearchResultKeys(): string[] {
  const src = readFileSync(path.join(here, "search.ts"), "utf8");
  const m = src.match(/export interface SearchResult \{([\s\S]*?)\n\}/);
  if (!m) throw new Error("could not find `export interface SearchResult` in api/search.ts");
  // Strip comments first: the block above this interface talks ABOUT the field names, and an
  // unanchored match over prose is how a sibling guard in this repo once counted its own
  // documentation as product (`224bdee`).
  const body = m[1].replace(/\/\/[^\n]*/g, "");
  return [...body.matchAll(/^\s*([a-z_]+)\??:/gm)].map((x) => x[1]);
}

describe("the SPA's search type is the wire's search type", () => {
  it("the parsers found a real struct and a real interface — the floor, before any comparison", () => {
    const go = goResultTags();
    const ts = tsSearchResultKeys();
    // Hardcoded literals. Comparing a parse against itself passes for every value.
    expect(go.length, "the Go Result struct parse yielded too few json tags").toBeGreaterThanOrEqual(8);
    expect(ts.length, "the TS SearchResult parse yielded too few keys").toBeGreaterThanOrEqual(8);
    for (const required of ["page_id", "source", "url", "ai_cost_usd"]) {
      expect(go, `the Go parse lost ${required} — the regex is not reading the struct`).toContain(required);
      expect(ts, `the TS parse lost ${required} — the regex is not reading the interface`).toContain(required);
    }
  });

  it("no field the wire emits is missing from the SPA type", () => {
    const missing = goResultTags().filter((t) => !tsSearchResultKeys().includes(t));
    expect(
      missing,
      `internal/search/handler.go emits ${JSON.stringify(missing)} and ` +
        `frontend/src/api/search.ts does not declare it. Those values arrive on every ` +
        `matching hit and are dropped at the type boundary, where nothing can be red about ` +
        `them. This is exactly how own_ai_cost_usd and total_ai_cost_usd went four merges ` +
        `unnoticed while the search list showed the wrong half of a document's cost.`,
    ).toEqual([]);
  });

  it("no field the SPA type declares has vanished from the wire", () => {
    const phantom = tsSearchResultKeys().filter((k) => !goResultTags().includes(k));
    expect(
      phantom,
      `frontend/src/api/search.ts declares ${JSON.stringify(phantom)} and ` +
        `internal/search/handler.go no longer emits it. Every read of it is undefined and the ` +
        `type says otherwise.`,
    ).toEqual([]);
  });

  it("all three cost fields are on both sides — the specific case the floor cannot check", () => {
    // A floor asserting "at least 8 fields, both directions agree" is satisfied by two sides
    // that agree on the WRONG set. These three names are the point of the guard, so they are
    // named, not counted.
    for (const f of ["ai_cost_usd", "own_ai_cost_usd", "total_ai_cost_usd"]) {
      expect(goResultTags(), `${f} is not emitted by the search wire`).toContain(f);
      expect(tsSearchResultKeys(), `${f} is not declared by the SPA search type`).toContain(f);
    }
  });
});
