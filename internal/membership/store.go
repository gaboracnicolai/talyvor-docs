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

// DistinctWorkspaceIDs returns every workspace Docs holds CONTENT for — the set the
// member-sync iterates. Union of spaces + pages (a space-only workspace still gets its
// roster synced; every page carries workspace_id directly, covering orphan cases).
//
// ── PARKED: THE COLD-START DEADLOCK ─────────────────────────────────────────────────
//
// Enumerating from content means a workspace with no content is never enumerated, and
// that closes a cycle no caller can break from outside:
//
//	no spaces/pages in W  ⇒  W is not in this result
//	                      ⇒  SyncMembers never pulls W's roster from Track
//	                      ⇒  workspace_members has no row for W
//	                      ⇒  authz.AuthorizeWorkspace(ctx, W) returns false
//	                      ⇒  space.Create 403s BEFORE its insert (space/handler.go)
//	                      ⇒  W can never acquire content — back to the top.
//
// Content is needed to get a roster, and a roster to create content. Nothing in this
// repo breaks the cycle: workspace_members has exactly one production writer
// (ReconcileWorkspace, called only by SyncMembers); spaces and pages have exactly one
// production writer each, both behind AuthorizeWorkspace; there is no seed subcommand
// (`docs [serve|migrate]`), no seeding migration, no member-add route, and the only authz
// exemption is /v1/public/* (share-token viewing, which creates nothing).
//
// THIS IS A LATENT DEFECT, NOT A CONSEQUENCE OF ANY LATER PLAN. It does not need per-user
// workspaces or a second tenant to appear: it is what a FIRST deploy against an empty
// database does. The pinned workspace is not privileged here — SyncMembers iterates this
// result and does not union its configured workspace in (trackintegration/syncer.go).
// Whoever deploys Docs must therefore create the first workspace_members row out of band,
// and no runbook step in talyvor-suite/deploy does; its troubleshooting table says "add
// the membership" without naming a mechanism, because there is not one.
//
//	REOPENING CONDITION — whichever comes first:
//	  · the first deploy of Docs against an empty database (soonest, and unavoidable);
//	  · a second trial cohort, or any second workspace;
//	  · the first customer who wants Docs without a shared workspace.
//
// FIX SHAPE — invert the enumeration: ask TRACK which workspaces exist rather than
// inferring the list from content Docs already holds. Track owns provisioning (it mints a
// workspace per identity at login, talyvor-track /v1/bootstrap) and is already this
// package's roster source, so it is the source of truth for the LIST as well — arguably
// always was, and this function is the one place that assumed otherwise. It replaces one
// query with one client call and needs no new tenancy concepts. The alternative — having
// the caller announce a workspace — must NOT be hung off the authz middleware: a security
// chokepoint that provisions on an unrecognised id will provision from a typo, and races
// on concurrent first requests.
//
// TestSyncMembers_CannotReachAWorkspaceWithNoContent (trackintegration) pins the deadlock
// and FAILS when it is broken, so the fix cannot land while this note quietly survives it.
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
//
// PROVENANCE: THE PRUNE ONLY DELETES ROWS THIS SYNCER CREATED (source = 'track'). It is a
// reconciler for TRACK's roster and has no authority over rows it did not write.
//
// This is deliberate and load-bearing, so it must not be "simplified" back out. Docs'
// tenancy currently has exactly ONE source — this syncer — which is why Docs cannot be sold
// without Track. That coupling was inherited from a roster sync built for a different job,
// not chosen, and the option to give Docs its own tenancy root is PARKED, not closed:
//
//	REOPENING CONDITION: the first customer who wants Docs without Track.
//
// Nothing forecloses it today — DistinctWorkspaceIDs is origin-agnostic, so enumeration
// works for rows of either provenance. ⚠ It enumerates from CONTENT (spaces ∪ pages), NOT
// from this table; an earlier version of this sentence said "from this very table", which
// reads as though a roster alone were enough to be enumerated. It is not, and that
// difference is the whole cold-start deadlock documented on DistinctWorkspaceIDs below.
// What WOULD foreclose it is this prune deleting Docs-native rows. Before the source column
// that was prevented only by COINCIDENCE: a workspace Track never heard of pulls an empty
// roster and hits the empty-pull guard above — a guard written to survive a bad fetch, which
// says nothing about who owns a row. And inside a MIXED workspace there was no protection at
// all, since a Docs-native member is simply absent from Track's roster and the prune deletes
// what is absent. TestReconcileWorkspace_NeverPrunesRowsItDidNotSync fails if that returns.
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
			// source is stamped on INSERT but deliberately NOT in the DO UPDATE set: if a
			// Docs-native row already exists for this email, Track reporting the same
			// person updates their role but must not seize ownership of the row — doing so
			// would make it prunable on the next pass, which is the very deletion this
			// design prevents.
			`INSERT INTO workspace_members (workspace_id, email, role, member_id, synced_at, source)
			 VALUES ($1, $2, $3, $4, now(), 'track')
			 ON CONFLICT (workspace_id, email)
			 DO UPDATE SET role = EXCLUDED.role, member_id = EXCLUDED.member_id, synced_at = now()`,
			workspaceID, m.Email, m.Role, m.MemberID); err != nil {
			return 0, 0, fmt.Errorf("membership: upsert %s: %w", m.Email, err)
		}
	}

	// Prune departed — SCOPED to this workspace, only among rows not in the pulled set, and
	// only among rows THIS SYNCER CREATED. See the provenance note above: without the source
	// filter a Docs-native member of a mixed workspace is deleted on the first reconcile,
	// because they are absent from Track's roster by definition.
	ct, err := tx.Exec(ctx,
		`DELETE FROM workspace_members
		 WHERE workspace_id = $1 AND source = 'track' AND email <> ALL($2::text[])`,
		workspaceID, emails)
	if err != nil {
		return 0, 0, fmt.Errorf("membership: prune: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return len(refs), int(ct.RowsAffected()), nil
}
