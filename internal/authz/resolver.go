package authz

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGResolver resolves memberships from workspace_members — the table the trackintegration
// syncer keeps in sync from Track (migration 0014). Keyed on email (idx_workspace_members_email),
// it returns one row per workspace the email is a member of: the (workspace_id, member_id, role)
// tuples Layer 2 scopes against.
type PGResolver struct{ pool *pgxpool.Pool }

func NewPGResolver(pool *pgxpool.Pool) *PGResolver { return &PGResolver{pool: pool} }

func (r *PGResolver) MembershipsByEmail(ctx context.Context, email string) ([]Membership, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT workspace_id, member_id, role FROM workspace_members WHERE email = $1`, email)
	if err != nil {
		return nil, fmt.Errorf("authz: memberships by email: %w", err)
	}
	defer rows.Close()
	var out []Membership
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.WorkspaceID, &m.MemberID, &m.Role); err != nil {
			return nil, fmt.Errorf("authz: scan membership: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
