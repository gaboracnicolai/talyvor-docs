import { describe, expect, it } from "vitest";
import { readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

// THE CENSUS: ONE ROUTE + METHOD MUST HAVE ONE DECLARED RESPONSE TYPE ACROSS THE WHOLE CLIENT.
//
// `search.wire-census.test.ts` next door compares ONE wire type against its Go struct. It is
// blind to the failure this file exists for, which is upstream of any single type: the SAME
// route reached through TWO client functions that disagree about what comes back.
//
//   GET /v1/spaces/{}/pages/{}/comments returned the 14-field comment shape
//   (thread_id, parent_id, author_name, resolved_at, replies — the thread tree),
//   `commentsApi.list` declared it,
//   and `pagesApi.listComments` declared a SECOND, 9-field `Comment` from api/types.ts
//   that had none of those five fields.
//
// Both compiled. Both were exported. Neither was wrong about a field it named. A set-equality
// census over declarations cannot see it, because BOTH declarations are internally consistent
// with something — the narrow one mirrored a Go struct (`model.Comment`) that no handler had
// written for the life of the column. The defect is only visible when the two are put beside
// the ROUTE they both call.
//
// ⚠ WHY IT IS A DEFECT AND NOT AN ALIAS: TypeScript would REFUSE `c.replies` and `c.author_name`
// through the narrow type. So the trap does not surface as a wrong value on a screen — it
// surfaces as the compiler telling the next developer that the server does not thread comments,
// while the server threads them. `createComment(body: Partial<Comment>)` likewise could not
// express `parent_id`, so a reply could not be created through it at all.
//
// ⚠ AND THE TYPE NAME IS NOT THE IDENTITY — THIS IS THE FIRST WAY THIS INSTRUMENT LIED TO ME.
// Both declarations are literally named `Comment`. Grouping by the name written at the call site
// scored the tree CLEAN over the exact defect. Every type is resolved to the MODULE it is
// imported from before anything is compared; `resolveTypeOrigin` is the load-bearing part, and
// C3 in the harness is the control that says so.

const here = path.dirname(fileURLToPath(import.meta.url));

// ⚠ THE FLOOR IS NOT DECORATION. Both sides of this census are derived by parsing source, so its
// failure mode is a parser that quietly matches less and reports agreement over a question it
// never asked. That is not hypothetical here: the first draft bounded each call site with a fixed
// 240-character window, which CONSUMED the next call site, and it saw 45 of 91. It printed
// "0 collisions" while blind to half the client — including the two sites that are the defect.
//
// So the parse is held to hardcoded numbers, never to values read back from the same parse:
//   - every `apiRequest<` occurrence in the directory is accounted for, as either a parsed call
//     site or a NAMED entry in UNPARSED_ALLOWLIST below;
//   - the parsed count has a floor.
// A parser that stops finding calls fails HERE rather than agreeing about nothing.
const MIN_APIREQUEST_OCCURRENCES = 88;
const MIN_PARSED_CALL_SITES = 84;

// The four `apiRequest<` occurrences that carry no inline template-literal path, each named with
// its reason. An allow-list, not a tolerance: a fifth one appearing is a parser blind spot that
// has to be looked at, so it reds here rather than shrinking the census silently.
const UNPARSED_ALLOWLIST: ReadonlyArray<{ file: string; why: string }> = [
  { file: "client.ts", why: "the declaration of apiRequest itself, not a call" },
  { file: "editsession.ts", why: "path built by the module-local base() helper (GET)" },
  { file: "editsession.ts", why: "path built by the module-local base() helper (POST)" },
  { file: "editsession.ts", why: "path built by the module-local base() helper (DELETE)" },
];

type Call = {
  method: string;
  route: string;
  type: string; // "<declaring module>#<type as written>"
  file: string;
};

/** Strip line comments. The blocks above these interfaces discuss field and type NAMES in prose. */
function stripLineComments(src: string): string {
  return src.replace(/\/\/[^\n]*/g, "");
}

/**
 * Replace every `${…}` interpolation with `{}`, counting braces so a nested object literal
 * (`${qs({ include_resolved: … })}`) collapses to ONE placeholder rather than terminating at its
 * first inner `}`. This was the SECOND way this instrument lied: with a non-counting regex the
 * two comment call sites normalised to different routes and never met.
 */
function normaliseRoute(template: string): string {
  let out = "";
  for (let i = 0; i < template.length; ) {
    if (template.startsWith("${", i)) {
      let depth = 1;
      let j = i + 2;
      for (; j < template.length && depth > 0; j++) {
        if (template[j] === "{") depth++;
        else if (template[j] === "}") depth--;
      }
      out += "{}";
      i = j;
    } else {
      out += template[i];
      i++;
    }
  }
  // A trailing querystring placeholder is not part of the route's identity.
  return out.replace(/\{\}$/, "");
}

/** The source of the call expression, paren-balanced from `(` to its match. */
function callBody(src: string, openParen: number): string {
  let depth = 0;
  for (let i = openParen; i < src.length; i++) {
    if (src[i] === "(") depth++;
    else if (src[i] === ")") {
      depth--;
      if (depth === 0) return src.slice(openParen, i + 1);
    }
  }
  return src.slice(openParen);
}

/**
 * name -> the module file that declares it. A locally declared interface wins, otherwise the
 * `import … from "./x"` that introduced the name. Unresolved names are marked, never guessed:
 * an inline object type has no module and must not be silently pooled with another module's.
 */
function resolveTypeOrigin(src: string, file: string): Map<string, string> {
  const m = new Map<string, string>();
  for (const im of src.matchAll(/import\s+type\s*\{([^}]*)\}\s*from\s*"\.\/([\w.-]+)"/g)) {
    for (const n of im[1].split(",")) {
      const name = n.trim();
      if (name) m.set(name, `${im[2]}.ts`);
    }
  }
  for (const d of src.matchAll(/(?:export )?interface (\w+)/g)) m.set(d[1], file);
  return m;
}

function scan(): { calls: Call[]; occurrences: number; unparsed: { file: string }[] } {
  const files = readdirSync(here)
    .filter((f) => f.endsWith(".ts") && !f.endsWith(".test.ts"))
    .sort();
  const calls: Call[] = [];
  const unparsed: { file: string }[] = [];
  let occurrences = 0;

  for (const file of files) {
    const src = stripLineComments(readFileSync(path.join(here, file), "utf8"));
    occurrences += [...src.matchAll(/apiRequest</g)].length;
    const origins = resolveTypeOrigin(src, file);

    for (const m of src.matchAll(/apiRequest<([^>]+)>\(/g)) {
      const body = callBody(src, m.index! + m[0].length - 1);
      const pathLit = body.match(/`([^`]+)`/);
      if (!pathLit) {
        unparsed.push({ file });
        continue;
      }
      // `method:` is read from THIS call expression only. Read from a fixed window instead and
      // a list GET inherits the POST of the create function beneath it — six spurious
      // collisions, which is what the first draft reported.
      const method = body.match(/method:\s*"(\w+)"/)?.[1] ?? "GET";
      const written = m[1].trim();
      const base = written.replace(/\[\]$/, "");
      calls.push({
        method,
        route: normaliseRoute(pathLit[1]),
        type: `${origins.get(base) ?? "(inline)"}#${written}`,
        file,
      });
    }
  }
  return { calls, occurrences, unparsed };
}

describe("one route + method, one declared response type", () => {
  it("the parse accounts for every apiRequest< in the directory — the floor, before any comparison", () => {
    const { calls, occurrences, unparsed } = scan();
    expect(occurrences, "apiRequest< occurrences — the parser is finding less than it did").toBeGreaterThanOrEqual(
      MIN_APIREQUEST_OCCURRENCES,
    );
    expect(calls.length, "parsed call sites — the parser is finding less than it did").toBeGreaterThanOrEqual(
      MIN_PARSED_CALL_SITES,
    );
    expect(
      calls.length + unparsed.length,
      "every apiRequest< must be either parsed or on the named allow-list",
    ).toBe(occurrences);
    expect(
      unparsed.length,
      `unparsed apiRequest< sites: ${unparsed.map((u) => u.file).join(", ")} — expected exactly the ${
        UNPARSED_ALLOWLIST.length
      } named in UNPARSED_ALLOWLIST`,
    ).toBe(UNPARSED_ALLOWLIST.length);
    expect(
      unparsed.map((u) => u.file).sort(),
      "the unparsed sites are not the ones the allow-list names",
    ).toEqual(UNPARSED_ALLOWLIST.map((u) => u.file).sort());
  });

  it("resolves type identity by declaring module, not by the name at the call site", () => {
    // ⚠ THIS ONE IS A FIXTURE, NOT A REPO READ, AND THE REASON MATTERS. Once the duplicate is
    // deleted NO interface name in this directory is declared in two modules — so a repo-derived
    // assertion here would have nothing to discriminate and would pass for a resolver that
    // returned the bare name. The fixture reproduces the collision the defect actually had.
    const imported = `import type { Comment } from "./types";`;
    const declared = `export interface Comment { id: string; }`;
    expect(resolveTypeOrigin(imported, "pages.ts").get("Comment")).toBe("types.ts");
    expect(resolveTypeOrigin(declared, "comments.ts").get("Comment")).toBe("comments.ts");
    // A locally declared interface must win over an import of the same name, or a module that
    // both imports and redeclares is filed under the wrong owner.
    expect(resolveTypeOrigin(`${imported}\n${declared}`, "comments.ts").get("Comment")).toBe("comments.ts");
    // And the live client still resolves through the path this file will actually take.
    const own = stripLineComments(readFileSync(path.join(here, "comments.ts"), "utf8"));
    expect(resolveTypeOrigin(own, "comments.ts").get("Comment")).toBe("comments.ts");
  });

  it("no route is reached through two client functions that disagree about the response", () => {
    const { calls } = scan();
    const byRoute = new Map<string, Call[]>();
    for (const c of calls) {
      const key = `${c.method} ${c.route}`;
      byRoute.set(key, [...(byRoute.get(key) ?? []), c]);
    }
    const conflicts = [...byRoute.entries()]
      .filter(([, cs]) => new Set(cs.map((c) => c.type)).size > 1)
      .map(
        ([key, cs]) =>
          `${key}\n${cs.map((c) => `      ${c.type}  (declared at the call in ${c.file})`).join("\n")}`,
      );
    expect(conflicts, `routes with more than one declared response type:\n${conflicts.join("\n")}`).toEqual([]);
  });
});
