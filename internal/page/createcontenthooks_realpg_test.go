package page_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelink"
	"github.com/talyvor/docs/internal/pagelock"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
	"github.com/talyvor/docs/internal/trackintegration"
)

// A PAGE BORN WITH CONTENT WAS BORN WITHOUT ITS CONTENT'S CONSEQUENCES: NO ISSUE LINK, SO IT
// COST $0.00 FOREVER, AND NO EMBEDDING, SO SEMANTIC SEARCH COULD NOT SEE IT — UNTIL SOMEBODY
// HAPPENED TO EDIT IT.
//
// page.Store.Update ends with two content-derived hooks: SyncLinks (reconcile page_links from
// the issue_embed nodes in the body) and IndexPage (embed the text for pgvector). A whole-tree
// census puts BOTH in exactly one place — store.go's Update — and NOWHERE in Create. The
// documented boot backfill for the second one, SemanticSearch.IndexAllPages ("Boots can call
// this when the embeddings table is empty"), has ZERO callers in this repository, tests
// included, so nothing catches up later either.
//
// MEASURED at 552bb43 on real Postgres through the shipped chain (gatewayauth + authz +
// permission.Enforcer), one fixture, the SAME content bytes both times:
//
//	POST  /v1/spaces/{sp}/pages   {"content": <doc with issue_embed ISSUE-42>} -> 201,
//	                                page_links rows = 0, indexer calls = 0
//	PATCH /v1/spaces/{sp}/pages/{pg} {"content": <the identical doc>}          -> 200,
//	                                page_links rows = 1, indexer calls = 1
//
// ⚠ THE MONEY HALF IS THE ONE THAT READS AS AN ANSWER RATHER THAN AS AN ABSENCE.
// trackintegration.Syncer sums a page's cost from pagelink.IssueIDsForPage — which reads those
// very rows (`WHERE link_type = 'embed'`) — and pageTotal treats an EMPTY issue list as a
// COMPLETE answer ("Zero linked issues is a COMPLETE answer, not an unknown one: the page's
// cost is $0.00 and gets written"). That rule is right; it is fed a set that was never
// populated. So the sweep does not stall and does not log: it writes $0.00 to ai_cost_usd with
// full confidence, and "this spec cost $342 to ship" reads as a measured zero.
//
// ⚠ CREATING WITH CONTENT IS NOT A CORNER, IT IS THREE SHIPPED PRODUCERS. Handler.Create
// decodes the WHOLE model.Page from the body (see the INSERT's own comment), the MCP
// `create_page` tool takes a `content` argument an agent authors, and internal/importer builds
// every Confluence and Notion page with Content + ContentText set. An imported 5,000-page space
// is 5,000 pages that no semantic query can reach and whose cost is a confident zero.
//
// ⚠ WHAT THIS IS NOT: not a tenancy or authorization defect — nothing here crosses a workspace,
// and the hooks the fix adds are the same two, on the same row, that the very next PATCH would
// have run. It is a reporting-integrity and search-coverage defect.
//
// ⚠ WHY NOTHING SAW IT: every existing test of the two hooks drives Update
// (versioning_title_only_save_test.go pins that a title-only save does NOT run them, which is
// the split this repo cared about), and the create tests assert the row that Create INSERTs.
// No test asked what Create does AFTER the insert, so the absence had nothing to disagree with.

const embedIssueID = "ISSUE-42"

// patchedIssueID is the SECOND issue, embedded by the PATCH in [PATCH-PARITY]. It must differ
// from embedIssueID or that assertion is satisfied by the row Create already wrote.
const patchedIssueID = "ISSUE-99"

// embedDocJSON is one ProseMirror doc carrying one issue_embed — the shape pagelink.ParseEmbeds
// walks. [PATCH-PARITY] reuses it with the issue id swapped for patchedIssueID, so the create
// path and the update path differ in nothing but which of the two they are.
const embedDocJSON = `{"type":"doc","content":[` +
	`{"type":"paragraph","content":[{"type":"text","text":"the rollout plan"}]},` +
	`{"type":"issue_embed","attrs":{"issue_id":"` + embedIssueID + `"}}]}`

// embeddedIssueCost is what the (faked) Track says ISSUE-42 cost. Any non-zero value works; a
// distinctive one keeps it from colliding with a default.
const embeddedIssueCost = 42.00

// chanIndexer records IndexPage calls on a channel. A channel and not a counter because both
// hooks are deliberately asynchronous (Update runs the embed in a goroutine, and the fix keeps
// that shape) — a counter read straight after the response is a race that passes by luck.
type chanIndexer struct{ seen chan string }

func newChanIndexer() *chanIndexer { return &chanIndexer{seen: make(chan string, 8)} }

func (c *chanIndexer) IndexPage(_ context.Context, pageID, _, _ string) error {
	select {
	case c.seen <- pageID:
	default:
	}
	return nil
}

// await waits up to d for an IndexPage call naming pageID. Returns false on timeout.
func (c *chanIndexer) await(pageID string, d time.Duration) bool {
	deadline := time.After(d)
	for {
		select {
		case got := <-c.seen:
			if got == pageID {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// fakeTrack is the cost half of trackintegration's Client: configured, and priced. It exists so
// the money assertion runs through the REAL Syncer and the REAL pagelink read rather than
// through a re-implementation of them.
type fakeTrack struct{ cost map[string]float64 }

func (f fakeTrack) IsConfigured() bool { return true }

func (f fakeTrack) IssueCost(_ context.Context, _, issueID string) (float64, error) {
	return f.cost[issueID], nil
}

type createHooksFixture struct {
	d       *testutil.DB
	router  *chi.Mux
	pages   *page.Store
	links   *pagelink.Store
	idx     *chanIndexer
	ws      string
	spaceID string
	email   string
}

func newCreateHooksFixture(t *testing.T) *createHooksFixture {
	t.Helper()
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	email := "alice-createhooks@example.com"
	alice := d.Member(t, ws, email)
	seed := d.Page(t, ws, alice, "seed")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, seed).Scan(&spaceID); err != nil {
		t.Fatalf("lookup space: %v", err)
	}

	permStore := permission.NewStore(d.Pool)
	links := pagelink.NewStore(d.Pool)
	idx := newChanIndexer()
	spaceStore := space.NewStore(d.Pool)
	pages := page.NewStore(d.Pool).
		WithGuard(pagelock.NewStore(d.Pool)).
		WithLinker(links).
		WithIndexer(idx)

	pageLooker := func(ctx context.Context, id string) (permission.PageMeta, error) {
		pg, err := pages.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
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
	spaceLooker := func(ctx context.Context, id string) (permission.SpaceMeta, error) {
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.SpaceMeta{}, err
		}
		return permission.SpaceMeta{WorkspaceID: sp.WorkspaceID, CreatedBy: sp.CreatedBy, Private: sp.Private}, nil
	}
	h := page.NewHandler(pages, d.Pool)
	h.WithAccess(
		permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore)),
		permission.NewEnforcer(permStore, permission.SpaceResolverFromParam("spaceID", spaceLooker)),
	)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(testGatewaySecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return &createHooksFixture{d: d, router: r, pages: pages, links: links, idx: idx, ws: ws, spaceID: spaceID, email: email}
}

func (f *createHooksFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Auth", testGatewaySecret)
	req.Header.Set("X-User-Email", f.email)
	rr := httptest.NewRecorder()
	f.router.ServeHTTP(rr, req)
	return rr
}

// create POSTs one page and returns its id, failing on anything but a 201.
func (f *createHooksFixture) create(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal create body: %v", err)
	}
	rr := f.do(t, http.MethodPost, "/v1/spaces/"+f.spaceID+"/pages", string(raw))
	if rr.Code != http.StatusCreated {
		t.Fatalf("[CREATE-201] POST /pages = %d %s — every assertion below would be about a page "+
			"that was never made", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.ID == "" {
		t.Fatalf("[CREATE-201] create response carries no id: %v body=%s", err, rr.Body.String())
	}
	return out.ID
}

func (f *createHooksFixture) embedLinks(t *testing.T, pageID string) []string {
	t.Helper()
	rows, err := f.d.Pool.Query(context.Background(),
		`SELECT issue_id FROM page_links WHERE page_id=$1 AND link_type='embed' ORDER BY issue_id`, pageID)
	if err != nil {
		t.Fatalf("read page_links: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan page_links: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read page_links: %v", err)
	}
	return out
}

func (f *createHooksFixture) trackCost(t *testing.T, pageID string) float64 {
	t.Helper()
	var v float64
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT ai_cost_usd FROM pages WHERE id=$1`, pageID).Scan(&v); err != nil {
		t.Fatalf("read ai_cost_usd: %v", err)
	}
	return v
}

// TestCreate_WithEmbeddedIssue_LinksAndCosts_RealPG is the money half: the link row, and the
// figure the product puts in front of a human once the shipped sweep has run over it.
func TestCreate_WithEmbeddedIssue_LinksAndCosts_RealPG(t *testing.T) {
	f := newCreateHooksFixture(t)
	ctx := context.Background()

	created := f.create(t, map[string]any{
		"title":        "Rollout spec",
		"content":      embedDocJSON,
		"content_text": "the rollout plan",
	})

	// [LINK-ON-CREATE] — the row the whole cost roll-up reads.
	if got := f.embedLinks(t, created); len(got) != 1 || got[0] != embedIssueID {
		t.Errorf("[LINK-ON-CREATE] a page CREATED with an issue embed has embed links %v, want [%s]. "+
			"SyncLinks runs in Update and in no other place, so the issue this document is about "+
			"is recorded nowhere until somebody edits the page.", got, embedIssueID)
	}

	// [COST-ON-CREATE] — through the SHIPPED syncer, not a re-implementation of it. No member
	// sync and no membership store wired ⇒ costWorkspaces() falls back to the pinned workspace,
	// which is this fixture's.
	syncer := trackintegration.NewSyncer(
		fakeTrack{cost: map[string]float64{embedIssueID: embeddedIssueCost}},
		f.pages, f.links, f.ws)
	syncer.SyncPageCosts(ctx)

	if got := f.trackCost(t, created); got != embeddedIssueCost {
		t.Errorf("[COST-ON-CREATE] after a full cost sweep the page reports ai_cost_usd=%.2f, want "+
			"%.2f. The sweep did not fail and did not log: pageTotal reads an EMPTY issue list as a "+
			"COMPLETE answer, so $0.00 is written with confidence and the cost-per-doc figure is a "+
			"measured zero.", got, embeddedIssueCost)
	}

	// [PATCH-PARITY] — the must-stay-green half, and the reason the two live in one test: the
	// finding is that CREATE and UPDATE disagreed about the same bytes, so a "fix" that MOVED the
	// hook off Update instead of ADDING it to Create would satisfy everything above.
	//
	// ⚠ IT PATCHES IN A DIFFERENT ISSUE ON PURPOSE, AND THE FIRST VERSION OF THIS ASSERTION DID
	// NOT. Re-sending the same doc and re-reading the same row is satisfied by the row Create just
	// wrote, whether or not Update's hook ran at all — a guard that cannot fail. Swapping the embed
	// makes the assertion require BOTH halves of SyncLinks' diff, the removal and the addition.
	patched := strings.ReplaceAll(embedDocJSON, embedIssueID, patchedIssueID)
	if rr := f.do(t, http.MethodPatch, "/v1/spaces/"+f.spaceID+"/pages/"+created,
		`{"content":`+strconv.Quote(patched)+`}`); rr.Code != http.StatusOK {
		t.Fatalf("[PATCH-PARITY] PATCH = %d %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if got := f.embedLinks(t, created); len(got) != 1 || got[0] != patchedIssueID {
		t.Errorf("[PATCH-PARITY] after a content save that swapped the embedded issue the links are "+
			"%v, want [%s] — Update's own reconcile must still run. The hook belongs in BOTH places.",
			got, patchedIssueID)
	}
}

// TestCreate_WithContent_IsSemanticallyIndexed_RealPG is the search half, plus the two
// counter-assertions that keep the fix a filter rather than a blanket.
func TestCreate_WithContent_IsSemanticallyIndexed_RealPG(t *testing.T) {
	f := newCreateHooksFixture(t)

	// [INDEX-ON-CREATE] — the hook is asynchronous by design, so this waits rather than reads.
	created := f.create(t, map[string]any{
		"title":        "Rollout spec",
		"content":      embedDocJSON,
		"content_text": "the rollout plan",
	})
	if !f.idx.await(created, 3*time.Second) {
		t.Errorf("[INDEX-ON-CREATE] a page created WITH content was never handed to the semantic " +
			"indexer. IndexPage is called from Update alone, and SemanticSearch.IndexAllPages — the " +
			"documented boot backfill — has no caller in this repository, so the page stays invisible " +
			"to every semantic query until somebody edits it. Imported spaces are born this way.")
	}

	// [TEMPLATE-NOT-INDEXED] — Update skips templates ("Templates skip indexing"); Create must
	// skip them for the same reason and not widen the rule while closing the gap. GREEN BEFORE
	// THE FIX (nothing indexed on create at all), so it is earned by a control, not by red-first.
	tpl := f.create(t, map[string]any{
		"title":        "Boilerplate",
		"content":      embedDocJSON,
		"content_text": "the rollout plan",
		"is_template":  true,
	})
	if f.idx.await(tpl, 750*time.Millisecond) {
		t.Errorf("[TEMPLATE-NOT-INDEXED] a TEMPLATE created with content was indexed. Update excludes " +
			"templates and so does every other reader of pages in this repo; Create must not be the " +
			"one place that disagrees.")
	}

	// [BLANK-NOT-INDEXED] — the SPA's "+ New page" posts a page with no content. Enqueuing an
	// embed for empty text is a scheduled no-op per blank page; the predicate that stops it is
	// the Create-time analogue of Update's `contentChanged`. Also green before the fix.
	blank := f.create(t, map[string]any{"title": "Untitled"})
	if f.idx.await(blank, 750*time.Millisecond) {
		t.Errorf("[BLANK-NOT-INDEXED] a page created with NO content was handed to the indexer. " +
			"Update runs the content hooks only when the content changed; Create must run them only " +
			"when there is content to derive from.")
	}
}
