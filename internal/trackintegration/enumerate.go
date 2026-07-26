package trackintegration

import (
	"context"
	"fmt"
	"log/slog"
)

// enumerate.go — WHICH workspaces the member sync should cover.
//
// This used to be `SELECT workspace_id FROM spaces UNION SELECT workspace_id FROM pages`: the set
// of workspaces Docs holds CONTENT for. That answers "has this workspace been written to", which
// is a different question from "does this workspace exist" — and the gap between the two was a
// deadlock a brand-new tenant could never get out of:
//
//	no content ⇒ not enumerated ⇒ no roster synced ⇒ every write 403s (no membership row)
//	           ⇒ no content
//
// Track is the tenancy source of truth — it mints a workspace per identity at login — so it can
// answer the question that was actually being asked. Asking it first is the entire fix.

// workspaceEnumerator is Track's answer: every workspace on the deployment.
type workspaceEnumerator interface {
	ListWorkspaceIDs(ctx context.Context) ([]string, error)
}

// contentEnumerator is the OLD source, kept as a fallback only.
type contentEnumerator interface {
	DistinctWorkspaceIDs(ctx context.Context) ([]string, error)
}

// enumerateWorkspaces asks Track, and falls back to content-derived only when Track cannot be
// reached at all.
//
// THREE DISTINCTIONS THAT LOOK ALIKE AND ARE NOT:
//
//   - Track ERRORS ⇒ fall back. A Track blip must not stop existing rosters being refreshed.
//   - Track returns EMPTY ⇒ that is an ANSWER, not a failure, and it is returned as-is. Treating
//     it as a failure would run the fallback, whose content-derived set could resurrect workspaces
//     Track no longer knows about — the sync would then keep alive exactly what was removed.
//   - BOTH fail ⇒ an ERROR, never an empty list. An empty list is handed to a loop that reconciles
//     each workspace, and "nothing to do" is silently the same shape as "everything is fine", so a
//     total outage would read as a clean run forever. This is the difference between reporting a
//     fact and hiding the question.
//
// The fallback cannot cause deletions: SyncMembers prunes only inside a workspace it successfully
// pulled a roster for, and skips the workspace entirely on a failed pull. A shorter enumeration
// therefore means "fewer workspaces refreshed this cycle", never "these workspaces were emptied".
func enumerateWorkspaces(ctx context.Context, track workspaceEnumerator, content contentEnumerator) ([]string, error) {
	ids, err := track.ListWorkspaceIDs(ctx)
	if err == nil {
		return ids, nil
	}
	trackErr := err

	slog.Warn("trackintegration: workspace enumeration — Track unreachable, falling back to content-derived",
		slog.String("err", trackErr.Error()),
		slog.String("effect", "workspaces with no content yet are not synced this cycle; existing rosters are unaffected"))

	ids, err = content.DistinctWorkspaceIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerate workspaces: track: %w; content fallback: %w", trackErr, err)
	}
	return ids, nil
}
