// Package permission is Docs's access-control surface. The Store
// owns the permissions table; the resolveAccess evaluator is the
// pure rule engine used by Check() and exposed for unit testing.
//
// Roles in a workspace are intentionally NOT modelled here — Docs
// treats workspace_id as an opaque tenant key (see migrations/0001).
// Two practical consequences:
//
//  1. "Workspace owner" can't be derived; we honour the contract by
//     treating the resource's creator as admin.
//  2. Team membership can't be resolved (teams live upstream and are
//     never propagated to Check), so team-based grants are NOT
//     supported: resolveAccess ignores subject_type="team", and Grant
//     REJECTS it at write time so an inert grant can't be persisted and
//     mislead an admin into thinking they shared something. Only
//     "member" and "everyone" subject types are honored.
package permission

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ResourceType string

const (
	ResourceSpace ResourceType = "space"
	ResourcePage  ResourceType = "page"
)

type AccessLevel string

const (
	AccessNone    AccessLevel = "none"
	AccessView    AccessLevel = "view"
	AccessComment AccessLevel = "comment"
	AccessEdit    AccessLevel = "edit"
	AccessAdmin   AccessLevel = "admin"
)

// validAccessForGrant enumerates the levels callers may persist.
// AccessNone is not a grant — revoking is done via Revoke().
var validAccessForGrant = map[AccessLevel]bool{
	AccessView: true, AccessComment: true, AccessEdit: true, AccessAdmin: true,
}

// validSubjectType enumerates the subject types the rule engine actually honors (resolveAccess).
// "team" is deliberately EXCLUDED: Docs cannot resolve team membership (teams live upstream and are
// never propagated to Check), so resolveAccess skips a team grant — it would persist and grant
// nothing. Rejecting it at write time turns that silent lie ("you shared this") into a loud error.
// A grant with any other subject type was equally inert on the read path.
var validSubjectType = map[string]bool{
	"member": true, "everyone": true,
}

type Permission struct {
	ID           string       `json:"id"`
	ResourceType ResourceType `json:"resource_type"`
	ResourceID   string       `json:"resource_id"`
	SubjectType  string       `json:"subject_type"`
	SubjectID    string       `json:"subject_id"`
	Access       AccessLevel  `json:"access"`
	WorkspaceID  string       `json:"workspace_id"`
	GrantedBy    string       `json:"granted_by"`
	CreatedAt    time.Time    `json:"created_at"`
}

type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct{ pool pgxDB }

func NewStore(pool *pgxpool.Pool) *Store {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newStore(db)
}

func newStore(db pgxDB) *Store { return &Store{pool: db} }

const cols = `id, resource_type, resource_id, subject_type, subject_id, access, workspace_id, granted_by, created_at`

func scan(s interface{ Scan(...any) error }) (*Permission, error) {
	var p Permission
	if err := s.Scan(
		&p.ID, &p.ResourceType, &p.ResourceID, &p.SubjectType, &p.SubjectID,
		&p.Access, &p.WorkspaceID, &p.GrantedBy, &p.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &p, nil
}

// ─── SEC-4 Layer 2: workspace-scoped by-id ops ─────────────
//
// The permissions table carries workspace_id directly, so a by-id op scopes with a single
// `AND workspace_id = ANY($n)` predicate. The wsIDs slice is the caller's VERIFIED membership set
// (authz.WorkspaceIDs) — never a URL/body value — so a grant in a workspace the caller doesn't
// belong to is invisible: 0 rows → ErrNotFound → 404, no existence oracle. wsIDs empty (caller has
// no membership) matches nothing → ErrNotFound. This is the fail-closed cure for the cross-tenant
// leak: RevokeByID could otherwise delete ANY grant (including admin) in ANY workspace by its id.

// ErrNotFound signals a by-id op resolved to no row IN THE CALLER'S WORKSPACES — the handler maps
// it to 404. Distinct from a raw DB error so a real failure is never masked as not-found.
var ErrNotFound = errors.New("permission: not found in workspace")

// Grant inserts or updates a permission. UNIQUE on (resource,
// subject) collapses duplicates so re-granting becomes an access
// upgrade rather than a duplicate row.
func (s *Store) Grant(ctx context.Context, p Permission) error {
	if s.pool == nil {
		return errors.New("permission: no pool")
	}
	if !validAccessForGrant[p.Access] {
		return fmt.Errorf("permission: invalid access %q", p.Access)
	}
	if p.SubjectType == "" || p.SubjectID == "" {
		return errors.New("permission: subject required")
	}
	if !validSubjectType[p.SubjectType] {
		// Only member/everyone are honored by resolveAccess; "team" (and anything else) is inert, so
		// persisting it would tell the admin a share happened when it did not. Fail loud instead.
		return fmt.Errorf("permission: unsupported subject_type %q (only \"member\" and \"everyone\" are honored)", p.SubjectType)
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO permissions (resource_type, resource_id, subject_type, subject_id, access, workspace_id, granted_by)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (resource_type, resource_id, subject_type, subject_id)
        DO UPDATE SET access = EXCLUDED.access, granted_by = EXCLUDED.granted_by`,
		string(p.ResourceType), p.ResourceID, p.SubjectType, p.SubjectID, string(p.Access), p.WorkspaceID, p.GrantedBy,
	)
	if err != nil {
		return fmt.Errorf("permission: grant: %w", err)
	}
	return nil
}

// Revoke removes the (resource, subject) permission. A revoke of a
// non-existent grant is a no-op.
func (s *Store) Revoke(ctx context.Context, resourceType ResourceType, resourceID, subjectType, subjectID string) error {
	if s.pool == nil {
		return errors.New("permission: no pool")
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM permissions
        WHERE resource_type = $1 AND resource_id = $2
          AND subject_type = $3 AND subject_id = $4`,
		string(resourceType), resourceID, subjectType, subjectID,
	)
	return err
}

// RevokeByID removes a single grant by its primary key. Used by the
// REST handler that exposes permission IDs. Scoped to the caller's verified workspaces (wsIDs):
// a grant outside them can't be deleted — 0 rows affected → ErrNotFound → 404. This is the worst
// op to leave unscoped, since a cross-tenant caller could otherwise revoke any grant (incl. admin)
// in a workspace they don't belong to.
func (s *Store) RevokeByID(ctx context.Context, id string, wsIDs []string) error {
	if s.pool == nil {
		return errors.New("permission: no pool")
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM permissions WHERE id = $1 AND workspace_id = ANY($2)`, id, wsIDs)
	if err != nil {
		return fmt.Errorf("permission: revoke by id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListForResource returns every grant attached to the given resource.
// The result is sorted by created_at so the UI shows the original
// owner first. Scoped to the caller's verified workspaces (wsIDs): a resource in a foreign
// workspace yields an empty list, never another tenant's grants. No ErrNotFound — a list of a
// foreign/absent resource is legitimately empty, not an error.
func (s *Store) ListForResource(ctx context.Context, resourceType ResourceType, resourceID string, wsIDs []string) ([]Permission, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+cols+` FROM permissions WHERE resource_type = $1 AND resource_id = $2
          AND workspace_id = ANY($3)
        ORDER BY created_at ASC`,
		string(resourceType), resourceID, wsIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("permission: list: %w", err)
	}
	defer rows.Close()
	var out []Permission
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ─── Resource context + rule engine ─────────────────────

// resourceContext carries the per-resource metadata the rule
// evaluator needs. The store's Check() hydrates this from pages /
// spaces; the evaluator below is pure so it can be unit-tested
// without DB fixtures.
type resourceContext struct {
	Type    ResourceType
	ID      string
	SpaceID string
	// WorkspaceID is the workspace that OWNS this resource. RequireAccess resolves the
	// caller's member id IN this workspace (authz.MemberIDForWorkspace) rather than
	// using authz.ActorOrEmpty, which collapses to "" for any caller with != 1
	// memberships and silently voided every per-member grant they held.
	WorkspaceID string
	// CreatedBy is the resource's author. Phase 8 honours the spec's
	// "owner always admin" contract via this — Docs has no
	// workspace-roles table.
	CreatedBy string
	// Private flips the default-deny / default-view behaviour.
	// Public spaces give workspace members at least view; private
	// require an explicit grant.
	Private bool
	// SpacePerms is the inherited-from-space permission list when
	// Type == ResourcePage. Always nil for ResourceSpace.
	SpacePerms []Permission
}

// rank turns an AccessLevel into a comparable integer. Anywhere the
// resolver picks between candidates it keeps the higher rank.
func rank(a AccessLevel) int {
	switch a {
	case AccessAdmin:
		return 4
	case AccessEdit:
		return 3
	case AccessComment:
		return 2
	case AccessView:
		return 1
	default:
		return 0
	}
}

// resolveAccess is the pure rule engine. Order of precedence:
//
//  1. If memberID == res.CreatedBy → admin.
//  2. Highest matching grant in (page-level + space-level) perms.
//  3. If still none and !res.Private → view default for members.
//  4. Otherwise none.
//
// "Highest" means the AccessLevel with the largest rank() value;
// explicit member grants and "everyone" grants both contribute.
func resolveAccess(res resourceContext, memberID string, perms []Permission) AccessLevel {
	if memberID != "" && memberID == res.CreatedBy {
		return AccessAdmin
	}
	best := AccessNone
	consider := func(p Permission) {
		// "member" must match exactly; "everyone" always matches. "team" (and any other subject
		// type) confers NOTHING: Docs can't resolve team membership, so a team grant is inert —
		// which is why Grant now rejects it at write time. Legacy team rows already in the table
		// fall through to the default and are ignored here, exactly as before.
		switch p.SubjectType {
		case "member":
			if p.SubjectID != memberID {
				return
			}
		case "everyone":
			// match
		default:
			// team, or anything else the write-side allow-list no longer permits: not applicable.
			return
		}
		if rank(p.Access) > rank(best) {
			best = p.Access
		}
	}
	for _, p := range perms {
		consider(p)
	}
	for _, p := range res.SpacePerms {
		consider(p)
	}
	if best != AccessNone {
		return best
	}
	// No explicit grant. Public space → workspace members get view;
	// private → none.
	if !res.Private {
		return AccessView
	}
	return AccessNone
}

// Check resolves the effective access for memberID against the
// resource. Callers prepare a resourceContext (typically from a
// space / page lookup) and pass it alongside the resource's own
// grants. Storing the lookup outside this package means handlers
// can wire any storage layer.
func (s *Store) Check(ctx context.Context, memberID string, res resourceContext, wsIDs []string) (AccessLevel, error) {
	perms, err := s.ListForResource(ctx, res.Type, res.ID, wsIDs)
	if err != nil {
		return AccessNone, err
	}
	return resolveAccess(res, memberID, perms), nil
}
