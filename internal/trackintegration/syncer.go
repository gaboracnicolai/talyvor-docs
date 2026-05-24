package trackintegration

import (
	"context"
	"log/slog"
	"time"
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

// Syncer rolls up AI cost from linked Track issues into each page's
// ai_cost_usd column. The cost-per-doc story is the integration's
// flagship feature; this background loop is what makes the number
// trustworthy without burdening the save path.
type Syncer struct {
	client      *Client
	pages       pageUpdater
	links       linkReader
	workspaceID string
}

// NewSyncer wires the pieces. workspaceID is the single tenant the
// loop refreshes — Phase 4 supports one workspace per Docs instance;
// Phase 5 will iterate the workspaces table.
func NewSyncer(client *Client, pages pageUpdater, links linkReader, workspaceID string) *Syncer {
	return &Syncer{
		client:      client,
		pages:       pages,
		links:       links,
		workspaceID: workspaceID,
	}
}

// Start runs the sync until ctx cancels. interval defaults to 15
// minutes in main.go; tests can pass a shorter cadence.
func (s *Syncer) Start(ctx context.Context, interval time.Duration) {
	if !s.client.IsConfigured() {
		// Without Track, the syncer is a no-op. Returning quietly
		// keeps the boot path clean — operators don't have to
		// suppress an error log when running Docs standalone.
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// First pass on boot so the page-level totals are populated
	// before the first 15-minute tick.
	s.SyncPageCosts(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SyncPageCosts(ctx)
		}
	}
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
