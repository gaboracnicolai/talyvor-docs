package customdomain_test

// THE {wsID} IN `/v1/workspaces/{wsID}/custom-domains` IS DECORATIVE ON THREE OF THE FOUR ROUTES.
//
// ⚠⚠ MEASURED, NOT READ. The zero-coverage census on main (`11a9e0cc`) —
// `go test -coverprofile -coverpkg=./... ./...` then `go tool cover -func | awk '$3=="0.0%"'` —
// returned 76 production functions, and the WHOLE ADMIN HALF of this package was among them:
//
//	handler.go:21  contains      0.0%
//	handler.go:78  Create        0.0%
//	handler.go:109 List          0.0%
//	handler.go:138 Delete        0.0%
//	handler.go:309 domainActor   0.0%
//	store.go:265   GetByWorkspace 0.0%
//
// The public renderer half IS covered (privateflip_realpg_test.go), and the store half is covered
// by pgxmock (store_test.go, space_mapping_test.go). Nothing had ever driven the four admin ROUTES.
//
// WHAT DRIVING THEM FOUND. `Create` authorizes the caller-supplied path {wsID} against the verified
// membership set before it becomes the new row's owner, and says so in a four-line comment.
// `List`, `Verify` and `Delete` DO NOT READ {wsID} AT ALL — each passes the caller's ENTIRE
// membership set (`authz.WorkspaceIDs`) to a store method whose filter is `workspace_id = ANY($n)`.
// So for a caller in workspaces A and B:
//
//	GET    /v1/workspaces/{A}/custom-domains          → A's domains AND B's
//	POST   /v1/workspaces/{A}/custom-domains/{idB}/verify → 200, verifies B's domain
//	DELETE /v1/workspaces/{A}/custom-domains/{idB}    → 200, deletes B's domain
//
// ⚠ THIS IS NOT A CROSS-TENANT LEAK AND MUST NOT BE REPORTED AS ONE. `authz.WorkspaceIDs` is the
// VERIFIED membership set, so every row reachable this way belongs to a workspace the caller is
// genuinely a member of. A stranger still gets nothing — [FOREIGN-WS-REFUSED] below pins that.
// The defect is SCOPE, the axis this queue item already named: the gate names ONE id and the
// response carries MORE.
//
// ⚠ WHY IT IS WORTH A MERGE RATHER THAN A NOTE. The consequence is a destructive write on a
// PUBLIC-PUBLISHING surface, and the UI cannot warn about it:
// `frontend/src/hooks/useCustomDomains.ts` caches the response under
// `queryKey: ["custom-domains", workspaceID]` — it is a claim about ONE workspace — and
// `frontend/src/pages/DomainSettings.tsx` renders every returned row under the heading "Custom
// domains" with a Trash2 button wired to `remove.mutate(d.id)`. It NEVER renders `workspace_id`.
// So workspace B's live documentation hostname appears in workspace A's settings page,
// indistinguishable from A's own, one click from deletion — and deleting it takes B's published
// docs off the internet.
//
// ⚠ WHY NOTHING SAW IT. Every existing test in this package is single-workspace. `store_test.go`
// and `space_mapping_test.go` are pgxmock and assert the SQL carries `= ANY($n)` — which it does,
// correctly, for its own purpose (a by-id route must not 403-vs-404 across tenants). The set
// handed IN is the handler's decision, and no test had ever built a caller with TWO memberships.
// The bug is unobservable with one workspace BY CONSTRUCTION.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/customdomain"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/testutil"
)

const wsScopeSecret = "sec4-test-gateway-secret-0123456789"

// fakeTXT answers the DNS lookup Store.Verify makes, so [VERIFY-SCOPED] measures the ROUTE's
// scoping rather than this machine's resolver. It returns the token for any host asked, which is
// the most generous possible answer — if the route reaches DNS at all, verification succeeds.
type fakeTXT struct{ token string }

func (f fakeTXT) LookupTXT(_ context.Context, _ string) ([]string, error) {
	return []string{f.token}, nil
}

type scopeFixture struct {
	srv  http.Handler
	d    *testutil.DB
	wsA  string
	wsB  string
	wsC  string
	mail string
}

// newScopeFixture builds the REAL chain cmd/docs/main.go builds — the /v1 group with
// gatewayauth + authz above it, both handed the SHARED exemption predicates — and seeds ONE
// caller with memberships in TWO workspaces. That second membership is the whole instrument.
func newScopeFixture(t *testing.T, txt customdomain.TXTResolver) *scopeFixture {
	t.Helper()
	d := testutil.New(t)

	f := &scopeFixture{d: d, mail: "alice@example.com"}
	f.wsA = d.Workspace(t)
	f.wsB = d.Workspace(t)
	f.wsC = d.Workspace(t) // alice is deliberately NOT a member of C
	d.Member(t, f.wsA, f.mail)
	d.Member(t, f.wsB, f.mail)
	d.Member(t, f.wsC, "mallory@example.com")

	store := customdomain.NewStoreWithResolver(d.Pool, txt)
	h := customdomain.NewHandler(store, nil)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(gatewayauth.Middleware(wsScopeSecret, gatewayauth.ExemptTransitProof))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), gatewayauth.ExemptMembership))
		h.Mount(r)
	})
	f.srv = r
	return f
}

// as drives the chain AS a gateway-verified caller: the transit proof plus the email claim,
// exactly the two headers cmd/docs/main.go's gateway supplies. Never a member id — authz resolves
// that from workspace_members, and handing it in would be testing a router that does not exist.
func (f *scopeFixture) as(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, url, rdr)
	req.Header.Set("X-Gateway-Auth", wsScopeSecret)
	req.Header.Set("X-User-Email", f.mail)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// createDomain mints a domain THROUGH THE REAL ROUTE, so the fixture itself exercises the one
// admin route that was already correct. A t.Fatalf here is a SETUP failure, not a caught
// mutation — the control harness scores by the named failing test AND its tag.
func (f *scopeFixture) createDomain(t *testing.T, wsID, domain string) customdomain.CustomDomain {
	t.Helper()
	code, body := f.as(t, http.MethodPost, "/v1/workspaces/"+wsID+"/custom-domains",
		`{"domain":"`+domain+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("[SETUP] create %q in %s: %d %s", domain, wsID, code, body)
	}
	var cd customdomain.CustomDomain
	if err := json.Unmarshal([]byte(body), &cd); err != nil {
		t.Fatalf("[SETUP] decode created domain: %v (body %s)", err, body)
	}
	return cd
}

// domainRow reads Postgres DIRECTLY — never GetByWorkspace, which is one of the functions under
// measurement here and could be moved by the same edit.
func (f *scopeFixture) domainRow(t *testing.T, id string) (workspaceID string, verified bool, found bool) {
	t.Helper()
	err := f.d.Pool.QueryRow(context.Background(),
		`SELECT workspace_id, verified FROM custom_domains WHERE id = $1`, id,
	).Scan(&workspaceID, &verified)
	if err != nil {
		return "", false, false
	}
	return workspaceID, verified, true
}

func listDomains(t *testing.T, body string) []customdomain.CustomDomain {
	t.Helper()
	var out []customdomain.CustomDomain
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode list: %v (body %s)", err, body)
	}
	return out
}

func hasDomain(ds []customdomain.CustomDomain, name string) bool {
	for _, d := range ds {
		if d.Domain == name {
			return true
		}
	}
	return false
}

// ─── LIST ───────────────────────────────────────────

// TestList_IsScopedToThePathWorkspace_RealPG is the finding. A caller in two workspaces asks for
// ONE workspace's domains and is served both workspaces'.
func TestList_IsScopedToThePathWorkspace_RealPG(t *testing.T) {
	f := newScopeFixture(t, fakeTXT{})
	f.createDomain(t, f.wsA, "docs.alpha.example")
	f.createDomain(t, f.wsB, "docs.beta.example")

	codeA, bodyA := f.as(t, http.MethodGet, "/v1/workspaces/"+f.wsA+"/custom-domains", "")
	if codeA != http.StatusOK {
		t.Fatalf("[SETUP] list A: %d %s", codeA, bodyA)
	}
	got := listDomains(t, bodyA)

	// ── LIVENESS FLOOR. "B's domain is absent from A's list" is satisfied by an EMPTY list, and
	// an empty list is what a broken scope filter, a broken fixture or a 404'd route all produce.
	// This fails the run before that can happen.
	if !hasDomain(got, "docs.alpha.example") {
		t.Fatalf("[LIST-OWN-VISIBLE] workspace A's OWN domain is missing from its own list (%d rows) "+
			"— every scoping assertion below would pass vacuously on an empty list", len(got))
	}

	if hasDomain(got, "docs.beta.example") {
		t.Errorf("[LIST-SCOPED] GET /v1/workspaces/%s/custom-domains served workspace B's domain "+
			"docs.beta.example. The handler passes authz.WorkspaceIDs (the caller's WHOLE "+
			"membership set) to GetByWorkspace and never reads the {wsID} it was asked about; "+
			"the SPA caches this under queryKey [\"custom-domains\", workspaceID] and renders "+
			"every row with a delete button that never shows which workspace owns it. Got %d rows: %s",
			f.wsA, len(got), bodyA)
	}
}

// TestList_TheOtherWorkspaceStillListsItsOwn_RealPG is the positive control for the assertion
// above: "scoped" must not be achieved by serving nobody anything. B's list must still hold B's
// domain — otherwise a fix that returns an empty slice unconditionally would score as correct.
func TestList_TheOtherWorkspaceStillListsItsOwn_RealPG(t *testing.T) {
	f := newScopeFixture(t, fakeTXT{})
	f.createDomain(t, f.wsA, "docs.alpha.example")
	f.createDomain(t, f.wsB, "docs.beta.example")

	code, body := f.as(t, http.MethodGet, "/v1/workspaces/"+f.wsB+"/custom-domains", "")
	if code != http.StatusOK {
		t.Fatalf("[SETUP] list B: %d %s", code, body)
	}
	got := listDomains(t, body)
	if !hasDomain(got, "docs.beta.example") {
		t.Errorf("[LIST-B-OWN-VISIBLE] workspace B's own domain is missing from B's list — a scope "+
			"fix that serves nothing is not a fix. Got %d rows: %s", len(got), body)
	}
	if hasDomain(got, "docs.alpha.example") {
		t.Errorf("[LIST-SCOPED-B] workspace B's list carried workspace A's domain: %s", body)
	}
}

// ─── DELETE ─────────────────────────────────────────

// TestDelete_IsScopedToThePathWorkspace_RealPG is the destructive half. This is the click a user
// makes on workspace A's settings page against a row that belongs to B.
func TestDelete_IsScopedToThePathWorkspace_RealPG(t *testing.T) {
	f := newScopeFixture(t, fakeTXT{})
	b := f.createDomain(t, f.wsB, "docs.beta.example")

	// ── LIVENESS FLOOR. The row must EXIST and belong to B before "the delete was refused" can
	// mean anything — a delete of a row that was never there is trivially harmless.
	if ws, _, ok := f.domainRow(t, b.ID); !ok || ws != f.wsB {
		t.Fatalf("[DELETE-ROW-LIVE-FIRST] B's domain row is not present as B's before the delete "+
			"(found=%v ws=%q) — the refusal assertion below would be vacuous", ok, ws)
	}

	code, body := f.as(t, http.MethodDelete, "/v1/workspaces/"+f.wsA+"/custom-domains/"+b.ID, "")
	if code != http.StatusNotFound {
		t.Errorf("[DELETE-SCOPED] DELETE /v1/workspaces/%s/custom-domains/%s removed a domain owned "+
			"by workspace B and answered %d %s — expected 404. The handler passes the caller's "+
			"whole membership set to Store.Delete and never reads {wsID}", f.wsA, b.ID, code, body)
	}

	// The status code is the smaller half. THE ROW IS THE CLAIM: this pins that B's published
	// hostname is still serving, which a 404 alone would not establish if the delete had already
	// happened before the error mapping.
	if _, _, ok := f.domainRow(t, b.ID); !ok {
		t.Errorf("[DELETE-ROW-SURVIVES] workspace B's custom-domain row was DELETED through " +
			"workspace A's URL — B's documentation hostname stops resolving, and nothing in the " +
			"SPA that offered the button showed it belonged to B")
	}
}

// TestDelete_OwnWorkspaceStillWorks_RealPG is the positive control: refusing EVERY delete would
// satisfy the test above. Deleting through the OWNING workspace's URL must still work, 200 + gone.
func TestDelete_OwnWorkspaceStillWorks_RealPG(t *testing.T) {
	f := newScopeFixture(t, fakeTXT{})
	b := f.createDomain(t, f.wsB, "docs.beta.example")

	code, body := f.as(t, http.MethodDelete, "/v1/workspaces/"+f.wsB+"/custom-domains/"+b.ID, "")
	if code != http.StatusOK {
		t.Fatalf("[DELETE-OWN-WORKS] deleting B's domain through B's own URL answered %d %s — "+
			"expected 200; a scope fix that refuses everything is not a fix", code, body)
	}
	if _, _, ok := f.domainRow(t, b.ID); ok {
		t.Errorf("[DELETE-OWN-REALLY-GONE] the route answered 200 but the row is still there — " +
			"a delete that reports success and removes nothing is the quieter lie")
	}
}

// ─── VERIFY ─────────────────────────────────────────

// TestVerify_IsScopedToThePathWorkspace_RealPG. Verify flips `verified` and `ssl_status`, which is
// what puts a hostname LIVE in DomainRouter — so the wrong-workspace path publishes, not just reads.
func TestVerify_IsScopedToThePathWorkspace_RealPG(t *testing.T) {
	f := newScopeFixture(t, fakeTXT{})
	b := f.createDomain(t, f.wsB, "docs.beta.example")
	// The resolver must answer with THIS row's token, so a verify that REACHES DNS succeeds.
	// Otherwise verified=false for a reason that has nothing to do with scoping, and the
	// assertion below would pass on a route that is not refusing anything.
	f.srv = rebuildWithToken(t, f, b.VerifyToken)

	if _, verified, ok := f.domainRow(t, b.ID); !ok || verified {
		t.Fatalf("[VERIFY-UNVERIFIED-FIRST] B's row must exist and be UNVERIFIED before the "+
			"cross-workspace verify (found=%v verified=%v) — verifying an already-verified row "+
			"short-circuits before DNS and would pass for the wrong reason", ok, verified)
	}

	code, body := f.as(t, http.MethodPost,
		"/v1/workspaces/"+f.wsA+"/custom-domains/"+b.ID+"/verify", "")
	if code != http.StatusNotFound {
		t.Errorf("[VERIFY-SCOPED] POST /v1/workspaces/%s/custom-domains/%s/verify acted on a domain "+
			"owned by workspace B and answered %d %s — expected 404", f.wsA, b.ID, code, body)
	}
	if _, verified, _ := f.domainRow(t, b.ID); verified {
		t.Errorf("[VERIFY-ROW-UNTOUCHED] workspace B's domain was marked verified through workspace " +
			"A's URL — that is the flag DomainRouter requires before it serves the hostname, so " +
			"the wrong workspace's URL PUBLISHED it")
	}
}

// TestVerify_OwnWorkspaceStillWorks_RealPG is the positive control for the assertion above.
func TestVerify_OwnWorkspaceStillWorks_RealPG(t *testing.T) {
	f := newScopeFixture(t, fakeTXT{})
	b := f.createDomain(t, f.wsB, "docs.beta.example")
	f.srv = rebuildWithToken(t, f, b.VerifyToken)

	code, body := f.as(t, http.MethodPost,
		"/v1/workspaces/"+f.wsB+"/custom-domains/"+b.ID+"/verify", "")
	if code != http.StatusOK {
		t.Fatalf("[VERIFY-OWN-WORKS] verifying B's domain through B's own URL answered %d %s — "+
			"expected 200", code, body)
	}
	if _, verified, _ := f.domainRow(t, b.ID); !verified {
		t.Errorf("[VERIFY-OWN-REALLY-VERIFIED] the route answered 200 but the row is still "+
			"unverified — body %s", body)
	}
}

// rebuildWithToken re-wires the same chain against a resolver that answers with tok, so a verify
// that REACHES DNS succeeds. Everything else — the DB, the memberships, the seeded rows — is
// unchanged, because they live in Postgres rather than in the handler.
func rebuildWithToken(t *testing.T, f *scopeFixture, tok string) http.Handler {
	t.Helper()
	store := customdomain.NewStoreWithResolver(f.d.Pool, fakeTXT{token: tok})
	h := customdomain.NewHandler(store, nil)
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(gatewayauth.Middleware(wsScopeSecret, gatewayauth.ExemptTransitProof))
		r.Use(authz.Middleware(authz.NewPGResolver(f.d.Pool), gatewayauth.ExemptMembership))
		h.Mount(r)
	})
	return r
}

// ─── MUST-STAY-GREEN: the tenancy boundary itself ───

// TestForeignWorkspaceIsStillRefused_RealPG pins what this finding is NOT. wsC is a workspace
// alice has no membership in. Every route must refuse it — if any of these ever passes, the
// defect above has been mis-described and the real one is a cross-tenant leak.
func TestForeignWorkspaceIsStillRefused_RealPG(t *testing.T) {
	f := newScopeFixture(t, fakeTXT{})

	code, body := f.as(t, http.MethodPost, "/v1/workspaces/"+f.wsC+"/custom-domains",
		`{"domain":"docs.gamma.example"}`)
	if code != http.StatusNotFound {
		t.Errorf("[FOREIGN-WS-REFUSED] creating a domain in a workspace the caller is NOT a member "+
			"of answered %d %s — expected 404", code, body)
	}

	// And the row must not exist by any route.
	var n int
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM custom_domains WHERE workspace_id = $1`, f.wsC).Scan(&n); err != nil {
		t.Fatalf("count wsC domains: %v", err)
	}
	if n != 0 {
		t.Errorf("[FOREIGN-WS-NO-ROW] %d custom_domains row(s) landed in a workspace the caller is "+
			"not a member of", n)
	}
}
