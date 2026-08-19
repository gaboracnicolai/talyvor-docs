package freshness

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/model"
)

// THE RULE buildReport CALLS "THE WHOLE POINT OF VERIFY" COULD NOT BE TAKEN BY ANY REAL PAGE.
//
// buildReport prefers the verification over the raw edit timestamp:
//
//	effective := p.UpdatedAt
//	if p.LastVerifiedAt != nil && p.LastVerifiedAt.After(effective) { effective = *p.LastVerifiedAt }
//
// "Use the fresher of UpdatedAt vs LastVerifiedAt so an explicit re-verify 'wins' over the raw
// content edit timestamp — that's the whole point of Verify."
//
// `last_verified_at` had exactly ONE production writer — page.Store.Verify — and that statement
// also set `updated_at = NOW()`. Postgres evaluates NOW() once per transaction, so the two
// columns came out EXACTLY equal (measured: both `21:01:27.845267`), and `After` is strict. No
// input the product could produce reached this branch; the freshness it was credited with came
// from the edit-clock bump instead. page.TestVerify_DoesNotForgeAnEdit_RealPG is the other half
// and is where the bump is removed.
//
// ⚠⚠ A CORRECTION TO MY OWN CLAIM, MEASURED BY THE CONTROL RATHER THAN ASSUMED, AND IT SHARPENS
// THE FINDING RATHER THAN SOFTENING IT. The branch was NOT untested: `TestGetStatus_
// VerifiedTrumpsUpdatedAt` has asserted it since the engine was written, and control C3 (blind
// the rule) reds it as well as the first case here. What that test never had — and could not
// have — is any evidence the state it constructs is REACHABLE. It hand-builds a page with
// LastVerifiedAt after UpdatedAt and was green for the entire life of a product in which no such
// row could exist. A passing test over a state the product cannot produce is the whole shape of
// this defect, one level up: the arithmetic was pinned, the reachability was assumed, and the
// assumption was false for as long as anyone had been reading the green.
//
// So this file is deliberately NOT a re-statement of that case. What it adds is the assertion
// nobody had written on either side — `report.updated_at` is still the CONTENT clock (below) —
// which is the one that would have caught the original defect from the freshness direction. And
// it is load-bearing only in company: page.TestVerify_DoesNotForgeAnEdit_RealPG proves the state
// now occurs, this proves the engine does the documented thing when it does. Neither alone says
// the feature works, which is why the merge carries both.
func TestVerifyWinsOverEditClock_IsReachable(t *testing.T) {
	// The state the fix makes producible: edited 30 days ago, verified yesterday, 7-day TTL.
	// Before the fix, last_verified_at could never exceed updated_at, so no row looked like this.
	now := time.Now().UTC()
	verified := now.Add(-24 * time.Hour)
	edited := now.Add(-30 * 24 * time.Hour)
	p := &model.Page{
		ID:             "pg-verify",
		Title:          "Runbook",
		WorkspaceID:    "ws-1",
		StaleAfterDays: 7,
		UpdatedAt:      edited,
		CreatedAt:      edited,
		LastVerifiedAt: &verified,
	}
	e := newEngine(&fakePageStore{byID: map[string]*model.Page{"pg-verify": p}}, &fakeLinks{}, &fakeTrack{})

	r, err := e.GetStatus(context.Background(), "pg-verify")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}

	// The verification wins: 30 days past a 7-day TTL by the edit clock alone, and the attestation
	// is one day old, so the page is fresh. This is the branch's entire purpose.
	if r.Status != FreshnessFresh {
		t.Errorf("status = %q, want %q — the page was re-verified yesterday against a 7-day TTL, "+
			"so the verification must win over a 30-day-old edit. Reason: %q", r.Status, FreshnessFresh, r.Reason)
	}
	if r.DaysSinceEdit != 1 {
		t.Errorf("days_since_edit = %d, want 1 — buildReport measures from the FRESHER of the two "+
			"clocks, so a page verified yesterday reads 1 however old the edit is", r.DaysSinceEdit)
	}

	// ⚠ AND THE RAW EDIT TIMESTAMP IS STILL REPORTED UNCHANGED. This is the assertion that would
	// have caught the original defect from this side: `UpdatedAt` is a separate field precisely so
	// a reader can see when the CONTENT last changed, independently of the attestation. If Verify
	// re-dates the row, this field carries the verification time and the distinction the report's
	// own shape draws is gone.
	if !r.UpdatedAt.Equal(edited) {
		t.Errorf("report.updated_at = %s, want the real edit time %s — the report carries "+
			"days_since_edit AND updated_at as separate facts; the second must stay the content "+
			"clock or an attestation is indistinguishable from an edit", r.UpdatedAt, edited)
	}
	if r.DaysSinceVerify == nil || *r.DaysSinceVerify != 1 {
		t.Errorf("days_since_verify = %v, want 1", r.DaysSinceVerify)
	}
}

// TestVerifyDoesNotWinWhenOlderThanTheEdit is the companion that keeps the rule from becoming
// "a verification always wins". A page verified long ago and edited yesterday must measure from
// the EDIT — otherwise `effective` would be a max in name only and a stale attestation could
// hold a genuinely-stale page off the list. Without this, deleting the `.After` guard entirely
// (taking LastVerifiedAt unconditionally) would leave the test above green.
func TestVerifyDoesNotWinWhenOlderThanTheEdit(t *testing.T) {
	now := time.Now().UTC()
	verified := now.Add(-30 * 24 * time.Hour)
	edited := now.Add(-24 * time.Hour)
	p := &model.Page{
		ID:             "pg-edit",
		Title:          "Runbook",
		WorkspaceID:    "ws-1",
		StaleAfterDays: 7,
		UpdatedAt:      edited,
		CreatedAt:      verified,
		LastVerifiedAt: &verified,
	}
	e := newEngine(&fakePageStore{byID: map[string]*model.Page{"pg-edit": p}}, &fakeLinks{}, &fakeTrack{})

	r, err := e.GetStatus(context.Background(), "pg-edit")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if r.DaysSinceEdit != 1 {
		t.Errorf("days_since_edit = %d, want 1 — the EDIT is the fresher clock here, so the "+
			"30-day-old verification must not be preferred", r.DaysSinceEdit)
	}
	if r.Status != FreshnessFresh {
		t.Errorf("status = %q, want %q", r.Status, FreshnessFresh)
	}
}
