// Package approval implements the document-approval workflow.
// Pages move through draft → in_review → approved | rejected;
// approval is granted by a quorum (all assigned reviewers
// approve), any single rejection blocks publication.
//
// The package is the single writer of the pages.doc_status column.
// page.Store reads pages but does NOT include doc_status in its
// SELECT/UPDATE allow-list — keeping the ownership boundary tight.
package approval

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── Types ──────────────────────────────────────────

type DocStatus string

const (
	DocDraft    DocStatus = "draft"
	DocInReview DocStatus = "in_review"
	DocApproved DocStatus = "approved"
	DocRejected DocStatus = "rejected"
	DocArchived DocStatus = "archived"
)

type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalRejected ApprovalStatus = "rejected"
)

type ApprovalRequest struct {
	ID          string         `json:"id"`
	PageID      string         `json:"page_id"`
	WorkspaceID string         `json:"workspace_id"`
	RequestedBy string         `json:"requested_by"`
	Reviewers   []string       `json:"reviewers"`
	Message     string         `json:"message"`
	DueDate     *time.Time     `json:"due_date,omitempty"`
	Status      ApprovalStatus `json:"status"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

type ReviewDecision struct {
	ID         string    `json:"id"`
	RequestID  string    `json:"request_id"`
	ReviewerID string    `json:"reviewer_id"`
	Decision   string    `json:"decision"`
	Comment    string    `json:"comment"`
	CreatedAt  time.Time `json:"created_at"`
}

// validDecisions enumerates the operator-side decisions a reviewer
// can submit. "pending" is the system-default we set on insert; it
// can't be re-submitted via Decide().
var validDecisions = map[string]bool{
	"approved": true,
	"rejected": true,
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

const requestCols = `id, page_id, workspace_id, requested_by, reviewers, message, due_date, status, created_at, updated_at`
const decisionCols = `id, request_id, reviewer_id, decision, comment, created_at`

func scanRequest(s interface{ Scan(...any) error }) (*ApprovalRequest, error) {
	var r ApprovalRequest
	if err := s.Scan(
		&r.ID, &r.PageID, &r.WorkspaceID, &r.RequestedBy, &r.Reviewers,
		&r.Message, &r.DueDate, &r.Status, &r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &r, nil
}

func scanDecision(s interface{ Scan(...any) error }) (*ReviewDecision, error) {
	var d ReviewDecision
	if err := s.Scan(&d.ID, &d.RequestID, &d.ReviewerID, &d.Decision, &d.Comment, &d.CreatedAt); err != nil {
		return nil, err
	}
	return &d, nil
}

// ─── RequestApproval ─────────────────────────────────

func (s *Store) RequestApproval(ctx context.Context, pageID, workspaceID, requestedBy string, reviewers []string, message string, dueDate *time.Time) (*ApprovalRequest, error) {
	if s.pool == nil {
		return nil, errors.New("approval: no pool")
	}
	if len(reviewers) == 0 {
		return nil, errors.New("approval: at least one reviewer required")
	}

	// Sanity check the page exists. Errors here surface as a 404 in
	// the handler — keeps a malicious caller from creating dangling
	// approval requests against deleted pages.
	var ok int
	if err := s.pool.QueryRow(ctx,
		`SELECT 1 FROM pages WHERE id = $1`, pageID,
	).Scan(&ok); err != nil {
		return nil, fmt.Errorf("approval: page not found: %w", err)
	}

	row := s.pool.QueryRow(ctx,
		`INSERT INTO approval_requests
        (page_id, workspace_id, requested_by, reviewers, message, due_date)
        VALUES ($1, $2, $3, $4, $5, $6)
        RETURNING `+requestCols,
		pageID, workspaceID, requestedBy, reviewers, message, dueDate,
	)
	req, err := scanRequest(row)
	if err != nil {
		return nil, fmt.Errorf("approval: insert request: %w", err)
	}

	// Seed a pending decision per reviewer. ON CONFLICT lets the
	// caller supply duplicate reviewer IDs without exploding.
	for _, r := range reviewers {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO review_decisions (request_id, reviewer_id)
            VALUES ($1, $2)
            ON CONFLICT (request_id, reviewer_id) DO NOTHING`,
			req.ID, r,
		); err != nil {
			return nil, fmt.Errorf("approval: seed decision: %w", err)
		}
	}

	// Flip the page into review.
	if _, err := s.pool.Exec(ctx,
		`UPDATE pages SET doc_status = $1 WHERE id = $2`,
		string(DocInReview), pageID,
	); err != nil {
		return nil, fmt.Errorf("approval: page status: %w", err)
	}

	return req, nil
}

// ─── Decide ──────────────────────────────────────────

// Decide records a reviewer's verdict and re-aggregates the request
// state. All-approved → ApprovalApproved (+ pages.doc_status =
// approved). Any-rejected → ApprovalRejected (+ rejected). Mixed
// with pending → no aggregate flip.
func (s *Store) Decide(ctx context.Context, requestID, reviewerID, decision, comment string) error {
	if s.pool == nil {
		return errors.New("approval: no pool")
	}
	if !validDecisions[decision] {
		return fmt.Errorf("approval: invalid decision %q", decision)
	}

	if _, err := s.pool.Exec(ctx,
		`UPDATE review_decisions
        SET decision = $1, comment = $2
        WHERE request_id = $3 AND reviewer_id = $4`,
		decision, comment, requestID, reviewerID,
	); err != nil {
		return fmt.Errorf("approval: update decision: %w", err)
	}

	agg, err := s.aggregate(ctx, requestID)
	if err != nil {
		return err
	}
	if agg == ApprovalPending {
		return nil
	}

	// Final state — flip the request + the page in lockstep.
	var pageID string
	if err := s.pool.QueryRow(ctx,
		`SELECT page_id FROM approval_requests WHERE id = $1`, requestID,
	).Scan(&pageID); err != nil {
		return fmt.Errorf("approval: lookup page: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE approval_requests SET status = $1, updated_at = NOW() WHERE id = $2`,
		string(agg), requestID,
	); err != nil {
		return fmt.Errorf("approval: update request: %w", err)
	}
	var nextDoc DocStatus
	if agg == ApprovalApproved {
		nextDoc = DocApproved
	} else {
		nextDoc = DocRejected
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE pages SET doc_status = $1 WHERE id = $2`,
		string(nextDoc), pageID,
	); err != nil {
		return fmt.Errorf("approval: update page status: %w", err)
	}
	return nil
}

// aggregate returns the final ApprovalStatus given the current
// review_decisions for the request. Any rejection wins; otherwise
// all-approved wins; otherwise still pending.
func (s *Store) aggregate(ctx context.Context, requestID string) (ApprovalStatus, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT decision FROM review_decisions WHERE request_id = $1`, requestID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var approved, rejected, pending int
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return "", err
		}
		switch d {
		case "approved":
			approved++
		case "rejected":
			rejected++
		default:
			pending++
		}
	}
	if rejected > 0 {
		return ApprovalRejected, nil
	}
	if pending == 0 && approved > 0 {
		return ApprovalApproved, nil
	}
	return ApprovalPending, nil
}

// ─── Lookups ─────────────────────────────────────────

func (s *Store) GetRequest(ctx context.Context, requestID string) (*ApprovalRequest, error) {
	if s.pool == nil {
		return nil, errors.New("approval: no pool")
	}
	row := s.pool.QueryRow(ctx, `SELECT `+requestCols+` FROM approval_requests WHERE id = $1`, requestID)
	return scanRequest(row)
}

func (s *Store) GetDecisions(ctx context.Context, requestID string) ([]ReviewDecision, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+decisionCols+` FROM review_decisions WHERE request_id = $1 ORDER BY created_at ASC`,
		requestID,
	)
	if err != nil {
		return nil, fmt.Errorf("approval: list decisions: %w", err)
	}
	defer rows.Close()
	var out []ReviewDecision
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

func (s *Store) ListByPage(ctx context.Context, pageID string) ([]ApprovalRequest, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+requestCols+` FROM approval_requests WHERE page_id = $1 ORDER BY created_at DESC`,
		pageID,
	)
	if err != nil {
		return nil, fmt.Errorf("approval: list by page: %w", err)
	}
	defer rows.Close()
	var out []ApprovalRequest
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ListPending returns every request where the named reviewer still
// has a pending decision. Powers the reviewer's "My approvals"
// inbox + the sidebar badge count.
func (s *Store) ListPending(ctx context.Context, reviewerID, workspaceID string) ([]ApprovalRequest, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+prefixed("a", requestCols)+`
        FROM approval_requests a
        JOIN review_decisions d ON d.request_id = a.id
        WHERE d.reviewer_id = $1 AND d.decision = 'pending'
          AND a.workspace_id = $2 AND a.status = 'pending'
        ORDER BY a.created_at DESC`,
		reviewerID, workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("approval: list pending: %w", err)
	}
	defer rows.Close()
	var out []ApprovalRequest
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ─── PublishApproved ─────────────────────────────────

// PublishApproved confirms the approved page is live. It re-checks
// the doc_status as a guard against a stale UI fire-and-forget POST.
func (s *Store) PublishApproved(ctx context.Context, pageID string) error {
	if s.pool == nil {
		return errors.New("approval: no pool")
	}
	var status string
	if err := s.pool.QueryRow(ctx,
		`SELECT doc_status FROM pages WHERE id = $1`, pageID,
	).Scan(&status); err != nil {
		return fmt.Errorf("approval: page not found: %w", err)
	}
	if status != string(DocApproved) {
		return errors.New("approval: page must be approved before publishing")
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE pages SET doc_status = $1 WHERE id = $2`,
		string(DocApproved), pageID,
	); err != nil {
		return err
	}
	return nil
}

// SetStatus is the small write surface other packages use to
// reset a page back to draft (e.g. on edit). Centralised here so
// the state-machine logic stays in one place.
func (s *Store) SetStatus(ctx context.Context, pageID string, status DocStatus) error {
	if s.pool == nil {
		return errors.New("approval: no pool")
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE pages SET doc_status = $1 WHERE id = $2`,
		string(status), pageID,
	)
	return err
}

// prefixed is a tiny helper that adds an alias to each column in a
// SELECT list. Used by ListPending where we JOIN approval_requests
// to review_decisions.
func prefixed(alias, list string) string {
	out := ""
	for _, part := range splitTrim(list, ",") {
		if out != "" {
			out += ", "
		}
		out += alias + "." + part
	}
	return out
}

// splitTrim splits on sep and trims whitespace from each piece.
// Tiny replacement for strings.Split + strings.TrimSpace to avoid
// pulling in `strings` here (the file imports nothing else).
func splitTrim(s, sep string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep[0] {
			out = append(out, trim(s[start:i]))
			start = i + 1
		}
	}
	out = append(out, trim(s[start:]))
	return out
}

func trim(s string) string {
	a, b := 0, len(s)
	for a < b && (s[a] == ' ' || s[a] == '\n' || s[a] == '\t') {
		a++
	}
	for b > a && (s[b-1] == ' ' || s[b-1] == '\n' || s[b-1] == '\t') {
		b--
	}
	return s[a:b]
}
