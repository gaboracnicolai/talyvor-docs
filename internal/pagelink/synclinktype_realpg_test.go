package pagelink_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelink"
	"github.com/talyvor/docs/internal/testutil"
)

// SYNCLINKS' REMOVAL PASS SAYS IT ONLY TOUCHES EMBED-TYPED ROWS. ITS `DELETE` NAMES NO
// link_type, SO REMOVING AN INLINE EMBED ALSO DELETES THE USER'S MANUAL ANNOTATION FOR THE
// SAME ISSUE.
//
//	pagelink/store.go SyncLinks:  "We only touch embed-typed links here so "mention" / "spec"
//	                               rows added via the UI survive a content edit."
//	pagelink/store.go Delete:     DELETE FROM page_links WHERE page_id = $1 AND issue_id = $2
//
// UNIQUE (page_id, issue_id, link_type) exists so one page can hold several typed links to the
// same issue, and both producers ship: the slash-menu embed writes `embed` through SyncLinks,
// and the panel's "Link issue" affordance (PageView.tsx LinkedIssuesSection) posts to
// POST /v1/pages/{pageID}/links, which defaults an unnamed type to `mention` (handler.go).
// Both rows render in the same panel, which ListByPage serves unfiltered.
//
// So the sequence that loses data is the ordinary one: link an issue from the panel, embed the
// same issue inline, then delete the embed from the body. The next content save reconciles the
// embed away and takes the mention with it — silently, because page.Store.Update discards
// SyncLinks' error at its only call site.
//
// THE TWO ASSERTIONS ARE ONE TEST ON PURPOSE. [MENTION-SURVIVES] alone is satisfied by a
// "fix" that stops removing anything, which would leave stale embeds behind and, through
// IssueIDsForPage, keep billing the page for an issue it no longer references.
// [EMBED-REMOVED] is the floor that refuses that.
func TestSyncLinks_RemovesTheEmbedAndKeepsTheManualMention_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pageID := d.Page(t, ws, alice, "Runbook")

	// The page store wired exactly as main.go wires it: the real pagelink store is the linker.
	linkStore := pagelink.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool).WithLinker(linkStore)
	chain := linkChain(d)

	// 1. The panel's affordance: no link_type in the body, so the route defaults it to "mention".
	body := `{"issue_id":"ENG-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/pages/"+pageID+"/links", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Auth", linkSecret)
	req.Header.Set("X-User-Email", "alice@corp.com")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("POST link = %d, want 2xx: %s", rr.Code, rr.Body.String())
	}
	if got := linkTypes(t, d, pageID, "ENG-1"); !has(got, "mention") {
		t.Fatalf("precondition: the route did not write a `mention` row, got %v — this test is "+
			"about that row surviving, so it must exist first", got)
	}

	// 2. The same issue embedded inline. SyncLinks adds the `embed` row beside the mention.
	if _, err := pageStore.Update(ctx, pageID, map[string]any{
		"content":      embedDoc("ENG-1"),
		"content_text": "see ENG-1",
		"updated_by":   alice,
	}); err != nil {
		t.Fatalf("save with the embed: %v", err)
	}
	if got := linkTypes(t, d, pageID, "ENG-1"); !has(got, "embed") || !has(got, "mention") {
		t.Fatalf("precondition: after the embedding save want both `embed` and `mention`, got %v", got)
	}

	// 3. The author deletes the embed from the body. Content changed, so the hook runs.
	if _, err := pageStore.Update(ctx, pageID, map[string]any{
		"content":      emptyDoc(),
		"content_text": "see nothing",
		"updated_by":   alice,
	}); err != nil {
		t.Fatalf("save without the embed: %v", err)
	}

	// Read it back the way the panel does — ListByPage, through the authorized GET route.
	after := listViaRoute(t, chain, pageID)
	var gotEmbed, gotMention bool
	for _, l := range after {
		if l.IssueID != "ENG-1" {
			continue
		}
		switch l.LinkType {
		case "embed":
			gotEmbed = true
		case "mention":
			gotMention = true
		}
	}
	if !gotMention {
		t.Errorf("[MENTION-SURVIVES] the manual `mention` row for ENG-1 is GONE after the embed was "+
			"removed from the content. SyncLinks' removal pass deletes by (page_id, issue_id) with no "+
			"link_type, so it destroyed a row it documents itself as leaving alone. links now = %v", after)
	}
	if gotEmbed {
		t.Errorf("[EMBED-REMOVED] the `embed` row for ENG-1 survived a save whose content no longer "+
			"embeds it — the reconcile did not happen. IssueIDsForPage reads exactly these rows for the "+
			"ai_cost_usd roll-up, so a leftover embed keeps the page billed for an issue it dropped. "+
			"links now = %v", after)
	}

	// THE ROUTE'S DELETE IS THE OTHER HALF OF THE SAME DISTINCTION AND IS ASSERTED HERE SO THE
	// REPAIR CANNOT SPREAD INTO IT. The panel's remove button is DELETE /pages/{id}/links/{issueID}
	// — no type in the URL, because "unlink this issue" means the pair, not one row of it. Scoping
	// that statement by `embed` too would leave the chip on screen after a click that reported ok:
	// true, and nothing else in this repo drives it on real Postgres.
	req = httptest.NewRequest(http.MethodDelete, "/v1/pages/"+pageID+"/links/ENG-1", nil)
	req.Header.Set("X-Gateway-Auth", linkSecret)
	req.Header.Set("X-User-Email", "alice@corp.com")
	rr = httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE link = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if got := linkTypes(t, d, pageID, "ENG-1"); len(got) != 0 {
		t.Errorf("[ROUTE-DELETE-UNTYPED] after the panel's remove button the pair still holds %v — "+
			"the route unlinks an ISSUE, so it must clear every typed row for it, not just `embed`", got)
	}
}

// embedDoc is the ProseMirror shape ParseEmbeds looks for.
func embedDoc(issueID string) string {
	return `{"type":"doc","content":[{"type":"issue_embed","attrs":{"issue_id":"` + issueID + `"}}]}`
}

func emptyDoc() string {
	return `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"see nothing"}]}]}`
}

func linkTypes(t *testing.T, d *testutil.DB, pageID, issueID string) []string {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(),
		`SELECT link_type FROM page_links WHERE page_id=$1 AND issue_id=$2 ORDER BY link_type`,
		pageID, issueID)
	if err != nil {
		t.Fatalf("read link types: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan link type: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read link types: %v", err)
	}
	return out
}

func has(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func listViaRoute(t *testing.T, chain http.Handler, pageID string) []pagelink.PageLink {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/pages/"+pageID+"/links", nil)
	req.Header.Set("X-Gateway-Auth", linkSecret)
	req.Header.Set("X-User-Email", "alice@corp.com")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET links = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var out []pagelink.PageLink
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode links: %v (%s)", err, rr.Body.String())
	}
	return out
}
