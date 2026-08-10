package ai_test

// FINDING (22) — THE BODY-DERIVED-OBJECT CLASS, AND THE ONE SITE THAT DID NOT CLOSE IT.
//
// #82/#83/#84/#85 closed four copies of one class: a route gated on {P} that also carries a CHILD
// id in the URL, whose handler acts on the child without naming {P}. `internal/routeguard` now
// polices exactly that population over the AST. Its documented boundary is the last param: a route
// whose enforcer param is the final one carries no child id and is skipped. FINDING (21) asked
// what lives outside that boundary — an object a handler takes from the request BODY, which no
// path-shaped guard can see at all.
//
// THE CENSUS OF THAT CLASS IN THIS REPO IS THREE SITES, and it is the shape of the other two that
// makes this one a finding rather than an oversight:
//
//	internal/templatelib  FromPage       body page_id   GATED — AuthorizePageRead, 404/403 fail-closed
//	internal/customdomain Create         body space_id  GATED — the store re-reads the space and
//	                                                    checks its workspace + private flag
//	internal/ai           Write/Transform/Translate/SuggestTitle
//	                                     body page_id   NOT GATED
//
// Both closed sites carry a comment naming the class in the same words — templatelib's says
// "page_id is in the body, so SpaceResolverFromParam can't gate it"; customdomain's store says
// "space_id arrives in the request BODY. The handler authorizes the {wsID} PATH param, but …".
// The primitive both reach for is `spaceauth.Authorizer.AuthorizePageRead`, whose own doc comment
// describes it as reading "a body-named PAGE". THE QUESTION WAS ASKED AND ANSWERED TWICE AND NEVER
// ASKED OF THIS HANDLER — which already HOLDS that gate (`aiHandler.WithPageRead(…)` in
// cmd/docs/main.go, used by Ask to filter its grounding set) and never calls it on the page_id it
// is handed.
//
// ⚠⚠ AND THIS ONE MOVES MONEY ACROSS A TENANT BOUNDARY. `page_id` is the attribution key:
// Engine.run binds it to Lens's request id, and page.Store.PriceAISpend later does
// `UPDATE pages SET own_ai_cost_usd = own_ai_cost_usd + … WHERE p.id = priced.page_id` — by bare
// id, with no workspace predicate anywhere on that path. MEASURED THROUGH THE REAL ROUTE ON REAL
// POSTGRES BEFORE ANY CHANGE (probe at 1581757): alice, a verified member of workspace A and of
// NOTHING ELSE, posts `/v1/workspaces/{A}/ai/write` naming a page that lives in workspace B →
//
//	200 {"text":"ok"}
//	page_ai_spend_events  page_id=<B's page>  workspace_id=<A>  operation=docs-ai-write
//	PriceAISpend(…, 12.34) → landed=true
//	B's page  own_ai_cost_usd = 12.34
//
// `own_ai_cost_usd` is customer-visible — PageView.tsx renders it per document and SearchModal.tsx
// per row — so a workspace that has never met the caller sees a cost appear on one of its
// documents. The workspace_id column on the ledger row is A's, so A's own pricing sweep
// (UnpricedRequestIDs is scoped by workspace) is what carries the money into B.
//
// THE FIX IS THE PRIMITIVE THE HANDLER ALREADY HAS, in templatelib's exact shape: a body-named
// page must be one the caller may VIEW. Unresolvable or foreign → 404 (no existence oracle);
// resolvable but under View → 403. It runs BEFORE the completion, so an unauthorized attribution
// costs no Lens call at all rather than becoming unattributed spend.
//
// AN EMPTY page_id IS STILL ALLOWED AND STILL BINDS NOTHING — Engine.run documents two operations
// that always pass it empty (ask, search) and SuggestTitle passes it empty before the first save.
// [A-EMPTY] is that assertion, so a fix that simply requires the field fails here.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// bindingLens is this file's own Lens double, not privatespace_realpg_test.go's recordingLens.
// That one records PROMPTS and never sets X-Talyvor-Request-ID; the binding this file is about
// exists only when that header does, so borrowing it would borrow a helper that cannot express
// the claim. Each call gets a distinct request id, because the ledger's primary key is that id
// and a shared one would make the second binding a silent ON CONFLICT DO NOTHING.
type bindingLens struct {
	mu   sync.Mutex
	n    int
	last string
	url  string
}

func newBindingLens(t *testing.T) *bindingLens {
	t.Helper()
	bl := &bindingLens{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/token" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"token":"tok","expires_at":%q}`,
				time.Now().Add(time.Hour).Format(time.RFC3339))))
			return
		}
		bl.mu.Lock()
		bl.n++
		bl.last = fmt.Sprintf("lensreq-%d", bl.n)
		id := bl.last
		bl.mu.Unlock()
		w.Header().Set("X-Talyvor-Request-ID", id)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	t.Cleanup(s.Close)
	bl.url = s.URL
	return bl
}

// completions is how many times Lens was actually asked for a completion. The refusal has to
// happen BEFORE the call, so this is the assertion that an unauthorized attribution costs
// nothing rather than becoming unattributed spend.
func (bl *bindingLens) completions() int {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	return bl.n
}

func seedSpaceB(t *testing.T, d *testutil.DB, wsID, creator, name string, private bool) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		wsID, name, "sp-"+name+"-"+wsID, creator, private,
	).Scan(&id); err != nil {
		t.Fatalf("seed space %q: %v", name, err)
	}
	return id
}

func seedPageB(t *testing.T, d *testutil.DB, wsID, spaceID, author, title string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		spaceID, wsID, title, "pg-"+title+"-"+wsID, author, "body of "+title,
	).Scan(&id); err != nil {
		t.Fatalf("seed page %q: %v", title, err)
	}
	return id
}

// aiRoute is one of the four single-page operations. All four take page_id from the body and all
// four reach Engine.run, so a fix applied to one of them and not the others must fail here.
type aiRoute struct {
	tag  string
	path string
	body func(pageID string) string
}

func aiRoutes() []aiRoute {
	return []aiRoute{
		{"write", "/ai/write", func(p string) string {
			return fmt.Sprintf(`{"prompt":"draft me a paragraph","context":"","page_id":%q}`, p)
		}},
		{"transform", "/ai/transform", func(p string) string {
			return fmt.Sprintf(`{"action":"summarize","text":"hello","page_id":%q}`, p)
		}},
		{"translate", "/ai/translate", func(p string) string {
			return fmt.Sprintf(`{"text":"hello","language":"fr","page_id":%q}`, p)
		}},
		{"suggest-title", "/ai/suggest-title", func(p string) string {
			return fmt.Sprintf(`{"content":"body text","page_id":%q}`, p)
		}},
	}
}

// TestAIAttribution_BodyPageID_MustBeReadableByTheCaller_RealPG drives the four real routes
// through the real handler wired as cmd/docs/main.go wires it.
func TestAIAttribution_BodyPageID_MustBeReadableByTheCaller_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	wsA := d.Workspace(t)
	wsB := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@a.example")
	carol := d.Member(t, wsA, "carol@a.example") // creator of the private space in wsA
	bob := d.Member(t, wsB, "bob@b.example")

	// THE VICTIM IN ANOTHER WORKSPACE. alice is a member of wsA and of nothing else.
	foreignSpace := seedSpaceB(t, d, wsB, bob, "Bs Handbook", false)
	foreignPage := seedPageB(t, d, wsB, foreignSpace, bob, "Bs Roadmap")

	// THE VICTIM IN alice's OWN WORKSPACE, in a private space she has no grant on. Same
	// mechanism, and the ring the other four findings were about.
	privSpace := seedSpaceB(t, d, wsA, carol, "Board Private", true)
	privPage := seedPageB(t, d, wsA, privSpace, carol, "Severance Model")

	// alice's OWN page. The honest path, which must keep working.
	ownSpace := seedSpaceB(t, d, wsA, alice, "Alice Space", false)
	ownPage := seedPageB(t, d, wsA, ownSpace, alice, "Alice Draft")

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

	lens := newBindingLens(t)
	engine := ai.New(lensintegration.New(lens.url, "k1").
		WithTokenProvider(lenscreds.New(lens.url, "k1", lenscreds.Options{}))).
		WithSpendBinder(store)

	// call drives one route as alice, exactly as the editor does.
	call := func(t *testing.T, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		h := ai.NewHandler(engine, store).
			WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				c := authz.WithMemberships(req.Context(), "alice@a.example",
					[]authz.Membership{{WorkspaceID: wsA, MemberID: alice}})
				next.ServeHTTP(w, req.WithContext(c))
			})
		})
		r.Route("/v1", func(r chi.Router) { h.Mount(r) })
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+wsA+path,
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// bindingsFor counts ledger rows naming a page, and priced() is what the money question
	// actually turns on. Both read the row with SQL against the pool, never through a Store
	// getter: an oracle that shares a code path with its subject is not an independent oracle.
	bindingsFor := func(t *testing.T, pageID string) int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM page_ai_spend_events WHERE page_id = $1`, pageID).Scan(&n); err != nil {
			t.Fatalf("count bindings: %v", err)
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

	// ── PREMISE. The fixture must be able to tell the cases apart: alice really is outside
	// wsB and really has no grant on the private space, and the victims start at zero. Asserted
	// FIRST, so "no binding" can never mean "the seed never landed".
	if got := ownCost(t, foreignPage); got != 0 {
		t.Fatalf("[A-PREMISE] foreign page starts at own_ai_cost_usd=%v, want 0", got)
	}
	if got := ownCost(t, privPage); got != 0 {
		t.Fatalf("[A-PREMISE] private page starts at own_ai_cost_usd=%v, want 0", got)
	}
	if n := bindingsFor(t, foreignPage) + bindingsFor(t, privPage); n != 0 {
		t.Fatalf("[A-PREMISE] victims start with %d ledger rows, want 0", n)
	}

	// ── 1. THE HONEST PATH, RUN FIRST AND UNCONDITIONALLY. alice's own page must still bind,
	// on every one of the four routes. A fix that refuses everything passes every leak
	// assertion below and fails here.
	for _, rt := range aiRoutes() {
		before := bindingsFor(t, ownPage)
		rec := call(t, rt.path, rt.body(ownPage))
		if rec.Code != http.StatusOK {
			t.Errorf("[A-HONEST/%s] alice's OWN page: POST %s = %d, want 200: %s",
				rt.tag, rt.path, rec.Code, strings.TrimSpace(rec.Body.String()))
			continue
		}
		if got := bindingsFor(t, ownPage); got != before+1 {
			t.Errorf("[A-HONEST/%s] alice's OWN page: ledger rows %d → %d, want +1 — the "+
				"attribution this feature exists for stopped working", rt.tag, before, got)
		}
	}

	// ── 2. THE LEAK, CROSS-WORKSPACE. alice is not a member of wsB by any route.
	for _, rt := range aiRoutes() {
		callsBefore := lens.completions()
		rec := call(t, rt.path, rt.body(foreignPage))
		if rec.Code != http.StatusNotFound {
			t.Errorf("[A-LEAK-XWS/%s] alice is a member of NO workspace but %s, and named a page "+
				"in another one: POST %s = %d, want 404. A page she cannot resolve must not be "+
				"an attribution target.", rt.tag, "wsA", rt.path, rec.Code)
		}
		if n := lens.completions(); n != callsBefore {
			t.Errorf("[A-LEAK-XWS/%s] the refusal happened AFTER the Lens call (%d → %d) — an "+
				"unauthorized attribution must cost no completion at all",
				rt.tag, callsBefore, n)
		}
	}
	if n := bindingsFor(t, foreignPage); n != 0 {
		t.Errorf("[A-LEAK-XWS-LEDGER] %d ledger rows name a page in workspace B, want 0. "+
			"page_ai_spend_events.page_id is the key PriceAISpend rolls money by, and it has no "+
			"workspace predicate.", n)
	}

	// ── 3. THE MONEY. Price every binding the way the sweep does and read the victim's
	// customer-visible number. This is the assertion the ledger-row count cannot make: a row
	// could exist and never be priced, and a row could be absent while some other path moved
	// the money.
	ids, err := store.UnpricedRequestIDs(ctx, wsA, 100)
	if err != nil {
		t.Fatalf("unpriced ids: %v", err)
	}
	for _, id := range ids {
		if _, err := store.PriceAISpend(ctx, id, 12.34, 1000); err != nil {
			t.Fatalf("price %s: %v", id, err)
		}
	}
	if got := ownCost(t, foreignPage); got != 0 {
		t.Errorf("[A-LEAK-XWS-MONEY] a page in workspace B now reports own_ai_cost_usd=%v after "+
			"alice's spend in workspace A. PageView.tsx renders this number to workspace B.", got)
	}
	if got := ownCost(t, ownPage); got == 0 {
		t.Errorf("[A-HONEST-MONEY] alice's own page reports own_ai_cost_usd=0 — the honest " +
			"attribution never reached the document, so this suite would pass for the wrong reason")
	}

	// ── 4. THE LEAK, SAME WORKSPACE, PRIVATE SPACE, NO GRANT. Run and priced separately so it
	// cannot be answered by case 2's evidence.
	for _, rt := range aiRoutes() {
		rec := call(t, rt.path, rt.body(privPage))
		if rec.Code != http.StatusForbidden {
			t.Errorf("[A-LEAK-PRIV/%s] alice has NO grant on carol's PRIVATE space: POST %s = %d, "+
				"want 403 — she resolved the page (same workspace) but may not view it",
				rt.tag, rt.path, rec.Code)
		}
	}
	if n := bindingsFor(t, privPage); n != 0 {
		t.Errorf("[A-LEAK-PRIV-LEDGER] %d ledger rows name a private page alice cannot open, want 0", n)
	}
	ids, err = store.UnpricedRequestIDs(ctx, wsA, 100)
	if err != nil {
		t.Fatalf("unpriced ids (2): %v", err)
	}
	for _, id := range ids {
		if _, err := store.PriceAISpend(ctx, id, 7.77, 500); err != nil {
			t.Fatalf("price %s: %v", id, err)
		}
	}
	if got := ownCost(t, privPage); got != 0 {
		t.Errorf("[A-LEAK-PRIV-MONEY] a private page alice cannot open reports own_ai_cost_usd=%v", got)
	}

	// ── 5. NOT OVER-CORRECTED. An explicit view grant on the private space makes it a legal
	// attribution target. Granted LAST so cases 1–4 ran while alice was still denied. This is
	// the assertion a blanket "same creator only" or "public spaces only" fix fails.
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO permissions (resource_type, resource_id, subject_type, subject_id, access, workspace_id)
		 VALUES ('space', $1, 'member', $2, 'view', $3)`,
		privSpace, alice, wsA); err != nil {
		t.Fatalf("grant: %v", err)
	}
	before := bindingsFor(t, privPage)
	rec := call(t, "/ai/write", aiRoutes()[0].body(privPage))
	if rec.Code != http.StatusOK {
		t.Errorf("[A-GRANT] OVER-CORRECTION: alice now holds an explicit 'view' grant on the "+
			"private space and POST /ai/write = %d, want 200: %s",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if got := bindingsFor(t, privPage); got != before+1 {
		t.Errorf("[A-GRANT] OVER-CORRECTION: with a view grant the ledger rows went %d → %d, "+
			"want +1 — the gate is not consulting resolveAccess", before, got)
	}

	// ── 6. AN EMPTY page_id IS STILL LEGAL AND STILL BINDS NOTHING. Engine.run documents this
	// state explicitly; a fix that makes the field required fails here.
	var totalBefore int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM page_ai_spend_events`).Scan(&totalBefore); err != nil {
		t.Fatalf("count all bindings: %v", err)
	}
	rec = call(t, "/ai/suggest-title", `{"content":"a brand new unsaved page","page_id":""}`)
	if rec.Code != http.StatusOK {
		t.Errorf("[A-EMPTY] an empty page_id (a title suggested before the first save) = %d, "+
			"want 200: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var totalAfter int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM page_ai_spend_events`).Scan(&totalAfter); err != nil {
		t.Fatalf("count all bindings (2): %v", err)
	}
	if totalAfter != totalBefore {
		t.Errorf("[A-EMPTY] an empty page_id recorded %d new ledger rows, want 0",
			totalAfter-totalBefore)
	}
}

// TestAIAttribution_UnwiredGate_RefusesRatherThanBindingBlind_RealPG is the nil-gate direction.
//
// cmd/docs/main.go always wires WithPageRead, so this is a report rather than the only thing
// between an unwired deployment and the leak — the same posture Ask takes (see WithPageRead's
// comment). It matters because the gate is optional at the type level: a handler constructed
// without it would otherwise bind every page_id it is handed, which is the defect this file
// closes, restored by omission rather than by an edit.
func TestAIAttribution_UnwiredGate_RefusesRatherThanBindingBlind_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@a.example")
	pageID := d.Page(t, ws, alice, "Alice Draft")

	lens := newBindingLens(t)
	store := page.NewStore(d.Pool)
	engine := ai.New(lensintegration.New(lens.url, "k1").
		WithTokenProvider(lenscreds.New(lens.url, "k1", lenscreds.Options{}))).
		WithSpendBinder(store)

	h := ai.NewHandler(engine, store) // NO WithPageRead
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			c := authz.WithMemberships(req.Context(), "alice@a.example",
				[]authz.Membership{{WorkspaceID: ws, MemberID: alice}})
			next.ServeHTTP(w, req.WithContext(c))
		})
	})
	r.Route("/v1", func(r chi.Router) { h.Mount(r) })

	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/ai/write",
		strings.NewReader(fmt.Sprintf(`{"prompt":"draft","context":"","page_id":%q}`, pageID)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("[A-UNWIRED] a handler with no page-read gate answered a page_id-carrying "+
			"request with %d, want 500 — an unwired gate must be reported, not answered around",
			rec.Code)
	}
	var n int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM page_ai_spend_events WHERE page_id = $1`, pageID).Scan(&n); err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	if n != 0 {
		t.Errorf("[A-UNWIRED-LEDGER] %d ledger rows were written with no gate wired, want 0", n)
	}
}

// aiRoutesJSONShapeIsTheHandlersShape guards the fixture rather than the product: every body
// above must decode into the struct its handler declares, or a leak assertion could pass because
// the request was malformed rather than because it was refused. json.Decode into a struct with
// an unknown field does not error, so a renamed field would silently send page_id:"" — which
// every "want 404" assertion would accept.
func TestAIAttribution_FixtureBodiesCarryTheirPageID(t *testing.T) {
	for _, rt := range aiRoutes() {
		var probe struct {
			PageID string `json:"page_id"`
		}
		body := rt.body("PG-SENTINEL")
		if err := json.Unmarshal([]byte(body), &probe); err != nil {
			t.Fatalf("%s: fixture body is not valid JSON: %v", rt.tag, err)
		}
		if probe.PageID != "PG-SENTINEL" {
			t.Errorf("%s: fixture body does not carry page_id (%q) — every leak assertion for "+
				"this route would pass on a request that never named a page", rt.tag, probe.PageID)
		}
	}
}
