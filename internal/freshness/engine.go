// Package freshness owns Docs's "is this still accurate?" surface.
// Pages declare a stale_after_days TTL; the engine reads each
// page's edit / verify timestamps + linked-Track-issue activity to
// classify it as fresh / warning / stale / unknown and surface why.
// This is the differentiating feature that turns a doc tool into a
// living spec — the spec stays connected to the work it describes.
package freshness

import (
	"context"
	"errors"
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
	pages      pageReader
	links      linkReader
	track      trackReader
	access     pageVisibility
	workspaces workspaceEnumerator
}

// pageVisibility authorizes a READ of ONE page for the verified caller. *spaceauth.Authorizer
// satisfies it — the same shipped primitive internal/search, page.Handler and internal/ai use, so
// this package introduces NO new access model.
type pageVisibility interface {
	AuthorizePageRead(ctx context.Context, pageID string) (found, canView bool)
}

// WithPageRead attaches the per-page read gate the two REQUEST-SCOPED stale reads need.
//
// ⚠ THE ENGINE HAS TWO KINDS OF READER AND THE GATE BELONGS TO ONLY ONE OF THEM. GetStaleReport
// answers a PERSON (the SPA's stale screen and sidebar count, and the MCP get_stale_pages tool);
// SendStaleDigest is a daily batch started from main.go on a context that has no caller and never
// will. Filtering the digest per-caller would report zero stale pages to an operator forever, so
// the unfiltered list stays — as `staleReportAll`, UNEXPORTED, so that the only stale report
// reachable from outside this package is the gated one. That is the whole design: not a naming
// convention a future caller has to notice, a compiler boundary.
//
// ⚠ NIL ERRORS RATHER THAN RETURNING AN EMPTY LIST. An empty stale report is not a neutral
// response — it is the positive claim "nothing in this workspace needs attention", and the SPA
// paints it as a zero on the sidebar. Same reasoning as internal/ai's refusal; deliberately
// unlike page.Handler's nil-is-an-empty-list, whose route returns rows rather than a verdict.
func (e *FreshnessEngine) WithPageRead(a pageVisibility) *FreshnessEngine {
	e.access = a
	return e
}

// ErrNoPageReadGate is what a request-scoped stale read returns when no gate was wired. It is a
// server misconfiguration, reported as one.
var ErrNoPageReadGate = errors.New("freshness: no page-read gate wired (cmd/docs/main.go must call freshEngine.WithPageRead)")

// workspaceEnumerator answers WHICH workspaces the daily digest covers.
//
// ⚠ CONTENT-DERIVED, AND FOR THE DIGEST THAT IS THE RIGHT QUESTION rather than a compromise.
// trackintegration/enumerate.go prefers asking Track "which workspaces EXIST" because a
// content-derived set gave member sync a cold-start deadlock: a workspace with no content was
// never enumerated, so it never got a roster, so it could never gain content. The stale digest
// has no such blind spot — a workspace with no pages has nothing that can be stale — which is
// the same argument Syncer.costWorkspaces already makes for COST. So this takes the union of
// spaces and pages (membership.Store.DistinctWorkspaceIDs) and needs no Track credential.
type workspaceEnumerator interface {
	DistinctWorkspaceIDs(ctx context.Context) ([]string, error)
}

// WithWorkspaces attaches the enumerator the DAILY DIGEST needs.
//
// ⚠ THE DIGEST USED TO TAKE ONE PINNED WORKSPACE ID FROM CONFIGURATION, AND THAT NUMBER WAS
// STRUCTURALLY ZERO. `freshEngine.Start(ctx, cfg.DefaultWorkspaceID)` ran the 09:00 UTC digest
// against `DOCS_DEFAULT_WORKSPACE`, which defaults to the literal string "default" and appears in
// no compose file and no README — while workspace ids are Track's, minted per identity at login.
// MEASURED on real Postgres: two workspaces each holding one page stale by the shipped predicate,
// and the digest main.go actually runs logged `workspace=default stale_pages=0 warning_pages=0`,
// every day, forever, while the same method for each real workspace logged `stale_pages=1`.
//
// ⚠ THIS IS THE THIRD INSTANCE OF A CLASS THIS BINARY HAS ALREADY NAMED AND FIXED TWICE.
// Syncer.costWorkspaces' own comment: "It used to be answered with a single pinned config value
// while SyncMembers thirty lines below enumerated every workspace; two loops in one struct
// disagreeing about how many tenants exist meant the cost-per-doc number was right for exactly one
// of them." Both of those loops enumerate now. This one is in a different struct, wired forty
// lines away in main.go, and was never looked at.
//
// ⚠ NIL ERRORS RATHER THAN FALLING BACK TO A PINNED ID. A fallback is what the defect looked
// like: a plausible workspace name that matches nothing, producing a confident zero. An operator
// reading a daily error line knows the digest did not run; an operator reading `stale_pages=0`
// does not.
func (e *FreshnessEngine) WithWorkspaces(w workspaceEnumerator) *FreshnessEngine {
	e.workspaces = w
	return e
}

// ErrNoWorkspaceEnumerator is what the digest returns when no enumerator was wired. See
// WithWorkspaces for why this is not a fallback.
var ErrNoWorkspaceEnumerator = errors.New("freshness: no workspace enumerator wired (cmd/docs/main.go must call freshEngine.WithWorkspaces)")

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

// GetStaleReport returns every page in the workspace that's past (or approaching) its TTL AND
// THAT THE CALLER MAY VIEW. The list is sorted by status (stale first, then warning) and then by
// days-since-edit DESC so the most-overdue pages come first.
//
// ⚠ THIS IS THE REQUEST-SCOPED READER. Both of its callers answer a caller — the SPA's stale
// screen and sidebar count via GET /v1/workspaces/{wsID}/freshness, and the MCP get_stale_pages
// tool — and both authorized the WORKSPACE and stopped, so a member with no grant on a private
// space received its page titles and a working /spaces/{space_id}/pages/{page_id} link. Measured
// in privatespace_realpg_test.go. The system-scoped list is staleReportAll, and it is unexported
// so that this is the only stale report reachable from outside the package.
func (e *FreshnessEngine) GetStaleReport(ctx context.Context, workspaceID string) ([]FreshnessReport, error) {
	// NIL-RECEIVER SAFE, AND THAT IS NOT DEFENSIVE PADDING — IT IS A CRASH THIS CONTROL RUN FOUND.
	// mcp.New takes a *FreshnessEngine and stores it in a freshDeps INTERFACE field, so a nil
	// engine becomes a NON-nil interface holding a nil pointer: internal/mcp/server.go:874's
	// `if s.deps.freshness == nil` reads as a nil-check and is not one, and the call proceeds on a
	// nil receiver. MEASURED: mcp.New(..., nil, "test") + get_stale_pages = SIGSEGV, before this
	// line and equally before it existed (the old body dereferenced e.pages one statement later).
	// The same Go footgun ai.Engine.IsAvailable documents and answers the same way — the receiver
	// check is part of the contract, rather than every caller being asked to remember it.
	//
	// ⚠ THE DEAD nil GUARD IN internal/mcp IS ITS OWN FINDING AND IS NOT FIXED HERE.
	if e == nil || e.access == nil {
		return nil, ErrNoPageReadGate
	}
	all, err := e.staleReportAll(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	// Drop every page the caller may not VIEW, asking the same engine the by-id freshness route
	// asks. There is no LIMIT anywhere on this path — GetStalePages returns the whole predicate —
	// so unlike the search seams this filter is complete rather than a mitigation, and no
	// over-fetch is needed.
	out := make([]FreshnessReport, 0, len(all))
	for _, r := range all {
		found, canView := e.access.AuthorizePageRead(ctx, r.PageID)
		if !found || !canView {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// staleReportAll is the SYSTEM view: every stale page in the workspace, with no caller in it.
// SendStaleDigest is its only caller and runs on a background context started from main.go, which
// has no memberships and never will — filtering it per-caller would report an empty workspace to
// an operator every day at 09:00.
// staleReportAll builds a report per page in the workspace's stale set.
//
// ⚠⚠ THE POPULATION IS `GetStalePages` AND NOTHING WIDENS IT — buildReport ANNOTATES ROWS, IT
// DOES NOT SELECT THEM. That one sentence is the whole contract of this route and both of its
// consumers used to describe a different one. GetStalePages is a TTL-only SQL predicate
// (stale_after_days > 0 AND updated_at/last_verified_at past it), so:
//
//   - `SuggestReview` / `LinkedIssuesClosed` can only ever DECORATE a page the TTL already
//     caught. A page with stale_after_days = 0 — the column DEFAULT, i.e. every page nobody
//     configured — is invisible here no matter how many linked Track issues are done.
//   - Every row's Status is therefore `stale`. The SQL requires age > TTL and daysBetween
//     FLOORS, so floor(age) >= TTL always holds; `warning`, `fresh` and `unknown` are
//     unreachable through this function, which makes statusRank's branch in the sort below a
//     tiebreak that cannot vary. Kept, because it is correct for any widened population.
//
// Both are MEASURED, not argued — stalereport_population_realpg_test.go drives the real
// page.Store against real Postgres with a live link store and a live Track reader, and its
// [CONTROL] tag is what makes those two absences a boundary rather than a dead fixture.
//
// ⚠ WIDENING IT IS A DECISION AND THE TWO HALVES DO NOT COST THE SAME. The warning band is
// free (pure SQL, no network) but changes what SendStaleDigest emails and what the sidebar
// counts. The linked-issue half is NOT computable here at any price today: page_links carries
// no status column, so Docs holds no local copy of an issue's state and answering it
// workspace-wide means one Track round trip per linked issue of every page, per request.
func (e *FreshnessEngine) staleReportAll(ctx context.Context, workspaceID string) ([]FreshnessReport, error) {
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

// DigestSummary is one workspace's line of the daily digest. It is RETURNED as well as logged so
// the digest can be asserted on: the log line was the only product of this batch, and a test that
// scrapes a log string cannot say which workspaces were visited, only what the last one said.
type DigestSummary struct {
	WorkspaceID string
	Stale       int
	Warning     int
}

// SendStaleDigest is the per-workspace digest. Phase 7 logs the
// summary; future phases ship Slack / email integrations.
func (e *FreshnessEngine) SendStaleDigest(ctx context.Context, workspaceID string) error {
	_, err := e.digestOne(ctx, workspaceID)
	return err
}

// digestOne counts and logs one workspace. Shared by the per-workspace entry point and the
// all-workspaces sweep, so the two cannot drift about what "stale" means or what is logged.
func (e *FreshnessEngine) digestOne(ctx context.Context, workspaceID string) (DigestSummary, error) {
	reports, err := e.staleReportAll(ctx, workspaceID)
	if err != nil {
		return DigestSummary{}, err
	}
	out := DigestSummary{WorkspaceID: workspaceID}
	for _, r := range reports {
		switch r.Status {
		case FreshnessStale:
			out.Stale++
		case FreshnessWarning:
			out.Warning++
		}
	}
	slog.Info("freshness: stale digest",
		slog.String("workspace", workspaceID),
		slog.Int("stale_pages", out.Stale),
		slog.Int("warning_pages", out.Warning))
	return out, nil
}

// SendStaleDigestAll is the DAILY-BATCH ENTRY POINT: every workspace Docs holds content for, not
// one pinned in configuration. See WithWorkspaces for the measurement that made this necessary.
//
// BEST-EFFORT AT THE LEVEL OF A WORKSPACE, mirroring Syncer.SyncPageCosts' documented posture: one
// workspace's failure logs and the sweep continues, because catching up tomorrow beats reporting
// nothing for every tenant. A failure of the ENUMERATION itself is different and is returned — an
// empty list is handed to a loop, and "nothing to do" is silently the same shape as "everything is
// fine" (enumerate.go's rule, applied here).
func (e *FreshnessEngine) SendStaleDigestAll(ctx context.Context) ([]DigestSummary, error) {
	if e == nil || e.workspaces == nil {
		return nil, ErrNoWorkspaceEnumerator
	}
	wsIDs, err := e.workspaces.DistinctWorkspaceIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("freshness: enumerate workspaces for the daily digest: %w", err)
	}
	out := make([]DigestSummary, 0, len(wsIDs))
	for _, ws := range wsIDs {
		s, err := e.digestOne(ctx, ws)
		if err != nil {
			slog.Warn("freshness: digest failed for one workspace — the sweep continues",
				slog.String("workspace", ws), slog.String("err", err.Error()))
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// Start runs SendStaleDigestAll at ~9am UTC daily until ctx cancels.
// The first tick fires at the next 09:00 UTC; subsequent ticks
// fire every 24h. Best-effort: a digest error is logged but doesn't
// stop the schedule.
//
// ⚠ IT TAKES NO WORKSPACE ID, DELIBERATELY. The previous signature took one and main.go handed it
// cfg.DefaultWorkspaceID; the compiler is what now stops that call site coming back.
func (e *FreshnessEngine) Start(ctx context.Context) {
	go func() {
		for {
			delay := untilNext9amUTC(time.Now().UTC())
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			if _, err := e.SendStaleDigestAll(ctx); err != nil {
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
