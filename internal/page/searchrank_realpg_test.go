package page_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// SearchWithRank AGAINST A REAL POSTGRES — THE ONLY INSTRUMENT THAT CAN SEE THIS DEFECT.
//
// Every existing SearchWithRank test (store_test.go:300/354/373) drives pgxmock, which matches
// the query TEXT against a regex and hands back hand-listed rows. It never sends the SQL to a
// parser, so a query that Postgres refuses to compile passes all three of them. Measured at
// `34ab2d5` on pgvector/pgvector:pg16, the query this function builds does not compile:
//
//	ERROR: syntax error at or near "'document'"   (SQLSTATE 42601)
//
// and with only that token corrected, the next statement Postgres reaches is:
//
//	ERROR: column reference "created_at" is ambiguous   (SQLSTATE 42702)
//
// Both come from prefixedColumns, which alias-qualified the `columns` const by splitting it on
// the two-character string ", ". `columns` is a multi-line raw string whose separator is ",\n    ",
// so the first column on every continuation line was never qualified — and the qualifier landed
// inside `COALESCE(page_type, 'document')`, producing `p.'document'`.
//
// CONSEQUENCE, MEASURED RATHER THAN REASONED: GET /v1/workspaces/{wsID}/search returned
// HTTP 500 {"error":"search failed"} for a workspace containing a matching page. See the
// sibling guard in internal/search for the HTTP half.
//
// ⚠ THIS TEST EXISTS TO EXECUTE THE SQL. It asserts the row comes back with its own columns
// attached, so a regression that re-breaks aliasing reds here rather than in production. Do not
// replace it with a pgxmock test: a mock cannot fail the way this defect failed.
func TestSearchWithRank_RealPG_CompilesAndReturnsTheRow(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "alice@example.com")
	pageID := d.Page(t, ws, author, "Deployment Runbook")

	if _, err := d.Pool.Exec(ctx,
		`UPDATE pages SET content_text = 'the deployment runbook for the auth flow' WHERE id = $1`,
		pageID); err != nil {
		t.Fatalf("seed content_text: %v", err)
	}

	s := page.NewStore(d.Pool)
	out, err := s.SearchWithRank(ctx, ws, "runbook", nil, 10, 0)
	if err != nil {
		t.Fatalf("SearchWithRank against a real Postgres: %v\n"+
			"The ranked-search SQL does not compile. Every consumer of this projection "+
			"(GET /workspaces/{wsID}/search, the MCP search_docs tool, and the MCP ask tool's "+
			"context lookup) is dead while this errors.", err)
	}
	if len(out) != 1 {
		t.Fatalf("want exactly 1 ranked result for a workspace with one matching page, got %d", len(out))
	}

	got := out[0]
	if got.Page.ID != pageID {
		t.Fatalf("row id = %q, want %q", got.Page.ID, pageID)
	}
	// The JOIN's own column, proving the join half of the projection survived aliasing.
	if got.SpaceName == "" {
		t.Fatal("space_name is empty — the JOIN column did not reach the projection")
	}
	// A pages column that sits FIRST on a continuation line of the `columns` const, i.e. exactly
	// the position the old splitter failed to qualify. `spaces` has a `created_at` too, so an
	// unqualified reference here is what Postgres called ambiguous.
	if got.Page.CreatedAt.IsZero() {
		t.Fatal("created_at is zero — a pages column did not reach the projection")
	}
	// The COALESCE expression that carried the misplaced qualifier.
	if got.Page.PageType == "" {
		t.Fatalf("page_type is empty, want the COALESCE default")
	}
	if got.Rank <= 0 {
		t.Fatalf("rank = %v, want > 0 for a page whose text matches the query", got.Rank)
	}
	if got.Headline == "" {
		t.Fatal("headline is empty — ts_headline did not reach the projection")
	}
}

// EVERY COLUMN, NOT JUST THE ONES A HAND-PICKED ASSERTION NAMES.
//
// The defect above was a per-column property: whether a given column got its table qualifier.
// Naming three columns catches the three named ones. This asserts the WHOLE projection round-trips
// by comparing SearchWithRank's page against Get's page for the same row — Get selects the same
// `columns` list with no JOIN and no alias, so it is an independent reader of the same schema.
// A column that silently drops out of the aliased form (NULL, zero, or the wrong table's value)
// diverges here without anyone having to have predicted which column it would be.
func TestSearchWithRank_RealPG_ProjectionMatchesTheUnaliasedReader(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "alice@example.com")
	pageID := d.Page(t, ws, author, "Deployment Runbook")

	// Give every column a non-default value we could notice losing.
	if _, err := d.Pool.Exec(ctx,
		`UPDATE pages SET content_text = 'the deployment runbook for the auth flow',
		     content = '{"v":1}', icon = '🚀', cover_url = 'https://example.test/c.png',
		     position = 3, depth = 1, ai_cost_usd = 1.5, own_ai_cost_usd = 12.34,
		     view_count = 7, doc_status = 'published', page_type = 'changelog',
		     stale_after_days = 30, verified_by = 'bob@example.com'
		 WHERE id = $1`, pageID); err != nil {
		t.Fatalf("seed page columns: %v", err)
	}

	s := page.NewStore(d.Pool)
	direct, err := s.GetByID(ctx, pageID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	out, err := s.SearchWithRank(ctx, ws, "runbook", nil, 10, 0)
	if err != nil {
		t.Fatalf("SearchWithRank: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 ranked result, got %d", len(out))
	}
	if diff := diffPages(t, direct, &out[0].Page); diff != "" {
		t.Fatalf("the aliased ranked-search projection disagrees with the unaliased reader:\n%s", diff)
	}
}

// diffPages compares two pages field by field through their JSON encoding, so it enumerates
// EVERY field of model.Page rather than the ones a human thought to name. Returns "" when equal.
//
// ⚠ WHAT IT CANNOT SEE, stated rather than implied: fields tagged `omitempty` that are nil on one
// side and empty on the other encode identically. `scan` normalises LinkedIssues from nil to
// []string{} and `scanPlus` does not, so that one pre-existing divergence is invisible here. It is
// invisible to a client too, for the same reason.
func diffPages(t *testing.T, a, b *model.Page) string {
	t.Helper()
	toMap := func(p *model.Page) map[string]any {
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal page: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal page: %v", err)
		}
		return m
	}
	am, bm := toMap(a), toMap(b)
	keys := map[string]struct{}{}
	for k := range am {
		keys[k] = struct{}{}
	}
	for k := range bm {
		keys[k] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)

	var out []string
	for _, k := range ordered {
		av, aok := am[k]
		bv, bok := bm[k]
		if aok != bok || fmt.Sprintf("%v", av) != fmt.Sprintf("%v", bv) {
			out = append(out, fmt.Sprintf("  %-18s unaliased=%v  ranked-search=%v", k, av, bv))
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n")
}
