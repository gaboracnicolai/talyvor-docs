// Census FINDER (not a guard): for every useQuery/useInfiniteQuery call in the SPA, which values
// reach the queryFn's request and are ABSENT from the queryKey?
//
// react-query caches on the key alone. A value that changes the REQUEST but not the KEY makes two
// different requests share one cache entry, so the second caller is served the first one's answer
// without a fetch.
//
// ⚠⚠ THE FIRST VERSION OF THIS FINDER WAS BLIND TO THE DEFECT THAT MOTIVATED IT, AND THAT IS WHY
// IT USES THE TYPE CHECKER. It looked for PROPERTY ACCESSES in the queryFn that the key did not
// carry — and `useSearch`'s queryFn is `() => searchApi.search(workspaceId, debounced, opts)`,
// which passes the whole OPTIONS OBJECT and spells no property at all. It reported 1 candidate and
// the one site it was written for was not in it. An identifier of object type is expanded to its
// FIELDS here: passing `opts` reaches the request with every field `opts` has.
//
// ⚠ IT IS A FINDER, NOT A VERDICT. It over-reports (a field a queryFn never forwards still gets
// listed) and it cannot see a value reaching the request through a closure it does not name.
// EVERY HIT MUST BE DRIVEN BEFORE IT IS BELIEVED.
//
// Usage:  node scripts/w31-querykey-census.mjs
import ts from "../frontend/node_modules/typescript/lib/typescript.js";
import { readFileSync } from "node:fs";
import { dirname } from "node:path";

const TSCONFIG = new URL("../frontend/tsconfig.json", import.meta.url).pathname;
const SRC = new URL("../frontend/src", import.meta.url).pathname;

const cfgFile = ts.readConfigFile(TSCONFIG, (p) => readFileSync(p, "utf8"));
if (cfgFile.error) throw new Error(ts.flattenDiagnosticMessageText(cfgFile.error.messageText, "\n"));
const parsed = ts.parseJsonConfigFileContent(cfgFile.config, ts.sys, dirname(TSCONFIG));
const program = ts.createProgram(parsed.fileNames, parsed.options);
const checker = program.getTypeChecker();

// `page?.id` and `page!.id` are the same value spelled two ways.
const norm = (s) => s.replace(/[?!]/g, "").replace(/\s+/g, "");

let calls = 0;
const findings = [];

for (const sf of program.getSourceFiles()) {
  if (!sf.fileName.startsWith(SRC) || /\.test\.tsx?$/.test(sf.fileName) || sf.isDeclarationFile) continue;
  const visit = (node) => {
    if (
      ts.isCallExpression(node) &&
      /^use(Infinite)?Query$/.test(node.expression.getText(sf)) &&
      node.arguments.length &&
      ts.isObjectLiteralExpression(node.arguments[0])
    ) {
      calls++;
      const obj = node.arguments[0];
      const prop = (n) => obj.properties.find((p) => p.name && p.name.getText(sf) === n);
      const keyProp = prop("queryKey");
      const fnProp = prop("queryFn");
      if (!keyProp || !fnProp) return;

      const keyNames = new Set();
      (function collect(n) {
        if (ts.isPropertyAccessExpression(n) || ts.isIdentifier(n)) keyNames.add(norm(n.getText(sf)));
        ts.forEachChild(n, collect);
      })(keyProp);

      // Every value the queryFn names, EXPANDED THROUGH ITS TYPE. An identifier whose type is an
      // object reaches the request carrying each of its fields.
      const reaches = new Set();
      (function collect(n) {
        if (ts.isPropertyAccessExpression(n)) {
          reaches.add(norm(n.getText(sf)));
          return; // do not also record its object half as a bare value
        }
        if (ts.isIdentifier(n) && !ts.isPropertyAssignment(n.parent)) {
          const text = n.getText(sf);
          const type = checker.getTypeAtLocation(n);
          // A PRIMITIVE IS ONE VALUE, NOT A BAG OF FIELDS. `string.length` is a property, and
          // expanding it turned every ordinary `workspaceID` into a hit — 28 of 32, which is a
          // finder reporting its own expansion rule rather than the product.
          const PRIM = ts.TypeFlags.StringLike | ts.TypeFlags.NumberLike |
            ts.TypeFlags.BooleanLike | ts.TypeFlags.BigIntLike | ts.TypeFlags.ESSymbolLike |
            ts.TypeFlags.Null | ts.TypeFlags.Undefined | ts.TypeFlags.Void;
          const isPrimitive = (t) => (t.isUnion?.() ? t.types : [t]).every((x) => x.flags & PRIM);
          if (isPrimitive(type)) {
            reaches.add(norm(text));
            return;
          }
          // FIELDS ONLY. `from.toFixed` / `includeResolved.valueOf` are the METHODS of a number
          // and a boolean — the first draft listed twelve of them for one call site and buried the
          // one real hit. A method is not a value that reaches the request.
          const props = (type.getProperties?.() ?? []).filter((p) => {
            const t = checker.getTypeOfSymbolAtLocation(p, n);
            return (t.getCallSignatures?.() ?? []).length === 0;
          });
          const isCallable = (type.getCallSignatures?.() ?? []).length > 0;
          if (!isCallable && props.length && props.length < 20) {
            for (const p of props) reaches.add(norm(`${text}.${p.getName()}`));
          } else if (!isCallable) {
            reaches.add(norm(text));
          }
        }
        ts.forEachChild(n, collect);
      })(fnProp);

      const missing = [...reaches]
        .filter((n) => !keyNames.has(n))
        // The api module object itself, and react refs, are never key members.
        .filter((n) => !/Api\.|\.current$/.test(n))
        .sort();
      if (missing.length) {
        const { line } = sf.getLineAndCharacterOfPosition(node.getStart(sf));
        findings.push({
          file: sf.fileName.replace(SRC, "frontend/src"),
          line: line + 1,
          key: keyProp.getText(sf).replace(/\s+/g, " "),
          fn: fnProp.getText(sf).replace(/\s+/g, " ").slice(0, 110),
          missing,
        });
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(sf);
}

console.log(`${calls} useQuery/useInfiniteQuery call sites parsed\n`);
for (const f of findings) {
  console.log(`${f.file}:${f.line}`);
  console.log(`   key      : ${f.key}`);
  console.log(`   fn       : ${f.fn}`);
  console.log(`   reaches  : ${f.missing.join(", ")}`);
  console.log("");
}
console.log(`${findings.length} of ${calls} call sites reach the request with a value the key does not carry.`);
console.log("EVERY ONE IS A CANDIDATE, NOT A FINDING. Drive it before believing it.");
