package changelog_test

// THE LAST ROUTE IN THE REPO STILL HOLDING #166's DECORATIVE-{wsID} SHAPE.
//
// ⚠ HOW THIS ONE WAS FOUND, because the method matters more than the defect. #166 repaired
// `/v1/workspaces/{wsID}/custom-domains` and #167 `/v1/workspaces/{wsID}/template-library`. Rather
// than assume those were the only two, every workspace-in-path route in the repo was enumerated
// from the Mount calls and checked for whether its handler READS the {wsID} it is asked about or
// hands the store `authz.WorkspaceIDs` — the caller's whole membership set — instead. Thirteen
// routes; twelve authorize the path id (page search/stale, the five ai routes, search, freshness,
// space list, analytics, approvals pending, and the trackintegration pair). `changelog/feed` was
// the one left.
//
// ⚠⚠ AND ITS COMMENT ARGUES FOR THE DEFECT IN SO MANY WORDS, which is why this is worth pinning
// rather than quietly patching. handler.go:231 said:
//
//	"SEC-4 L2 DECEPTIVE shape: the feed is scoped to the caller's VERIFIED workspace set,
//	 never the {wsID} URL param — otherwise any caller could read another workspace's
//	 published feed by naming its id in the path."
//
// Every clause is true about a STRANGER and none of it is true about the second workspace the same
// caller belongs to. "Never the URL param" is not the safe choice here; it is the bug. The safe
// choice is to AUTHORIZE the param — which is what the sibling routes do and what this change does.
//
// MEASURED on real Postgres through the shipped chain, one caller in workspaces A and B:
//
//	GET /v1/workspaces/{A}/changelog/feed → an RSS document titled "Changelog" carrying
//	                                        A's published entries AND B's
//
// ⚠ NOT A CROSS-TENANT LEAK, and [FEED-FOREIGN-WS-REFUSED] is what stops it being sold as one:
// `authz.WorkspaceIDs` is the verified membership set, so every item belongs to a workspace the
// caller is really in. The defect is SCOPE — a feed that names one workspace and syndicates two.
// It matters because this route's output is a SYNDICATION FORMAT: an RSS reader is handed a URL
// that says "workspace A" and every item in it is presented as A's release notes.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/changelog"
	"github.com/talyvor/docs/internal/testutil"
)

type feedScope struct {
	srv  http.Handler
	d    *testutil.DB
	wsA  string
	wsB  string
	wsC  string
	mail string
}

// newFeedScope seeds ONE caller with memberships in TWO workspaces plus a third she is not in.
// The second membership is the whole instrument: with one workspace this defect cannot be seen.
func newFeedScope(t *testing.T) *feedScope {
	t.Helper()
	d := testutil.New(t)
	f := &feedScope{d: d, srv: clChain(d), mail: "alice@corp.com"}
	f.wsA = d.Workspace(t)
	f.wsB = d.Workspace(t)
	f.wsC = d.Workspace(t) // alice is deliberately NOT a member of C
	d.Member(t, f.wsA, f.mail)
	d.Member(t, f.wsB, f.mail)
	d.Member(t, f.wsC, "mallory@corp.com")
	return f
}

// publish seeds a PUBLISHED entry in wsID and returns its title. Published is the precondition
// GetPublicFeed filters on (`published_at IS NOT NULL`), so an unpublished seed would leave every
// assertion below passing for a reason that has nothing to do with scope.
func (f *feedScope) publish(t *testing.T, wsID, title string) string {
	t.Helper()
	ctx := context.Background()
	member := f.d.Member(t, wsID, f.mail+".seed")
	pageID := f.d.Page(t, wsID, member, title+" Page")
	store := changelog.NewStore(f.d.Pool, nil)
	e, err := store.CreateEntry(ctx, changelog.ChangelogEntry{
		PageID: pageID, WorkspaceID: wsID, Version: "1.0.0", Title: title,
		Summary: title + " summary", Type: changelog.EntryFeature, CreatedBy: member,
	})
	if err != nil {
		t.Fatalf("[SETUP] create entry %q in %s: %v", title, wsID, err)
	}
	if _, err := store.PublishEntry(ctx, e.ID, pageID, []string{wsID}); err != nil {
		t.Fatalf("[SETUP] publish entry %q in %s: %v", title, wsID, err)
	}
	// Read the row back rather than trust the write — the feed filters on published_at, and a
	// seed that silently left it NULL makes every assertion below vacuous.
	var published bool
	if err := f.d.Pool.QueryRow(ctx,
		`SELECT published_at IS NOT NULL FROM changelog_entries WHERE id = $1`, e.ID,
	).Scan(&published); err != nil || !published {
		t.Fatalf("[SETUP] entry %q in %s is not published after PublishEntry (err=%v)", title, wsID, err)
	}
	return title
}

func (f *feedScope) feed(t *testing.T, wsID string) (int, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+wsID+"/changelog/feed", nil)
	req.Header.Set("X-Gateway-Auth", clSecret)
	req.Header.Set("X-User-Email", f.mail)
	f.srv.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// ─── THE FINDING ────────────────────────────────────

func TestFeed_IsScopedToThePathWorkspace_RealPG(t *testing.T) {
	f := newFeedScope(t)
	f.publish(t, f.wsA, "Alpha Release")
	f.publish(t, f.wsB, "Beta Release")

	code, body := f.feed(t, f.wsA)
	// An assertion, not setup: asking for the feed of a workspace you ARE in must answer 200.
	// This is the positive control for the narrowing below — a fix that 404s everything would
	// otherwise satisfy it.
	if code != http.StatusOK {
		t.Fatalf("[FEED-OWN-WORKS] GET workspace A's own feed answered %d — expected 200; a scope "+
			"fix that refuses the caller's own workspace is not a fix. Body: %s", code, body)
	}

	// ── LIVENESS FLOOR. "B's item is absent" is satisfied by an EMPTY feed, and an empty feed is
	// what a broken filter, a broken seed or an unpublished entry all produce.
	if !strings.Contains(body, "Alpha Release") {
		t.Fatalf("[FEED-OWN-ITEM-PRESENT] workspace A's OWN published entry is missing from its own "+
			"feed — the scoping assertion below would pass vacuously on an empty document. Body: %s",
			body)
	}

	if strings.Contains(body, "Beta Release") {
		t.Errorf("[FEED-SCOPED] GET /v1/workspaces/%s/changelog/feed syndicated workspace B's "+
			"published changelog entry. Feed passes authz.WorkspaceIDs (the caller's WHOLE "+
			"membership set) to GetPublicFeed and never reads the {wsID} it was asked about — so an "+
			"RSS document whose URL names ONE workspace presents another workspace's release notes "+
			"as that workspace's. Body: %s", f.wsA, body)
	}
}

// TestFeed_TheOtherWorkspaceStillSyndicatesItsOwn_RealPG is the positive control from B's side:
// "scoped" must not be achieved by serving nobody anything.
func TestFeed_TheOtherWorkspaceStillSyndicatesItsOwn_RealPG(t *testing.T) {
	f := newFeedScope(t)
	f.publish(t, f.wsA, "Alpha Release")
	f.publish(t, f.wsB, "Beta Release")

	code, body := f.feed(t, f.wsB)
	if code != http.StatusOK {
		t.Fatalf("[FEED-B-OWN-WORKS] GET workspace B's own feed answered %d — expected 200. Body: %s",
			code, body)
	}
	if !strings.Contains(body, "Beta Release") {
		t.Errorf("[FEED-B-OWN-ITEM-PRESENT] workspace B's own published entry is missing from B's "+
			"feed — a scope fix that syndicates nothing is not a fix. Body: %s", body)
	}
	if strings.Contains(body, "Alpha Release") {
		t.Errorf("[FEED-SCOPED-B] workspace B's feed carried workspace A's entry. Body: %s", body)
	}
}

// TestFeed_UnpublishedNeverSyndicates_RealPG is the must-stay-green that is NOT about scope: the
// feed's other filter. Narrowing the workspace scope must not be mistaken for — or accidentally
// replace — `published_at IS NOT NULL`, which is what keeps a draft off the wire.
func TestFeed_UnpublishedNeverSyndicates_RealPG(t *testing.T) {
	f := newFeedScope(t)
	f.publish(t, f.wsA, "Alpha Release")

	ctx := context.Background()
	member := f.d.Member(t, f.wsA, f.mail+".draft")
	pageID := f.d.Page(t, f.wsA, member, "Draft Page")
	if _, err := changelog.NewStore(f.d.Pool, nil).CreateEntry(ctx, changelog.ChangelogEntry{
		PageID: pageID, WorkspaceID: f.wsA, Version: "9.9.9", Title: "Unpublished Draft",
		Summary: "must not syndicate", Type: changelog.EntryFeature, CreatedBy: member,
	}); err != nil {
		t.Fatalf("[SETUP] create draft entry: %v", err)
	}

	_, body := f.feed(t, f.wsA)
	if !strings.Contains(body, "Alpha Release") {
		t.Fatalf("[FEED-DRAFT-FLOOR] the published entry is missing, so 'the draft is absent' says "+
			"nothing. Body: %s", body)
	}
	if strings.Contains(body, "Unpublished Draft") {
		t.Errorf("[FEED-DRAFT-EXCLUDED] an UNPUBLISHED entry was syndicated. Body: %s", body)
	}
}

// ─── MUST-STAY-GREEN: the tenancy boundary itself ───

// TestFeed_ForeignWorkspaceIsStillRefused_RealPG pins what this finding is NOT. wsC is a workspace
// alice has no membership in.
//
// ⚠ THIS HALF IS RED BEFORE THE FIX TOO, AND IT IS A DIFFERENT STATEMENT FROM THE ONE ABOVE: today
// /workspaces/{C}/changelog/feed answers 200 carrying ALICE'S OWN entries, because nothing on the
// route reads {wsID} at all. No row of C's escapes — so this is not a leak; it is the same
// decorative-path-param defect seen from the other side, and it is why the fix must AUTHORIZE the
// param rather than merely intersect with it.
func TestFeed_ForeignWorkspaceIsStillRefused_RealPG(t *testing.T) {
	f := newFeedScope(t)
	f.publish(t, f.wsA, "Alpha Release")
	f.publish(t, f.wsC, "Gamma Release") // C's own entry — mallory's workspace, not alice's

	code, body := f.feed(t, f.wsC)
	if code == http.StatusOK {
		t.Errorf("[FEED-FOREIGN-WS-REFUSED] GET /v1/workspaces/%s/changelog/feed — a workspace the "+
			"caller is NOT a member of — answered 200. Body: %s", f.wsC, body)
	}
	// The harder claim: whatever the status, C's entry must never appear.
	if strings.Contains(body, "Gamma Release") {
		t.Errorf("[FEED-FOREIGN-WS-NO-ITEM] workspace C's published entry was syndicated to a "+
			"caller who is not a member of C — that IS a cross-tenant leak. Body: %s", body)
	}
	// And alice's own feed still works, so the refusal above is not measuring a dead route.
	if code, own := f.feed(t, f.wsA); code != http.StatusOK || !strings.Contains(own, "Alpha Release") {
		t.Errorf("[FEED-FOREIGN-OWN-INTACT] after the foreign request, A's own feed answered %d and "+
			"%s its own entry — the refusal above would be measuring an empty database", code,
			map[bool]string{true: "carried", false: "did NOT carry"}[strings.Contains(own, "Alpha Release")])
	}
}
