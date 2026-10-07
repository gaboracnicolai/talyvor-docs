// Package team keeps the teams a page or space can be shared with, and who is in each (B28.446).
// A team belongs to one workspace; its creator manages the roster. The permission rule engine reads
// team_members directly when it meets a subject_type="team" grant.
package team

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Team is one team with its members, as the share panel shows it. CanManage is computed for the
// caller: only the creator may change the roster.
type Team struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	Members   []string  `json:"members"`
	CanManage bool      `json:"can_manage"`
}

var (
	// ErrNotFound — no such team in this workspace (or no such member on it).
	ErrNotFound = errors.New("team: not found")
	// ErrForbidden — the caller did not create the team, so may not change who is in it.
	ErrForbidden = errors.New("team: only the team's creator can change its members")
	// ErrDuplicate — the workspace already has a team with this name.
	ErrDuplicate = errors.New("team: a team with this name already exists")
	// ErrUnknownMember — the member id is not a member of this workspace.
	ErrUnknownMember = errors.New("team: not a member of this workspace")
	// ErrInvalidName — the name is empty or longer than maxNameLen.
	ErrInvalidName = fmt.Errorf("team: a name of 1 to %d characters is required", maxNameLen)
)

// maxNameLen bounds a team name; the share panel shows it on one line.
const maxNameLen = 80

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
	return &Store{pool: db}
}

// List returns the workspace's teams, by name, each with its member ids.
func (s *Store) List(ctx context.Context, wsID, caller string) ([]Team, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.name, t.created_by, t.created_at,
		       COALESCE(array_agg(m.member_id ORDER BY m.added_at) FILTER (WHERE m.member_id IS NOT NULL), '{}')
		  FROM teams t
		  LEFT JOIN team_members m ON m.team_id = t.id AND m.workspace_id = t.workspace_id
		 WHERE t.workspace_id = $1
		 GROUP BY t.id
		 ORDER BY t.name`, wsID)
	if err != nil {
		return nil, fmt.Errorf("team: list: %w", err)
	}
	defer rows.Close()
	out := []Team{}
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedBy, &t.CreatedAt, &t.Members); err != nil {
			return nil, fmt.Errorf("team: list scan: %w", err)
		}
		t.CanManage = t.CreatedBy == caller
		out = append(out, t)
	}
	return out, rows.Err()
}

// Create adds a team to the workspace, created (and so managed) by createdBy.
func (s *Store) Create(ctx context.Context, wsID, name, createdBy string) (*Team, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxNameLen {
		return nil, ErrInvalidName
	}
	t := Team{Name: name, CreatedBy: createdBy, Members: []string{}, CanManage: true}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO teams (workspace_id, name, created_by) VALUES ($1, $2, $3)
		RETURNING id, created_at`, wsID, name, createdBy).Scan(&t.ID, &t.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, ErrDuplicate
	}
	if err != nil {
		return nil, fmt.Errorf("team: create: %w", err)
	}
	return &t, nil
}

// manage confirms teamID is in wsID and that actor created it.
func (s *Store) manage(ctx context.Context, wsID, teamID, actor string) error {
	var createdBy string
	err := s.pool.QueryRow(ctx,
		`SELECT created_by FROM teams WHERE id = $1 AND workspace_id = $2`, teamID, wsID).Scan(&createdBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("team: read: %w", err)
	}
	if createdBy != actor {
		return ErrForbidden
	}
	return nil
}

// AddMember puts memberID on the team. memberID must belong to the workspace. Adding someone who is
// already on the team is not an error.
func (s *Store) AddMember(ctx context.Context, wsID, teamID, memberID, actor string) error {
	if err := s.manage(ctx, wsID, teamID, actor); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO team_members (team_id, workspace_id, member_id, added_by)
		SELECT $1, $2, $3, $4
		 WHERE EXISTS (SELECT 1 FROM workspace_members WHERE workspace_id = $2 AND member_id = $3)
		ON CONFLICT (team_id, member_id) DO NOTHING`, teamID, wsID, memberID, actor)
	if err != nil {
		return fmt.Errorf("team: add member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var onTeam bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM team_members WHERE team_id = $1 AND member_id = $2)`,
			teamID, memberID).Scan(&onTeam); err != nil {
			return fmt.Errorf("team: add member: %w", err)
		}
		if !onTeam {
			return ErrUnknownMember
		}
	}
	return nil
}

// RemoveMember takes memberID off the team; every team grant stops applying to them at once.
func (s *Store) RemoveMember(ctx context.Context, wsID, teamID, memberID, actor string) error {
	if err := s.manage(ctx, wsID, teamID, actor); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM team_members WHERE team_id = $1 AND workspace_id = $2 AND member_id = $3`,
		teamID, wsID, memberID)
	if err != nil {
		return fmt.Errorf("team: remove member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
