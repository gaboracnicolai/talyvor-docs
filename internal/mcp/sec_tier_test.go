package mcp_test

// SEC-4 MCP write-TIER gate. The chokepoint authorizes the WORKSPACE (membership), but the write tools
// create_page / update_page / verify_page mutated with NO within-workspace tier check — so a view-tier
// member could create/update/verify a page through MCP, a mutation the REST doors reserve for
// AccessEdit. RED (no gate in callTool): the viewer-write asserts below FAIL (the tool succeeds).
// GREEN (gate wired, reusing permission.CheckPage/CheckSpace): viewer writes are denied, editor/owner
// writes still succeed. A separate case proves fail-closed: a server with NO AccessController denies
// every write, even an admin's — a dropped wiring is a loud total denial, not a silent hole.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/mcp"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// toolDenied reports whether a JSON-RPC tool call came back as an error (no result).
func toolDenied(rr *httptest.ResponseRecorder) bool {
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	return resp["error"] != nil
}

// tierDenied is stricter: the call was refused specifically by the TIER gate (message names the edit
// requirement), not by some other error. This distinguishes the gate's denial from, e.g., create_page's
// unrelated pre-existing "workspace_id required" store error, so the create_page assertion is a genuine
// red→green on the gate rather than a false pass on a different failure.
func tierDenied(rr *httptest.ResponseRecorder) bool {
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	e, ok := resp["error"].(map[string]any)
	if !ok {
		return false
	}
	msg, _ := e["message"].(string)
	return strings.Contains(msg, "requires edit access")
}

func TestSEC4_MCP_TierEnforcement(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")   // space/page creator → admin
	viewer := d.Member(t, W, "viewer@corp.com") // view grant on the page → must NOT write via MCP
	editor := d.Member(t, W, "editor@corp.com") // edit grant on the page → may update/verify

	sp, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: W, Name: "S", Slug: "s-" + owner[len(owner)-6:], CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	pg, err := page.NewStore(d.Pool).Create(ctx, model.Page{
		SpaceID: sp.ID, WorkspaceID: W, Title: "P", CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed page: %v", err)
	}
	grant := func(subject string, lvl permission.AccessLevel) {
		if err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
			ResourceType: permission.ResourcePage, ResourceID: pg.ID, SubjectType: "member",
			SubjectID: subject, Access: lvl, WorkspaceID: W, GrantedBy: owner,
		}); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}
	grant(viewer, permission.AccessView)
	grant(editor, permission.AccessEdit)

	chain := newMCPChain(t, d)
	titleOf := func() string {
		var s string
		if err := d.Pool.QueryRow(ctx, `SELECT title FROM pages WHERE id=$1`, pg.ID).Scan(&s); err != nil {
			t.Fatalf("read title: %v", err)
		}
		return s
	}

	// ── RED: a view-tier member must be REFUSED by the TIER gate on every MCP write tool. ──
	if rr := callTool(chain, "viewer@corp.com", true, "update_page", map[string]any{"page_id": pg.ID, "title": "hax"}); !tierDenied(rr) {
		t.Errorf("viewer update_page not tier-denied — view-tier must not mutate via MCP:\n%s", rr.Body.String())
	}
	if rr := callTool(chain, "viewer@corp.com", true, "verify_page", map[string]any{"page_id": pg.ID}); !tierDenied(rr) {
		t.Errorf("viewer verify_page not tier-denied:\n%s", rr.Body.String())
	}
	// create_page needs edit on the SPACE; the viewer has only view there. The tier gate runs BEFORE
	// dispatch, so this is a clean tier denial even though create_page has an unrelated store bug
	// (missing workspace_id) that would otherwise surface only after the gate.
	if rr := callTool(chain, "viewer@corp.com", true, "create_page", map[string]any{"space_id": sp.ID, "title": "hax page"}); !tierDenied(rr) {
		t.Errorf("viewer create_page not tier-denied (needs edit on the space):\n%s", rr.Body.String())
	}
	if titleOf() == "hax" {
		t.Errorf("viewer's update_page PERSISTED — the write went through despite the view tier")
	}

	// ── POSITIVE controls — the gate must not over-block the edit tier. (update_page/verify_page work
	//    against a real store; create_page's separate workspace_id bug keeps it out of the positives.) ──
	if rr := callTool(chain, "editor@corp.com", true, "update_page", map[string]any{"page_id": pg.ID, "title": "editor edit"}); toolDenied(rr) {
		t.Errorf("editor update_page denied — edit grant must be allowed:\n%s", rr.Body.String())
	}
	if got := titleOf(); got != "editor edit" {
		t.Errorf("editor update_page did not persist: title=%q", got)
	}
	if rr := callTool(chain, "editor@corp.com", true, "verify_page", map[string]any{"page_id": pg.ID}); toolDenied(rr) {
		t.Errorf("editor verify_page denied:\n%s", rr.Body.String())
	}
}

// A server wired WITHOUT an AccessController must deny every write tool — even an admin's — so a dropped
// wiring fails closed (loud) instead of silently reopening the tier hole.
func TestSEC4_MCP_WriteFailsClosedWithoutController(t *testing.T) {
	d := testutil.New(t)
	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")
	pg := d.Page(t, W, owner, "P") // owner is creator → admin: the tier WOULD allow this if a gate were wired

	// Same gateway+authz wiring as newMCPChain, but the server has NO .WithAccess — s.access is nil.
	srv := mcp.New(page.NewStore(d.Pool), space.NewStore(d.Pool), nil, nil, nil, "test")
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		exempt := func(string) bool { return false }
		r.Use(gatewayauth.Middleware(testGatewaySecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		r.Post("/mcp", srv.HandleRPC)
	})
	if rr := callTool(r, "owner@corp.com", true, "update_page", map[string]any{"page_id": pg, "title": "x"}); !toolDenied(rr) {
		t.Errorf("update_page with NO AccessController was allowed — write tools must fail closed:\n%s", rr.Body.String())
	}
}
