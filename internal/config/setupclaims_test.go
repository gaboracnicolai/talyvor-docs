package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The documented way to run this repository is a claim about files that can be read, and
// every claim below was MEASURED FALSE before this file existed:
//
//   1. README "Quick start (2 commands)" — `cp .env.example .env && docker compose up -d`
//      exits 1 with ZERO containers created: compose declares POSTGRES_PASSWORD and
//      GATEWAY_AUTH_SECRET as `${VAR:?}` and .env.example ships both BLANK, deliberately
//      (see TestEnvExampleShipsNoUsableSecret and the compose comments — that blankness is
//      the fix for a published credential and must not be undone). Filling the two by hand
//      and re-running boots the stack healthy, so the quick start is not two commands; it
//      is two commands and two secrets the page never names.
//   2. README said migrations "are mounted into the container's init-db hook and run in
//      order on first boot". /docker-entrypoint-initdb.d inside the running postgres
//      container is EMPTY — the mount was deliberately deleted — and the docs service
//      logged `migrations applied count=18` on a fresh volume and `migrations up to date`
//      on the next boot. Every boot, by the embedded runner, not the first boot by a hook.
//   3. README's local-development block said `cp .env.example .env` before `go run
//      ./cmd/docs`. Nothing in this repo reads a .env file: with a FULLY POPULATED .env on
//      disk, `go run ./cmd/docs` exits 1 on `missing required env var: DOCS_DATABASE_URL`.
//   4. README said the Vite dev server runs on :5173. vite.config.ts pins `port: 5174`,
//      and `npm run dev` binds 5174 with nothing answering on 5173.
//
// So these are TRIPWIRES ON CROSS-FILE AGREEMENT, not prose review. Each assertion derives
// both sides from files that already exist and compares them; none of them can be satisfied
// by editing the sentence alone while the mechanism it describes moves.
//
// WHAT THEY CANNOT SEE, PINNED HERE SO NOBODY READS MORE INTO A GREEN RUN: A2 and A3 match
// TOKENS, so a reworded restatement of the same false claim escapes them (one-directional by
// design — controls C5b/C6b record exactly that). They are worth keeping because the failure
// they do catch is the one that happened: a mechanism changed and its documentation did not.

const repoRoot = "../../"

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// section returns the body of the markdown `## <title>` section, up to the next `## `.
func section(t *testing.T, doc, prefix string) string {
	t.Helper()
	lines := strings.Split(doc, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "## ") && strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(l, "## ")), prefix) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no `## %s...` section found — the guard cannot check a section that is not there", prefix)
	}
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			return strings.Join(lines[start:i], "\n")
		}
	}
	return strings.Join(lines[start:], "\n")
}

func activeLines(doc string) []string {
	var out []string
	for _, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// A1. Every variable compose makes REQUIRED (`${VAR:?}`) and .env.example leaves blank or
// absent is a value the operator must generate by hand. The quick start must name each one,
// or following it verbatim fails before a container exists.
//
// A1-FLOOR is a separate assertion and a separate failure message on purpose: this predicate
// is derived from two files, so its silent failure mode is a regex that matches nothing and
// a vacuous pass over an empty set. The floor is what notices that; it CANNOT notice a
// blinded name-check, which is what C1 is for.
func TestQuickStartNamesEverySecretTheOperatorMustSupply(t *testing.T) {
	compose := readRepoFile(t, "docker-compose.yaml")
	envExample := readRepoFile(t, ".env.example")
	readme := readRepoFile(t, "README.md")

	required := map[string]bool{}
	req := regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*):\?`)
	for _, l := range activeLines(compose) {
		for _, m := range req.FindAllStringSubmatch(l, -1) {
			required[m[1]] = true
		}
	}

	// Present-and-empty is a THIRD state, and it is the one that matters here: `NAME=` in the
	// template resolves to "" and `${NAME:?}` rejects it exactly as it rejects unset.
	supplied := map[string]bool{}
	assign := regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	for _, l := range strings.Split(envExample, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if m := assign.FindStringSubmatch(l); m != nil && strings.TrimSpace(m[2]) != "" {
			supplied[m[1]] = true
		}
	}

	var mustGenerate []string
	for name := range required {
		if !supplied[name] {
			mustGenerate = append(mustGenerate, name)
		}
	}
	sort.Strings(mustGenerate)

	// A1-FLOOR.
	if len(mustGenerate) < 2 {
		t.Fatalf("A1-FLOOR: derived only %d operator-supplied secret(s) %v from %d `${VAR:?}` "+
			"declaration(s) in docker-compose.yaml — the measured population is 2 "+
			"(POSTGRES_PASSWORD, GATEWAY_AUTH_SECRET). A set this small means the parse broke, "+
			"not that the quick start got simpler; the name check below would pass vacuously",
			len(mustGenerate), mustGenerate, len(required))
	}

	quick := section(t, readme, "Quick start")
	for _, name := range mustGenerate {
		if !strings.Contains(quick, name) {
			t.Errorf("A1: docker-compose.yaml requires %s (`${%s:?}`) and .env.example ships it "+
				"BLANK, so `cp .env.example .env && docker compose up -d` exits 1 before any "+
				"container is created — but the README quick start never names %s. Measured: "+
				"compose exits 1 with `required variable ... is missing a value` and `docker ps "+
				"-a` lists nothing. Either the quick start tells the reader to generate it, or "+
				"the reader follows the page and gets an error.",
				name, name, name)
		}
	}
}

// A2. The init-db-hook mechanism is GONE from compose (it runs only on first boot of an empty
// data directory, so it could bootstrap a new install and never upgrade an existing one).
// Documentation may only describe it while compose actually mounts it.
func TestReadmeDoesNotClaimAnInitDbHookThatComposeDoesNotMount(t *testing.T) {
	compose := readRepoFile(t, "docker-compose.yaml")
	readme := readRepoFile(t, "README.md")

	mounted := false
	for _, l := range activeLines(compose) {
		if strings.Contains(l, "docker-entrypoint-initdb.d") {
			mounted = true
			break
		}
	}
	if mounted {
		return // The mechanism is back; the prose describing it would be true.
	}

	// A pinned token list, not a phrase census: a rewording escapes it and that limit is
	// stated in this file's header rather than implied by a green run.
	for _, token := range []string{"docker-entrypoint-initdb.d", "init-db hook"} {
		if strings.Contains(readme, token) {
			t.Errorf("A2: README says %q, but no ACTIVE line in docker-compose.yaml mounts "+
				"docker-entrypoint-initdb.d — measured directly: that directory is EMPTY inside "+
				"the running postgres container. Migrations are applied by the docs service on "+
				"EVERY boot (`migrations applied count=18` on a fresh volume, `migrations up to "+
				"date` on the next), which is the opposite of the first-boot-only limitation "+
				"this sentence describes.", token)
		}
	}
}

// A3. `cp .env.example .env` is a real step for `docker compose` (compose reads .env for
// interpolation) and an inert one for `go run`, because nothing in this repo loads that file.
// Same line, two sections, one true and one false — so the check is scoped to the section.
func TestLocalDevDoesNotTellYouToCopyAnEnvFileNothingReads(t *testing.T) {
	readme := readRepoFile(t, "README.md")

	loader := false
	err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		s := string(b)
		if strings.Contains(s, "godotenv") || strings.Contains(s, `".env"`) {
			loader = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if loader {
		return // Something reads .env now; copying it would be a real step.
	}

	local := section(t, readme, "Local development")
	if strings.Contains(local, "cp .env.example .env") {
		t.Errorf("A3: the Local development block says `cp .env.example .env`, but no non-test " +
			"Go file in this repo reads a .env file. MEASURED: with a FULLY POPULATED .env on " +
			"disk, `go run ./cmd/docs` exits 1 on `missing required env var: DOCS_DATABASE_URL`; " +
			"with the same values EXPORTED it gets past config and fails at the database ping " +
			"instead. The step is inert here — it is real one section up, where compose reads it.")
	}
}

// A4. Every port the local-development instructions send a reader to must be a port
// something in this repo actually binds. Both sides are derived — comparing a literal in
// this file to itself would pass for every value, including the wrong one that shipped.
//
// Deliberately a SUBSET check rather than "the README says the Vite port": it does not
// depend on how the sentence is phrased, and it reds for any port named that nothing serves,
// which is the general shape of the defect (:5173 was named; nothing has ever answered there).
func TestLocalDevOnlyNamesPortsSomethingActuallyBinds(t *testing.T) {
	viteCfg := readRepoFile(t, "frontend/vite.config.ts")
	envExample := readRepoFile(t, ".env.example")
	readme := readRepoFile(t, "README.md")

	bound := map[string]string{}
	if m := regexp.MustCompile(`(?m)^\s*port:\s*(\d+)\s*,`).FindStringSubmatch(viteCfg); m != nil {
		bound[m[1]] = "frontend/vite.config.ts `port:`"
	} else {
		t.Fatalf("A4-FLOOR: no `port: NNNN` in frontend/vite.config.ts — half the bound set is " +
			"missing, so the subset check below would reject the true port")
	}
	if m := regexp.MustCompile(`(?m)^\s*DOCS_LISTEN_ADDR=\S*:(\d+)`).FindStringSubmatch(envExample); m != nil {
		bound[m[1]] = ".env.example DOCS_LISTEN_ADDR"
	} else {
		t.Fatalf("A4-FLOOR: no `DOCS_LISTEN_ADDR=host:port` in .env.example — part of the bound " +
			"set is missing, so the subset check below would reject a true port")
	}
	// Every `host:port` the template's own URLs use is a port this stack talks to — the
	// database among them. Added because this check red on `:5432` in a DOCS_DATABASE_URL
	// example the same commit introduced, which is the predicate being too narrow, not the
	// instructions being wrong.
	for _, m := range regexp.MustCompile(`(?m)^\s*[A-Z_]+=\S*://[^\s/]*:(\d+)`).FindAllStringSubmatch(envExample, -1) {
		if _, ok := bound[m[1]]; !ok {
			bound[m[1]] = ".env.example URL"
		}
	}

	local := section(t, readme, "Local development")
	named := regexp.MustCompile(`:(\d{4})\b`).FindAllStringSubmatch(local, -1)
	if len(named) == 0 {
		t.Fatalf("A4-FLOOR: the Local development section names no `:NNNN` port at all, so this " +
			"check has nothing to falsify")
	}
	for _, n := range named {
		if _, ok := bound[n[1]]; !ok {
			var have []string
			for p, src := range bound {
				have = append(have, p+" ("+src+")")
			}
			sort.Strings(have)
			t.Errorf("A4: README's Local development section sends the reader to :%s; nothing "+
				"this repo configures binds or dials that port. What is: %s. This shipped as "+
				"`Vite on :5173` — MEASURED: `npm run dev` prints `Local: http://localhost:5174/` "+
				"and a request to 5173 got no answer at all.",
				n[1], strings.Join(have, ", "))
		}
	}
}

// A5. A prose count decays silently while the thing it counts grows. Derive it instead.
func TestBuildStateMigrationHighWaterMatchesTheMigrationsDirectory(t *testing.T) {
	buildState := readRepoFile(t, "BUILD_STATE.md")

	files, err := filepath.Glob(filepath.Join(repoRoot, "migrations", "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("A5-FLOOR: no migrations/*.sql found — nothing to derive a high-water from")
	}
	high := ""
	num := regexp.MustCompile(`^(\d{4})_`)
	for _, f := range files {
		if m := num.FindStringSubmatch(filepath.Base(f)); m != nil && m[1] > high {
			high = m[1]
		}
	}
	if high == "" {
		t.Fatalf("A5-FLOOR: %d migration file(s) and not one NNNN_ prefix among them", len(files))
	}

	m := regexp.MustCompile(`high-water is \*\*(\d{4})\*\*`).FindStringSubmatch(buildState)
	if m == nil {
		t.Fatalf("A5-FLOOR: BUILD_STATE.md makes no `high-water is **NNNN**` claim to check "+
			"(the directory's is %s)", high)
	}
	if m[1] != high {
		t.Errorf("A5: BUILD_STATE.md says the migration high-water is **%s**; migrations/ holds "+
			"%d .sql files and the highest is %s, so a reader adding the next one would number "+
			"it wrong.", m[1], len(files), high)
	}
}
