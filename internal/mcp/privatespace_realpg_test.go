package mcp_test

// THE FIFTH COPY OF THE SEAM, AND THE LARGEST — SIX TOOLS BEHIND ONE DOOR. THE HANDED-DOWN LIST
// OF THEM WAS SHORT BY THE WORST ONE, AND ONE OF THE TWO IT TICKED OFF WAS STILL OPEN.
//
// `internal/search`, page.Handler's /pages/search + /pages/stale (#78), internal/ai's /ask (#79)
// and freshness.GetStaleReport (#80) each authorized the WORKSPACE and stopped. So does every
// READ tool on /mcp, and for one reason stated in the code itself: `Server.authorizeWrite` maps
// `create_page` / `update_page` / `verify_page` and nothing else, so THE AccessController WIRED BY
// `mcpServer.WithAccess` IS A WRITE GATE. A read tool passes the membership chokepoint at the
// WORKSPACE tier and reaches the store with no second question asked.
//
// ⚠⚠ #78's NOTE NAMED FIVE READ TOOLS — search_docs, ask_docs, get_stale_pages, get_space_tree,
// list_pages — AND DRIVING THEM FOUND TWO IT DID NOT NAME. One is the biggest leak in the set:
// `get_page` RETURNS `content_text`, THE WHOLE DOCUMENT, which is the payload #78 called "the
// document" rather than an excerpt. The other is `get_page_analytics`. A handed-down scope is a
// claim about a census somebody else ran; this one was short by the tool that returns the body.
//
// ⚠⚠ AND `ask_docs` IS STILL OPEN DESPITE BEING RECORDED AS CLOSED. #79 gated internal/ai's REST
// /ask. `toolAskDocs` NEVER CALLS THAT HANDLER — it runs its own SearchWithRank and assembles the
// prompt itself, so the fix landed one layer away from this caller. Measured on the wire to Lens:
//
//	ask_docs   the prompt sent to the model contained, verbatim:
//	           "--- Page 1: Q3 Quarterly Layoff Plan ---
//	            SECRETLAYOFF 240 roles board only do not circulate
//	            Source: /spaces/2b9c3f59…/pages/0133435b…"
//	           under the system message "Use ONLY the provided documentation to answer",
//	           and `sources` cited the page back with its id.  ← RECORDED AS CLOSED BY #79
//
// A ROW IN A LIST CAN BE CHECKED IN THE PAYLOAD; A DOCUMENT PASTED INTO A PROMPT CANNOT. The model
// is then asked to answer FROM it, so the disclosure arrives as prose and no absence-of-a-row
// assertion anywhere can see it — which is why [M-LEAK-ASK] asserts on the Lens request body.
//
// MEASURED ON REAL POSTGRES THROUGH POST /mcp behind the production gatewayauth+authz middleware,
// BEFORE ANY CHANGE, and re-measured against THIS file's fixture with the four gates removed
// (control C1). bob is a workspace member with NO grant on alice's private space:
//
//	search_docs         {"title":"Q3 Quarterly Layoff Plan","space_name":"Board Private",
//	                     "excerpt":"SECRETLAYOFF 240 roles board only do not circulate",
//	                     "url":"/spaces/f038866b…/pages/5b07272b…"}   ← a working deep link
//	get_page            {"content_text":"SECRETLAYOFF 240 roles board only do not circulate", …}
//	                                       THE WHOLE DOCUMENT        ← NOT IN #78's LIST
//	list_pages          [{"title":"Q3 Quarterly Layoff Plan", …}]     bob names the private space_id
//	get_space_tree      [{"name":"Board Private","pages":[{"title":"Q3 Quarterly Layoff Plan"}]}, …]
//	get_page_analytics  {"total_views":1,"unique_viewers":1,"avg_duration_sec":42,"last_viewed_at":…}
//	                                                                  ← NOT IN #78's LIST
//
// ⚠⚠ AND NOTHING ELSE IN THE REPOSITORY NOTICED. C1 removes all five gates and runs `go test ./...`
// against real Postgres: the seven leak assertions in THIS file fire and NOT ONE OTHER TEST in the
// repo reddens. The whole suite was green over six ungated read tools.
//
// THE FIX IS TWO SHAPES BECAUSE THE TOOLS ARE TWO SHAPES, and calling them one would be wrong in
// one direction or the other:
//
//   - THE OBJECT-KEYED TOOLS (get_page, get_page_analytics, list_pages) name ONE object the caller
//     already chose. There is no honest partial answer, so they DENY — the same answer the REST
//     by-id doors give through permission.Enforcer.Require(AccessView).
//   - THE LIST TOOLS (search_docs, get_space_tree, ask_docs) legitimately return a MIXED set — for
//     ask_docs the "set" is the grounding corpus. Denying the
//     call would take the readable rows away with the unreadable ones, so they FILTER per row —
//     #78's primitive, over an over-fetched window so a row the caller may not read cannot eat one
//     of their result slots ([M-SLOT] is that case, at limit=1).
//
// ⚠ get_space_tree FILTERS BY SPACE AND NOT BY PAGE, AND THAT IS A PROPERTY OF THE RULE ENGINE
// RATHER THAN A SHORTCUT: resolveAccess has no DENY — a grant only ever RAISES the level, and
// CheckPage builds its resourceContext from the SPACE's privacy and creator. So a page inside a
// space the caller may view is viewable by construction, and a per-page pass there could only ever
// agree with the space's answer.
//
// ⚠ NIL FAILS CLOSED, matching this file's write half (`authorizeWrite`) and #78's WithPageRead,
// and deliberately NOT internal/search's nil-means-unfiltered. A dropped wiring line is a loud
// total denial, never a silent reopening.

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
	"github.com/talyvor/docs/internal/analytics"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/lenscreds"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/mcp"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// seedSpaceP / seedPageP write the fixture directly. Deliberately NOT testutil.Page: that helper
// creates a page in a space it makes itself, and this whole file turns on WHICH space a page is in
// and whether that space is private.
func seedSpaceP(t *testing.T, d *testutil.DB, wsID, creator, name string, private bool) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		 VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		wsID, name, "sp-"+name+"-"+wsID, creator, private).Scan(&id); err != nil {
		t.Fatalf("seed space %q: %v", name, err)
	}
	return id
}

func seedPageP(t *testing.T, d *testutil.DB, wsID, spaceID, author, title, body string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text, content)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		spaceID, wsID, title, "pg-"+title+"-"+wsID, author, body, `{"type":"doc"}`).Scan(&id); err != nil {
		t.Fatalf("seed page %q: %v", title, err)
	}
	return id
}

// recordingLens is a real HTTP Lens that KEEPS THE COMPLETION BODY IT WAS SENT. The leak in
// ask_docs is not a row in a list — it is a private document pasted into a PROMPT — so the only
// instrument that can see it is the wire between Docs and the model. An assertion on the ANSWER
// cannot: the fake's answer is a constant, and the real model's would be prose.
type recordingLens struct {
	mu   sync.Mutex
	last string
}

func (l *recordingLens) prompt() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

func newRecordingLens(t *testing.T) (*recordingLens, *ai.Engine) {
	t.Helper()
	l := &recordingLens{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/token" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"token":"tok","expires_at":%q}`,
				time.Now().Add(time.Hour).Format(time.RFC3339))))
			return
		}
		body, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		l.last = string(body)
		l.mu.Unlock()
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"an answer"}]}`))
	}))
	t.Cleanup(s.Close)
	return l, ai.New(lensintegration.New(s.URL, "k1").WithTokenProvider(lenscreds.New(s.URL, "k1", lenscreds.Options{})))
}

// readChain mounts the server behind the SAME middleware main.go uses, with the analytics store
// and a real AI engine wired — newMCPChain passes nil for both, which would make
// get_page_analytics and ask_docs unreachable for reasons this file is not testing.
func readChain(t *testing.T, d *testutil.DB, engine *ai.Engine) http.Handler {
	t.Helper()
	srv := mcp.New(page.NewStore(d.Pool), space.NewStore(d.Pool), analytics.NewStore(d.Pool), engine, nil, "test").
		WithAccess(mcpAccess(d))
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		exempt := func(string) bool { return false }
		r.Use(gatewayauth.Middleware(testGatewaySecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		r.Post("/mcp", srv.HandleRPC)
	})
	return r
}

// toolText is what an MCP client actually reads: the tool payload when the envelope carried no
// error, and "" plus denied=true when it did. Both halves matter here — a leak is a string IN the
// payload, and a correct denial is an ERROR rather than an empty payload.
func toolText(t *testing.T, chain http.Handler, email, tool string, args map[string]any) (payload string, denied bool) {
	t.Helper()
	rr := callTool(chain, email, true, tool, args)
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s: decode envelope: %v — %s", tool, err, rr.Body.String())
	}
	if len(env.Error) > 0 {
		return "", true
	}
	if len(env.Result.Content) == 0 {
		t.Fatalf("%s: neither error nor content: %s", tool, rr.Body.String())
	}
	return env.Result.Content[0].Text, false
}

func TestMCPReadTools_PrivateSpace_NotVisibleWithoutGrant_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	bob := d.Member(t, ws, "bob@corp.com")

	// ⚠ THE PRIVATE PAGE CARRIES THE QUERY TERM IN ITS TITLE AND THE PUBLIC ONE ONLY IN ITS BODY,
	// AND THAT IS LOAD-BEARING FOR [M-SLOT] RATHER THAN DECORATIVE. SearchWithRank weights title 'A'
	// and content 'B', so this makes the hidden row rank ABOVE the readable one DETERMINISTICALLY.
	// The first fixture put "quarterly" in both bodies and both rows scored 0.2431708425283432 —
	// identical — and `ORDER BY rank DESC` names no unique column, so which row a LIMIT 1 returns
	// was the planner's choice. [M-SLOT] would then have passed or failed on the tie, not on the
	// over-fetch. [M-ORDER] pins the ordering so that can never quietly stop being true.
	const (
		pubTitle  = "Onboarding Handbook"
		pubBody   = "quarterly onboarding checklist for every new joiner"
		privTitle = "Q3 Quarterly Layoff Plan"
		privBody  = "SECRETLAYOFF 240 roles board only do not circulate"
		privSpace = "Board Private"
	)
	pubSpaceID := seedSpaceP(t, d, ws, alice, "Public Handbook", false)
	pubPage := seedPageP(t, d, ws, pubSpaceID, alice, pubTitle, pubBody)
	privSpaceID := seedSpaceP(t, d, ws, alice, privSpace, true)
	privPage := seedPageP(t, d, ws, privSpaceID, alice, privTitle, privBody)

	// A real view event, so get_page_analytics has a number to leak. Without it the tool answers
	// all-zeros for a page that has never been read, and "bob got zeros" is satisfied by a page
	// nobody ever opened — an absence assertion that a dead fixture passes perfectly.
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO page_views (page_id, workspace_id, viewer_id, viewer_name, duration_sec)
		 VALUES ($1,$2,$3,$4,$5)`, privPage, ws, alice, "alice@corp.com", 42); err != nil {
		t.Fatalf("seed page_view: %v", err)
	}

	lens, engine := newRecordingLens(t)
	chain := readChain(t, d, engine)

	// ── ONE READING PER TOOL, PER CALLER. Each returns the tool's payload text.
	read := func(email string) (search, getPage, listPages, tree, stats string, denials map[string]bool) {
		t.Helper()
		denials = map[string]bool{}
		search, denials["search_docs"] = toolText(t, chain, email, "search_docs",
			map[string]any{"query": "quarterly", "workspace_id": ws})
		getPage, denials["get_page"] = toolText(t, chain, email, "get_page",
			map[string]any{"page_id": privPage})
		listPages, denials["list_pages"] = toolText(t, chain, email, "list_pages",
			map[string]any{"space_id": privSpaceID})
		tree, denials["get_space_tree"] = toolText(t, chain, email, "get_space_tree",
			map[string]any{"workspace_id": ws})
		stats, denials["get_page_analytics"] = toolText(t, chain, email, "get_page_analytics",
			map[string]any{"page_id": privPage})
		return
	}

	// ═══ 1. bob, NO grant. The defect, on all five tools.
	bSearch, bGetPage, bList, bTree, bStats, bDenied := read("bob@corp.com")

	// THE PREMISE COMES FIRST AND IT IS A HARD STOP. Without it "the private title is absent from
	// the search payload" is satisfied by a search that matched nothing at all — a fixture whose
	// tsquery never fired, a workspace with no rows, a tool that errored. The public page shares
	// the word "quarterly" with the private one precisely so ONE query reaches both.
	if !strings.Contains(bSearch, pubTitle) {
		t.Fatalf("[M-PREMISE] PREMISE FAILED: bob's search_docs does not carry the PUBLIC page "+
			"either, so its missing the private page proves nothing: %s", bSearch)
	}

	if strings.Contains(bSearch, privTitle) || strings.Contains(bSearch, privBody) {
		t.Errorf("[M-LEAK-SEARCH] LEAK: search_docs handed bob the private space's page. The "+
			"excerpt is ts_headline over content_text, so the payload carries the BODY, not just "+
			"a name: %s", bSearch)
	}
	if !bDenied["get_page"] {
		t.Errorf("[M-LEAK-GETPAGE] LEAK, AND THE LARGEST OF THE FIVE: get_page returned the "+
			"private page to bob — `content_text` is the WHOLE DOCUMENT. #78's list of MCP read "+
			"tools did not name this one: %s", bGetPage)
	}
	if !bDenied["list_pages"] {
		t.Errorf("[M-LEAK-LIST] LEAK: list_pages enumerated the private space for bob. The "+
			"space_id is a client-supplied arg, so naming a space is the whole attack: %s", bList)
	}
	if strings.Contains(bTree, privTitle) || strings.Contains(bTree, privSpace) {
		t.Errorf("[M-LEAK-TREE] LEAK: get_space_tree handed bob the private SPACE and its page "+
			"titles. It is workspace-keyed, so no id has to be guessed — it enumerates: %s", bTree)
	}
	if !bDenied["get_page_analytics"] {
		t.Errorf("[M-LEAK-ANALYTICS] LEAK: get_page_analytics reported readership for a page bob "+
			"cannot open — who read it, how many, how long. Also absent from #78's list: %s", bStats)
	}

	// ═══ 1b. ask_docs — THE SIXTH READ TOOL, AND THE ONE THE QUEUE ALREADY BELIEVED CLOSED.
	//
	// #79 gated `internal/ai/handler.go`'s /ask. THIS TOOL NEVER CALLS THAT HANDLER: toolAskDocs
	// runs its OWN `s.deps.pages.SearchWithRank(ctx, wsID, question, nil, 3, 0)` and hands the rows
	// straight to the model. A sweep by ROUTE finds the REST half and misses this one — #80's
	// lesson, arriving a second time in the same seam.
	//
	// ⚠ THE ASSERTION IS ON THE WIRE TO LENS, NOT ON THE ANSWER, because the leak is what the model
	// was TOLD. A row in a list can be checked in the payload; a document pasted into a prompt is
	// invisible there — the answer would be prose either way.
	askPayload, askDenied := toolText(t, chain, "bob@corp.com", "ask_docs",
		map[string]any{"question": "quarterly", "workspace_id": ws})
	askPrompt := lens.prompt()
	if askDenied || !strings.Contains(askPrompt, pubBody) {
		t.Fatalf("[M-ASKPREMISE] PREMISE FAILED: bob's ask_docs was not grounded in the PUBLIC "+
			"page either (denied=%v) — an absent private body would then prove nothing about the "+
			"gate and everything about a broken fixture. prompt=%q payload=%q",
			askDenied, askPrompt, askPayload)
	}
	if strings.Contains(askPrompt, privBody) || strings.Contains(askPrompt, privTitle) {
		t.Errorf("[M-LEAK-ASK] LEAK, AND THE ONE THE QUEUE RECORDS AS CLOSED: the private "+
			"document was pasted into the PROMPT sent to Lens. #79 gated internal/ai's /ask; this "+
			"tool has its own SearchWithRank and never calls that handler. The model is then asked "+
			"to answer FROM it, so the disclosure is prose no absence-of-a-row assertion can "+
			"see: %s", askPrompt)
	}
	if strings.Contains(askPayload, privTitle) || strings.Contains(askPayload, privPage) {
		t.Errorf("[M-LEAK-ASKCITE] LEAK: ask_docs CITED the private page back to bob in `sources` "+
			"— title and page_id, which is a working deep link: %s", askPayload)
	}

	// ═══ 2. THE OVER-CORRECTION DIRECTION, IN THE SAME SAMPLE. Denying everything satisfies every
	// assertion above perfectly, so each tool is also driven at an object bob MAY read.
	if p, denied := toolText(t, chain, "bob@corp.com", "get_page", map[string]any{"page_id": pubPage}); denied || !strings.Contains(p, pubBody) {
		t.Errorf("[M-PUBGET] OVER-CORRECTION: bob cannot get_page the PUBLIC page (denied=%v): %s", denied, p)
	}
	if p, denied := toolText(t, chain, "bob@corp.com", "list_pages", map[string]any{"space_id": pubSpaceID}); denied || !strings.Contains(p, pubTitle) {
		t.Errorf("[M-PUBLIST] OVER-CORRECTION: bob cannot list_pages the PUBLIC space (denied=%v): %s", denied, p)
	}
	if !strings.Contains(bTree, pubTitle) {
		t.Errorf("[M-PUBTREE] OVER-CORRECTION: bob's get_space_tree lost the PUBLIC space's page: %s", bTree)
	}
	if p, denied := toolText(t, chain, "bob@corp.com", "get_page_analytics", map[string]any{"page_id": pubPage}); denied || !strings.Contains(p, "total_views") {
		t.Errorf("[M-PUBANALYTICS] OVER-CORRECTION: bob cannot read analytics for the PUBLIC page (denied=%v): %s", denied, p)
	}

	// ═══ 3. alice CREATED the private space: resolveAccess's owner-is-admin arm must keep all five.
	aSearch, aGetPage, aList, aTree, aStats, aDenied := read("alice@corp.com")
	if !strings.Contains(aSearch, privTitle) || aDenied["get_page"] || aDenied["list_pages"] ||
		!strings.Contains(aTree, privTitle) || aDenied["get_page_analytics"] {
		t.Errorf("[M-OWNER] OVER-CORRECTION: the private space's CREATOR lost her own page on at "+
			"least one tool. search=%q getPage(denied=%v)=%q list(denied=%v)=%q tree=%q "+
			"stats(denied=%v)=%q", aSearch, aDenied["get_page"], aGetPage, aDenied["list_pages"],
			aList, aTree, aDenied["get_page_analytics"], aStats)
	}

	// ═══ 4. THE ORDERING [M-SLOT] DEPENDS ON, ASSERTED RATHER THAN ASSUMED. Read from ALICE, who
	// sees both rows, so it is a fact about the store's ranking and not about the gate. If the
	// readable row ever ranks first, limit=1 returns it with or without the over-fetch and [M-SLOT]
	// becomes a test that cannot fail.
	if i, j := strings.Index(aSearch, privTitle), strings.Index(aSearch, pubTitle); i < 0 || j < 0 || i > j {
		t.Errorf("[M-ORDER] the hidden page no longer ranks ABOVE the readable one (priv@%d "+
			"pub@%d) — [M-SLOT] below is vacuous in that state, because limit=1 would return the "+
			"readable row whether or not the window is over-fetched: %s", i, j, aSearch)
	}

	// A HIDDEN ROW MUST NOT EAT A RESULT SLOT. With limit=1 the private page can be the
	// only row the SQL LIMIT returns; filtering after the limit then tells bob NOTHING MATCHED for
	// a document he may open. #78 measured exactly this and over-fetches for it. Asserted while
	// bob is still DENIED, so the readable row has to arrive from a widened window.
	if p, denied := toolText(t, chain, "bob@corp.com", "search_docs",
		map[string]any{"query": "quarterly", "workspace_id": ws, "limit": 1}); denied || !strings.Contains(p, pubTitle) {
		t.Errorf("[M-SLOT] a page bob may not read consumed his only result slot at limit=1 "+
			"(denied=%v): %s — the filter runs after the SQL LIMIT, so the window must be "+
			"over-fetched before truncation", denied, p)
	}

	// ═══ 5. bob WITH an explicit 'view' grant on the private space. Granted LAST so every case
	// above ran while he was still denied. This is what proves the gate consults resolveAccess
	// rather than reading `spaces.private` and stopping.
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO permissions (resource_type, resource_id, subject_type, subject_id, access, workspace_id)
		 VALUES ('space', $1, 'member', $2, 'view', $3)`, privSpaceID, bob, ws); err != nil {
		t.Fatalf("grant: %v", err)
	}
	gSearch, gGetPage, gList, gTree, gStats, gDenied := read("bob@corp.com")
	if !strings.Contains(gSearch, privTitle) || gDenied["get_page"] || gDenied["list_pages"] ||
		!strings.Contains(gTree, privTitle) || gDenied["get_page_analytics"] {
		t.Errorf("[M-GRANT] OVER-CORRECTION: bob holds an explicit 'view' grant on the private "+
			"space and still cannot reach it on at least one tool — the gate is not asking the "+
			"permission engine. search=%q getPage(denied=%v)=%q list(denied=%v)=%q tree=%q "+
			"stats(denied=%v)=%q", gSearch, gDenied["get_page"], gGetPage, gDenied["list_pages"],
			gList, gTree, gDenied["get_page_analytics"], gStats)
	}
}
