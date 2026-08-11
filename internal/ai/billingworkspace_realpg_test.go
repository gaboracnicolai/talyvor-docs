package ai_test

// THE BILLING WORKSPACE AND THE ATTRIBUTED PAGE ARE AUTHORIZED SEPARATELY AND NOTHING TIES THEM.
//
// `/v1/workspaces/{wsID}/ai/write` authorizes TWO objects from TWO sources:
//
//	{wsID}    from the PATH — authz.AuthorizeWorkspace, and it is what reaches Lens, so it is
//	          the workspace that PAYS for the completion.
//	page_id   from the BODY — spaceauth.AuthorizePageRead, which resolves against
//	          authz.WorkspaceIDs(ctx) — EVERY workspace the caller belongs to, not this one.
//
// #86 closed the case where those two belong to different PEOPLE (a member of A naming a page in
// B she cannot read). It did not close the case where they belong to the SAME person: a caller
// who is a verified member of A *and* B passes both gates on a request that bills A and lands the
// money on a page in B.
//
// THAT IS NOT AN ACCESS QUESTION, IT IS A LEDGER ONE, which is why no read gate can see it. Both
// gates are correctly satisfied. What is false is the row they produce:
// page_ai_spend_events carries workspace_id = A beside a page_id whose page.workspace_id = B, and
// page.Store.PriceAISpend rolls the cost onto that page with `WHERE p.id = priced.page_id` and no
// workspace predicate at all. Afterwards:
//
//	A's Lens bill  contains a completion no A page accounts for;
//	B's document   shows own_ai_cost_usd for spend B's Lens ledger never contained.
//
// Both numbers are customer-visible (PageView.tsx per document, SearchModal.tsx per row) and
// migration 0018 defines own_ai_cost_usd as "cost of AI operations performed ON this document,
// accumulated exactly-once from page_ai_spend_events" — it does not say "paid for by someone
// else's workspace", and a reconciliation against B's Lens spend cannot ever balance.
//
// spaceauth ALREADY STATES THE RULE THIS PATH BREAKS, for the sibling gate, in its own source:
// Decision.WorkspaceID is documented as "the space's OWNING workspace — the tenancy the new
// content must carry … Callers use this, never a client-supplied workspace id." The AI path takes
// the client-supplied one.
//
// WHY THE FIXTURE HAS NO PRIVATE SPACE AND NO STRANGER. The page in B is one alice CREATED, in a
// public space, in a workspace she is a full member of — she can read it, edit it and delete it.
// Every access-shaped explanation is removed on purpose, so the only thing the assertions can be
// answering is the workspace MISMATCH. A fixture where she also lacked access would be caught by
// #86's gate and would prove nothing new.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/ai"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/lenscreds"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/spaceauth"
	"github.com/talyvor/docs/internal/testutil"
)

// seedSpaceW / seedPageW are this file's own seeds. They are the same two INSERTs as
// bodypage_attribution_realpg_test.go's, deliberately duplicated rather than shared: that file's
// helpers were written to build a victim the caller cannot reach, and reusing them here would
// quietly borrow evidence gathered for a different claim. The slug suffix differs because two
// files seeding the same workspace in one package must not collide on the slug unique index.
func seedSpaceW(t *testing.T, d *testutil.DB, wsID, creator, name string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		 VALUES ($1, $2, $3, $4, false) RETURNING id`,
		wsID, name, "bw-"+name+"-"+wsID, creator,
	).Scan(&id); err != nil {
		t.Fatalf("seed space %q: %v", name, err)
	}
	return id
}

func seedPageW(t *testing.T, d *testutil.DB, wsID, spaceID, author, title string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		spaceID, wsID, title, "bw-"+title+"-"+wsID, author, "body of "+title,
	).Scan(&id); err != nil {
		t.Fatalf("seed page %q: %v", title, err)
	}
	return id
}

// TestAIAttribution_BilledWorkspaceMustOwnTheAttributedPage_RealPG drives the four body-page
// routes through the real handler, wired as cmd/docs/main.go wires it, as a caller who is a
// verified member of BOTH workspaces.
func TestAIAttribution_BilledWorkspaceMustOwnTheAttributedPage_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	wsA := d.Workspace(t)
	wsB := d.Workspace(t)

	// ONE PERSON, TWO MEMBERSHIPS. This is the ordinary shape of a contractor or of one human
	// with two orgs — it takes no privilege and no misconfiguration to arrive at.
	aliceA := d.Member(t, wsA, "alice@both.example")
	aliceB := d.Member(t, wsB, "alice@both.example")

	spaceA := seedSpaceW(t, d, wsA, aliceA, "A Handbook")
	pageA := seedPageW(t, d, wsA, spaceA, aliceA, "A Draft")

	// alice's OWN page in her OTHER workspace: public space, she created it, full access.
	spaceB := seedSpaceW(t, d, wsB, aliceB, "B Handbook")
	pageB := seedPageW(t, d, wsB, spaceB, aliceB, "B Draft")

	store := page.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)

	// cmd/docs/main.go's pageLooker, re-derived here rather than imported.
	pageLooker := func(ctx context.Context, id string) (permission.PageMeta, error) {
		pg, err := store.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, pg.SpaceID, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		return permission.PageMeta{
			WorkspaceID: pg.WorkspaceID,
			SpaceID:     pg.SpaceID, SpaceCreatedBy: sp.CreatedBy, SpacePrivate: sp.Private,
			PageCreatedBy: pg.CreatedBy,
		}, nil
	}

	// newBindingLens is reused from bodypage_attribution_realpg_test.go on purpose and the reason
	// is a property of the double, not convenience: it is the only Lens stand-in in this package
	// that sets X-Talyvor-Request-ID and gives every call a DISTINCT one. A ledger keyed on
	// request_id cannot express any of the claims below without that.
	lens := newBindingLens(t)
	engine := ai.New(lensintegration.New(lens.url, "k1").
		WithTokenProvider(lenscreds.New(lens.url, "k1", lenscreds.Options{}))).
		WithSpendBinder(store)

	// call drives one route as alice, billing whichever workspace is named in the path.
	call := func(t *testing.T, billWS, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		h := ai.NewHandler(engine, store).
			WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				c := authz.WithMemberships(req.Context(), "alice@both.example",
					[]authz.Membership{
						{WorkspaceID: wsA, MemberID: aliceA},
						{WorkspaceID: wsB, MemberID: aliceB},
					})
				next.ServeHTTP(w, req.WithContext(c))
			})
		})
		r.Route("/v1", func(r chi.Router) { h.Mount(r) })
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+billWS+path,
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// Oracles read the rows with SQL against the pool, never through a Store getter: an oracle
	// that shares a code path with its subject is not an independent oracle.
	bindings := func(t *testing.T, pageID string) int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM page_ai_spend_events WHERE page_id = $1`, pageID).Scan(&n); err != nil {
			t.Fatalf("count bindings: %v", err)
		}
		return n
	}
	// crossWorkspaceRows is the claim stated as SQL over the whole ledger: a row whose
	// workspace_id is not the workspace of the page it names. It is the assertion that survives
	// any future route, because it names no route.
	crossWorkspaceRows := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM page_ai_spend_events e
			   JOIN pages p ON p.id = e.page_id
			  WHERE p.workspace_id <> e.workspace_id`).Scan(&n); err != nil {
			t.Fatalf("count cross-workspace ledger rows: %v", err)
		}
		return n
	}
	ownCost := func(t *testing.T, pageID string) float64 {
		t.Helper()
		var v float64
		if err := d.Pool.QueryRow(ctx,
			`SELECT own_ai_cost_usd FROM pages WHERE id = $1`, pageID).Scan(&v); err != nil {
			t.Fatalf("read own_ai_cost_usd: %v", err)
		}
		return v
	}

	// ── PREMISE. Assert the fixture can tell the cases apart BEFORE asserting anything about
	// behaviour: the two pages really are in different workspaces, and the victim starts clean.
	// Without this, "no binding" could mean "the seed never landed".
	var pw string
	if err := d.Pool.QueryRow(ctx, `SELECT workspace_id FROM pages WHERE id = $1`, pageB).Scan(&pw); err != nil {
		t.Fatalf("[W-PREMISE] read pageB workspace: %v", err)
	}
	if pw != wsB || wsA == wsB {
		t.Fatalf("[W-PREMISE] fixture cannot express the mismatch: pageB.workspace_id=%s wsB=%s wsA=%s",
			pw, wsB, wsA)
	}
	if got := ownCost(t, pageB); got != 0 {
		t.Fatalf("[W-PREMISE] pageB starts at own_ai_cost_usd=%v, want 0", got)
	}
	if n := bindings(t, pageB); n != 0 {
		t.Fatalf("[W-PREMISE] pageB starts with %d ledger rows, want 0", n)
	}

	// ── 1. THE HONEST PATHS, RUN FIRST AND UNCONDITIONALLY. Same workspace in the path as the
	// page lives in, for BOTH of alice's workspaces. A fix that refuses everything, or that
	// pins one workspace, fails here.
	for _, c := range []struct {
		tag    string
		billWS string
		pageID string
	}{
		{"A", wsA, pageA},
		{"B", wsB, pageB},
	} {
		for _, rt := range aiRoutes() {
			before := bindings(t, c.pageID)
			rec := call(t, c.billWS, rt.path, rt.body(c.pageID))
			if rec.Code != http.StatusOK {
				t.Errorf("[W-HONEST/%s/%s] own workspace: POST %s = %d, want 200: %s",
					c.tag, rt.tag, rt.path, rec.Code, strings.TrimSpace(rec.Body.String()))
				continue
			}
			if got := bindings(t, c.pageID); got != before+1 {
				t.Errorf("[W-HONEST/%s/%s] own workspace: ledger rows %d → %d, want +1 — the "+
					"attribution this feature exists for stopped working", c.tag, rt.tag, before, got)
			}
		}
	}

	// ── 2. THE MISMATCH. Bill wsA, name a page in wsB. alice may read that page; the request is
	// refused because the workspace paying for the completion does not own it.
	for _, rt := range aiRoutes() {
		beforeCompletions := lens.completions()
		beforeRows := bindings(t, pageB)

		rec := call(t, wsA, rt.path, rt.body(pageB))

		if rec.Code != http.StatusNotFound {
			t.Errorf("[W-LEAK/%s] billed wsA, page in wsB: POST %s = %d, want 404 — the "+
				"completion is paid for by a workspace that does not own the page it is "+
				"attributed to: %s", rt.tag, rt.path, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		if got := lens.completions(); got != beforeCompletions {
			t.Errorf("[W-NOCALL/%s] Lens was asked for a completion (%d → %d) on a request that "+
				"must be refused — refusing after the call turns misattributed spend into "+
				"unattributed spend the workspace still pays for", rt.tag, beforeCompletions, got)
		}
		if got := bindings(t, pageB); got != beforeRows {
			t.Errorf("[W-LEDGER/%s] pageB gained a ledger row (%d → %d) from a wsA-billed request",
				rt.tag, beforeRows, got)
		}
	}

	// ── 3. THE LEDGER INVARIANT, stated over the whole table rather than over the four calls
	// above. Every row must name a page in its own workspace.
	if n := crossWorkspaceRows(t); n != 0 {
		t.Errorf("[W-INVARIANT] %d page_ai_spend_events row(s) name a page in a DIFFERENT "+
			"workspace than the one billed. PriceAISpend rolls those onto the page with no "+
			"workspace predicate, so each one is money one tenant paid appearing on another "+
			"tenant's document", n)
	}

	// ── 4. THE MONEY, LANDED. The invariant above is about a row; this is about the number a
	// customer reads. Price every binding that names pageB and check nothing arrived from wsA.
	rows, err := d.Pool.Query(ctx,
		`SELECT request_id FROM page_ai_spend_events WHERE page_id = $1 AND workspace_id = $2`,
		pageB, wsA)
	if err != nil {
		t.Fatalf("read wsA-billed bindings on pageB: %v", err)
	}
	var foreign []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan request id: %v", err)
		}
		foreign = append(foreign, id)
	}
	rows.Close()
	for i, id := range foreign {
		landed, err := store.PriceAISpend(ctx, id, 12.34, 1000)
		if err != nil {
			t.Fatalf("PriceAISpend(%s): %v", id, err)
		}
		t.Logf("PriceAISpend(%s) landed=%v (%d/%d)", id, landed, i+1, len(foreign))
	}
	if got := ownCost(t, pageB); got != 0 {
		t.Errorf("[W-MONEY] pageB.own_ai_cost_usd = %v after pricing %d wsA-billed binding(s), "+
			"want 0 — %s", got, len(foreign),
			fmt.Sprintf("workspace %s paid for this and workspace %s's document reports it", wsA, wsB))
	}
}
