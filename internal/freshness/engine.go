// Package freshness owns Docs's "is this still accurate?" surface.
// Pages declare a stale_after_days TTL; the engine reads each
// page's edit / verify timestamps + linked-Track-issue activity to
// classify it as fresh / warning / stale / unknown and surface why.
// This is the differentiating feature that turns a doc tool into a
// living spec — the spec stays connected to the work it describes.
package freshness

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/trackintegration"
)

type FreshnessStatus string

const (
	FreshnessFresh   FreshnessStatus = "fresh"
	FreshnessWarning FreshnessStatus = "warning"
	FreshnessStale   FreshnessStatus = "stale"
	FreshnessUnknown FreshnessStatus = "unknown"
)

// warningRatio is the share of stale_after_days that flips a page to
// "warning". 50% matches the spec — past the halfway point the doc
// has aged enough that a casual reader should be reminded the
// timestamp is approaching its expiry.
const warningRatio = 0.5

type FreshnessReport struct {
	PageID             string          `json:"page_id"`
	SpaceID            string          `json:"space_id"`
	Title              string          `json:"title"`
	Status             FreshnessStatus `json:"status"`
	DaysSinceEdit      int             `json:"days_since_edit"`
	DaysSinceVerify    *int            `json:"days_since_verify,omitempty"`
	StaleAfterDays     int             `json:"stale_after_days"`
	VerifiedBy         *string         `json:"verified_by,omitempty"`
	LinkedIssuesClosed int             `json:"linked_issues_closed"`
	SuggestReview      bool            `json:"suggest_review"`
	Reason             string          `json:"reason"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

// pageReader is the narrow read surface freshness needs from the
// page store. We accept this rather than the full *page.Store so
// the engine can be unit-tested without spinning up a real DB.
type pageReader interface {
	GetByID(ctx context.Context, id string) (*model.Page, error)
	GetStalePages(ctx context.Context, workspaceID string) ([]model.Page, error)
}

type linkReader interface {
	IssueIDsForPage(ctx context.Context, pageID string) ([]string, error)
}

// trackReader is the narrow Track surface the engine uses. The real
// *trackintegration.Client satisfies this; tests stub it.
type trackReader interface {
	IsConfigured() bool
	GetIssue(ctx context.Context, workspaceID, issueID string) (*trackintegration.IssueRef, error)
}

type FreshnessEngine struct {
	pages pageReader
	links linkReader
	track trackReader
}

func New(pages pageReader, links linkReader, track trackReader) *FreshnessEngine {
	return newFreshnessEngine(pages, links, track)
}

func newFreshnessEngine(pages pageReader, links linkReader, track trackReader) *FreshnessEngine {
	return &FreshnessEngine{pages: pages, links: links, track: track}
}

// GetStatus computes a single page's freshness report. Errors are
// returned only for genuine failures (DB down) — a missing page
// returns (nil, nil) so the handler can map it to a 404.
func (e *FreshnessEngine) GetStatus(ctx context.Context, pageID string) (*FreshnessReport, error) {
	p, err := e.pages.GetByID(ctx, pageID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	return e.buildReport(ctx, p), nil
}

// GetStaleReport returns every page in the workspace that's past
// (or approaching) its TTL. The list is sorted by status (stale
// first, then warning) and then by days-since-edit DESC so the
// most-overdue pages come first.
func (e *FreshnessEngine) GetStaleReport(ctx context.Context, workspaceID string) ([]FreshnessReport, error) {
	pages, err := e.pages.GetStalePages(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]FreshnessReport, 0, len(pages))
	for i := range pages {
		out = append(out, *e.buildReport(ctx, &pages[i]))
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Stale > Warning > Fresh > Unknown for sort priority.
		if statusRank(out[i].Status) != statusRank(out[j].Status) {
			return statusRank(out[i].Status) > statusRank(out[j].Status)
		}
		return out[i].DaysSinceEdit > out[j].DaysSinceEdit
	})
	return out, nil
}

func statusRank(s FreshnessStatus) int {
	switch s {
	case FreshnessStale:
		return 3
	case FreshnessWarning:
		return 2
	case FreshnessFresh:
		return 1
	default:
		return 0
	}
}

// SendStaleDigest is the daily-batch entry point. Phase 7 logs the
// summary; future phases ship Slack / email integrations.
func (e *FreshnessEngine) SendStaleDigest(ctx context.Context, workspaceID string) error {
	reports, err := e.GetStaleReport(ctx, workspaceID)
	if err != nil {
		return err
	}
	stale := 0
	warning := 0
	for _, r := range reports {
		switch r.Status {
		case FreshnessStale:
			stale++
		case FreshnessWarning:
			warning++
		}
	}
	slog.Info("freshness: stale digest",
		slog.String("workspace", workspaceID),
		slog.Int("stale_pages", stale),
		slog.Int("warning_pages", warning))
	return nil
}

// Start runs SendStaleDigest at ~9am UTC daily until ctx cancels.
// The first tick fires at the next 09:00 UTC; subsequent ticks
// fire every 24h. Best-effort: a digest error is logged but doesn't
// stop the schedule.
func (e *FreshnessEngine) Start(ctx context.Context, workspaceID string) {
	go func() {
		for {
			delay := untilNext9amUTC(time.Now().UTC())
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			if err := e.SendStaleDigest(ctx, workspaceID); err != nil {
				slog.Warn("freshness: digest failed", slog.String("err", err.Error()))
			}
		}
	}()
}

// untilNext9amUTC returns the duration until the next 09:00 UTC
// from `now`. If the clock is already past 9am today, the next
// tick is tomorrow at 9am.
func untilNext9amUTC(now time.Time) time.Duration {
	target := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, time.UTC)
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
	}
	return target.Sub(now)
}

// buildReport applies the same threshold math GetStatus uses. The
// "fresher of updated_at / last_verified_at" rule means a page that
// was explicitly re-verified stays fresh even if its raw edit
// timestamp is stale — that's the whole point of Verify.
func (e *FreshnessEngine) buildReport(ctx context.Context, p *model.Page) *FreshnessReport {
	now := time.Now().UTC()
	r := &FreshnessReport{
		PageID:         p.ID,
		SpaceID:        p.SpaceID,
		Title:          p.Title,
		StaleAfterDays: p.StaleAfterDays,
		UpdatedAt:      p.UpdatedAt,
		VerifiedBy:     p.VerifiedBy,
	}
	// Days-since-edit. Use the fresher of UpdatedAt vs
	// LastVerifiedAt so an explicit re-verify "wins" over the raw
	// content edit timestamp.
	effective := p.UpdatedAt
	if p.LastVerifiedAt != nil && p.LastVerifiedAt.After(effective) {
		effective = *p.LastVerifiedAt
	}
	r.DaysSinceEdit = daysBetween(effective, now)
	if p.LastVerifiedAt != nil {
		d := daysBetween(*p.LastVerifiedAt, now)
		r.DaysSinceVerify = &d
	}

	// Classification.
	if p.StaleAfterDays <= 0 {
		r.Status = FreshnessUnknown
	} else if r.DaysSinceEdit >= p.StaleAfterDays {
		r.Status = FreshnessStale
		r.Reason = fmt.Sprintf("Not updated in %d days (TTL: %d days)", r.DaysSinceEdit, p.StaleAfterDays)
	} else if float64(r.DaysSinceEdit) >= float64(p.StaleAfterDays)*warningRatio {
		r.Status = FreshnessWarning
		r.Reason = fmt.Sprintf("Last update was %d days ago; threshold is %d", r.DaysSinceEdit, p.StaleAfterDays)
	} else {
		r.Status = FreshnessFresh
	}

	// Linked-issue activity. Phase 7 counts closed issues among the
	// page's embedded Track refs — when several are done, the spec
	// likely needs a refresher.
	if e.track != nil && e.track.IsConfigured() && e.links != nil {
		ids, err := e.links.IssueIDsForPage(ctx, p.ID)
		if err == nil {
			closed := 0
			for _, id := range ids {
				ref, _ := e.track.GetIssue(ctx, p.WorkspaceID, id)
				if ref == nil {
					continue
				}
				if isClosed(ref.Status) {
					closed++
				}
			}
			if closed > 0 {
				r.LinkedIssuesClosed = closed
				r.SuggestReview = true
				if r.Reason == "" {
					r.Reason = fmt.Sprintf("%d linked issues completed since last edit", closed)
				} else {
					r.Reason = fmt.Sprintf("%s · %d linked issues completed", r.Reason, closed)
				}
			}
		}
	}

	return r
}

// isClosed maps Track issue statuses to a binary closed flag. Both
// "done" and "cancelled" terminate work — they're equivalent for
// the spec-freshness signal.
func isClosed(status string) bool {
	switch status {
	case "done", "cancelled":
		return true
	}
	return false
}

// daysBetween returns the count of full 24-hour periods between
// `from` and `to`. We deliberately use floor (not round) so a page
// edited 23 hours ago reports 0 days — "today" is more honest than
// "1 day ago".
func daysBetween(from, to time.Time) int {
	if from.IsZero() || to.IsZero() {
		return 0
	}
	d := to.Sub(from)
	if d < 0 {
		return 0
	}
	return int(d / (24 * time.Hour))
}
