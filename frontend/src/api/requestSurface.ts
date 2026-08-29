/**
 * WHAT THIS SPA PUTS IN A REQUEST, AND WHAT THE HANDLER ON THE OTHER END ACTUALLY READS.
 *
 * The shared half of `request-field.census.test.ts`. Two extractions — the client from the
 * TypeScript type checker, the server from the Go source — joined on verb + normalised path.
 *
 * ⚠ WHY THIS EXISTS. `route-response-type.census.test.ts` next door pins what comes BACK, and
 * `search.wire-census.test.ts` pins one wire type against its Go struct. Nothing pins what goes
 * OUT. talyvor-track proved the class by execution (W3.68, `8359a30`): its `DisallowUnknownFields`
 * turned a retired `member_id` the SPA still sent into a hard 400 and two shipped write paths were
 * dead. **Measured here before this file was written: `grep -rn DisallowUnknownFields internal/
 * cmd/` returns ZERO in this repository.** So the identical defect is SILENT — the field is
 * decoded away, the request answers 200, and a control that does nothing looks exactly like a
 * control that works. A query parameter fails the same way: the request succeeds and returns the
 * UNFILTERED result.
 *
 * ⚠⚠ THE CLIENT HALF COMES FROM THE COMPILER, NOT A REGEX, AND THAT IS MEASURED RATHER THAN
 * STYLISTIC. Two regex censuses of this question were built and thrown away in talyvor-track
 * (W3.69) because a `body: { … }` PARAMETER TYPE ANNOTATION sits BEFORE its own function's request
 * literal, so both a character-bounded and a next-literal-bounded window mis-attribute a body to
 * the wrong request. `getTypeAtLocation(body)` has no such failure mode and resolves `Partial<T>`
 * and spreads, which no scan does.
 *
 * ⚠ THE SERVER HALF IS A SOURCE PARSE AND IT WAS BLIND FIVE TIMES BEFORE IT WAS RIGHT. Each way is
 * commented AT the code that fixes it, because every one of them failed in a direction that looked
 * like agreement:
 *   1. `r.With(h.enf.Require(permission.AccessEdit)).Post(…)` — a `[^)]*` inside `With(` stops at
 *      the FIRST `)`, so 55 of 103 routes were invisible.
 *   2. `r.With(…).Method(http.MethodGet, "/p", http.HandlerFunc(h.x))` — a third registration form.
 *   3. `base := "/spaces/{spaceID}/…"` then `Post(base+"/heartbeat", …)` — the path is a Go
 *      expression, not a literal; five edit-session routes read as unrouted.
 *   4. `MountPublic` — "Mount" is not the only mount method, and the name decides the `/v1` base.
 *   5. **The receiver type. `internal/page` defines BOTH `(h *Handler) Delete` and
 *      `(s *Store) Delete`, and 53 of 103 handler lookups resolved to TWO functions without it.**
 * The join coverage floor in the census is what makes any of those a red rather than a smaller,
 * quieter, agreeing census.
 */

import { readdirSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import ts from "typescript";

export const WEB_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
export const REPO_ROOT = resolve(WEB_ROOT, "..");

// ── the client half ─────────────────────────────────────────────────────────────────────────────

export interface WebSite {
  file: string;
  line: number;
  verb: string;
  /** the path with every `${…}` span folded to `{}`; null when it cannot be rendered at all */
  path: string | null;
  /** query keys, from `qs({…})` AND from a hand-built `?k=` suffix */
  queryFields: string[];
  queryUnbounded: boolean;
  /** query keys that came from a hand-built string rather than `qs()` — named, not hidden */
  handBuiltQuery: string[];
  hasBody: boolean;
  /** the UPPER BOUND of keys this site can send; `Partial<T>` contributes every property of T */
  bodyFields: string[];
  bodyUnbounded: boolean;
  bodyRaw: string | null;
}

const PRIMITIVE =
  ts.TypeFlags.Any |
  ts.TypeFlags.Unknown |
  ts.TypeFlags.StringLike |
  ts.TypeFlags.NumberLike |
  ts.TypeFlags.BooleanLike |
  ts.TypeFlags.BigIntLike |
  ts.TypeFlags.ESSymbolLike |
  ts.TypeFlags.EnumLike |
  ts.TypeFlags.VoidLike |
  ts.TypeFlags.Null |
  ts.TypeFlags.Undefined |
  ts.TypeFlags.Never;

let webCache: WebSite[] | null = null;

export function webSites(): WebSite[] {
  if (webCache) return webCache;
  const cfgPath = ts.findConfigFile(WEB_ROOT, ts.sys.fileExists, "tsconfig.json");
  if (!cfgPath) throw new Error("frontend/tsconfig.json not found");
  const cfg = ts.parseJsonConfigFileContent(
    ts.readConfigFile(cfgPath, ts.sys.readFile).config,
    ts.sys,
    dirname(cfgPath),
  );
  const program = ts.createProgram(cfg.fileNames, cfg.options);
  const checker = program.getTypeChecker();

  const unwrap = (n: ts.Expression): ts.Expression => {
    let e = n;
    while (
      ts.isAsExpression(e) ||
      ts.isTypeAssertionExpression(e) ||
      ts.isNonNullExpression(e) ||
      ts.isParenthesizedExpression(e)
    ) {
      e = e.expression;
    }
    return e;
  };
  const propNames = (node: ts.Expression): { unbounded: boolean; names: string[] } => {
    const t = checker.getTypeAtLocation(unwrap(node));
    if (t.getFlags() & (ts.TypeFlags.Any | ts.TypeFlags.Unknown)) return { unbounded: true, names: [] };
    const names = new Set<string>();
    let unbounded = false;
    for (const p of t.isUnion() ? t.types : [t]) {
      const f = p.getFlags();
      if (f & (ts.TypeFlags.Any | ts.TypeFlags.Unknown)) { unbounded = true; continue; }
      if (f & PRIMITIVE) continue;
      if (checker.getIndexInfoOfType(p, ts.IndexKind.String)) unbounded = true;
      for (const s of checker.getPropertiesOfType(p)) names.add(s.getName());
    }
    return { unbounded, names: [...names].sort() };
  };

  let qsArg: ts.Expression | null = null;
  // resolveSpan renders ONE `${…}` span statically when it can: a path-building helper whose body
  // is a single `return <template>` (internal editsession-style `base(spaceID, pageID)`), or a
  // local holding a hand-built query suffix. Without it five call sites had no path at all.
  const resolveSpan = (e: ts.Expression, depth: number): string | null => {
    if (depth > 3) return null;
    if (ts.isCallExpression(e) && ts.isIdentifier(e.expression)) {
      const sym = checker.getSymbolAtLocation(e.expression);
      for (const d of sym?.getDeclarations() ?? []) {
        let body: ts.Block | null = null;
        if (ts.isFunctionDeclaration(d) && d.body) body = d.body;
        else if (
          ts.isVariableDeclaration(d) &&
          d.initializer &&
          (ts.isArrowFunction(d.initializer) || ts.isFunctionExpression(d.initializer))
        ) {
          const b = d.initializer.body;
          if (ts.isBlock(b)) body = b;
          else return pathOf(b, depth + 1);
        }
        const first = body?.statements[0];
        if (body && body.statements.length === 1 && first && ts.isReturnStatement(first) && first.expression)
          return pathOf(first.expression, depth + 1);
      }
      return null;
    }
    if (ts.isIdentifier(e)) {
      const sym = checker.getSymbolAtLocation(e);
      for (const d of sym?.getDeclarations() ?? []) {
        if (!ts.isVariableDeclaration(d) || !d.initializer) continue;
        const init = d.initializer;
        if (ts.isConditionalExpression(init)) {
          const w = pathOf(init.whenTrue, depth + 1);
          const f = pathOf(init.whenFalse, depth + 1);
          if (w !== null && f !== null) return w.length >= f.length ? w : f;
          return null;
        }
        const r = pathOf(init, depth + 1);
        if (r !== null) return r;
      }
    }
    return null;
  };
  const pathOf = (n: ts.Expression, depth = 0): string | null => {
    if (ts.isStringLiteral(n) || ts.isNoSubstitutionTemplateLiteral(n)) return n.text;
    if (ts.isTemplateExpression(n)) {
      let out = n.head.text;
      for (const sp of n.templateSpans) {
        const e = sp.expression;
        if (ts.isCallExpression(e) && ts.isIdentifier(e.expression) && e.expression.text === "qs" && e.arguments.length) {
          qsArg = e.arguments[0];
        } else {
          const r = resolveSpan(e, depth);
          out += r === null ? "{}" : r;
        }
        out += sp.literal.text;
      }
      return out;
    }
    if (ts.isIdentifier(n) || ts.isCallExpression(n)) {
      const r = resolveSpan(n, depth);
      if (r !== null) return r;
    }
    return null;
  };

  const out: WebSite[] = [];
  for (const sf of program.getSourceFiles()) {
    if (sf.isDeclarationFile) continue;
    const rel = sf.fileName.startsWith(WEB_ROOT) ? sf.fileName.slice(WEB_ROOT.length + 1) : "";
    if (!rel.startsWith("src")) continue;
    if (/\.test\.tsx?$/.test(rel)) continue;
    const walk = (node: ts.Node): void => {
      if (ts.isCallExpression(node) && ts.isIdentifier(node.expression) && node.expression.text === "apiRequest" && node.arguments.length >= 1) {
        qsArg = null;
        let p = pathOf(node.arguments[0]);
        let verb = "GET";
        let body: ts.Expression | null = null;
        const opts = node.arguments[1];
        if (opts && ts.isObjectLiteralExpression(opts)) {
          for (const pr of opts.properties) {
            let key: string | null = null;
            let val: ts.Expression | null = null;
            if (ts.isPropertyAssignment(pr) && pr.name) { key = pr.name.getText(); val = pr.initializer; }
            // ⚠ SHORTHAND IS A DIFFERENT NODE KIND. In talyvor-track missing it reported 10
            // body-carrying sites where there were 22 — silently, in the flattering direction.
            else if (ts.isShorthandPropertyAssignment(pr)) { key = pr.name.getText(); val = pr.name; }
            else continue;
            if (key === "method") {
              const u = unwrap(val);
              verb = ts.isStringLiteral(u) ? u.text.toUpperCase() : `?${u.getText()}`;
            } else if (key === "body") body = val;
          }
        }
        const q = qsArg ? propNames(qsArg) : { unbounded: false, names: [] as string[] };
        // a hand-built `?view_id=${…}` suffix is a query parameter too, and `qs()` never saw it
        const handBuilt: string[] = [];
        if (p && p.includes("?")) {
          const qpart = p.slice(p.indexOf("?"));
          p = p.slice(0, p.indexOf("?"));
          for (const m of qpart.matchAll(/[?&]([a-z_][a-z0-9_]*)=/g)) handBuilt.push(m[1]);
        }
        const b = body ? propNames(body) : null;
        out.push({
          file: rel,
          line: sf.getLineAndCharacterOfPosition(node.getStart()).line + 1,
          verb,
          path: p,
          queryFields: [...new Set([...q.names, ...handBuilt])].sort(),
          queryUnbounded: q.unbounded,
          handBuiltQuery: handBuilt,
          hasBody: body !== null,
          bodyFields: b ? b.names : [],
          bodyUnbounded: b ? b.unbounded : false,
          bodyRaw: body ? unwrap(body).getText().slice(0, 90).replace(/\s+/g, " ") : null,
        });
      }
      ts.forEachChild(node, walk);
    };
    ts.forEachChild(sf, walk);
  }
  out.sort((a, b) => `${a.path}${a.verb}${a.file}${a.line}`.localeCompare(`${b.path}${b.verb}${b.file}${b.line}`));
  webCache = out;
  return out;
}

// ── the server half ─────────────────────────────────────────────────────────────────────────────

export interface GoRoute {
  file: string;
  line: number;
  verb: string;
  /** null when the path expression could not be resolved — a red, never a silent skip */
  pattern: string | null;
  handler: string | null;
  recvType: string | null;
}

const PATHEXPR = String.raw`((?:"[^"]*"|[A-Za-z_]\w*)(?:\s*\+\s*(?:"[^"]*"|[A-Za-z_]\w*))*)`;
const VERB_FORM = new RegExp(String.raw`\.(Get|Post|Put|Patch|Delete|Head|Options)\(\s*${PATHEXPR}\s*,\s*(.+)\)\s*$`);
const METHOD_FORM = new RegExp(String.raw`\.Method\(\s*http\.Method([A-Za-z]+)\s*,\s*${PATHEXPR}\s*,\s*(.+)\)\s*$`);

function goFilesUnder(dir: string, out: string[] = []): string[] {
  for (const e of readdirSync(join(REPO_ROOT, dir), { withFileTypes: true })) {
    const p = join(dir, e.name);
    if (e.isDirectory()) {
      if (["node_modules", ".git", "frontend", "migrations", "docs"].includes(e.name)) continue;
      goFilesUnder(p, out);
    } else if (e.name.endsWith(".go") && !e.name.endsWith("_test.go")) out.push(p);
  }
  return out;
}

function lastHandlerName(argText: string): string | null {
  let last: string | null = null;
  for (const m of argText.matchAll(/([A-Za-z_]\w*)\.([A-Za-z_]\w*)/g)) {
    if (m[1] === "http" || m[1] === "permission") continue;
    last = m[2];
  }
  return last;
}

let routeCache: GoRoute[] | null = null;

export function goRoutes(): GoRoute[] {
  if (routeCache) return routeCache;
  const routes: GoRoute[] = [];
  for (const f of [...goFilesUnder("internal"), ...goFilesUnder("cmd")]) {
    const lines = readFileSync(join(REPO_ROOT, f), "utf8").split("\n");
    const stack: { prefix: string; depth: number }[] = [];
    const locals = new Map<string, string>();
    let depth = 0;
    let fn: { recv: string; name: string; depth: number } | null = null;
    let fnDepth = 0;
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      const fnM = /^func\s+(?:\(([^)]*)\)\s+)?([A-Za-z_]\w*)\s*\(/.exec(line);
      const routeM = /\br\.Route\(\s*"([^"]+)"\s*,\s*func\(/.exec(line);
      const before = depth;
      for (const c of line) { if (c === "{") depth++; else if (c === "}") depth--; }
      if (fnM) { fn = { recv: fnM[1] ?? "", name: fnM[2], depth: before }; fnDepth = before; locals.clear(); }
      else if (fn && depth <= fnDepth) fn = null;
      const localM = /^\s*([A-Za-z_]\w*)\s*:?=\s*"([^"]*)"\s*$/.exec(line);
      if (localM) locals.set(localM[1], localM[2]);
      if (routeM) stack.push({ prefix: routeM[1], depth: before });
      while (stack.length && depth <= stack[stack.length - 1].depth) stack.pop();
      const t = line.trim();
      if (!t.startsWith("r.")) continue;
      const m1 = VERB_FORM.exec(t);
      const m2 = METHOD_FORM.exec(t);
      let verb: string, pathExpr: string, args: string;
      if (m1) { verb = m1[1].toUpperCase(); pathExpr = m1[2]; args = m1[3]; }
      else if (m2) { verb = m2[1].toUpperCase(); pathExpr = m2[2]; args = m2[3]; }
      else continue;
      // ⚠ "Mount" IS NOT THE ONLY MOUNT METHOD (internal/sharing has MountPublic) and the name
      // decides the /v1 base. Keying on the exact name left one real route unprefixed.
      const base = fn && fn.recv && /^Mount/.test(fn.name) ? "/v1" : "";
      const recvType = fn && fn.recv ? (/\*?([A-Za-z_]\w*)\s*$/.exec(fn.recv) ?? [])[1] ?? null : null;
      let resolved: string | null = "";
      for (const part of pathExpr.split("+").map((x) => x.trim())) {
        if (part.startsWith('"')) { resolved += part.slice(1, -1); continue; }
        const v = locals.get(part);
        if (v !== undefined) { resolved += v; continue; }
        resolved = null;
        break;
      }
      const pattern = resolved === null ? null : (base + stack.map((s) => s.prefix).join("") + resolved).replace(/\/$/, "") || "/";
      routes.push({ file: f, line: i + 1, verb, pattern, handler: lastHandlerName(args), recvType });
    }
  }
  routes.sort((a, b) => `${a.pattern}${a.verb}`.localeCompare(`${b.pattern}${b.verb}`));
  routeCache = routes;
  return routes;
}

const pkgCache = new Map<string, { name: string; text: string }[]>();
function pkgFiles(dir: string) {
  const hit = pkgCache.get(dir);
  if (hit) return hit;
  const out: { name: string; text: string }[] = [];
  for (const e of readdirSync(join(REPO_ROOT, dir), { withFileTypes: true }))
    if (e.isFile() && e.name.endsWith(".go") && !e.name.endsWith("_test.go"))
      out.push({ name: e.name, text: readFileSync(join(REPO_ROOT, dir, e.name), "utf8") });
  pkgCache.set(dir, out);
  return out;
}

/** funcBodies returns every `func (recv <recvType>) name(` body in a package. */
function funcBodies(dir: string, name: string, recvType: string | null) {
  const hits: { file: string; line: number; body: string }[] = [];
  for (const f of pkgFiles(dir)) {
    const lines = f.text.split("\n");
    for (let i = 0; i < lines.length; i++) {
      const m = new RegExp(`^func\\s+\\(([^)]*)\\)\\s+${name}\\s*\\(`).exec(lines[i]);
      if (!m) continue;
      // ⚠ THE RECEIVER TYPE, AND IT IS LOAD-BEARING: internal/page has (h *Handler) Delete AND
      // (s *Store) Delete. Without this, 53 of 103 lookups matched two functions and the walk
      // order decided which one answered.
      if (recvType && (/\*?([A-Za-z_]\w*)\s*$/.exec(m[1]) ?? [])[1] !== recvType) continue;
      let depth = 0;
      let started = false;
      const body: string[] = [];
      for (let j = i; j < lines.length; j++) {
        for (const c of lines[j]) { if (c === "{") { depth++; started = true; } else if (c === "}") depth--; }
        body.push(lines[j]);
        if (started && depth === 0) break;
      }
      hits.push({ file: f.name, line: i + 1, body: body.join("\n") });
    }
  }
  return hits;
}

/** helpers whose body reads `Query().Get(<one of their own parameters>)` — daysParam-shaped. */
function fixedHelperKeys(dir: string) {
  const map = new Map<string, string[]>();
  for (const f of pkgFiles(dir)) {
    for (const m of f.text.matchAll(/func\s+([a-zA-Z_]\w*)\s*\(\s*r\s+\*http\.Request\s*\)[^{]*\{/g)) {
      const chunk = f.text.slice(f.text.indexOf(m[0]), f.text.indexOf(m[0]) + 1200);
      const keys = [...chunk.matchAll(/Query\(\)\.Get\("([a-z_][a-z0-9_]*)"\)/g)].map((x) => x[1]);
      if (keys.length) map.set(m[1], keys);
    }
  }
  return map;
}

function namedStructKeys(dir: string, typeName: string, seen = new Set<string>()): string[] | null {
  if (seen.has(typeName)) return null;
  seen.add(typeName);
  // ⚠ A QUALIFIED NAME IS ANOTHER PACKAGE. Without this hop every `var in model.Page` resolved to
  // an EMPTY key set — which reads exactly like "the handler decodes nothing" and produced 31
  // false dropped-field reports on one route.
  if (typeName.includes(".")) {
    const [pkg, name] = typeName.split(".");
    try { readdirSync(join(REPO_ROOT, "internal", pkg)); } catch { return null; }
    return namedStructKeys(join("internal", pkg), name, seen);
  }
  for (const f of pkgFiles(dir)) {
    const lines = f.text.split("\n");
    for (let i = 0; i < lines.length; i++) {
      if (!new RegExp(`^type\\s+${typeName}\\s+struct\\s*\\{`).test(lines[i])) continue;
      const out: string[] = [];
      for (let j = i + 1; j < lines.length; j++) {
        if (/^\}/.test(lines[j])) return [...new Set(out)].sort();
        const t = /`json:"([a-z_][a-z0-9_]*)/.exec(lines[j]);
        if (t) out.push(t[1]);
      }
    }
  }
  return null;
}

export interface HandlerFacts {
  verb: string;
  pattern: string;
  handler: string;
  file: string;
  line: number;
  /** query keys the handler reads, helper indirection resolved */
  queryRead: string[];
  /** true when the decode target is a map — any key decodes, nothing is dropped */
  unconstrainedBody: boolean;
  /** json keys the decode target accepts; null when the handler decodes no body at all */
  bodyAccepts: string[] | null;
  /** set when the decode lives one hop down in a shared helper (grantSpace → grant) */
  decodeVia: string | null;
  /** set when a decode exists but its target type could not be resolved — a red, not a skip */
  unresolvedTarget: string | null;
}

let factCache: Map<string, HandlerFacts> | null = null;

export function handlerFacts(): Map<string, HandlerFacts> {
  if (factCache) return factCache;
  const out = new Map<string, HandlerFacts>();
  for (const rt of goRoutes()) {
    if (!rt.pattern || !rt.handler) continue;
    const dir = dirname(rt.file);
    const hits = funcBodies(dir, rt.handler, rt.recvType);
    if (hits.length !== 1) continue;
    const body = hits[0].body;
    const helpers = fixedHelperKeys(dir);
    const q = new Set<string>();
    for (const m of body.matchAll(/Query\(\)\s*\.?Get\("([a-z_][a-z0-9_]*)"\)/g)) q.add(m[1]);
    for (const m of body.matchAll(/\bq\.Get\("([a-z_][a-z0-9_]*)"\)/g)) q.add(m[1]);
    for (const [h, keys] of helpers) if (new RegExp(`\\b${h}\\(`).test(body)) keys.forEach((k) => q.add(k));

    const read = (src: string): Omit<HandlerFacts, "verb" | "pattern" | "handler" | "file" | "line" | "queryRead" | "decodeVia"> => {
      const decodes = /Decode\(&|Unmarshal\(/.test(src);
      if (!decodes) return { unconstrainedBody: false, bodyAccepts: null, unresolvedTarget: null };
      if (/var\s+\w+\s+map\[string\]/.test(src)) return { unconstrainedBody: true, bodyAccepts: null, unresolvedTarget: null };
      const anon: string[] = [];
      const lines = src.split("\n");
      let named: string | null = null;
      for (let i = 0; i < lines.length; i++) {
        if (/^\s*var\s+\w+\s+struct\s*\{\s*$/.test(lines[i])) {
          for (let j = i + 1; j < lines.length; j++) {
            if (/^\s*\}/.test(lines[j])) break;
            const t = /`json:"([a-z_][a-z0-9_]*)/.exec(lines[j]);
            if (t) anon.push(t[1]);
          }
          return { unconstrainedBody: false, bodyAccepts: [...new Set(anon)].sort(), unresolvedTarget: null };
        }
        const nm = /^\s*var\s+\w+\s+([A-Za-z_][\w.]*)\s*$/.exec(lines[i]);
        if (nm) named = nm[1];
      }
      if (named) {
        const keys = namedStructKeys(dir, named);
        if (keys) return { unconstrainedBody: false, bodyAccepts: keys, unresolvedTarget: null };
        return { unconstrainedBody: false, bodyAccepts: null, unresolvedTarget: named };
      }
      return { unconstrainedBody: false, bodyAccepts: null, unresolvedTarget: "unknown" };
    };

    let facts = read(body);
    let via: string | null = null;
    if (facts.bodyAccepts === null && !facts.unconstrainedBody && !facts.unresolvedTarget) {
      // ⚠ ONE DELEGATE HOP. grantSpace/grantPage decode NOTHING themselves — the decode lives in a
      // shared grant(w, r, …). Without this the census reported "decodes nothing" for two handlers
      // that accept four fields, i.e. two false findings on a permissions write path.
      const d = /\b[a-z]\.([A-Za-z_]\w*)\(\s*w\s*,\s*r\b/.exec(body);
      if (d) {
        const dh = funcBodies(dir, d[1], rt.recvType);
        if (dh.length === 1) { facts = read(dh[0].body); via = d[1]; }
      }
    }
    out.set(`${rt.verb} ${rt.pattern.replace(/\{[^}]*\}/g, "{}")}`, {
      verb: rt.verb,
      pattern: rt.pattern,
      handler: rt.handler,
      file: rt.file,
      line: rt.line,
      queryRead: [...q].sort(),
      decodeVia: via,
      ...facts,
    });
  }
  factCache = out;
  return out;
}
