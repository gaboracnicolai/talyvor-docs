package page_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelock"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// THE SECOND DOOR ONTO THE SAME MONEY COLUMN — and the first one being shut is what makes
// this worth its own guard rather than a line in the other one.
//
// `c3daaf7` removed ai_cost_usd from Update's allowlist, and sec_aicost_body_test.go pins
// that a PATCH cannot set it. That guard is GREEN with this door wide open. Handler.Create
// does `var in model.Page; json.NewDecoder(r.Body).Decode(&in)` — the WHOLE struct from the
// body — and then overrides exactly three fields (SpaceID from the path, WorkspaceID and
// CreatedBy from the verified identity, all three added by SEC-4). AICostUSD is not one of
// them, and Store.Create passed p.AICostUSD straight into the INSERT.
//
// MEASURED at eeb1a39 on real Postgres through gatewayauth + authz + the space enforcer:
//
//	POST /v1/spaces/{sp}/pages
//	{"title":"forged","content":"{}","ai_cost_usd":999.99,"own_ai_cost_usd":555.55,"view_count":4242}
//	-> HTTP 201; row ai_cost_usd=999.99, own_ai_cost_usd=0, view_count=0;
//	   response "ai_cost_usd":999.99,"total_ai_cost_usd":999.99
//
// The two refused fields are the control: own_ai_cost_usd and view_count are decoded from the
// body into the same struct and are simply not in the INSERT column list, so they defaulted.
// Exactly one forged value landed, and it was the money one.
//
// ⚠ THE FIX IS IN THE STORE, NOT THE HANDLER, AND THE SCHEMA IS WHY. migrations/0002 declares
// `ai_cost_usd FLOAT NOT NULL DEFAULT 0`, and migrations/0018's COMMENT ON COLUMN says of it:
// "Do not add to this column: the next sweep overwrites it." The column is owned by
// trackintegration.Syncer. A whole-tree census found SIX callers of Store.Create — importer
// (x3), templatelib (x2), mcp — and ZERO of them set AICostUSD, so dropping it from the INSERT
// closes every door at once instead of one route's. Same altitude as the allowlist fix.
//
// ⚠ NOT A NEW CLASS — THE SAME CLASS, COUNTED PROPERLY. This is the lesson the queue calls
// "enumerate every copy of the seam", arriving one merge later on my own work: a guard at door
// one reads as closure of the column.
func TestSec_Create_AICostIsNotSettableFromTheRequestBody(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	seed := d.Page(t, ws, alice, "seed")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, seed).Scan(&spaceID); err != nil {
		t.Fatalf("lookup space: %v", err)
	}

	permStore := permission.NewStore(d.Pool)
	lockStore := pagelock.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool).WithGuard(lockStore)
	pageLooker := func(ctx context.Context, id string) (permission.PageMeta, error) {
		pg, err := pageStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, pg.SpaceID, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		return permission.PageMeta{
			WorkspaceID: pg.WorkspaceID, SpaceID: pg.SpaceID, SpaceCreatedBy: sp.CreatedBy,
			SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
		}, nil
	}
	spaceResolver := permission.SpaceResolverFromParam("spaceID", func(ctx context.Context, id string) (permission.SpaceMeta, error) {
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.SpaceMeta{}, err
		}
		return permission.SpaceMeta{WorkspaceID: sp.WorkspaceID, CreatedBy: sp.CreatedBy, Private: sp.Private}, nil
	})
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	spaceEnf := permission.NewEnforcer(permStore, spaceResolver)
	pageHandler := page.NewHandler(pageStore, d.Pool)
	pageHandler.WithAccess(pageEnf, spaceEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(testGatewaySecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		pageHandler.Mount(r)
	})

	// One request carrying the forged money field AND ordinary creatable fields, because the
	// fix must ignore one value rather than refuse or empty the whole create.
	body := `{"title":"a forged page","content":"{}","stale_after_days":45,"page_type":"changelog",` +
		`"ai_cost_usd":999.99,"own_ai_cost_usd":555.55,"view_count":4242}`
	req := httptest.NewRequest(http.MethodPost, "/v1/spaces/"+spaceID+"/pages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Auth", testGatewaySecret)
	req.Header.Set("X-User-Email", "alice@corp.com")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	// [HONEST-201] — the floor. "The cost is zero" is satisfied by a 403, a 400 and a route
	// that was never mounted; without this, everything below could be earned by a refusal.
	if rr.Code != http.StatusCreated {
		t.Fatalf("[HONEST-201] POST create = %d, want 201 — the assertions below cannot tell a "+
			"working gate from a refused request. body=%s", rr.Code, rr.Body.String())
	}

	var out struct {
		ID             string  `json:"id"`
		Title          string  `json:"title"`
		PageType       string  `json:"page_type"`
		StaleAfterDays int     `json:"stale_after_days"`
		AICostUSD      float64 `json:"ai_cost_usd"`
		OwnAICostUSD   float64 `json:"own_ai_cost_usd"`
		TotalAICostUSD float64 `json:"total_ai_cost_usd"`
		ViewCount      int     `json:"view_count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("[HONEST-201] decode create response: %v — body=%s", err, rr.Body.String())
	}

	var track, own float64
	var views int
	if err := d.Pool.QueryRow(ctx,
		`SELECT ai_cost_usd, own_ai_cost_usd, view_count FROM pages WHERE id = $1`,
		out.ID).Scan(&track, &own, &views); err != nil {
		t.Fatalf("[HONEST-201] read the created row: %v", err)
	}

	// [NOT-SETTABLE] — the defect. The column is owned by trackintegration.Syncer; a page is
	// born having cost nothing.
	if track != 0 {
		t.Errorf("[NOT-SETTABLE] the created page's ai_cost_usd is %v, want 0 — a caller set a "+
			"sweep-owned money column through the create body. migrations/0018 says of this "+
			"column: \"Do not add to this column: the next sweep overwrites it.\"", track)
	}
	// ⚠ THE DERIVED total_ai_cost_usd IS DELIBERATELY NOT ASSERTED UNDER THIS TAG, AND IT WAS
	// HERE UNTIL A CONTROL SAID OTHERWISE. `total_ai_cost_usd` is ai_cost_usd + own_ai_cost_usd,
	// so control E2 — which inserts the SIBLING column from the body — moved the total and fired
	// [NOT-SETTABLE] alongside [SIBLING-PARITY], justifying neither alone. Both halves are pinned
	// at 0 here, which determines the total; asserting it as well bought nothing and cost the
	// campaign an isolating control.
	if out.AICostUSD != 0 {
		t.Errorf("[NOT-SETTABLE] the create response reported ai_cost_usd=%v, want 0 — this field "+
			"feeds the total_ai_cost_usd that PageView, SearchModal and the MCP page projection render.",
			out.AICostUSD)
	}

	// [SIBLING-PARITY] — the other two body-supplied server-owned fields in the same request.
	// They are already refused, by not being in the INSERT at all; this is the behaviour
	// ai_cost_usd is being brought into line with, so it is pinned rather than assumed.
	if own != 0 || out.OwnAICostUSD != 0 {
		t.Errorf("[SIBLING-PARITY] own_ai_cost_usd is %v (response %v), want 0 — it is incremented "+
			"per Lens ledger row by internal/lensintegration and is not settable.", own, out.OwnAICostUSD)
	}
	if views != 0 || out.ViewCount != 0 {
		t.Errorf("[SIBLING-PARITY] view_count is %v (response %v), want 0 — a page is born unread.",
			views, out.ViewCount)
	}

	// [CREATE-STILL-WORKS] — scope, not breakage. The ordinary creatable fields in the SAME
	// body must land; a fix that emptied the create would satisfy everything above.
	if out.Title != "a forged page" {
		t.Errorf("[CREATE-STILL-WORKS] title = %q, want %q", out.Title, "a forged page")
	}
	if out.StaleAfterDays != 45 {
		t.Errorf("[CREATE-STILL-WORKS] stale_after_days = %d, want 45", out.StaleAfterDays)
	}
	if out.PageType != "changelog" {
		t.Errorf("[CREATE-STILL-WORKS] page_type = %q, want %q", out.PageType, "changelog")
	}
}
