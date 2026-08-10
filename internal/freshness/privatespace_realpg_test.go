package freshness_test

// THE FOURTH COPY OF THE SEAM, AND THE ONLY ONE WITH TWO ENTRY POINTS SHARING ONE METHOD.
//
// `internal/search`, `page.Handler`'s /pages/search + /pages/stale (#78) and `internal/ai`'s
// /ask (#79) each authorized the WORKSPACE and stopped. So does this one — and the leaking call
// is `page.Store.GetStalePages`, the SAME store method #78 gated:
//
//	FreshnessEngine.GetStaleReport(ctx, wsID)
//	    e.pages.GetStalePages(ctx, wsID)          // WHERE workspace_id = $1, and nothing else
//	    → FreshnessReport{PageID, SpaceID, Title, Reason, …}
//
// ⚠⚠ TWO CALLERS, NEITHER OF WHICH ASKS THE PERMISSION ENGINE ANYTHING, AND A SWEEP BY ROUTE
// FINDS ONLY ONE OF THEM:
//
//	internal/freshness/handler.go:52  GET /v1/workspaces/{wsID}/freshness   (AuthorizeWorkspace, stop)
//	internal/mcp/server.go:877        the MCP `get_stale_pages` tool         (workspace tier, stop)
//
// The REST route is what the SPA's stale screen and the sidebar's stale COUNT actually read —
// `pagesApi.stale` has no frontend caller at all, so #78's fix to /pages/stale did not touch the
// screen a person sees. The MCP tool hands the same rows to an agent.
//
// The payload is title + space_id + page_id, so it is smaller than #78's whole-document leak — but
// `/spaces/{space_id}/pages/{page_id}` is a working deep link, and a page TITLE is frequently the
// finding ("Q3 layoff plan"). It is also a positive claim about a document's existence.
//
// ⚠⚠ AND THE FIX HAD TO SPLIT TWO KINDS OF READER APART RATHER THAN FILTER THE METHOD.
// `SendStaleDigest` — started by `freshEngine.Start(ctx, …)` in main.go — calls the SAME method on
// a background context that has no caller and never will. Filtering per-caller there would report
// ZERO stale pages to an operator every day at 09:00 forever. So the unfiltered list survives as
// an UNEXPORTED `staleReportAll` and the exported `GetStaleReport` is gated: the compiler, not a
// comment, is what stops the next caller picking the wrong one. [D-DIGEST] is the assertion that
// the system view still sees everything, and it is the reason this is not a blanket filter.
//
// THE CASES ARE ONE TEST ON PURPOSE, across BOTH surfaces, so a fix that drops private spaces or
// empties the report fails here rather than passing quietly:
//
//	1. bob, no grant             the private page MUST NOT appear   ← the defect
//	2. bob, no grant             the PUBLIC page MUST appear        ← the positive control in-sample
//	3. alice, the space creator  the private page MUST appear       ← owner-is-admin
//	4. bob, explicit view grant  the private page MUST appear       ← not `private = false`
//	5. the daily digest          MUST still count the private page  ← the system view is untouched

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/freshness"
	"github.com/talyvor/docs/internal/mcp"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelink"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/spaceauth"
	"github.com/talyvor/docs/internal/testutil"
)

func seedSpaceF(t *testing.T, d *testutil.DB, wsID, creator, name string, private bool) string {
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

// seedStalePageF writes a page that the stale predicate will actually select: a positive TTL, an
// updated_at well past it, and no verification. Backdating is the only way to reach the predicate,
// so a fixture that forgets it produces an empty report — and an empty report satisfies every
// absence assertion in this file perfectly.
func seedStalePageF(t *testing.T, d *testutil.DB, wsID, spaceID, author, title string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text, content,
		                    stale_after_days, last_verified_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, 30, NULL, NOW() - INTERVAL '400 days') RETURNING id`,
		spaceID, wsID, title, "pg-"+title+"-"+wsID, author, "body of "+title, `{"type":"doc"}`,
	).Scan(&id); err != nil {
		t.Fatalf("seed page %q: %v", title, err)
	}
	return id
}

func TestFreshness_PrivateSpace_NotReportedWithoutGrant_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	bob := d.Member(t, ws, "bob@example.com")

	pubSpace := seedSpaceF(t, d, ws, alice, "Public Handbook", false)
	const pubTitle = "Onboarding Handbook"
	pubPage := seedStalePageF(t, d, ws, pubSpace, alice, pubTitle)

	privSpace := seedSpaceF(t, d, ws, alice, "Board Private", true)
	const privTitle = "Q3 Layoff Plan"
	privPage := seedStalePageF(t, d, ws, privSpace, alice, privTitle)

	store := page.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)

	// cmd/docs/main.go's pageLooker, re-derived here rather than imported: it is the metadata the
	// permission engine decides on, and borrowing another test's helper borrows its evidence.
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

	engine := freshness.New(store, pagelink.NewStore(d.Pool), nil).
		WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))

	// ── SURFACE 1: the REST route, through the real handler as main.go mounts it.
	rest := func(email, memberID string) []freshness.FreshnessReport {
		t.Helper()
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				c := authz.WithMemberships(req.Context(), email,
					[]authz.Membership{{WorkspaceID: ws, MemberID: memberID}})
				next.ServeHTTP(w, req.WithContext(c))
			})
		})
		r.Route("/v1", func(r chi.Router) { freshness.NewHandler(engine).Mount(r) })

		req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/freshness", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /freshness as %s = %d, want 200: %s", email, rr.Code, rr.Body.String())
		}
		var out []freshness.FreshnessReport
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode as %s: %v (%s)", email, err, rr.Body.String())
		}
		return out
	}

	// ── SURFACE 2: the MCP get_stale_pages tool, through the real Server.HandleRPC.
	//
	// Memberships are stamped on the request context directly, which is exactly what
	// authz.Middleware does and all this package's sibling tool tests do (internal/mcp's own
	// callTool drives HandleRPC with no transport). The claim under test is what the TOOL emits
	// for a verified caller, not how the caller was verified — internal/mcp/sec4_mcp_test.go
	// already owns the workspace tier.
	mcpTool := func(email, memberID string) string {
		t.Helper()
		srv := mcp.New(store, spaceStore, nil, nil, engine, "test")
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{
				"name":      "get_stale_pages",
				"arguments": map[string]any{"workspace_id": ws},
			},
		})
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(authz.WithMemberships(req.Context(), email,
			[]authz.Membership{{WorkspaceID: ws, MemberID: memberID}}))
		rr := httptest.NewRecorder()
		srv.HandleRPC(rr, req)
		return rr.Body.String()
	}

	hasID := func(rs []freshness.FreshnessReport, id string) bool {
		for _, r := range rs {
			if r.PageID == id {
				return true
			}
		}
		return false
	}
	titles := func(rs []freshness.FreshnessReport) []string {
		out := make([]string, 0, len(rs))
		for _, r := range rs {
			out = append(out, r.Title)
		}
		return out
	}

	// ── 1 + 2. bob, no grant, on BOTH surfaces.
	//
	// THE POSITIVE CONTROL COMES FIRST AND IS A HARD PREMISE ONLY FOR THE PUBLIC PAGE'S PRESENCE
	// ON THE REST SURFACE. Without it "the private page is absent" is satisfied by a report that
	// is empty for any reason at all — a fixture that never became stale, a route that served
	// nothing.
	bobRest := rest("bob@example.com", bob)
	if !hasID(bobRest, pubPage) {
		t.Fatalf("[D-PREMISE] PREMISE FAILED: bob cannot see the PUBLIC stale page either "+
			"(%d rows: %v) — the report is empty for some other reason, so an absent private "+
			"page would prove nothing", len(bobRest), titles(bobRest))
	}
	if hasID(bobRest, privPage) {
		t.Errorf("[D-LEAK-REST] LEAK: bob has NO grant on the private space and GET /freshness "+
			"reported its page. titles=%v — each row carries space_id + page_id, which is a "+
			"working deep link", titles(bobRest))
	}

	bobMCP := mcpTool("bob@example.com", bob)
	if !strings.Contains(bobMCP, pubTitle) {
		t.Errorf("[D-MCPPUB] the MCP get_stale_pages tool did not report the PUBLIC page to bob, "+
			"so its private-page absence below is not evidence: %s", bobMCP)
	}
	if strings.Contains(bobMCP, privTitle) {
		t.Errorf("[D-LEAK-MCP] LEAK: the MCP get_stale_pages tool handed bob the private space's "+
			"page. A sweep by ROUTE finds the REST half and misses this one — both call the same "+
			"engine method: %s", bobMCP)
	}

	// ── 5. THE DAILY DIGEST IS A SYSTEM VIEW AND MUST BE UNTOUCHED. It runs on a background
	// context with no caller at all, so a per-caller filter here would report zero stale pages to
	// an operator forever. Measured through the real method, reading its real log line.
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	digestErr := engine.SendStaleDigest(context.Background(), ws)
	slog.SetDefault(prev)
	//
	// ⚠ ONE ASSERTION, NOT TWO. The first draft also checked `digestErr != nil` separately. It can
	// never fire alone: an error means SendStaleDigest returns BEFORE writing its log line, so the
	// count check fires too. An invariant held twice cannot be breached by any single control, so
	// the copy goes and the error is folded into this message instead.
	if digestErr != nil || !strings.Contains(buf.String(), "stale_pages=2") {
		t.Errorf("[D-DIGEST] the daily digest no longer counts BOTH stale pages (err=%v, log=%q). "+
			"The digest is an OPERATOR view running on a background context with no caller: "+
			"filtering it per-caller reports an empty workspace every day at 09:00 forever, which "+
			"is why the unfiltered list survives unexported rather than being deleted.",
			digestErr, strings.TrimSpace(buf.String()))
	}

	// ── 3. alice created the private space: resolveAccess's owner-is-admin arm must keep it.
	aliceRest := rest("alice@example.com", alice)
	if !hasID(aliceRest, privPage) {
		t.Errorf("[D-OWNER] OVER-CORRECTION: the private space's CREATOR does not get her own "+
			"stale page. titles=%v", titles(aliceRest))
	}

	// ── 4. bob WITH an explicit view grant. Granted LAST so 1-3 run while he is still denied.
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO permissions (resource_type, resource_id, subject_type, subject_id, access, workspace_id)
		 VALUES ('space', $1, 'member', $2, 'view', $3)`,
		privSpace, bob, ws); err != nil {
		t.Fatalf("grant: %v", err)
	}
	grantedRest := rest("bob@example.com", bob)
	if !hasID(grantedRest, privPage) {
		t.Errorf("[D-GRANT] OVER-CORRECTION: bob holds an explicit 'view' grant on the private "+
			"space and still does not get its stale page — the filter is not consulting "+
			"resolveAccess. titles=%v", titles(grantedRest))
	}
	// ⚠ THERE WAS A "granted bob must still see the PUBLIC page" ASSERTION HERE AND IT WAS DELETED
	// RATHER THAN DECORATED. The control harness reported it claimed by nothing, and the reason is
	// structural: bob-with-a-grant losing the public page implies bob-WITHOUT one lost it too,
	// which trips the [D-PREMISE] t.Fatalf at the top and aborts before this line is reached. It
	// could therefore only ever agree with an assertion that had already spoken.
}
