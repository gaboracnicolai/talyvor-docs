package page_test

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// VERIFY IS AN ATTESTATION THAT NOTHING CHANGED, AND IT WAS RECORDED AS AN EDIT.
//
// `Verify` stamps `last_verified_at` + `verified_by` — Docs's "this is still accurate" claim —
// and its UPDATE also set `updated_at = NOW()`. Its own docstring names only the first two and
// says nothing about the edit clock, while FOUR other places in this same file exist solely to
// say that clock must not be touched by a non-edit:
//
//	store.go RecordView block  — the view bump "DIVERGED: it also set updated_at = NOW()",
//	                             measured to drop a 30-day-stale page off the stale list
//	store.go#appendVersionRows — "⚠⚠ IT MUST NOT TOUCH `updated_at`, AND NOTHING BUT THIS
//	                             SENTENCE STOPS IT"
//	store.go#reparent cascade  — "THE FOURTH COPY OF THE SEAM"
//	ai_spend.go                — "IT DOES NOT TOUCH updated_at, AND IT USED TO — THE THIRD COPY"
//
// Verify was the copy nobody wrote the sentence on.
//
// ⚠ WHAT IT PUTS ON A SHIPPED SCREEN, WHICH IS WHY THIS IS NOT A TIDY-UP. `FreshnessPanel`
// ships a "Mark as verified" button (FreshnessPanel.tsx) and invalidates the page query on
// success, so the page object is refetched and redrawn. `PageView.tsx` renders
//
//	Last edited by {page.updated_by || "unknown"} · {new Date(page.updated_at)}
//
// and Verify does NOT touch `updated_by`. So after bob verifies a page alice wrote a month ago,
// that footer reads "Last edited by alice · <the instant bob clicked Verify>" — a person and a
// time that never occurred together, asserting an edit that never happened, on the one control
// whose entire meaning is "I changed nothing".
//
// ⚠ AND IT MADE THE DOCUMENTED FRESHNESS RULE UNREACHABLE. freshness/engine.go#buildReport takes
// the fresher of the two timestamps —
//
//	effective := p.UpdatedAt
//	if p.LastVerifiedAt != nil && p.LastVerifiedAt.After(effective) { effective = *p.LastVerifiedAt }
//
// — commented "so an explicit re-verify 'wins' over the raw content edit timestamp — that's the
// whole point of Verify". `last_verified_at` has exactly ONE production writer (this statement;
// it is not in `updatableFields` and no migration defaults it), and that writer set BOTH columns
// from the SAME `NOW()`. Postgres evaluates `NOW()` once per transaction, so the two were always
// EXACTLY equal and `After` is strict: the branch could not be taken by any input the product
// can produce. The rule the comment calls the whole point of the feature was inert, and the
// stale-list behaviour it was credited with was actually coming from the edit-clock bump.
// TestVerifyWinsOverEditClock_IsReachable in internal/freshness is the other half of this.
//
// ⚠ WHY THE OUTPUT LOOKED RIGHT ANYWAY, which is why nothing caught it: bumping `updated_at`
// drops the page off the stale list too (GetStalePages requires BOTH clocks past the TTL), so
// every stale-list assertion in this repo passes either way. The two mechanisms agree on the one
// question the tests asked and disagree on every other reader of `updated_at`.
func TestVerify_DoesNotForgeAnEdit_RealPG(t *testing.T) {
	db := testutil.New(t)
	ctx := context.Background()
	store := page.NewStore(db.Pool)

	wsID := db.Workspace(t)
	alice := db.Member(t, wsID, "alice@example.com")
	bob := db.Member(t, wsID, "bob@example.com")
	pageID := db.Page(t, wsID, alice, "Runbook")

	// The page as a real one is: last genuinely edited by alice, 30 days ago, with a 7-day TTL.
	// Backdating is the only way to express "an edit that happened in the past" — the fixture
	// helper writes NOW() — and the TTL is what puts the row in the stale set at all.
	edited := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Second)
	if _, err := db.Pool.Exec(ctx,
		`UPDATE pages SET updated_at = $1, updated_by = $2, stale_after_days = 7 WHERE id = $3`,
		edited, alice, pageID); err != nil {
		t.Fatalf("seed edit clock: %v", err)
	}

	// PRE-CONDITION: the page is on the stale list. Without this the "still drops off" assertion
	// below is satisfied by a page that was never on it — a green that measures nothing.
	if stale, err := store.GetStalePages(ctx, wsID); err != nil {
		t.Fatalf("stale pre-check: %v", err)
	} else if !containsPage(stale, pageID) {
		t.Fatalf("pre-condition failed: seeded page is not on the stale list, so this test "+
			"cannot tell whether Verify takes it off (stale set had %d rows)", len(stale))
	}

	// The shipped chain: the authed handler calls VerifyInWorkspaces, never Verify directly.
	if err := store.VerifyInWorkspaces(ctx, pageID, bob, []string{wsID}); err != nil {
		t.Fatalf("VerifyInWorkspaces: %v", err)
	}

	var (
		gotUpdatedAt  time.Time
		gotUpdatedBy  *string
		gotVerifedAt  *time.Time
		gotVerifiedBy *string
	)
	if err := db.Pool.QueryRow(ctx,
		`SELECT updated_at, updated_by, last_verified_at, verified_by FROM pages WHERE id = $1`,
		pageID).Scan(&gotUpdatedAt, &gotUpdatedBy, &gotVerifedAt, &gotVerifiedBy); err != nil {
		t.Fatalf("read back: %v", err)
	}

	// (1) THE EDIT CLOCK IS NOT AN EDIT. Nobody edited this page, so the timestamp the footer
	// prints beside alice's name must still be alice's edit.
	if !gotUpdatedAt.UTC().Equal(edited) {
		t.Errorf("Verify moved pages.updated_at from %s to %s.\n\n"+
			"Nothing was edited. PageView renders `Last edited by {updated_by} · {updated_at}` and "+
			"Verify does not touch updated_by, so this footer now names alice beside the moment bob "+
			"clicked \"Mark as verified\" — an edit that never happened, attributed to someone who "+
			"was not there.", edited, gotUpdatedAt.UTC())
	}

	// (2) THE ATTESTATION IS STILL RECORDED. The must-stay-green half: a "fix" that simply drops
	// the write would satisfy (1) and destroy the feature.
	//
	// ⚠ AND THE PRE-EXISTING GUARD ON THIS STATEMENT CANNOT SAY IT. `TestVerify_
	// SetsTimestampAndOwner` is a pgxmock expectation on the regex `UPDATE pages SET
	// last_verified_at` — a PREFIX. It is satisfied by any statement that starts that way and
	// asserts nothing about the rest of the column list, which is measured, not inferred: removing
	// `updated_at = NOW()` from that same statement left it GREEN. Its name claims a column set it
	// does not check. These lines read the ROW rather than the SQL, which is the difference.
	if gotVerifedAt == nil {
		t.Fatal("Verify did not stamp last_verified_at — the attestation was not recorded at all")
	}
	if gotVerifiedBy == nil || *gotVerifiedBy != bob {
		t.Errorf("verified_by = %v, want %q — the attestation must name the verifier", gotVerifiedBy, bob)
	}

	// (3) THE FRESHNESS RULE IS NOW REACHABLE. buildReport's documented "a re-verify wins over the
	// raw edit timestamp" branch is `LastVerifiedAt.After(UpdatedAt)`, strictly. This asserts the
	// exact predicate that branch tests, on a row the shipped chain produced.
	if !gotVerifedAt.After(gotUpdatedAt) {
		t.Errorf("last_verified_at (%s) is not strictly after updated_at (%s).\n\n"+
			"freshness/engine.go#buildReport only prefers the verification when "+
			"LastVerifiedAt.After(UpdatedAt), and calls that \"the whole point of Verify\". While "+
			"one statement writes both columns from one NOW(), they are equal, the branch cannot "+
			"be taken by any product input, and the stale behaviour credited to it is really the "+
			"edit-clock bump.", gotVerifedAt.UTC(), gotUpdatedAt.UTC())
	}

	// (4) THE STATED PURPOSE SURVIVES. Verify's docstring promises the page drops off the stale
	// list; GetStalePages requires BOTH clocks past the TTL, so a fresh last_verified_at is
	// sufficient and the edit-clock bump was never needed for it.
	stale, err := store.GetStalePages(ctx, wsID)
	if err != nil {
		t.Fatalf("stale post-check: %v", err)
	}
	if containsPage(stale, pageID) {
		t.Errorf("the page is STILL on the stale list after Verify — the docstring's stated " +
			"purpose (\"so the page drops off the stale-pages list\") is broken")
	}
}

// TestVerify_WritesNoPageVersion_RealPG pins the claim FreshnessPanel.tsx makes about this
// endpoint. Its header said "Verifying creates a new page version server-side"; Verify is a bare
// UPDATE and touches page_versions nowhere. The comment is corrected in the same merge, and this
// is what stops the corrected text drifting back — and what would red if a later change made
// Verify snapshot, which would be a real behaviour change hiding behind a doc fix.
func TestVerify_WritesNoPageVersion_RealPG(t *testing.T) {
	db := testutil.New(t)
	ctx := context.Background()
	store := page.NewStore(db.Pool)

	wsID := db.Workspace(t)
	alice := db.Member(t, wsID, "alice@example.com")
	pageID := db.Page(t, wsID, alice, "Runbook")

	var before int
	if err := db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM page_versions WHERE page_id = $1`, pageID).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if err := store.VerifyInWorkspaces(ctx, pageID, alice, []string{wsID}); err != nil {
		t.Fatalf("VerifyInWorkspaces: %v", err)
	}
	var after int
	if err := db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM page_versions WHERE page_id = $1`, pageID).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Errorf("page_versions for this page went %d -> %d across a Verify. Verify is a bare "+
			"UPDATE; if it now snapshots, that is a behaviour change and the SPA copy describing "+
			"it must be re-derived rather than assumed.", before, after)
	}
}

func containsPage(pages []model.Page, id string) bool {
	for _, p := range pages {
		if p.ID == id {
			return true
		}
	}
	return false
}
