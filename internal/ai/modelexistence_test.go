package ai_test

// modelexistence_test.go — EVERY MODEL ID THIS SERVICE NAMES MUST BE A MODEL THAT EXISTS.
//
// ⚠⚠ THE FINDING THIS EXISTS FOR, MEASURED ACROSS TWO REPOSITORIES: `internal/ai/engine.go`
// declared `modelFast = "claude-haiku-4-6"`, AND THERE IS NO HAIKU 4.6 AT ANY VERSION.
// talyvor-lens carries the primary evidence — `internal/catalog/verified_models_test.go` pins a
// live `GET https://api.anthropic.com/v1/models` capture and names this exact string as the
// phantom it was written to catch ("a claude-sonnet-4-6 request was downgraded to the phantom
// 'cheapest' and 404'd"), `internal/catalog/seed.go` refuses to seed it in two places, and
// `internal/router/router.go`'s cheap tier records replacing this same literal with
// `claude-haiku-4-5`, "the real cheapest Anthropic model".
//
// ⚠ LENS FIXED LENS. THE CLIENT THAT NAMES THE MODEL DIRECTLY WAS NEVER LOOKED AT. Lens's guard
// is scoped to its own COST-ROUTING CATALOG — the models Lens itself may select. A caller that
// puts a model id in the request body does not go through that catalog: the proxy passes an
// unknown model straight upstream (it prices it on a derived fallback and alerts —
// `lens_unpriced_model_requests_total`, `internal/proxy/lxc_gate.go`, `internal/modelwatch`), so
// the id reaches Anthropic and Anthropic answers `404 not_found_error`.
//
// ⚠ WHAT THAT COST, STATED AS A COUNT RATHER THAN A WORRY: `modelFast` is the model for SEVEN of
// this service's nine AI operations — write, summarize, grammar, shorter, longer, translate,
// suggest-title. Only `Ask` (modelSmart, `claude-sonnet-4-6`, which IS in the capture) and the
// embedding path used a real model. Every one of the seven 404'd on every call in any deployment
// with a real Lens behind it.
//
// ⚠⚠ AND NOTHING COULD BE RED, WHICH IS THE PART WORTH COPYING. Every AI test in this repository
// stands up a FAKE Lens (`httptest`) that echoes whatever model it is handed, so the id was never
// asked to exist — `internal/lensintegration/client_test.go` went further and ASSERTED the
// phantom (`body["model"] != "claude-haiku-4-6"` → fail), which turned the defect into a pinned
// expectation. A fixture that accepts every string cannot notice a string that names nothing.
//
// ⚠ WHY THE CHECK IS A CENSUS OF LITERALS AND NOT AN ASSERTION ABOUT THE TWO CONSTANTS. A guard
// keyed on `modelFast`/`modelSmart` is blind to the next call site that inlines a model id — the
// same narrowing that let `formatterReach` in talyvor-suite watch its own defect happen. The walk
// below reads every non-test .go file under internal/ and cmd/ and takes every string literal
// SHAPED like a provider model id, so a new hardcoded model is covered on the day it lands.
//
// ⚠ IT DOES NOT PASS ON ITS FIRST RUN BY CONSTRUCTION — it was written against the phantom and was
// RED before the fix, which is the property `frontendmanifest_test.go` and `shortflag_test.go`
// have to note the absence of. Its discriminating power is nonetheless asserted IN the test
// (TestModelAllowListRejectsAPhantom): the checker is run against ids that must be refused, so a
// list widened until everything passes fails here rather than reporting agreement.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// lensServableModels is what talyvor-lens can actually dispatch, mirrored here because Go has no
// cross-repository import and a comment is not a check.
//
// ANTHROPIC ENTRIES ARE THE PINNED `GET /v1/models` CAPTURE from talyvor-lens
// `internal/catalog/verified_models_test.go` (captured 2026-07-19, plus claude-opus-5 on the
// evidence recorded there). NON-ANTHROPIC ENTRIES ARE talyvor-lens `internal/catalog/seed.go`'s
// seeded ids. Mirrored at lens `3f798749f6ef025547ada330b2c709a4e2359449`.
//
// ⚠ ADDING A NAME HERE IS A CLAIM THAT A PROVIDER SERVES IT. The failure this guard exists for is
// exactly a plausible-looking id nobody checked: "claude-haiku-4-6" reads as the obvious sibling
// of claude-sonnet-4-6 and claude-opus-4-6, and it does not exist. Widen this list from a
// /v1/models capture or a seeded catalog row, never from a naming pattern.
var lensServableModels = map[string]struct{}{
	// Anthropic — pinned capture (undated forms).
	"claude-opus-5":     {},
	"claude-sonnet-5":   {},
	"claude-fable-5":    {},
	"claude-opus-4-8":   {},
	"claude-opus-4-7":   {},
	"claude-opus-4-6":   {},
	"claude-sonnet-4-6": {},
	"claude-opus-4-5":   {},
	"claude-sonnet-4-5": {},
	"claude-haiku-4-5":  {},
	"claude-opus-4-1":   {},
	// Anthropic — dated snapshots served under the aliases above.
	"claude-opus-4-5-20251101":   {},
	"claude-sonnet-4-5-20250929": {},
	"claude-haiku-4-5-20251001":  {},
	"claude-opus-4-1-20250805":   {},
	// OpenAI / Google / Mistral / Groq — seeded catalog rows.
	"gpt-4o":                  {},
	"gpt-4o-mini":             {},
	"gpt-4o-2024-11-20":       {},
	"text-embedding-3-small":  {},
	"text-embedding-3-large":  {},
	"gemini-2.5-pro":          {},
	"gemini-2.5-flash":        {},
	"gemini-2.0-flash":        {},
	"gemini-1.5-pro":          {},
	"gemini-1.5-flash":        {},
	"mistral-large-latest":    {},
	"mistral-small-latest":    {},
	"mistral-nemo":            {},
	"open-mistral-7b":         {},
	"llama-3.3-70b-versatile": {},
	"llama-3.1-8b-instant":    {},
}

// modelIDShape matches a string VALUE shaped like a provider model id. It is deliberately
// SHAPE-based rather than a list of the ids we expect: the point is to find ids nobody registered.
var modelIDShape = regexp.MustCompile(`^(?:claude|gpt|gemini|mistral|llama|open-mistral|text-embedding)-[a-zA-Z0-9._:-]+$`)

// modelLiteralFloor guards this guard against a walk that finds nothing. Three model literals were
// in production Go when this was written (two in internal/ai/engine.go, one in
// internal/search/semantic.go); the floor sits below that so an ordinary removal is not a false
// red, and above zero so a mis-rooted walk — the failure mode of every source-parsing check in
// this repository — is a red rather than a silent agreement between two empty sets.
const modelLiteralFloor = 2

// allowListFloor is the same shape one level up: a list emptied by a bad merge would make every
// comparison below trivially... fail, actually — but an emptied list plus an emptied walk passes,
// and that pair is what this floor forbids.
const allowListFloor = 10

type modelSite struct {
	file  string
	model string
}

// productionModelLiterals walks the service's non-test Go and returns every model-id-shaped
// string literal with the file it was found in.
//
// ⚠ IT PARSES, IT DOES NOT GREP, AND THE FIRST DRAFT PROVED WHY. A regex over raw bytes matched
// the phantom inside the comment that RECORDS the phantom, so the guard failed on the very
// sentence explaining the fix — the same "counted its own documentation as product" failure
// `frontend/src/api/search.wire-census.test.ts` strips comments to avoid. go/parser yields only
// `*ast.BasicLit` STRING nodes, so prose is excluded by construction rather than by a second
// regex that can itself drift.
func productionModelLiterals(t *testing.T) []modelSite {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var out []modelSite
	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			// READ-NOTHING IS NOT FIND-NOTHING.
			t.Fatalf("cannot stat %s: %v — the walk read nothing, which is not the same as finding nothing", base, err)
		}
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if perr != nil {
				return perr
			}
			rel, _ := filepath.Rel(root, path)
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				v, uerr := strconv.Unquote(lit.Value)
				if uerr != nil || !modelIDShape.MatchString(v) {
					return true
				}
				out = append(out, modelSite{file: filepath.ToSlash(rel), model: v})
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", base, err)
		}
	}
	return out
}

func TestEveryModelThisServiceNamesExists(t *testing.T) {
	sites := productionModelLiterals(t)

	if len(sites) < modelLiteralFloor {
		t.Fatalf("[FLOOR] found only %d model-id literal(s) in production Go, want at least %d — the walk "+
			"is mis-rooted or the regex has stopped matching, and a census that finds nothing agrees with "+
			"every allow-list", len(sites), modelLiteralFloor)
	}
	if len(lensServableModels) < allowListFloor {
		t.Fatalf("[FLOOR] lensServableModels holds only %d id(s), want at least %d — an emptied list makes "+
			"this comparison meaningless", len(lensServableModels), allowListFloor)
	}

	for _, s := range sites {
		if _, ok := lensServableModels[s.model]; !ok {
			t.Errorf("[PHANTOM-MODEL] %s names the model %q, which is not a model talyvor-lens can dispatch.\n\n"+
				"Lens passes an unknown model id STRAIGHT UPSTREAM (it prices it on a derived fallback and "+
				"alerts), so the provider is what answers — and for an id that does not exist that answer is "+
				"404 not_found_error, on every call, silently, because every AI test in this repository "+
				"stands up a fake Lens that echoes whatever model it is given.\n\n"+
				"If the model is real, add it to lensServableModels FROM A /v1/models CAPTURE OR A SEEDED "+
				"CATALOG ROW — never from the fact that the name looks like its siblings. That is precisely "+
				"how claude-haiku-4-6 got here.", s.file, s.model)
		}
	}
}

// TestModelAllowListRejectsAPhantom is this guard's own positive control, run every time rather
// than only when someone remembers to mutate the source.
//
// The check above is a map lookup, and a map lookup passes for every input once the map contains
// every input. A list widened "to make CI green" would leave every assertion above satisfied and
// mean nothing. These three ids must NEVER be in it: the first is the phantom this file exists
// for, the second and third are the same mistake one version further on.
func TestModelAllowListRejectsAPhantom(t *testing.T) {
	for _, phantom := range []string{"claude-haiku-4-6", "claude-haiku-5", "claude-sonnet-4-7"} {
		if _, ok := lensServableModels[phantom]; ok {
			t.Errorf("[CONTROL] lensServableModels contains %q, which talyvor-lens's pinned /v1/models "+
				"capture does not. The allow-list has been widened past the evidence, and "+
				"TestEveryModelThisServiceNamesExists now passes for a model that does not exist.", phantom)
		}
	}
}

// TestTheCensusActuallyReachesTheAIEngine pins the walk to the file that carried the defect. A
// census whose regex or root drifts off internal/ai reports "no phantom models" about a directory
// it never opened — the [FLOOR] above catches an EMPTY walk, not a walk that lost one package.
func TestTheCensusActuallyReachesTheAIEngine(t *testing.T) {
	var seen bool
	for _, s := range productionModelLiterals(t) {
		if s.file == "internal/ai/engine.go" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("the census found no model literal in internal/ai/engine.go — that file declares this "+
			"service's model policy (modelFast/modelSmart), so a census that cannot see it is reporting "+
			"about the wrong tree. Sites found: %v", productionModelLiterals(t))
	}
}
