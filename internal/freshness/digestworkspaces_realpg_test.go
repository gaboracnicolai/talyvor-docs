package freshness_test

// THE DAILY STALE DIGEST RAN AGAINST A WORKSPACE ID THAT EXISTS IN NO DEPLOYMENT.
//
// `cmd/docs/main.go` started the 09:00 UTC batch as `freshEngine.Start(ctx, cfg.DefaultWorkspaceID)`.
// `DOCS_DEFAULT_WORKSPACE` defaults to the literal string **"default"** and appears in no compose
// file and no README, while workspace ids are Track's — minted per identity at login, shaped
// `ws_<hex>`. So the digest's population was a name nothing matches.
//
// ⚠ MEASURED BEFORE THE FIX, through the method main.go actually ran, on real Postgres: two
// workspaces each holding one page stale by the shipped `GetStalePages` predicate, and
//
//	SendStaleDigest(ctx, "default")  ->  workspace=default stale_pages=0 warning_pages=0
//	SendStaleDigest(ctx, ws_fe1338…) ->  workspace=ws_fe1338… stale_pages=1 warning_pages=0
//	SendStaleDigest(ctx, ws_4ab5c9…) ->  workspace=ws_4ab5c9… stale_pages=1 warning_pages=0
//
// A structural zero, logged daily as a measurement. "Nothing needs attention" and "we asked about
// a tenant that does not exist" were the same line, and the feature this batch exists for is the
// one the product calls the difference between a doc tool and a living spec.
//
// ⚠ THIRD INSTANCE OF A CLASS THIS BINARY HAD ALREADY NAMED AND FIXED TWICE. See
// Syncer.costWorkspaces: "It used to be answered with a single pinned config value while
// SyncMembers thirty lines below enumerated every workspace". Both of those enumerate now. This
// loop lives in a different struct, wired forty lines away in main.go, and was never looked at.
//
// ⚠ RED-FIRST IS UNDEFINED FOR THE +ALL ASSERTIONS AND THAT IS STATED RATHER THAN IMPLIED: the
// entry point they drive did not exist before, so they could not have been red — they would not
// have compiled. What stands in for red is the measurement quoted above, plus control C2 in
// ~/talyvor-queue/w31-digestworkspaces-controls-2d7a.py, which restores the pinned-config call
// (`SendStaleDigestAll` replaced by a digest of `cfg.DefaultWorkspaceID`) and turns this file red.
//
// ⚠ [C-QUIET] IS WHAT STOPS THE OTHER ASSERTIONS PASSING FOR THE WRONG REASON. Without it, "the
// digest visits every workspace with a stale page" is satisfied by a sweep that visits only
// workspaces WITH stale pages — which is a different, smaller claim, and one that cannot tell a
// workspace that is healthy from a workspace that was never asked.

import (
	"context"
	"fmt"
	"testing"

	"github.com/talyvor/docs/internal/freshness"
	"github.com/talyvor/docs/internal/membership"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelink"
	"github.com/talyvor/docs/internal/testutil"
)

// theConfigDefault is the literal internal/config gives DefaultWorkspaceID when
// DOCS_DEFAULT_WORKSPACE is unset — which is every deployment, since the variable is in no
// compose file. Named here so the assertion below is about the shipped value, not a guess.
const theConfigDefault = "default"

func TestStaleDigest_CoversEveryWorkspaceWithContent_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	pages := page.NewStore(d.Pool)

	// seed returns a workspace holding `stale` pages past their TTL and `fresh` pages inside it.
	seed := func(stale, fresh int) string {
		t.Helper()
		ws := d.Workspace(t)
		owner := d.Member(t, ws, "owner@corp.com")
		anchor := d.Page(t, ws, owner, "Anchor")
		var spaceID string
		if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, anchor).Scan(&spaceID); err != nil {
			t.Fatalf("read seed space: %v", err)
		}
		n := 0
		mk := func(title string, ancient bool) {
			t.Helper()
			n++
			p, err := pages.Create(ctx, model.Page{
				SpaceID: spaceID, WorkspaceID: ws, Title: fmt.Sprintf("%s %d", title, n),
				CreatedBy: owner, StaleAfterDays: 30,
			})
			if err != nil {
				t.Fatalf("seed page: %v", err)
			}
			if ancient {
				if _, err := d.Pool.Exec(ctx,
					`UPDATE pages SET updated_at = NOW() - INTERVAL '400 days' WHERE id = $1`, p.ID); err != nil {
					t.Fatalf("age page: %v", err)
				}
			}
		}
		for i := 0; i < stale; i++ {
			mk("Ancient", true)
		}
		for i := 0; i < fresh; i++ {
			mk("Recent", false)
		}
		return ws
	}

	wsOne := seed(1, 0)   // one overdue page
	wsTwo := seed(2, 0)   // two overdue pages — so the counts are per workspace, not a total
	wsQuiet := seed(0, 1) // content, nothing overdue — the workspace that must be VISITED, not skipped

	eng := freshness.New(pages, pagelink.NewStore(d.Pool), nil).
		WithWorkspaces(membership.NewStore(d.Pool))

	got, err := eng.SendStaleDigestAll(ctx)
	if err != nil {
		t.Fatalf("SendStaleDigestAll: %v", err)
	}
	by := map[string]freshness.DigestSummary{}
	for _, s := range got {
		by[s.WorkspaceID] = s
	}

	// [C-ONE] / [C-TWO] — every workspace with overdue pages is in the digest, with ITS OWN count.
	if s, ok := by[wsOne]; !ok || s.Stale != 1 {
		t.Errorf("[C-ONE] the digest reports %+v for the workspace holding 1 overdue page (present=%v); "+
			"want stale=1. The batch used to run against cfg.DefaultWorkspaceID alone.", s, ok)
	}
	if s, ok := by[wsTwo]; !ok || s.Stale != 2 {
		t.Errorf("[C-TWO] the digest reports %+v for the workspace holding 2 overdue pages (present=%v); "+
			"want stale=2 — the counts are per workspace, not one total", s, ok)
	}

	// [C-QUIET] — a workspace with content and nothing overdue is VISITED and reports zero. This
	// is what separates "the sweep covers the workspaces" from "the sweep covers the workspaces
	// that happen to have findings", which are different claims and only one of them is a digest.
	if s, ok := by[wsQuiet]; !ok || s.Stale != 0 {
		t.Errorf("[C-QUIET] the digest %+v (present=%v) for a workspace with content and nothing "+
			"overdue; want it PRESENT with stale=0 — a workspace that was never asked and a "+
			"workspace that is healthy must not be the same answer", s, ok)
	}

	// [C-NOT-PINNED] — the literal the old call site handed the batch is not a workspace, so it
	// must not appear. If it ever does, the enumeration has started inventing names.
	if s, ok := by[theConfigDefault]; ok {
		t.Errorf("[C-NOT-PINNED] the digest reports %+v for %q, which is internal/config's DEFAULT "+
			"STRING and not a workspace id in any deployment", s, theConfigDefault)
	}
}

func TestStaleDigest_WithNoEnumeratorRefusesRatherThanReportingZero_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	// An engine wired exactly as main.go wires it, MINUS the enumerator.
	eng := freshness.New(page.NewStore(d.Pool), pagelink.NewStore(d.Pool), nil)

	// [C-NO-ENUM] — a missing enumerator is a server misconfiguration and is reported as one. An
	// empty digest is not a neutral answer: it is the positive claim "no workspace has anything
	// overdue", which is the exact false statement the pinned-config default produced.
	got, err := eng.SendStaleDigestAll(ctx)
	if err == nil {
		t.Errorf("[C-NO-ENUM] SendStaleDigestAll with no enumerator returned %v and NO error — an "+
			"unwired sweep must refuse, not report an empty world", got)
	}
	if len(got) != 0 {
		t.Errorf("[C-NO-ENUM] SendStaleDigestAll with no enumerator returned %d summaries, want 0", len(got))
	}
}
