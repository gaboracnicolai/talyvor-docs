// audit.mjs — B28.456: fail the build on any known-vulnerable frontend dependency.
//
// `npm audit` has no per-advisory ignore list, so this runs it, drops the advisories below, and
// fails on anything left. Each ignore is an advisory with no fix to take; drop its line when that
// stops holding.
import { execFileSync } from "node:child_process";

const IGNORED = {
  // braces <=3.0.3 (via tailwindcss 3 > chokidar/micromatch): no patched braces release exists
  // (3.0.3 is the latest); the only way out is tailwindcss 4, a rewrite of the styling setup.
  "GHSA-vfj7-8cjw-p6xm": "braces: no patched release",
};

let out;
try {
  out = execFileSync("npm", ["audit", "--json"], { encoding: "utf8" });
} catch (err) {
  out = err.stdout; // npm audit exits 1 whenever it finds anything
}
const report = JSON.parse(out);
if (!report.vulnerabilities) {
  console.error("npm audit returned no report:", out);
  process.exit(2);
}

const found = new Map();
for (const vuln of Object.values(report.vulnerabilities)) {
  for (const via of vuln.via) {
    if (typeof via !== "object") continue; // a string names the dependency that carries it
    const ghsa = via.url.split("/").pop();
    if (!(ghsa in IGNORED)) found.set(ghsa, `${via.severity} ${via.name} ${via.range}: ${via.title}`);
  }
}

for (const [ghsa, line] of found) console.error(`${ghsa} ${line}`);
console.log(`${found.size} unignored advisories (${Object.keys(IGNORED).length} ignored with a reason)`);
process.exit(found.size ? 1 : 0);
