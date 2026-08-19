import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

// THE REASON THE SCREEN GIVES A CUSTOMER FOR A MISSING NUMBER MUST BE THE REASON IN THE CODE.
//
// ⚠⚠ THE FINDING THIS EXISTS FOR: `SearchModal.tsx` renders an em-dash for a search hit with no
// cost and explains it to the reader in a `title` attribute — "AI cost not reported for this
// match — it was found by meaning, and its page row was not read". THE SECOND HALF WAS FALSE, and
// this repository had already measured it false and written the measurement down IN THE SERVER
// FILE THE COMMENT CITES. `internal/search/handler.go` says, of the identical sentence it used to
// carry: "MEASURED: SemanticSearch.Search's SQL is `FROM page_embeddings pe JOIN pages p ON
// p.id = pe.page_id`, so a pages row IS read". The Go side was corrected; the two frontend copies
// were not, and one of them is the only version a user ever sees.
//
// ⚠ IT IS NOT COSMETIC, AND THE SERVER COMMENT SAYS WHY IN ITS NEXT SENTENCE. The open question
// on this surface is whether a semantic-only hit should carry costs at all, and that question is
// settled by exactly this fact: "So 'should a semantic hit pay for a page read?' is a question
// about a read that already happens." A stale premise here does not merely misinform a reader —
// it is the input to a decision, and it points the decision the wrong way.
//
// ⚠ THE CHECK IS DRIVEN BY THE SQL, WHICH IS WHAT KEEPS IT FROM BEING A PINNED STRING — AND ITS
// FIRST DRAFT CLAIMED MORE THAN IT DID. That draft said it was symmetric: "joins present ⇒ the
// SPA may not say the row was not read; joins absent ⇒ the SPA may not say it was". Control D3
// (drop `JOIN pages` from the query) came back NOT CAUGHT, and the reason is structural: the SPA
// never asserts the POSITIVE in rendered code — only in comments, which this file strips — so the
// second branch had nothing it could ever match. A branch that cannot fire is not a check, and
// the header describing it was the kind of sentence this whole finding is about.
//
// WHAT IT DOES INSTEAD, AND IT IS A REAL INVARIANT: the join is a PREMISE OF THE CURRENT WORDING.
// Every user-facing sentence here was chosen against a query that reads a pages row; if that stops
// being true the wording has to be re-derived by a person, not silently inherited. So the absent-
// join branch fails ON THE PREMISE CHANGE rather than pretending to detect a claim nobody makes.

const here = path.dirname(fileURLToPath(import.meta.url));
const repo = path.resolve(here, "..", "..", ".."); // src/components -> src -> frontend -> repo root

// The Search SQL as the server actually issues it. Anchored on the function so a second query
// elsewhere in the file cannot stand in for this one.
function semanticSearchSQL(): string {
  const src = readFileSync(path.join(repo, "internal", "search", "semantic.go"), "utf8");
  const fn = src.indexOf("func (s *SemanticSearch) Search(");
  if (fn < 0) throw new Error("could not find SemanticSearch.Search in internal/search/semantic.go");
  const body = src.slice(fn);
  const m = body.match(/`(SELECT[\s\S]*?)`/);
  if (!m) throw new Error("could not find the SELECT literal inside SemanticSearch.Search");
  return m[1];
}

// The one string a customer reads. Extracted from the element, not from the file at large, so a
// corrected COMMENT cannot satisfy a check about the TOOLTIP.
function costUnknownTitle(): string {
  const src = readFileSync(path.join(here, "SearchModal.tsx"), "utf8");
  const el = src.match(/data-testid="search-cost-unknown"[\s\S]{0,400}?title="([^"]*)"/);
  if (!el) throw new Error('could not find the title= on the data-testid="search-cost-unknown" element');
  return el[1];
}

// Every place the SPA states the premise IN CODE THE USER CAN SEE — comments stripped.
//
// ⚠ THE FIRST DRAFT MATCHED THE WHOLE FILE AND THAT WAS WRONG IN A WAY WORTH RECORDING, because
// it is the second time this exact mistake appeared in one session. A file-wide match forbids
// the phrase ANYWHERE, so it forbids the sentence that RECORDS the phrase as false — and this
// repository's entire commenting style is "say what the old claim was and why it was wrong". The
// guard went red on the correction. Stripping comments makes the check what the finding actually
// is: a claim the PRODUCT makes, not one the source discusses.
//
// ⚠ WHAT IT THEREFORE DOES NOT COVER, said plainly: a comment that re-asserts the false premise
// as current. Nothing here would notice. That residual is a reviewable diff, and it is the same
// place `internal/testutil/frontendmanifest_test.go` leaves its own.
function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^[ \t]*\/\/[^\n]*$/gm, "");
}

function spaSources(): Array<{ file: string; text: string }> {
  return [
    { file: "src/components/SearchModal.tsx", text: stripComments(readFileSync(path.join(here, "SearchModal.tsx"), "utf8")) },
    { file: "src/api/search.ts", text: stripComments(readFileSync(path.join(repo, "frontend", "src", "api", "search.ts"), "utf8")) },
  ];
}

// Phrasings that ASSERT NO PAGE ROW IS READ. Kept as a small list of the forms actually written
// rather than a prose classifier: a fuzzy matcher over English is a guard nobody can predict.
const CLAIMS_NO_PAGE_READ = [
  /page\s+row\s+was\s+not\s+read/i,
  /no\s+`?pages`?\s+row\s+read/i,
  /without\s+reading\s+(?:the\s+)?`?pages`?\s+row/i,
];

// The mirror image, for the branch where the join is genuinely gone.
const CLAIMS_PAGE_IS_READ = [/a\s+`?pages`?\s+row\s+IS\s+read/, /its\s+page\s+row\s+was\s+read/i];

describe("the search cost tooltip's stated reason matches the search SQL", () => {
  it("the parsers found a real query and a real tooltip — the floor, before any comparison", () => {
    const sql = semanticSearchSQL();
    // Hardcoded expectations. A parse compared against itself agrees for every value.
    expect(sql.length, "the SemanticSearch.Search SQL parse yielded a suspiciously short string").toBeGreaterThan(150);
    expect(sql, "the parsed SQL is not the vector query — the anchor has drifted").toContain("page_embeddings");
    expect(
      costUnknownTitle().length,
      "the search-cost-unknown tooltip parsed as empty — the element or its title= has moved",
    ).toBeGreaterThan(20);
    expect(spaSources().length).toBe(2);
    for (const s of spaSources()) {
      expect(s.text.length, `${s.file} read as empty — READ-NOTHING IS NOT FIND-NOTHING`).toBeGreaterThan(500);
    }
  });

  it("the semantic query does read a pages row, and the SPA does not tell the user otherwise", () => {
    const sql = semanticSearchSQL();
    const joinsPages = /JOIN\s+pages\s+p\b/i.test(sql);

    if (joinsPages) {
      const offenders: string[] = [];
      for (const s of spaSources()) {
        for (const re of CLAIMS_NO_PAGE_READ) {
          if (re.test(s.text)) offenders.push(`${s.file} matches ${re}`);
        }
      }
      expect(
        offenders,
        "SemanticSearch.Search's SQL JOINs `pages`, so a pages row IS read for every semantic hit — " +
          "and the SPA still says it is not. internal/search/handler.go already carries the measurement " +
          "that falsifies this sentence; these are the copies that were not corrected with it. One of " +
          "them is a `title` attribute, which is the only version of this explanation a customer ever sees.",
      ).toEqual([]);
    } else {
      // THE PREMISE IS GONE, SO THE WORDING IS UNSUPPORTED — that is the failure, and it is a
      // failure even though nothing rendered has become literally false yet. The tooltip, the
      // type comment and handler.go's note were all written against a query that reads a pages
      // row; inheriting them across a change to that query is how the sentence this guard exists
      // for survived in three places for four merges.
      const stillAsserted = spaSources()
        .flatMap((s) => CLAIMS_PAGE_IS_READ.filter((re) => re.test(s.text)).map((re) => `${s.file} ${re}`));
      expect.fail(
        "SemanticSearch.Search's SQL no longer JOINs `pages`. Every user-facing sentence about why a " +
          "semantic-only hit carries no cost was chosen against a query that DID read a pages row — " +
          "including the `search-cost-unknown` tooltip, `api/search.ts`'s type comment and " +
          "`internal/search/handler.go`'s note. Re-derive them against the new query rather than " +
          "inheriting them; the old text became true again the moment the join left, and nothing else " +
          "here would say so. Rendered assertions still claiming the read: " +
          (stillAsserted.length ? stillAsserted.join(", ") : "none"),
      );
    }
  });

  it("the user-visible tooltip states only what is true of the row", () => {
    const title = costUnknownTitle();
    for (const re of CLAIMS_NO_PAGE_READ) {
      expect(
        re.test(title),
        `the search-cost-unknown tooltip says "${title}", and a pages row IS read for a semantic hit. ` +
          "This is the only sentence in this finding that reaches a customer.",
      ).toBe(false);
    }
    // It must still SAY something — an explanation deleted rather than corrected leaves an
    // unlabelled em-dash, which is a worse answer than a wrong one.
    expect(title.toLowerCase(), "the tooltip no longer names the cost at all").toContain("cost");
  });
});
