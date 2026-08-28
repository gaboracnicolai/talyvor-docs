package analytics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/testutil"
)

// sec_spacestats_foreign_test.go — GET /v1/spaces/{spaceID}/analytics must refuse a caller who is
// not a member of the workspace that OWNS {spaceID}.
//
// ⚠ WHY THIS EXISTS, AND WHY IT IS THE SHARPEST ROW IN THE CENSUS THAT PRODUCED IT.
// `f64e967` fixed this package's OTHER route, `WorkspaceStats`, after measuring that its verdict
// could be neutered with all 45 packages green. `SpaceStats` — thirteen lines away in the same
// file, with the same `authz.AuthorizeWorkspace(...); !ok` shape — was left undefended, and the
// commit's own handover said why it might be: analytics has two identical call sites and a
// positive control had silently mutated the wrong one.
//
// MEASURED (~/talyvor-queue/w338-authz-verdict-census-k2w8.py), running that mutation over ALL 21
// of this repository's AuthorizeWorkspace verdict sites, anchored BY LINE NUMBER precisely so two
// identical sites cannot be confused: 15 caught, 6 not. This route was one of the 6.
//
// ⚠ THIS ROUTE RESOLVES THE WORKSPACE FROM THE SPACE, which is what makes it worth its own test
// rather than a copy of its neighbour. The caller never names a workspace: `WorkspaceOfSpace` looks
// up who owns the space, and the verdict is taken on THAT. So the gate is the only thing standing
// between a member of any workspace and the readership of a space in any other — there is no
// workspace id in the URL for a middleware to have checked.
//
// ⚠ THE POSITIVE CONTROL IS NOT OPTIONAL. A 403 earned by a missing space or a malformed request
// says nothing about authorization, so the same call by a member of the OWNING workspace must NOT
// be 403.

// foreignSpaceCall drives SpaceStats with a verified membership in memberWS while the URL names a
// space owned by someone else — the shape authz.Middleware produces for a real request.
func foreignSpaceCall(h *Handler, email, memberWS, spaceID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/spaces/"+spaceID+"/analytics", nil)
	req = req.WithContext(authz.WithMemberships(req.Context(), email,
		[]authz.Membership{{WorkspaceID: memberWS, MemberID: "m-" + memberWS}}))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("spaceID", spaceID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	h.SpaceStats(rr, req)
	return rr
}

func TestSpaceStats_RefusesASpaceInAWorkspaceTheCallerIsNotIn(t *testing.T) {
	d := testutil.New(t)
	h := NewHandler(NewStore(d.Pool))

	const victim, member = "ws-victim-spacestats", "ws-caller-spacestats"

	// A real space owned by the VICTIM workspace. The id must exist, or the handler would answer
	// 404 at WorkspaceOfSpace and never reach the verdict this test is about.
	var spaceID string
	if err := d.Pool.QueryRow(context.Background(), `
		INSERT INTO spaces (workspace_id, name, slug, created_by)
		VALUES ($1, 'Victim Space', 'victim-space', 'owner@x.test') RETURNING id`,
		victim,
	).Scan(&spaceID); err != nil {
		t.Fatalf("seed the victim's space: %v", err)
	}

	if rr := foreignSpaceCall(h, "caller@x.test", member, spaceID); rr.Code != http.StatusForbidden {
		t.Errorf("a caller with membership only in %q read the analytics for a space owned by %q "+
			"and got %d, want 403.\n"+
			"    The caller never names a workspace on this route — WorkspaceOfSpace resolves it "+
			"from the space id — so the handler's own AuthorizeWorkspace is the only gate there is.",
			member, victim, rr.Code)
	}

	// PREMISE CHECK, and it is not decoration: if the seeded space were not resolvable the
	// assertion above would be satisfied by a 404-shaped failure rather than by the gate.
	ws, err := NewStore(d.Pool).WorkspaceOfSpace(context.Background(), spaceID)
	if err != nil || ws != victim {
		t.Fatalf("premise: the seeded space must resolve to %q, got %q (err %v) — the refusal above "+
			"would then be about a missing space, not about authorization", victim, ws, err)
	}

	// POSITIVE CONTROL. Same space, same handler, a member of the OWNING workspace.
	if rr := foreignSpaceCall(h, "owner@x.test", victim, spaceID); rr.Code == http.StatusForbidden {
		t.Fatalf("a member of the OWNING workspace %q was refused (403), so the refusal above proves "+
			"nothing about authorization — this test is not armed.", victim)
	}
}
