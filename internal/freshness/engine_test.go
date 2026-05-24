package freshness

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/trackintegration"
)

// fakePageStore stubs the page-lookup dependency. Each test wires a
// canned page so we can exercise the threshold math without a real
// Postgres.
type fakePageStore struct {
	byID  map[string]*model.Page
	stale []model.Page
}

func (f *fakePageStore) GetByID(_ context.Context, id string) (*model.Page, error) {
	return f.byID[id], nil
}
func (f *fakePageStore) GetStalePages(_ context.Context, _ string) ([]model.Page, error) {
	return f.stale, nil
}

// fakeLinks returns linked issues for a page. We test the "linked
// issues completed since last edit" surface by canning what the
// engine sees.
type fakeLinks struct {
	byPage map[string][]string
}

func (f *fakeLinks) IssueIDsForPage(_ context.Context, pageID string) ([]string, error) {
	return f.byPage[pageID], nil
}

// fakeTrack mimics the Track client. The engine calls GetIssue per
// linked ID; we hand back canned IssueRefs with status / done flags.
type fakeTrack struct {
	configured bool
	issues     map[string]*trackintegration.IssueRef
}

func (f *fakeTrack) IsConfigured() bool { return f.configured }
func (f *fakeTrack) GetIssue(_ context.Context, _, id string) (*trackintegration.IssueRef, error) {
	return f.issues[id], nil
}

func freshPage(id string, updatedDaysAgo int, staleAfter int) *model.Page {
	now := time.Now().UTC().Add(-time.Duration(updatedDaysAgo) * 24 * time.Hour)
	return &model.Page{
		ID:             id,
		Title:          "P-" + id,
		WorkspaceID:    "ws-1",
		StaleAfterDays: staleAfter,
		UpdatedAt:      now,
		CreatedAt:      now.Add(-24 * time.Hour),
	}
}

func newEngine(pages *fakePageStore, links *fakeLinks, track *fakeTrack) *FreshnessEngine {
	return newFreshnessEngine(pages, links, track)
}

func TestGetStatus_FreshWhenWellWithinThreshold(t *testing.T) {
	p := freshPage("pg-1", 5, 30) // 5 of 30 days used → fresh
	e := newEngine(&fakePageStore{byID: map[string]*model.Page{"pg-1": p}}, &fakeLinks{}, &fakeTrack{})

	r, err := e.GetStatus(context.Background(), "pg-1")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if r.Status != FreshnessFresh {
		t.Fatalf("want fresh, got %q", r.Status)
	}
	if r.DaysSinceEdit != 5 {
		t.Fatalf("days_since_edit = %d, want 5", r.DaysSinceEdit)
	}
}

func TestGetStatus_WarningAt50PercentOfThreshold(t *testing.T) {
	p := freshPage("pg-1", 16, 30) // 16 / 30 = 53% → warning
	e := newEngine(&fakePageStore{byID: map[string]*model.Page{"pg-1": p}}, &fakeLinks{}, &fakeTrack{})

	r, err := e.GetStatus(context.Background(), "pg-1")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if r.Status != FreshnessWarning {
		t.Fatalf("want warning, got %q", r.Status)
	}
}

func TestGetStatus_StaleWhenPastThreshold(t *testing.T) {
	p := freshPage("pg-1", 45, 30)
	e := newEngine(&fakePageStore{byID: map[string]*model.Page{"pg-1": p}}, &fakeLinks{}, &fakeTrack{})

	r, _ := e.GetStatus(context.Background(), "pg-1")
	if r.Status != FreshnessStale {
		t.Fatalf("want stale, got %q", r.Status)
	}
}

func TestGetStatus_UnknownWhenNoThreshold(t *testing.T) {
	p := freshPage("pg-1", 365, 0) // no expiry
	e := newEngine(&fakePageStore{byID: map[string]*model.Page{"pg-1": p}}, &fakeLinks{}, &fakeTrack{})

	r, _ := e.GetStatus(context.Background(), "pg-1")
	if r.Status != FreshnessUnknown {
		t.Fatalf("want unknown, got %q", r.Status)
	}
}

func TestGetStatus_VerifiedTrumpsUpdatedAt(t *testing.T) {
	p := freshPage("pg-1", 45, 30) // would be stale on updated_at
	verified := time.Now().UTC().Add(-3 * 24 * time.Hour)
	p.LastVerifiedAt = &verified
	e := newEngine(&fakePageStore{byID: map[string]*model.Page{"pg-1": p}}, &fakeLinks{}, &fakeTrack{})

	r, _ := e.GetStatus(context.Background(), "pg-1")
	if r.Status != FreshnessFresh {
		t.Fatalf("verified-within-window should be fresh, got %q", r.Status)
	}
	if r.DaysSinceVerify == nil || *r.DaysSinceVerify != 3 {
		t.Fatalf("days_since_verify wrong: %+v", r.DaysSinceVerify)
	}
}

func TestGetStatus_SuggestsReviewWhenLinkedIssuesClosed(t *testing.T) {
	p := freshPage("pg-1", 5, 30) // would normally be fresh
	track := &fakeTrack{
		configured: true,
		issues: map[string]*trackintegration.IssueRef{
			"i-1": {ID: "i-1", Status: "done"},
			"i-2": {ID: "i-2", Status: "in_progress"},
			"i-3": {ID: "i-3", Status: "cancelled"},
		},
	}
	links := &fakeLinks{byPage: map[string][]string{"pg-1": {"i-1", "i-2", "i-3"}}}
	e := newEngine(&fakePageStore{byID: map[string]*model.Page{"pg-1": p}}, links, track)

	r, _ := e.GetStatus(context.Background(), "pg-1")
	// "done" + "cancelled" count as completed → 2.
	if r.LinkedIssuesClosed != 2 {
		t.Fatalf("linked closed = %d, want 2", r.LinkedIssuesClosed)
	}
	if !r.SuggestReview {
		t.Fatal("expected SuggestReview when linked issues closed")
	}
	if r.Reason == "" {
		t.Fatal("expected Reason to be populated")
	}
}

func TestGetStatus_NoTrackDoesNotSurfaceLinkedIssueCount(t *testing.T) {
	p := freshPage("pg-1", 5, 30)
	links := &fakeLinks{byPage: map[string][]string{"pg-1": {"i-1"}}}
	e := newEngine(
		&fakePageStore{byID: map[string]*model.Page{"pg-1": p}},
		links,
		&fakeTrack{configured: false},
	)
	r, _ := e.GetStatus(context.Background(), "pg-1")
	if r.LinkedIssuesClosed != 0 || r.SuggestReview {
		t.Fatalf("when Track unconfigured, no closed-issue count: %+v", r)
	}
}

func TestGetStaleReport_SortsByStalenessThenDaysSinceEdit(t *testing.T) {
	stale := []model.Page{
		// 8 days over the 30-day threshold.
		*freshPage("pg-a", 38, 30),
		// 100 days over the 60-day threshold.
		*freshPage("pg-b", 160, 60),
		// 5 days over the 30-day threshold.
		*freshPage("pg-c", 35, 30),
	}
	pages := &fakePageStore{stale: stale}
	e := newEngine(pages, &fakeLinks{}, &fakeTrack{})

	out, err := e.GetStaleReport(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("GetStaleReport: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("want 3 reports, got %d", len(out))
	}
	// All three are stale; sort should be by days_since_edit DESC.
	if !(out[0].DaysSinceEdit >= out[1].DaysSinceEdit && out[1].DaysSinceEdit >= out[2].DaysSinceEdit) {
		t.Fatalf("not sorted: %+v", out)
	}
}
