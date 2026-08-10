package ai_test

// THE THIRD COPY OF THE SEAM `internal/search` WAS HARDENED FOR, AND IT IS THE ONE THAT ANSWERS
// IN PROSE.
//
// `internal/search` (fixed) and `page.Handler`'s /pages/search + /pages/stale (fixed by #78) both
// authorized the WORKSPACE and stopped. So does this one:
//
//	internal/ai/handler.go — Handler.Ask
//	    authz.AuthorizeWorkspace(ctx, wsID)            // the workspace, and nothing else
//	    pages, _ := h.pages.Search(ctx, wsID, q, 3)    // page.Store.Search — WHERE workspace_id = $1
//	    ctxPages = {Title, ContentText, URL} …         // → the model's grounding context
//	    sources  = {Title, URL} …                      // → the caller's response
//
// `page.Store.Search` is the SAME store method #78 gated. Nothing between it and the answer asks
// the permission engine about a space or a page, and `cmd/docs/main.go` gives `aiHandler` no
// access wiring of any kind — it is the only handler in the mount block with none.
//
// ⚠⚠ THE PAYLOAD IS A DIFFERENT KIND, WHICH IS WHY THIS NEEDED ITS OWN MEASUREMENT AND ITS OWN
// ASSERTION SHAPE. The other two leaked ROWS, and a row is absent or present. Here the private
// document's `content_text` is pasted VERBATIM into the prompt, so the leak is a sentence a model
// composed — and no absence-of-a-row assertion can see it. MEASURED THROUGH THE REAL ROUTE ON REAL
// POSTGRES WITH A REAL lensintegration.Client against a Lens that records what it was sent, before
// any change (probe at `d736678`, bob a workspace member with NO grant on a private space):
//
//	RESPONSE TO BOB   {"answer":"Yes, 240 roles are being cut.",
//	                   "sources":[{"title":"Quarterly Board Memo","url":"/spaces/…/pages/…"}, …]}
//	PROMPT TO LENS    Question: layoff
//	                  Documentation:
//	                  --- Page 1: Quarterly Board Memo ---
//	                  SECRETLAYOFF 240 roles board only layoff programme
//
// So the private memo was (a) named to bob with a working deep link, (b) copied to a third-party
// model on bob's request, and (c) used to ANSWER him. The AskAI panel is mounted in
// `frontend/src/router/Layout.tsx` on every signed-in screen behind Cmd+Shift+A, so this is the
// ordinary surface, not a corner of the API.
//
// ⚠ AND THE HIDDEN ROWS EAT THE CONTEXT WINDOW, which is why the fixture seeds THREE private
// pages above one public one. `Ask` asks for the top 3 and `Store.Search` is `ORDER BY updated_at
// DESC`, so a filter applied to those three alone leaves bob with ZERO grounding — a confident
// answer from an empty corpus, which is exactly the failure `#57` closed at the MCP tool. The
// window has to be over-fetched, and [B-SLOT] is that assertion.
//
// THE FOUR DIRECTIONS ARE ONE TEST ON PURPOSE, so a fix that simply drops private spaces, or one
// that empties the context, fails here rather than passing quietly:
//
//	1. bob, no grant             the private pages MUST NOT be cited or quoted   ← the defect
//	2. bob, no grant             the PUBLIC page MUST still ground the answer    ← the slot
//	3. bob, explicit view grant  the private page MUST come back                 ← not `private = false`
//	4. alice, the space creator  the private page MUST come back                 ← owner-is-admin

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
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/spaceauth"
	"github.com/talyvor/docs/internal/testutil"
)

// seedSpaceA / seedPageA are this file's own, not borrowed from internal/page's guard. A helper
// lends the evidence of the test it was written for; these seed what THIS claim is about — a
// `content_text` long enough to be quotable, and an `updated_at` that decides which rows the
// top-3 context window takes.
func seedSpaceA(t *testing.T, d *testutil.DB, wsID, creator, name string, private bool) string {
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

func seedPageA(t *testing.T, d *testutil.DB, wsID, spaceID, author, title, body string, ageDays int) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text, content, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NOW() - make_interval(days => $8)) RETURNING id`,
		spaceID, wsID, title, "pg-"+title+"-"+wsID, author, body,
		`{"type":"doc","content":[{"type":"paragraph","text":"`+body+`"}]}`, ageDays,
	).Scan(&id); err != nil {
		t.Fatalf("seed page %q: %v", title, err)
	}
	return id
}

// recordingLens is a REAL HTTP Lens that keeps every completion body it was sent. The prompt is
// the payload under test here, so a stub engine would measure nothing: the whole claim is about
// what crosses the process boundary on the caller's behalf.
type recordingLens struct {
	mu      sync.Mutex
	prompts []string
	url     string
}

func newRecordingLens(t *testing.T, answer string) *recordingLens {
	t.Helper()
	rl := &recordingLens{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/token" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"token":"tok","expires_at":%q}`,
				time.Now().Add(time.Hour).Format(time.RFC3339))))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		rl.mu.Lock()
		rl.prompts = append(rl.prompts, string(raw))
		rl.mu.Unlock()
		_, _ = w.Write([]byte(fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, answer)))
	}))
	t.Cleanup(s.Close)
	rl.url = s.URL
	return rl
}

func (rl *recordingLens) last() string {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if len(rl.prompts) == 0 {
		return ""
	}
	return rl.prompts[len(rl.prompts)-1]
}

type askOut struct {
	Answer  string `json:"answer"`
	Sources []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"sources"`
}

func (o askOut) cites(title string) bool {
	for _, s := range o.Sources {
		if s.Title == title {
			return true
		}
	}
	return false
}

func (o askOut) titles() []string {
	out := make([]string, 0, len(o.Sources))
	for _, s := range o.Sources {
		out = append(out, s.Title)
	}
	return out
}

func TestAskAI_PrivateSpace_NotGroundedOrCitedWithoutGrant_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	bob := d.Member(t, ws, "bob@example.com")

	// One shared query token, so every page below is a genuine full-text match and "absent" can
	// never mean "did not match".
	const q = "layoff"

	// A PRIVATE space created by alice. bob is a workspace member with NO grant, so
	// permission.resolveAccess gives him AccessNone on it.
	//
	// THREE pages, and they are the NEWEST — Store.Search is `ORDER BY updated_at DESC` and Ask
	// takes the top 3, so an unfiltered window is exactly these and the public page is number
	// FOUR. That is what makes [B-SLOT] sharp rather than decorative.
	privSpace := seedSpaceA(t, d, ws, alice, "Board Private", true)
	privPages := []struct{ title, body string }{
		{"Quarterly Board Memo", "SECRETLAYOFFALPHA the layoff programme cuts 240 roles, board only"},
		{"Severance Model", "SECRETLAYOFFBETA the layoff severance model is 12 weeks, board only"},
		{"Site Closure List", "SECRETLAYOFFGAMMA the layoff closes the Tallinn site, board only"},
	}
	for i, p := range privPages {
		seedPageA(t, d, ws, privSpace, alice, p.title, p.body, i+1)
	}

	// A PUBLIC space + page every member may read. Oldest, so it is the row a filter that runs
	// after the top-3 truncation never reaches.
	pubSpace := seedSpaceA(t, d, ws, alice, "Public Handbook", false)
	const pubTitle = "Onboarding Handbook"
	const pubBody = "PUBLICHANDBOOKBODY the layoff policy everyone in the company may read"
	seedPageA(t, d, ws, pubSpace, alice, pubTitle, pubBody, 100)

	store := page.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)

	// cmd/docs/main.go's pageLooker, re-derived here rather than imported: this is the metadata
	// the permission engine decides on, and borrowing it would borrow another test's evidence.
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

	lens := newRecordingLens(t, "Yes — 240 roles are being cut and severance is 12 weeks.")
	engine := ai.New(lensintegration.New(lens.url, "k1").
		WithTokenProvider(lenscreds.New(lens.url, "k1", lenscreds.Options{})))

	// ask drives the REAL route through the REAL handler, wired as cmd/docs/main.go wires it.
	ask := func(email, memberID string) askOut {
		t.Helper()
		h := ai.NewHandler(engine, store).
			WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))

		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				c := authz.WithMemberships(req.Context(), email,
					[]authz.Membership{{WorkspaceID: ws, MemberID: memberID}})
				next.ServeHTTP(w, req.WithContext(c))
			})
		})
		r.Route("/v1", func(r chi.Router) { h.Mount(r) })

		body, _ := json.Marshal(map[string]string{"question": q})
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/ai/ask",
			strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST /ai/ask as %s = %d, want 200: %s", email, rr.Code, rr.Body.String())
		}
		var out askOut
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode ask as %s: %v (%s)", email, err, rr.Body.String())
		}
		return out
	}

	// ── 1 + 2. bob, no grant.
	//
	// THE PREMISE IS DELIBERATELY THE WEAKEST TRUE ONE. It asserts only that the engine was
	// reached and given a question — never that the public page arrived, because on the
	// unfixed tree it does NOT, and a premise that fails there would abort before the two leak
	// assertions this test exists for were ever evaluated.
	bobOut := ask("bob@example.com", bob)
	prompt := lens.last()
	if !strings.Contains(prompt, "Question: "+q) {
		t.Fatalf("[B-PREMISE] PREMISE FAILED: Lens was never sent bob's question — every "+
			"assertion below would be measuring an empty prompt. body=%q", prompt)
	}

	for _, p := range privPages {
		if strings.Contains(prompt, p.body) {
			t.Errorf("[B-LEAK-PROMPT] LEAK: bob has NO grant on the private space and its document "+
				"body was pasted into the prompt sent to Lens — %q reached a third-party model, "+
				"and the answer returned to him is grounded in it.\n  answer: %s\n  prompt: %s",
				p.body, bobOut.Answer, prompt)
		}
		if bobOut.cites(p.title) {
			t.Errorf("[B-LEAK-SOURCE] LEAK: bob has NO grant on the private space and the answer "+
				"CITED its page %q with a deep link. sources=%v", p.title, bobOut.titles())
		}
	}

	// THE HIDDEN ROWS MUST NOT CONSUME THE CONTEXT WINDOW. Three private pages sort above the
	// public one and Ask takes the top 3, so a filter applied to that window alone leaves bob
	// grounded in NOTHING — the confident-answer-from-an-empty-corpus shape #57 closed at the
	// MCP tool. This is also the positive control inside the sample: it proves the query matched
	// and the route served, so "no private page" can never be read off an empty prompt.
	if !strings.Contains(prompt, pubBody) {
		t.Errorf("[B-SLOT] bob's answer is not grounded in the PUBLIC page he may read: its body "+
			"never reached the prompt. Three pages he may NOT read sort above it and Ask takes "+
			"the top 3, so the window must be over-fetched and filtered BEFORE it is truncated.\n"+
			"  prompt: %s", prompt)
	}
	if !bobOut.cites(pubTitle) {
		t.Errorf("[B-SLOTSRC] bob's answer cites %v — the public page he may read is not among "+
			"them", bobOut.titles())
	}

	// ── 4. alice created the private space: resolveAccess's owner-is-admin arm must keep it.
	aliceOut := ask("alice@example.com", alice)
	if !aliceOut.cites(privPages[0].title) {
		t.Errorf("[B-OWNER] OVER-CORRECTION: the private space's CREATOR cannot get her own page "+
			"back — owner-is-admin lost. sources=%v", aliceOut.titles())
	}

	// ── 3. bob WITH an explicit view grant on the private space. Granted LAST, so cases 1, 2
	// and 4 run while he is still denied. This is the assertion a blanket "drop private spaces"
	// fails.
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO permissions (resource_type, resource_id, subject_type, subject_id, access, workspace_id)
		 VALUES ('space', $1, 'member', $2, 'view', $3)`,
		privSpace, bob, ws); err != nil {
		t.Fatalf("grant: %v", err)
	}
	grantedOut := ask("bob@example.com", bob)
	if !grantedOut.cites(privPages[0].title) {
		t.Errorf("[B-GRANT] OVER-CORRECTION: bob holds an explicit 'view' grant on the private "+
			"space and the answer still will not cite its page — the filter is not consulting "+
			"resolveAccess. sources=%v", grantedOut.titles())
	}
}
