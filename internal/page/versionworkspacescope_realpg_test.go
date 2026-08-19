package page_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// THE SECOND LAYER migration 0015 SAYS THE VERSION READS ALREADY HAVE.
//
// 0015_page_versions_workspace.sql added page_versions.workspace_id and an index on
// (workspace_id, page_id, version DESC), and states two things about the reads:
//
//	"it is defense-in-depth plus the direct-scope filter the new get-one / compare reads use"
//	"The new get-one / compare reads scope on (workspace_id, page_id, version)."
//
// Neither was true when this file was written. Every read of page_versions in the repository
// scoped on page_id alone; workspace_id appeared in exactly two statements, both INSERTs. So the
// column was written and never used as a predicate, the index's leading column was never bound by
// the queries it was built for, and the advertised depth was one layer — the same
// JOIN-to-pages posture (assertInWorkspaces) that 0015 says it improved on.
//
// WHAT THIS TEST ASSERTS, AND WHY IT HAS TO TAMPER WITH A ROW. Layer 1 authorizes the PAGE. It
// therefore cannot, by construction, notice a version row whose own recorded tenant is somebody
// else's — the page genuinely belongs to the caller, so the assert genuinely passes. A test that
// only used ordinary writes would be measuring layer 1 twice and would pass with layer 2 absent,
// which is exactly how the claim survived. The tampered row is the ONLY state in which the two
// layers give different answers, so it is the only state in which the second one is observable.
//
// ⚠ THIS IS NOT A CLAIM THAT PRODUCTION CAN PRODUCE THAT ROW TODAY, and the measurement is
// recorded rather than implied: both INSERTs take workspace_id from the page row the caller's own
// write returned (store.go — `out.WorkspaceID`), never from a request; `workspace_id` is not in
// page.updatableFields and no production statement UPDATEs pages.workspace_id, so a page never
// changes tenant and a version's recorded tenant always equals its page's. That is precisely why
// the missing filter was invisible — and why adding it hides no legitimate history.
func TestVersionReads_ScopeDirectlyOnWorkspaceID_RealPG(t *testing.T) {
	d := testutil.New(t)
	wsA := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	wsB := d.Workspace(t)
	d.Member(t, wsB, "bob@corp.com")

	pA := d.Page(t, wsA, alice, "A's runbook")
	store := page.NewStore(d.Pool)
	ctx := context.Background()

	// Three committed saves → versions 1, 2 and 3, all recorded against wsA.
	for _, rev := range []string{`{"rev":1}`, `{"rev":2}`, `{"rev":3}`} {
		if _, err := store.Update(ctx, pA, map[string]any{"content": rev, "updated_by": alice}); err != nil {
			t.Fatalf("save %s: %v", rev, err)
		}
	}

	// The state layer 2 exists for: ONE version row now records a tenant that is not the page's.
	// The page is untouched and still lives in wsA, so assertInWorkspaces still passes for wsA.
	if _, err := d.Pool.Exec(ctx,
		`UPDATE page_versions SET workspace_id = $1 WHERE page_id = $2 AND version = 2`,
		wsB, pA); err != nil {
		t.Fatalf("seed the mismatched version row: %v", err)
	}

	// Sanity: layer 1 is genuinely still satisfied. If this ever fails the test below is
	// measuring the page gate rather than the version scope, and proves nothing.
	if _, err := store.GetVersionInWorkspaces(ctx, pA, 3, []string{wsA}); err != nil {
		t.Fatalf("layer 1 precondition: wsA must still reach its own page's versions, got %v", err)
	}

	t.Run("get-one refuses a version row recorded to another workspace", func(t *testing.T) {
		v, err := store.GetVersionInWorkspaces(ctx, pA, 2, []string{wsA})
		if !errors.Is(err, page.ErrNotFound) {
			t.Errorf("get-one v2 = (%v, %v), want ErrNotFound — the read must scope on workspace_id, not page_id alone", v, err)
		}
	})

	t.Run("compare refuses when either side is recorded to another workspace", func(t *testing.T) {
		from, to, err := store.CompareVersionsInWorkspaces(ctx, pA, 1, 2, []string{wsA})
		if !errors.Is(err, page.ErrNotFound) {
			t.Errorf("compare(1,2) = (%v, %v, %v), want ErrNotFound on the `to` side", from, to, err)
		}
		from, to, err = store.CompareVersionsInWorkspaces(ctx, pA, 2, 3, []string{wsA})
		if !errors.Is(err, page.ErrNotFound) {
			t.Errorf("compare(2,3) = (%v, %v, %v), want ErrNotFound on the `from` side", from, to, err)
		}
	})

	t.Run("the list omits it", func(t *testing.T) {
		vers, err := store.GetVersionsInWorkspaces(ctx, pA, []string{wsA})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		got := make([]int, 0, len(vers))
		for _, v := range vers {
			got = append(got, v.Version)
			if v.WorkspaceID != wsA {
				t.Errorf("list returned a row recorded to %s; every row a caller sees must be its own tenant's", v.WorkspaceID)
			}
		}
		if len(got) != 2 {
			t.Errorf("list = versions %v, want the two wsA rows [3 1] — the wsB row must not be listed", got)
		}
	})

	// RESTORE IS THE ONE THAT WRITES, so it is asserted separately and its effect is read back
	// rather than inferred from the error. A restore that reaches a foreign row does not merely
	// leak it — it copies that row's title and content ONTO the live page.
	t.Run("restore refuses it, and the live page is unchanged", func(t *testing.T) {
		before, err := store.GetByID(ctx, pA)
		if err != nil {
			t.Fatalf("read live page before: %v", err)
		}
		if _, err := store.RestoreVersionInWorkspaces(ctx, pA, 2, []string{wsA}); !errors.Is(err, page.ErrNotFound) {
			t.Errorf("restore v2 = %v, want ErrNotFound", err)
		}
		after, err := store.GetByID(ctx, pA)
		if err != nil {
			t.Fatalf("read live page after: %v", err)
		}
		if after.Content != before.Content {
			t.Errorf("live content moved from %q to %q — a refused restore must write nothing", before.Content, after.Content)
		}
	})

	// MUST STAY GREEN. Without this the whole file is satisfied by a read that returns
	// ErrNotFound for everything, which is a different broken product with the same test result.
	t.Run("the caller's own rows are still fully readable", func(t *testing.T) {
		v1, err := store.GetVersionInWorkspaces(ctx, pA, 1, []string{wsA})
		if err != nil || v1.Version != 1 || v1.WorkspaceID != wsA {
			t.Fatalf("owner get v1 = (%v, %v), want version 1 in %s", v1, err, wsA)
		}
		from, to, err := store.CompareVersionsInWorkspaces(ctx, pA, 1, 3, []string{wsA})
		if err != nil || from.Version != 1 || to.Version != 3 {
			t.Fatalf("owner compare(1,3) = (%v, %v, %v), want (1,3)", from, to, err)
		}
		if _, err := store.RestoreVersionInWorkspaces(ctx, pA, 1, []string{wsA}); err != nil {
			t.Fatalf("owner restore v1: %v", err)
		}
	})
}
