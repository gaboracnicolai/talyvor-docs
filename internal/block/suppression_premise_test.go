package block

// THE PREMISE OF A SUPPRESSION IS NOT SELF-ENFORCING.
//
// internal/block/handler.go carries this tree's ONLY exemption for the class guard rule
// docs-no-body-supplied-authority. Its reason is not "this is fine" — it is a CONDITIONAL:
//
//	"model.Block carries NO authority field: it is {ID, PageID, Type, Content, Position,
//	 ParentID, CreatedAt, UpdatedAt} ... IF model.Block EVER GAINS a workspace_id/created_by/*_by
//	 FIELD, DELETE THIS SUPPRESSION — the finding becomes real that day."
//
// Nothing re-checked that condition. MEASURED on 67baa87 rather than reasoned about: adding
// `WorkspaceID string` to model.Block — the exact trigger the comment names — left the WHOLE
// gauntlet green. gofmt clean, `go vet` 0, `go test -race -count=1 ./...` on real Postgres 36
// packages / 0 FAIL, `semgrep --config .semgrep/ --error` 0 findings, check-semgrep-rule-scope.py
// ok. The suppression is not inert — deleting it DOES produce the finding at handler.go:80, with
// and without the added field — so the line is load-bearing and silences a rule that would
// otherwise speak. The instruction to delete it was addressed to a human reading a diff.
//
// The same premise is spelled a SECOND time, at internal/block/store.go's assertInWorkspaces
// ("a block carries no workspace_id, so it reaches its tenant through its page"). Both copies
// rest on this file.
//
// WHAT THIS GUARD DOES NOT COVER, said plainly: its vocabulary is the rule's own
// metavariable-regex, so a tenancy field named outside that set (TenantID, OwnerID) matches
// neither the rule nor this guard. That is deliberate — one vocabulary, defined in one place,
// inherited here. Widening it is an edit to .semgrep/body-supplied-authority.yml, which this
// guard then picks up with no change of its own.

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/model"
)

// suppressionFile is the ONE file allowed to carry the exemption, relative to the repo root.
const suppressionFile = "internal/block/handler.go"

// authorityRuleID and marker are kept APART in this file's source on purpose, and every string
// below is assembled rather than written out. The census reads .go source text and this file is
// inside the tree it walks: spelling the marker and the id adjacently anywhere here would make
// this guard count ITSELF and report a population that does not exist. (It did, on the first
// run — that is how this note came to be written.) Do not "simplify" these into one literal.
const (
	authorityRuleID = "docs-no-body-supplied-authority"
	marker          = "nosemgrep: "
)

// exactSuppression matches the rule id and NOT its `-field` sibling. RE2 has no lookahead, so
// the boundary is spelled as "next char is neither `-` nor a word char, or end of line".
// Both directions are pinned by TestSuppressionCensus_TheReaderCanTellTheTwoRuleIDsApart.
var exactSuppression = regexp.MustCompile(`(?m)` + regexp.QuoteMeta(marker+authorityRuleID) + `([^-\w]|$)`)

func repoRootOrFail(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd)) // internal/block -> repo root
}

// authorityVocabulary reads the authority field names out of the CLASS GUARD ITSELF
// (.semgrep/body-supplied-authority.yml, rule B's $FIELD metavariable-regex) so this guard and
// the rule cannot drift apart. It FAILS rather than returns empty on anything unexpected: a
// vocabulary that silently read nothing would make every assertion below vacuously true.
func authorityVocabulary(t *testing.T, root string) []string {
	t.Helper()
	path := filepath.Join(root, ".semgrep", "body-supplied-authority.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read the rule file this guard derives its vocabulary from: %v", err)
	}
	re := regexp.MustCompile(`(?s)id: docs-no-body-supplied-authority-field.*?metavariable: \$FIELD\s*\n\s*regex: \^\(([^)]*)\)\$`)
	m := re.FindSubmatch(b)
	if m == nil {
		t.Fatalf("could not extract $FIELD metavariable-regex from %s.\n"+
			"This guard derives its vocabulary from the rule and has just read NOTHING, which "+
			"would make it pass on any input. Fix the extraction or the rule, not this message.", path)
	}
	var names []string
	for _, n := range strings.Split(string(m[1]), "|") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if len(names) < 5 {
		t.Fatalf("authority vocabulary is implausibly small (%d: %v) — extraction is probably broken",
			len(names), names)
	}
	return names
}

// snake folds a Go field name to the wire/db spelling: WorkspaceID -> workspace_id,
// ResolvedBy -> resolved_by, IsAdmin -> is_admin.
var snakeBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

func snake(s string) string {
	return strings.ToLower(snakeBoundary.ReplaceAllString(s, "${1}_${2}"))
}

// tagValue returns the bare name from a struct tag (drops ",omitempty" etc).
func tagValue(tag reflect.StructTag, key string) string {
	v, ok := tag.Lookup(key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(strings.Split(v, ",")[0])
}

// TestSuppressionPremise_ModelBlockCarriesNoAuthorityField is the guard the suppression's
// conditional needed and did not have.
func TestSuppressionPremise_ModelBlockCarriesNoAuthorityField(t *testing.T) {
	root := repoRootOrFail(t)
	vocab := authorityVocabulary(t, root)

	byName := map[string]bool{}
	byWire := map[string]bool{}
	for _, n := range vocab {
		byName[n] = true
		byWire[snake(n)] = true
	}

	// LIVENESS, before the subject is examined. The rule's own message names model.Page as a
	// type that carries this class of field ("model.Page, PageLink, ChangelogEntry, model.Space
	// all do"), so the vocabulary MUST match something on it. If the extraction above ever
	// degrades into a set that matches nothing, this reddens here rather than reporting a clean
	// model.Block that was never actually checked.
	pageT := reflect.TypeOf(model.Page{})
	var live []string
	for i := 0; i < pageT.NumField(); i++ {
		f := pageT.Field(i)
		if byName[f.Name] || byWire[tagValue(f.Tag, "json")] || byWire[tagValue(f.Tag, "db")] {
			live = append(live, f.Name)
		}
	}
	if len(live) == 0 {
		t.Fatalf("the authority vocabulary %v matched NO field of model.Page, which the class "+
			"guard's own message says carries workspace_id/created_by. The instrument is dead; "+
			"a green result below would mean nothing.", vocab)
	}
	t.Logf("vocabulary live: %d names, matching model.Page fields %v", len(vocab), live)

	blockT := reflect.TypeOf(model.Block{})
	if blockT.NumField() == 0 {
		t.Fatal("model.Block has no fields — this guard read nothing")
	}
	for i := 0; i < blockT.NumField(); i++ {
		f := blockT.Field(i)
		json, db := tagValue(f.Tag, "json"), tagValue(f.Tag, "db")
		if !byName[f.Name] && !byWire[json] && !byWire[db] {
			continue
		}
		t.Errorf(`model.Block gained the authority field %s (json:%q db:%q).

THE CONDITION NAMED BY %s's nosemgrep HAS BEEN MET, and the suppression
on the store call in Create is still there, so `+"`semgrep --config .semgrep/ --error`"+` is SILENT
about a request body that now names its own tenant/author. Measured: with this field present and
the suppression kept, gofmt, go vet, the real-Postgres suite and the product scan are ALL green.

Do all three, not one:
  1. DELETE the nosemgrep line in %s (the rule then covers the call site — verified: it fires
     at handler.go:80 the moment the line goes).
  2. Assign the value in the handler from the resource the route already authorized
     (permission.WorkspaceFromContext / permission.ActorFromContext), never from the decoded body.
  3. Update the premise comment on internal/block/store.go's assertInWorkspaces, which says a
     block carries no workspace_id and reaches its tenant through its page.`,
			f.Name, json, db, suppressionFile, suppressionFile)
	}
}

// TestSuppressionCensus_StillExactlyOneAndStillWhereThePremiseIsPinned keeps the guard above
// pointed at something real. If the exemption moves, or a SECOND one appears on a different
// type, the premise test above is silently covering only the first — so that must fail here and
// be pinned deliberately, the same way DECLARED_UNFIXTURED names the unfixtured rules.
func TestSuppressionCensus_StillExactlyOneAndStillWhereThePremiseIsPinned(t *testing.T) {
	root := repoRootOrFail(t)

	var goFiles, anyNosemgrep int
	found := map[string]int{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		goFiles++
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "nosemgrep:") {
			anyNosemgrep++
		}
		if n := len(exactSuppression.FindAll(b, -1)); n > 0 {
			rel, _ := filepath.Rel(root, path)
			found[filepath.ToSlash(rel)] = n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// The reader must be proven able to SEE a suppression before "exactly one" means anything.
	if goFiles == 0 || anyNosemgrep == 0 {
		t.Fatalf("read %d .go files, %d containing any nosemgrep — this census read nothing",
			goFiles, anyNosemgrep)
	}

	want := map[string]int{suppressionFile: 1}
	if !reflect.DeepEqual(found, want) {
		t.Errorf(`docs-no-body-supplied-authority suppressions moved: got %v, want %v.

Each one is a CONDITIONAL premise about the type its handler decodes, and
TestSuppressionPremise_ModelBlockCarriesNoAuthorityField only covers model.Block. A new
suppression needs its own premise pinned here; a removed one means the rule now covers that call
site on its own and this census should say so.`, found, want)
	}
}

// TestSuppressionCensus_TheReaderCanTellTheTwoRuleIDsApart pins the boundary in
// exactSuppression. docs-no-body-supplied-authority is a strict PREFIX of
// docs-no-body-supplied-authority-field, so a plain substring match would count the sibling's
// exemptions as this rule's and the census above would be measuring the wrong population.
func TestSuppressionCensus_TheReaderCanTellTheTwoRuleIDsApart(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"\t// " + marker + authorityRuleID + " -- reason goes here", true},
		{"\t// " + marker + authorityRuleID, true},
		{"\t// " + marker + authorityRuleID + "-field -- reason goes here", false},
		{"\t// " + marker + authorityRuleID + "-field", false},
		{"\t// " + marker + "docs-by-id-write-requires-workspace-scope -- reason", false},
	} {
		if got := exactSuppression.MatchString(tc.line); got != tc.want {
			t.Errorf("exactSuppression.MatchString(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}
