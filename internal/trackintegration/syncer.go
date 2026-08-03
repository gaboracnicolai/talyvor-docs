package trackintegration

import (
	"context"
	"log/slog"
	"time"

	"github.com/talyvor/docs/internal/membership"
)

// linkReader is the subset of pagelink.Store the syncer reads. The
// narrow interface keeps this file free of a pagelink import — the
// dep graph stays one-way (main.go wires both).
type linkReader interface {
	IssueIDsForPage(ctx context.Context, pageID string) ([]string, error)
}

// pageUpdater is the subset of page.Store the syncer touches. Same
// pattern as linkReader.
type pageUpdater interface {
	WorkspacePageIDs(ctx context.Context, workspaceID string) ([]string, error)
	UpdateAICost(ctx context.Context, pageID string, costUSD float64) error
}

// costSource is the subset of Client the cost loop reads.
type costSource interface {
	IsConfigured() bool
	IssueCost(ctx context.Context, workspaceID, issueID string) (float64, error)
}

// memberSource pulls a workspace's roster from Track (the Client satisfies it). Kept as an
// interface so SyncMembers is unit-testable with a fake, no live Track.
type memberSource interface {
	MemberSyncConfigured() bool
	GetWorkspaceMembers(ctx context.Context, workspaceID string) ([]membership.MemberRef, error)
	// ListWorkspaceIDs answers "which workspaces exist" — the question the old content-derived
	// enumeration could not answer for a workspace that had never been written to. See
	// enumerate.go.
	ListWorkspaceIDs(ctx context.Context) ([]string, error)
}

// membershipStore enumerates Docs's workspaces and reconciles one workspace's roster
// (membership.Store satisfies it).
type membershipStore interface {
	DistinctWorkspaceIDs(ctx context.Context) ([]string, error)
	ReconcileWorkspace(ctx context.Context, workspaceID string, refs []membership.MemberRef) (upserted, pruned int, err error)
}

// Syncer rolls up AI cost from linked Track issues into each page's
// ai_cost_usd column. The cost-per-doc story is the integration's
// flagship feature; this background loop is what makes the number
// trustworthy without burdening the save path. It ALSO (A0b PR-2) syncs
// each workspace's member roster from Track into workspace_members.
//
// Both sweeps cover EVERY workspace and enumerate through the same helper. They did not
// always: the cost sweep was pinned to one configured workspace while member sync a few lines
// below enumerated all of them.
type Syncer struct {
	client costSource
	pages  pageUpdater
	links  linkReader
	// workspaceID is the LAST-RESORT enumeration only — used when no workspace enumerator is
	// wired at all. It is not the tenant the cost loop refreshes; see costWorkspaces.
	workspaceID string

	// member-sync (A0b PR-2) — nil until WithMemberSync wires it.
	members memberSource
	store   membershipStore
}

// NewSyncer wires the cost-sync pieces. BOTH loops are multi-workspace. workspaceID is the
// fallback the cost sweep uses only when nothing was wired to enumerate workspaces with — it
// was once the single pinned tenant the whole cost loop ran for, which is what made the
// cost-per-doc number wrong for every workspace but one.
func NewSyncer(client costSource, pages pageUpdater, links linkReader, workspaceID string) *Syncer {
	return &Syncer{
		client:      client,
		pages:       pages,
		links:       links,
		workspaceID: workspaceID,
	}
}

// WithMemberSync enables the member roster sync. It ALSO gives the cost sweep its workspace
// enumerator (costWorkspaces) — the two loops must agree on how many tenants exist, and the
// only way to guarantee that is to have them ask the same source. Absent ⇒ SyncMembers no-ops
// and the cost sweep falls back to the single pinned workspaceID.
func (s *Syncer) WithMemberSync(members memberSource, store membershipStore) *Syncer {
	s.members = members
	s.store = store
	return s
}

func (s *Syncer) memberSyncOn() bool {
	return s.members != nil && s.store != nil && s.members.MemberSyncConfigured()
}

func (s *Syncer) costSyncOn() bool {
	return s.client != nil && s.client.IsConfigured()
}

// Start runs the sync until ctx cancels. interval defaults to 15
// minutes in main.go; tests can pass a shorter cadence.
func (s *Syncer) Start(ctx context.Context, interval time.Duration) {
	if !s.costSyncOn() && !s.memberSyncOn() {
		// Nothing configured — no-op quietly (Docs runs standalone).
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.runOnce(ctx) // boot pass
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

// runOnce does one sweep. Both halves self-guard, so the gate lives with the loop it governs
// rather than in the caller — SyncPageCosts is also called directly by tests.
func (s *Syncer) runOnce(ctx context.Context) {
	s.SyncPageCosts(ctx)
	s.SyncMembers(ctx)
}

// costWorkspaces answers WHICH workspaces the cost loop must refresh — the same question, asked
// the same way, as member sync. It used to be answered with a single pinned config value while
// SyncMembers thirty lines below enumerated every workspace; two loops in one struct disagreeing
// about how many tenants exist meant the cost-per-doc number was right for exactly one of them.
//
// Three sources, in descending order of what they can see, and each branch says why it is next:
// Track (every workspace that exists), Docs' own content (every workspace with pages — no blind
// spot for COST), and finally the pinned workspaceID when nothing at all was wired. The last is
// the pre-existing behaviour, kept so this change can never cover LESS than before.
func (s *Syncer) costWorkspaces(ctx context.Context) ([]string, error) {
	if s.memberSyncOn() {
		// Track can be asked which workspaces EXIST — the same source, and the same answer,
		// member sync gets. See enumerate.go for why that beats asking Docs' own content.
		return enumerateWorkspaces(ctx, s.members, s.store)
	}
	if s.store != nil {
		// No member-sync secret, so Track's service surface is closed to us. Ask Docs instead
		// — for COST that is anyway the right question: a workspace with no pages has nothing
		// to cost, so content-derived enumeration has no blind spot here (unlike member sync,
		// where it was a cold-start deadlock). Calling the gated surface just to be refused
		// would log "Track unreachable" every tick while Track is perfectly reachable, and a
		// warning that cannot distinguish an unset secret from a real outage is worse than
		// none the day there IS one.
		return s.store.DistinctWorkspaceIDs(ctx)
	}
	return []string{s.workspaceID}, nil
}

// SyncPageCosts walks every page in EVERY workspace, sums the AI cost of its linked Track
// issues, and writes the total back to pages.ai_cost_usd. The cost-per-doc story is the
// integration's flagship feature, so the sweep is best-effort at the level of a page — one
// page's failure logs and continues, because catching up next tick beats aborting the loop.
//
// ⚠ BEST-EFFORT IS ABOUT WHETHER A PAGE IS REFRESHED, NEVER ABOUT WHAT GETS WRITTEN. See
// pageTotal: a total that could not be completed is not written at all.
func (s *Syncer) SyncPageCosts(ctx context.Context) {
	if !s.costSyncOn() {
		return
	}
	wsIDs, err := s.costWorkspaces(ctx)
	if err != nil {
		slog.Warn("trackintegration: cost sync — enumerate workspaces", slog.String("err", err.Error()))
		return
	}
	for _, wsID := range wsIDs {
		s.syncWorkspaceCosts(ctx, wsID)
	}
}

// syncWorkspaceCosts refreshes one workspace's pages. Everything downstream is scoped to wsID —
// including the issue fetch, which must ask Track under the PAGE's workspace and not under some
// pinned default, or every other tenant's issue 404s.
func (s *Syncer) syncWorkspaceCosts(ctx context.Context, wsID string) {
	pageIDs, err := s.pages.WorkspacePageIDs(ctx, wsID)
	if err != nil {
		slog.Warn("trackintegration: list pages",
			slog.String("workspace_id", wsID),
			slog.String("err", err.Error()))
		return
	}
	for _, pageID := range pageIDs {
		issueIDs, err := s.links.IssueIDsForPage(ctx, pageID)
		if err != nil {
			slog.Warn("trackintegration: list links",
				slog.String("workspace_id", wsID),
				slog.String("page_id", pageID),
				slog.String("err", err.Error()))
			continue
		}
		total, complete := s.pageTotal(ctx, wsID, pageID, issueIDs)
		if !complete {
			continue // the previous total stands; see pageTotal
		}
		if err := s.pages.UpdateAICost(ctx, pageID, total); err != nil {
			slog.Warn("trackintegration: page cost update",
				slog.String("workspace_id", wsID),
				slog.String("page_id", pageID),
				slog.String("err", err.Error()))
		}
	}
}

// pageTotal sums one page's linked issue costs, and reports whether the sum is COMPLETE.
//
// ⚠ A PARTIAL TOTAL MUST NEVER OVERWRITE A COMPLETE ONE. This loop used to call GetIssue and
// discard the error — deliberately, and correctly, for the embed that method was written for,
// where an unreachable issue should render a placeholder rather than fail the page. Reused here
// it meant an unreachable issue contributed $0.00 and the too-low total was written straight
// over the good one. Nothing on the page said the number had changed for a reason that had
// nothing to do with the work it describes.
//
// So: one unreadable issue makes the whole page's total unknown, and an unknown total is not
// written. Three candidate rules were on the table and this is why this one wins:
//
//   - WRITE IT WITH A COMPLETENESS MARKER would be strictly better — if anything displayed the
//     marker. Adding a column no surface reads is the exact defect class this repo keeps
//     finding (see the page_type audit); it would fix the number and ship a fresh lie beside it.
//     It is the right follow-up once the API and the frontend can carry "partial".
//   - RETRY INSIDE THE TICK just moves the same failure a few seconds later. The loop already
//     is the retry: it runs every couple of minutes, and the next pass that reaches Track writes
//     the correct total with no special case.
//   - SKIP THE WRITE keeps a number that was true at some point instead of publishing one that
//     was never true, and self-heals on the next successful pass. On a figure a customer
//     reconciles against an invoice, stale-and-once-true beats fresh-and-wrong.
//
// The residual is real and named: a page can hold a silently stale total while Track is down
// for it. That is why the failure logs at ERROR with the page and the issue that broke it —
// the skip must be diagnosable, not merely safe.
//
// Zero linked issues is a COMPLETE answer, not an unknown one: the page's cost is $0.00 and
// gets written. Fail-closed must not decay into never-write.
func (s *Syncer) pageTotal(ctx context.Context, wsID, pageID string, issueIDs []string) (float64, bool) {
	var total float64
	for _, id := range issueIDs {
		cost, err := s.client.IssueCost(ctx, wsID, id)
		if err != nil {
			slog.Error("trackintegration: page cost INCOMPLETE — keeping the previous total",
				slog.String("workspace_id", wsID),
				slog.String("page_id", pageID),
				slog.String("issue_id", id),
				slog.String("err", err.Error()),
				slog.String("effect", "ai_cost_usd left at its last complete value; it may now be stale"))
			return 0, false
		}
		total += cost
	}
	return total, true
}

// SyncMembers full-pulls every workspace's roster from Track into workspace_members —
// MULTI-WORKSPACE: it enumerates the distinct workspaces Docs holds content for and syncs
// each. No-op unless member-sync is wired AND configured (secret set). One workspace's pull
// failing logs and continues — a single bad workspace never aborts the whole sync. Logs
// count only (never member emails/PII).
func (s *Syncer) SyncMembers(ctx context.Context) {
	if !s.memberSyncOn() {
		return
	}
	// Ask TRACK which workspaces exist, not Docs which ones have content. The old
	// content-derived query could never enumerate a brand-new workspace, so its first write
	// 403d for want of a membership row and it never got content — a deadlock a new tenant
	// could not escape. Falls back to content-derived only if Track is unreachable; see
	// enumerate.go for why an empty Track answer is NOT a fallback trigger.
	wsIDs, err := enumerateWorkspaces(ctx, s.members, s.store)
	if err != nil {
		slog.Warn("trackintegration: member sync — enumerate workspaces", slog.String("err", err.Error()))
		return
	}
	for _, wsID := range wsIDs {
		// A failure on one workspace never aborts the sweep — SyncOneWorkspace logs and returns.
		_ = s.SyncOneWorkspace(ctx, wsID)
	}
}

// SyncOneWorkspace pulls ONE workspace's roster from Track and reconciles it. Extracted verbatim
// from the sweep above so the periodic path and the on-demand one cannot drift: the ticker calls it
// per workspace, and the service route calls it for exactly the workspace a new identity just got.
//
// WHY IT EXISTS. The sweep runs on a timer, so a brand-new identity had no membership row until the
// next tick — up to the full interval — and the first thing a new tester does is the thing that
// 403s. This is the entry point that closes that window; see the service route in cmd/docs.
//
// IDEMPOTENT: ReconcileWorkspace is a full-pull upsert plus prune, so calling it twice costs a
// second round-trip and changes nothing. That is what makes a retry free and a duplicate nudge
// harmless.
//
// A transient failure returns an error and leaves the EXISTING roster intact rather than pruning it
// — a pull that failed is not evidence that nobody is a member.
func (s *Syncer) SyncOneWorkspace(ctx context.Context, wsID string) error {
	if !s.memberSyncOn() {
		return nil
	}
	refs, err := s.members.GetWorkspaceMembers(ctx, wsID)
	if err != nil {
		slog.Warn("trackintegration: member sync — pull failed, skipping workspace",
			slog.String("workspace_id", wsID), slog.String("err", err.Error()))
		return err
	}
	upserted, pruned, err := s.store.ReconcileWorkspace(ctx, wsID, refs)
	if err != nil {
		slog.Warn("trackintegration: member sync — reconcile failed",
			slog.String("workspace_id", wsID), slog.String("err", err.Error()))
		return err
	}
	slog.Info("trackintegration: member sync — workspace reconciled",
		slog.String("workspace_id", wsID),
		slog.Int("upserted", upserted),
		slog.Int("pruned", pruned))
	return nil
}
