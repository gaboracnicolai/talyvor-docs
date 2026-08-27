package authz_test

// boundary_realpg_test.go — THE FIRST TESTS internal/authz HAS EVER HAD.
//
// This package is SEC-4 Layer 1. gatewayauth proves WHO the caller is; authz resolves that email to
// the workspaces they are a member of and puts the set in context, and EVERY by-id store query in
// this repository scopes to it (`WHERE workspace_id = ANY(authz.WorkspaceIDs(ctx))`). It is 292
// lines of source and, until this file, ZERO test files — measured by a source-vs-test census over
// every package in internal/ (tab-n8k4, W3.12): the only package in the repository with real logic
// and no test of its own, and it is the tenancy boundary.
//
// ⚠ THE PROPERTY IT MOST NEEDED WAS A CLAIM ABOUT POSTGRES, WRITTEN IN A GO COMMENT. WorkspaceIDs's
// docstring states the whole fail-closed argument as a fact:
//
//	"An empty slice (caller has no memberships, or the request never passed the boundary) correctly
//	 denies all by-id access: ANY(empty) matches nothing → 404. This is the fail-closed property."
//
// Nothing had ever executed it. It is true — measured below, through the real page store on real
// Postgres — but it is true of pgx's encoding of a Go slice into a Postgres array parameter, not of
// anything in this package, and it would change silently if the scoping predicate or the encoder
// changed. A security property asserted in prose about another system is exactly the shape this
// repository keeps finding elsewhere; this file executes it instead.
//
// ⚠ AND ONE CONTRACT IN THIS PACKAGE WAS FALSE, MEASURED BEFORE IT WAS FIXED. Memberships and Email
// both promise "ok=false when the request never passed the boundary" and both derived that from
// whether an authCtx was present. WithAuthorized installs one unconditionally, so on a bare context
// it made Memberships report ok=TRUE with an empty set and Email report ("", TRUE). It was a TRAP
// rather than a live defect — WorkspaceIDs still returned the empty set and AuthorizeWorkspace still
// refused — and the fix is authCtx.passed. The tests below pin BOTH halves: that the contract now
// holds, and that nothing a production path does changed.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	tt "github.com/talyvor/docs/internal/testutil"
)

// ─── The fail-closed property, executed rather than asserted ────────────────────────────────

// TestTheFailClosedPropertyIsPostgresNotAComment_RealPG drives the REAL by-id read with the REAL
// value WorkspaceIDs returns, on real Postgres. It is the only thing in the repository that checks
// the sentence the whole boundary rests on.
func TestTheFailClosedPropertyIsPostgresNotAComment_RealPG(t *testing.T) {
	d := tt.New(t)
	ctx := context.Background()
	ps := page.NewStore(d.Pool)

	wsID := d.Workspace(t)
	pageID := d.Page(t, wsID, "u1", "a page in a real workspace")

	// PREMISE, asserted first: the page IS readable in its own workspace. Without this every
	// "denied" below could be a page that was never written, and the test would pass on nothing.
	if _, err := ps.GetByIDInWorkspaces(ctx, pageID, []string{wsID}); err != nil {
		t.Fatalf("PREMISE FAILED: the seeded page is not readable in its own workspace (%v) — every "+
			"denial below would then be measuring an absent row, not a scope", err)
	}

	for _, tc := range []struct {
		name  string
		scope []string
	}{
		{"WorkspaceIDs of a context that never passed the boundary", authz.WorkspaceIDs(context.Background())},
		{"WorkspaceIDs of a verified caller with NO memberships", authz.WorkspaceIDs(authz.WithMemberships(ctx, "nobody@example.com", nil))},
		{"a nil slice", nil},
		{"an unrelated workspace", []string{"ws-somebody-else"}},
	} {
		_, err := ps.GetByIDInWorkspaces(ctx, pageID, tc.scope)
		if !errors.Is(err, page.ErrNotFound) {
			t.Errorf("%s: GetByIDInWorkspaces returned %v, want ErrNotFound. The fail-closed property "+
				"is what makes an unauthenticated or unaffiliated caller get a 404 instead of a "+
				"document — it is stated in WorkspaceIDs's docstring as a fact about Postgres and "+
				"this is the only place it is executed", tc.name, err)
		}
	}
}

// TestWorkspaceIDsIsEmptyAndNotNil pins the representation, because the two are not the same
// question downstream: a nil slice encodes to NULL and `= ANY(NULL)` is NULL, an empty slice
// encodes to '{}' and `= ANY('{}')` is false. Both deny today (the test above measures that), but
// only one of them is what this function promises to return, and a change from one to the other
// would be invisible without this.
func TestWorkspaceIDsIsEmptyAndNotNil(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"a context that never passed the boundary", context.Background()},
		{"a verified caller with no memberships", authz.WithMemberships(context.Background(), "a@b.c", nil)},
	} {
		got := authz.WorkspaceIDs(tc.ctx)
		if got == nil {
			t.Errorf("%s: WorkspaceIDs returned nil, want an empty non-nil slice", tc.name)
		}
		if len(got) != 0 {
			t.Errorf("%s: WorkspaceIDs returned %v, want length 0", tc.name, got)
		}
	}
}

// ─── The contract that was false ────────────────────────────────────────────────────────────

// TestABareContextDressedByWithAuthorizedIsStillNotVerified is the measured trap, now pinned.
func TestABareContextDressedByWithAuthorizedIsStillNotVerified(t *testing.T) {
	dressed := authz.WithAuthorized(context.Background(), "ws-x", "member-x")

	if _, ok := authz.Memberships(dressed); ok {
		t.Errorf("Memberships reported ok=true on a context that never passed the boundary. Its own " +
			"docstring says ok=false there, and WithAuthorized installs an authCtx unconditionally — " +
			"so the presence of the value was standing in for the boundary having been crossed")
	}
	if email, ok := authz.Email(dressed); ok {
		t.Errorf("Email reported ok=true (email=%q) on a context that never passed the boundary. The "+
			"one consumer that reads this ok — internal/collab's refreshRoster — refuses on "+
			"`!ok || email == \"\"`, so it was protected by its own second check rather than by the "+
			"contract it was reading", email)
	}

	// The two halves that were ALREADY fail-closed, pinned so the fix cannot be read as having
	// created safety that was already there.
	if got := authz.WorkspaceIDs(dressed); len(got) != 0 {
		t.Errorf("WorkspaceIDs = %v, want empty — this is what kept the trap from being a defect", got)
	}
	if _, ok := authz.AuthorizeWorkspace(dressed, "ws-x"); ok {
		t.Error("AuthorizeWorkspace allowed the workspace WithAuthorized names. It must match against " +
			"the verified MEMBERSHIP set, which a bare context does not have")
	}

	// And the values WithAuthorized exists to carry are still carried — it is not a refusal.
	if ws, ok := authz.AuthorizedWorkspace(dressed); !ok || ws != "ws-x" {
		t.Errorf("AuthorizedWorkspace = (%q, %v), want (\"ws-x\", true)", ws, ok)
	}
	if m, ok := authz.AuthorizedMember(dressed); !ok || m != "member-x" {
		t.Errorf("AuthorizedMember = (%q, %v), want (\"member-x\", true)", m, ok)
	}
}

// MUST STAY GREEN, AND IT IS THE HALF THAT MATTERS. The production path is
// WithMemberships (middleware) -> AuthorizeWorkspace -> WithAuthorized (internal/mcp callTool), and
// none of it may change. A fix that made a bare context honest by breaking the real one would
// satisfy the test above.
func TestTheProductionOrderIsUnchanged(t *testing.T) {
	ms := []authz.Membership{
		{WorkspaceID: "ws-1", MemberID: "m-1", Role: "admin"},
		{WorkspaceID: "ws-2", MemberID: "m-2", Role: "member"},
	}
	ctx := authz.WithMemberships(context.Background(), "alice@example.com", ms)

	m, ok := authz.AuthorizeWorkspace(ctx, "ws-2")
	if !ok || m.MemberID != "m-2" {
		t.Fatalf("AuthorizeWorkspace(ws-2) = (%+v, %v), want the ws-2 membership", m, ok)
	}
	ctx = authz.WithAuthorized(ctx, m.WorkspaceID, m.MemberID)

	got, ok := authz.Memberships(ctx)
	if !ok || len(got) != 2 {
		t.Errorf("after WithAuthorized: Memberships = (%v, %v), want the two memberships and ok=true — "+
			"WithAuthorized copies the base context and must not drop what the boundary put there", got, ok)
	}
	if email, ok := authz.Email(ctx); !ok || email != "alice@example.com" {
		t.Errorf("after WithAuthorized: Email = (%q, %v), want the verified email and ok=true", email, ok)
	}
	if ws, ok := authz.AuthorizedWorkspace(ctx); !ok || ws != "ws-2" {
		t.Errorf("AuthorizedWorkspace = (%q, %v), want (\"ws-2\", true)", ws, ok)
	}
	if a, ok := authz.AuthorizedMember(ctx); !ok || a != "m-2" {
		t.Errorf("AuthorizedMember = (%q, %v), want (\"m-2\", true)", a, ok)
	}
	if ids := authz.WorkspaceIDs(ctx); len(ids) != 2 {
		t.Errorf("WorkspaceIDs = %v, want both workspaces — the scope every by-id query uses", ids)
	}
	// MemberIDForWorkspace resolves per workspace, not from "the" membership.
	if id, ok := authz.MemberIDForWorkspace(ctx, "ws-1"); !ok || id != "m-1" {
		t.Errorf("MemberIDForWorkspace(ws-1) = (%q, %v), want (\"m-1\", true)", id, ok)
	}
	if _, ok := authz.MemberIDForWorkspace(ctx, "ws-3"); ok {
		t.Error("MemberIDForWorkspace(ws-3) reported ok for a workspace the caller is not in")
	}
}

// ─── AuthorizeWorkspace, the MCP arg-trust cure ─────────────────────────────────────────────

func TestAuthorizeWorkspaceIsFailClosed(t *testing.T) {
	withMs := authz.WithMemberships(context.Background(), "a@b.c",
		[]authz.Membership{{WorkspaceID: "ws-1", MemberID: "m-1", Role: "member"}})

	for _, tc := range []struct {
		name string
		ctx  context.Context
		ws   string
	}{
		{"an empty workspace id", withMs, ""},
		{"a workspace the caller is not in", withMs, "ws-9"},
		{"no memberships in context at all", context.Background(), "ws-1"},
		{"a verified caller with an empty membership set", authz.WithMemberships(context.Background(), "a@b.c", nil), "ws-1"},
	} {
		if _, ok := authz.AuthorizeWorkspace(tc.ctx, tc.ws); ok {
			t.Errorf("%s: AuthorizeWorkspace allowed it. This is the arg-trust cure — the workspace a "+
				"JSON-RPC tool acts on is authorized against membership, never trusted from the arg",
				tc.name)
		}
	}
	if m, ok := authz.AuthorizeWorkspace(withMs, "ws-1"); !ok || m.MemberID != "m-1" {
		t.Errorf("AuthorizeWorkspace(ws-1) = (%+v, %v), want the membership and ok=true — a rule that "+
			"refuses everything is not fail-closed, it is broken", m, ok)
	}
}

// ─── The middleware ─────────────────────────────────────────────────────────────────────────

type stubResolver struct {
	ms  []authz.Membership
	err error
}

func (s stubResolver) MembershipsByEmail(context.Context, string) ([]authz.Membership, error) {
	return s.ms, s.err
}

func TestMiddlewareOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		identity   *gatewayauth.Identity
		resolver   authz.Resolver
		exempt     func(string) bool
		wantStatus int
		wantNext   bool
		// wantMemberships is only read when wantNext is true.
		wantMemberships int
		wantOK          bool
	}{
		{
			name:       "no verified identity → 403, and the handler never runs",
			identity:   nil,
			resolver:   stubResolver{},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "a verified request with an EMPTY email → 403",
			identity:   &gatewayauth.Identity{Email: ""},
			resolver:   stubResolver{},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "the resolver errors → 500, and the handler never runs",
			identity:   &gatewayauth.Identity{Email: "a@b.c"},
			resolver:   stubResolver{err: errors.New("pg down")},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:            "a verified email that resolves to NO memberships still proceeds, with an empty set",
			identity:        &gatewayauth.Identity{Email: "a@b.c"},
			resolver:        stubResolver{},
			wantStatus:      http.StatusOK,
			wantNext:        true,
			wantMemberships: 0,
			wantOK:          true,
		},
		{
			name:     "a verified email with memberships proceeds carrying them",
			identity: &gatewayauth.Identity{Email: "a@b.c"},
			resolver: stubResolver{ms: []authz.Membership{
				{WorkspaceID: "ws-1", MemberID: "m-1", Role: "member"}}},
			wantStatus:      http.StatusOK,
			wantNext:        true,
			wantMemberships: 1,
			wantOK:          true,
		},
		{
			name:       "an exempt path skips resolution entirely — no identity needed",
			identity:   nil,
			resolver:   stubResolver{err: errors.New("must not be called")},
			exempt:     func(string) bool { return true },
			wantStatus: http.StatusOK,
			wantNext:   true,
			// The handler runs WITHOUT a boundary, which is the whole point of the lane: it must
			// still report not-verified rather than an empty-but-verified caller.
			wantMemberships: 0,
			wantOK:          false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ranNext bool
			var gotMS []authz.Membership
			var gotOK bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ranNext = true
				gotMS, gotOK = authz.Memberships(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			h := authz.Middleware(tc.resolver, tc.exempt)(next)
			req := httptest.NewRequest(http.MethodGet, "/v1/pages/p1", nil)
			if tc.identity != nil {
				req = req.WithContext(gatewayauth.WithIdentity(req.Context(), *tc.identity))
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if ranNext != tc.wantNext {
				t.Fatalf("handler ran = %v, want %v — a refusal that still runs the handler has "+
					"refused nothing", ranNext, tc.wantNext)
			}
			if !tc.wantNext {
				// The refusals must be JSON with a code, not a bare status: the SPA classifies on it.
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Errorf("refusal body is not JSON (%v): %s", err, rec.Body.String())
				} else if body["code"] == "" {
					t.Errorf("refusal body has no code: %s", rec.Body.String())
				}
				return
			}
			if gotOK != tc.wantOK {
				t.Errorf("Memberships ok = %v, want %v", gotOK, tc.wantOK)
			}
			if len(gotMS) != tc.wantMemberships {
				t.Errorf("memberships = %v, want %d", gotMS, tc.wantMemberships)
			}
		})
	}
}
