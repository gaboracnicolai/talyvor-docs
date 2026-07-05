// Package membership is Docs's local mirror of workspace rosters, full-pulled from
// Track's service-members endpoint (Docs owns no members table). SEC-4 Layer 1 (PR-3)
// resolves a verified x-user-email against workspace_members to decide access; PR-2
// (this package + the trackintegration syncer) keeps that table in sync.
package membership

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MemberRef is one membership tuple — the minimum Track's endpoint returns and the
// minimum SEC-4 needs.
type MemberRef struct {
	Email    string `json:"email"`
	Role     string `json:"role"`
	MemberID string `json:"member_id"`
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// DistinctWorkspaceIDs returns every workspace Docs holds content for — the set the
// member-sync iterates. Union of spaces + pages (a space-only workspace still gets its
// roster synced; every page carries workspace_id directly, covering orphan cases).
func (s *Store) DistinctWorkspaceIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT workspace_id FROM spaces
		 UNION
		 SELECT workspace_id FROM pages`)
	if err != nil {
		return nil, fmt.Errorf("membership: enumerate workspaces: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			return nil, fmt.Errorf("membership: scan workspace: %w", err)
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// ReconcileWorkspace makes workspace_members for ONE workspace match refs, in a single
// transaction: upsert every present member, then prune rows for THIS workspace whose email
// isn't in refs (departed members). Returns (upserted, pruned).
//
// EMPTY-PULL SAFETY: refs empty ⇒ no-op (upsert nothing, prune NOTHING). Every real Track
// workspace has ≥1 owner, so an empty pull means an anomaly (a transient fetch returning [],
// or a misconfig) — pruning-to-empty would wipe every member's access. The syncer separately
// treats a FETCH ERROR as skip-this-workspace (never reaching here), so this guards the
// successful-but-empty case. The prune is scoped to $1 — it can never touch another workspace.
func (s *Store) ReconcileWorkspace(ctx context.Context, workspaceID string, refs []MemberRef) (upserted, pruned int, err error) {
	if workspaceID == "" {
		return 0, 0, errors.New("membership: ReconcileWorkspace requires a workspace_id")
	}
	if len(refs) == 0 {
		return 0, 0, nil // empty-pull safety — never prune a roster to zero
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	emails := make([]string, len(refs))
	for i, m := range refs {
		emails[i] = m.Email
		if _, err := tx.Exec(ctx,
			`INSERT INTO workspace_members (workspace_id, email, role, member_id, synced_at)
			 VALUES ($1, $2, $3, $4, now())
			 ON CONFLICT (workspace_id, email)
			 DO UPDATE SET role = EXCLUDED.role, member_id = EXCLUDED.member_id, synced_at = now()`,
			workspaceID, m.Email, m.Role, m.MemberID); err != nil {
			return 0, 0, fmt.Errorf("membership: upsert %s: %w", m.Email, err)
		}
	}

	// Prune departed — SCOPED to this workspace, and only among rows not in the pulled set.
	ct, err := tx.Exec(ctx,
		`DELETE FROM workspace_members WHERE workspace_id = $1 AND email <> ALL($2::text[])`,
		workspaceID, emails)
	if err != nil {
		return 0, 0, fmt.Errorf("membership: prune: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return len(refs), int(ct.RowsAffected()), nil
}
