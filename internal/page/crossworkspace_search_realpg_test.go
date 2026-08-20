package page_test

// THE THIRD INSTANCE OF THE RULE #187 AND #188 ESTABLISHED, AND THE ONLY ONE FEEDING TWO SURFACES.
//
// `page.Store.Search` scopes on ONE SQL predicate and nothing else:
//
//	SELECT … FROM pages
//	 WHERE workspace_id = $1                            ← the ONLY tenancy scope on this path
//	   AND to_tsvector(…) @@ websearch_to_tsquery(…)
//	 ORDER BY updated_at DESC LIMIT $3
//
// and BOTH of its callers filter the rows below it with `AuthorizePageRead`:
//
//	internal/page/handler.go:410  GET  /v1/workspaces/{wsID}/pages/search   → visibleTo (h.go:87)
//	internal/ai/handler.go:397    POST /v1/workspaces/{wsID}/ai/ask         → visibleTo (h.go:354)
//
// ⚠⚠ THAT FILTER ANSWERS ABOUT THE CALLER, NOT ABOUT THE WORKSPACE, AND ITS OWN DOC COMMENT SAYS
// SO: "AuthorizePageRead resolves against every workspace the caller belongs to, so a caller in two
// workspaces satisfies it for a page in either" (internal/ai/handler.go:31-33). So for a caller who
// is a member of A and B, asking under A's URL, the SQL predicate is the WHOLE of the separation —
// and `WithPageRead`'s own comment states the premise ("`Store.Search` scopes to the workspace and
// nothing else") that nothing tested.
//
// ⚠⚠ MEASURED SILENT BEFORE THIS FILE EXISTED, WHICH IS WHY IT EXISTS. At `d8d9b4e`, with the
// predicate widened to `(workspace_id = $1 OR TRUE)` — the whole tenancy scope of both surfaces
// removed — `go test -count=1 ./...` returned EXIT 0. Not one test in the repository noticed. The
// sibling predicate in `GetStalePages` is NOT silent (the stale digest's per-workspace counts red
// on the same mutation), so this was measured per predicate rather than assumed from the class.
//
// ⚠ AND THE REASON IT WAS SILENT IS STRUCTURAL, NOT AN OVERSIGHT IN ANY ONE TEST. Every existing
// cross-tenant fixture in this repo gives the caller EXACTLY ONE membership
// (`[]authz.Membership{{WorkspaceID: ws, MemberID: …}}` — the shape at page/privatespace:140,
// ai/privatespace:242, search/privatespace:87, freshness/privatespace:144, and ~15 more). With a
// one-workspace caller `AuthorizePageRead` denies the foreign page on its own, so the SQL predicate
// is never the thing under test and deleting it changes no observable output. A guard for this
// class has to seed the DUAL membership first; that is [C-REACH]'s job below.
//
// THE FIVE ASSERTIONS ARE ONE TEST ON PURPOSE, so a "fix" that empties either surface, or one that
// leans on the caller gate instead of the workspace scope, fails here rather than passing quietly:
//
//	[X-SEARCH]  bob under A's URL   B's page MUST NOT be in /pages/search      ← the defect
//	[P-SEARCH]  bob under A's URL   A's page MUST be                           ← absence is not emptiness
//	[C-REACH]   bob under B's URL   B's page MUST come back                    ← the control: the CALLER GATE
//	                                                                             PERMITS that page, so
//	                                                                             [X-SEARCH] is the workspace
//	                                                                             predicate's doing and nothing
//	                                                                             else's
//	[X-ASK]     bob under A's URL   B's body MUST NOT reach Lens, B's title MUST NOT be cited
//	[P-ASK]     bob under A's URL   A's body MUST reach Lens                   ← the corpus is non-empty
//
// [C-REACH] is the assertion that makes this file a measurement instead of a restatement: without
// it, a `AuthorizePageRead` that denied B for an unrelated reason would satisfy [X-SEARCH] while
// proving nothing about the predicate this test is named for.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/spaceauth"
	"github.com/talyvor/docs/internal/testutil"
)

// One shared query token, so every page below is a genuine full-text match on both surfaces and
// "absent" can never be explained by "did not match".
const xwsQuery = "acquisition"

const (
	xwsHomeTitle  = "A Integration Checklist"
	xwsHomeBody   = "HOMEACQBODY the acquisition integration checklist for workspace A"
	xwsOtherTitle = "B Acquisition Runbook"
	xwsOtherBody  = "OTHERACQBODY the acquisition runbook and buyer list, workspace B only"
)

// xwsLens is a REAL HTTP Lens that records every completion body it is sent. The prompt is half the
// payload under test — a stub engine would measure the citation list and miss the quoted content,
// which is the larger of the two leaks.
type xwsLens struct {
	mu      sync.Mutex
	prompts []string
	url     string
}

func newXWSLens(t *testing.T, answer string) *xwsLens {
	t.Helper()
	xl := &xwsLens{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/token" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"token":"tok","expires_at":%q}`,
				time.Now().Add(time.Hour).Format(time.RFC3339))))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		xl.mu.Lock()
		xl.prompts = append(xl.prompts, string(raw))
		xl.mu.Unlock()
		_, _ = w.Write([]byte(fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, answer)))
	}))
	t.Cleanup(s.Close)
	xl.url = s.URL
	return xl
}

func (xl *xwsLens) last() string {
	xl.mu.Lock()
	defer xl.mu.Unlock()
	if len(xl.prompts) == 0 {
		return ""
	}
	return xl.prompts[len(xl.prompts)-1]
}

type xwsAskOut struct {
	Answer  string `json:"answer"`
	Sources []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"sources"`
}

func (o xwsAskOut) cites(title string) bool {
	for _, s := range o.Sources {
		if s.Title == title {
			return true
		}
	}
	return false
}

func (o xwsAskOut) titles() []string {
	out := make([]string, 0, len(o.Sources))
	for _, s := range o.Sources {
		out = append(out, s.Title)
	}
	return out
}

func TestPageSearch_DoesNotCrossWorkspaces_ForADualMember_RealPG(t *testing.T) {
	d := testutil.New(t)
	wsHome := d.Workspace(t)
	wsOther := d.Workspace(t)

	// alice authors in each workspace; bob is a REAL member of BOTH. The dual membership is the
	// whole fixture — see the header: with one membership the caller gate does the separating and
	// the predicate under test is unreachable as a cause.
	aliceHome := d.Member(t, wsHome, "alice@example.com")
	aliceOther := d.Member(t, wsOther, "alice@example.com")
	bobHome := d.Member(t, wsHome, "bob@example.com")
	bobOther := d.Member(t, wsOther, "bob@example.com")

	// BOTH spaces are PUBLIC. A private space would let `AuthorizePageRead` deny the foreign page
	// on space permissions, and the test would then pass with the predicate deleted — measuring the
	// permission engine instead of the workspace scope.
	homeSpace := seedSpaceP(t, d, wsHome, aliceHome, "A Handbook", false)
	otherSpace := seedSpaceP(t, d, wsOther, aliceOther, "B Ops", false)

	homePage := seedPageP(t, d, wsHome, homeSpace, aliceHome, xwsHomeTitle, xwsHomeBody,
		`{"type":"doc","content":[{"type":"paragraph","text":"`+xwsHomeBody+`"}]}`)
	otherPage := seedPageP(t, d, wsOther, otherSpace, aliceOther, xwsOtherTitle, xwsOtherBody,
		`{"type":"doc","content":[{"type":"paragraph","text":"`+xwsOtherBody+`"}]}`)

	store := page.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)

	// cmd/docs/main.go's two lookers, re-derived here rather than imported: this is the metadata the
	// permission engine decides on, and borrowing another test's helper borrows its evidence.
	spaceLooker := func(ctx context.Context, id string) (permission.SpaceMeta, error) {
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.SpaceMeta{}, err
		}
		return permission.SpaceMeta{WorkspaceID: sp.WorkspaceID, Private: sp.Private, CreatedBy: sp.CreatedBy}, nil
	}
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
	spaceEnf := permission.NewEnforcer(permStore, permission.SpaceResolverFromParam("spaceID", spaceLooker))
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))

	// bobBoth is the verified identity under test: ONE person, TWO memberships, which is what makes
	// `AuthorizePageRead` return (found, canView) = (true, true) for a page in EITHER workspace.
	bobBoth := []authz.Membership{
		{WorkspaceID: wsHome, MemberID: bobHome},
		{WorkspaceID: wsOther, MemberID: bobOther},
	}

	// reached marks an assertion as REACHED. Without it "the tag did not fire" conflates two very
	// different things — the assertion ran and passed, or it never ran at all because an earlier
	// one aborted. A must-stay-green control read the second as the first, so the distinction is
	// recorded in the output rather than inferred from it. (Measured: the first draft of the
	// control harness scored a mutation a CATCH on a tag that appears only inside ANOTHER
	// assertion's message. An instrument that reports a catch it did not observe is the same
	// defect this file exists to guard against, one level up.)
	reached := func(tag string) { t.Logf("[EVAL] %s", tag) }

	// search drives the REAL route through the REAL handler, wired as cmd/docs/main.go wires it.
	// It returns the STATUS as well as the rows: a 403 is a legitimate outcome for one of the two
	// calls below, and swallowing it in a helper-level Fatalf leaves the failure untagged and the
	// control unscoreable.
	search := func(wsID string) ([]model.Page, int) {
		t.Helper()
		h := page.NewHandler(store, d.Pool).
			WithAccess(pageEnf, spaceEnf).
			WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))

		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				c := authz.WithMemberships(req.Context(), "bob@example.com", bobBoth)
				next.ServeHTTP(w, req.WithContext(c))
			})
		})
		r.Route("/v1", func(r chi.Router) { h.Mount(r) })

		path := "/v1/workspaces/" + wsID + "/pages/search?q=" + xwsQuery + "&limit=50"
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			return nil, rr.Code
		}
		var out []model.Page
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode search %s: %v (%s)", path, err, rr.Body.String())
		}
		return out, rr.Code
	}

	has := func(rows []model.Page, id string) bool {
		for _, p := range rows {
			if p.ID == id {
				return true
			}
		}
		return false
	}
	titlesOf := func(rows []model.Page) []string {
		out := make([]string, 0, len(rows))
		for _, p := range rows {
			out = append(out, p.Title)
		}
		return out
	}

	// ── [X-SEARCH] + [P-SEARCH]. bob asks under A's URL.
	home, homeCode := search(wsHome)
	if homeCode != http.StatusOK {
		t.Fatalf("[P-SEARCH] bob is a member of workspace A and GET A's /pages/search returned "+
			"%d, not 200 — the fixture is broken and nothing below is interpretable", homeCode)
	}
	reached("X-SEARCH")
	if has(home, otherPage) {
		t.Errorf("[X-SEARCH] /workspaces/%s/pages/search served bob workspace B's page %q (id=%s); "+
			"rows=%v. The caller is a member of both workspaces, so AuthorizePageRead permits that "+
			"page — `WHERE workspace_id = $1` in page.Store.Search is the only thing that may keep "+
			"it out of an A-scoped answer", wsHome, xwsOtherTitle, otherPage, titlesOf(home))
	}
	reached("P-SEARCH")
	if !has(home, homePage) {
		t.Errorf("[P-SEARCH] A's own page %q (id=%s) is missing from A's search (rows=%v) — the "+
			"cross-workspace absence asserted just above would be satisfied by an empty response, "+
			"so this must hold for that one to mean anything", xwsHomeTitle, homePage, titlesOf(home))
	}

	// ── [C-REACH]. The control. The SAME caller, under B's URL, MUST get B's page: that is what
	// establishes the caller gate permits it, and therefore that the cross-workspace absence above
	// is the workspace predicate's doing rather than a page bob could never have read either way.
	//
	// Errorf, not Fatalf: the two /ask assertions below measure a different surface and are worth
	// evaluating even when this control fails. A Fatalf here left them unreached, and "unreached"
	// is indistinguishable from "passed" to anything reading the output.
	other, otherCode := search(wsOther)
	reached("C-REACH")
	if otherCode != http.StatusOK || !has(other, otherPage) {
		t.Errorf("[C-REACH] bob is a member of workspace B and B's page %q (id=%s) did NOT come "+
			"back from B's own URL (status=%d rows=%v). Without this the cross-workspace absence "+
			"proves nothing about the workspace predicate — it would be explained by the caller "+
			"gate alone", xwsOtherTitle, otherPage, otherCode, titlesOf(other))
	}

	// ── [X-ASK] + [P-ASK]. The same store method, the other surface — and here the leak is not a
	// row but a sentence: `content_text` is pasted verbatim into the prompt that crosses the process
	// boundary to Lens, so the citation list alone would not see it.
	lens := newXWSLens(t, "Yes — the runbook covers it.")
	engine := ai.New(lensintegration.New(lens.url, "k1").
		WithTokenProvider(lenscreds.New(lens.url, "k1", lenscreds.Options{})))

	ah := ai.NewHandler(engine, store).
		WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))
	ar := chi.NewRouter()
	ar.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			c := authz.WithMemberships(req.Context(), "bob@example.com", bobBoth)
			next.ServeHTTP(w, req.WithContext(c))
		})
	})
	ar.Route("/v1", func(r chi.Router) { ah.Mount(r) })

	body, _ := json.Marshal(map[string]string{"question": xwsQuery})
	areq := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+wsHome+"/ai/ask",
		strings.NewReader(string(body)))
	areq.Header.Set("Content-Type", "application/json")
	arr := httptest.NewRecorder()
	ar.ServeHTTP(arr, areq)
	if arr.Code != http.StatusOK {
		t.Fatalf("POST /v1/workspaces/%s/ai/ask as bob = %d, want 200: %s", wsHome, arr.Code, arr.Body.String())
	}
	var askOut xwsAskOut
	if err := json.Unmarshal(arr.Body.Bytes(), &askOut); err != nil {
		t.Fatalf("decode ask: %v (%s)", err, arr.Body.String())
	}

	prompt := lens.last()
	if prompt == "" {
		// DELIBERATELY UNTAGGED. The engine was never reached, so neither /ask assertion below was
		// evaluated — and a tagged failure here would be scored as one of them having fired. An
		// unreached measurement must read as INVALID, never as a catch.
		t.Fatal("Lens received no completion request — the /ask engine was never reached, so the " +
			"quoted-content and citation assertions were not evaluated at all")
	}
	reached("X-ASK")
	if strings.Contains(prompt, xwsOtherBody) {
		t.Errorf("[X-ASK] workspace B's page body reached Lens on an A-scoped /ai/ask: prompt "+
			"contains %q. The document was copied to a third-party model on the caller's behalf", xwsOtherBody)
	}
	if askOut.cites(xwsOtherTitle) {
		t.Errorf("[X-ASK] workspace B's page %q was cited back to bob under A's URL (sources=%v)",
			xwsOtherTitle, askOut.titles())
	}
	reached("P-ASK")
	if !strings.Contains(prompt, xwsHomeBody) {
		t.Errorf("[P-ASK] A's own page body never reached Lens (prompt=%q) — an empty grounding "+
			"corpus satisfies the cross-workspace absence above perfectly, so this must hold for "+
			"it to mean anything", prompt)
	}
}
