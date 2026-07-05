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

// memberSource pulls a workspace's roster from Track (the Client satisfies it). Kept as an
// interface so SyncMembers is unit-testable with a fake, no live Track.
type memberSource interface {
	MemberSyncConfigured() bool
	GetWorkspaceMembers(ctx context.Context, workspaceID string) ([]membership.MemberRef, error)
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
type Syncer struct {
	client      *Client
	pages       pageUpdater
	links       linkReader
	workspaceID string

	// member-sync (A0b PR-2) — nil until WithMemberSync wires it.
	members memberSource
	store   membershipStore
}

// NewSyncer wires the cost-sync pieces. workspaceID is the single tenant the cost loop
// refreshes; member-sync (wired via WithMemberSync) is multi-workspace.
func NewSyncer(client *Client, pages pageUpdater, links linkReader, workspaceID string) *Syncer {
	return &Syncer{
		client:      client,
		pages:       pages,
		links:       links,
		workspaceID: workspaceID,
	}
}

// WithMemberSync enables the multi-workspace member roster sync. Absent ⇒ SyncMembers
// no-ops.
func (s *Syncer) WithMemberSync(members memberSource, store membershipStore) *Syncer {
	s.members = members
	s.store = store
	return s
}

func (s *Syncer) memberSyncOn() bool {
	return s.members != nil && s.store != nil && s.members.MemberSyncConfigured()
}

// Start runs the sync until ctx cancels. interval defaults to 15
// minutes in main.go; tests can pass a shorter cadence.
func (s *Syncer) Start(ctx context.Context, interval time.Duration) {
	costOn := s.client != nil && s.client.IsConfigured()
	memberOn := s.memberSyncOn()
	if !costOn && !memberOn {
		// Nothing configured — no-op quietly (Docs runs standalone).
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.runOnce(ctx, costOn) // boot pass
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnce(ctx, costOn)
		}
	}
}

// runOnce does one sweep: cost-sync (if configured) then member-sync (self-guards).
func (s *Syncer) runOnce(ctx context.Context, costOn bool) {
	if costOn {
		s.SyncPageCosts(ctx)
	}
	s.SyncMembers(ctx)
}

// SyncPageCosts walks every page in the configured workspace, sums
// the AI cost of its linked Track issues, and writes the total back
// to pages.ai_cost_usd. Best-effort: a single page failure logs and
// continues; we'd rather catch up next tick than abort the loop.
func (s *Syncer) SyncPageCosts(ctx context.Context) {
	pageIDs, err := s.pages.WorkspacePageIDs(ctx, s.workspaceID)
	if err != nil {
		slog.Warn("trackintegration: list pages", slog.String("err", err.Error()))
		return
	}
	for _, pageID := range pageIDs {
		issueIDs, err := s.links.IssueIDsForPage(ctx, pageID)
		if err != nil {
			slog.Warn("trackintegration: list links",
				slog.String("page_id", pageID),
				slog.String("err", err.Error()))
			continue
		}
		var total float64
		for _, id := range issueIDs {
			ref, _ := s.client.GetIssue(ctx, s.workspaceID, id)
			if ref != nil {
				total += ref.AICostUSD
			}
		}
		if err := s.pages.UpdateAICost(ctx, pageID, total); err != nil {
			slog.Warn("trackintegration: page cost update",
				slog.String("page_id", pageID),
				slog.String("err", err.Error()))
		}
	}
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
	wsIDs, err := s.store.DistinctWorkspaceIDs(ctx)
	if err != nil {
		slog.Warn("trackintegration: member sync — enumerate workspaces", slog.String("err", err.Error()))
		return
	}
	for _, wsID := range wsIDs {
		refs, err := s.members.GetWorkspaceMembers(ctx, wsID)
		if err != nil {
			// A fetch failure skips THIS workspace (never reaches reconcile) — its existing
			// roster stays intact rather than being pruned on a transient error.
			slog.Warn("trackintegration: member sync — pull failed, skipping workspace",
				slog.String("workspace_id", wsID), slog.String("err", err.Error()))
			continue
		}
		upserted, pruned, err := s.store.ReconcileWorkspace(ctx, wsID, refs)
		if err != nil {
			slog.Warn("trackintegration: member sync — reconcile failed",
				slog.String("workspace_id", wsID), slog.String("err", err.Error()))
			continue
		}
		slog.Info("trackintegration: member sync — workspace reconciled",
			slog.String("workspace_id", wsID),
			slog.Int("upserted", upserted),
			slog.Int("pruned", pruned))
	}
}
