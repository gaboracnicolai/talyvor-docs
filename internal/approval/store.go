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

// PendingItem is an inbox row: the request plus the space its page
// lives in. The space is NOT on approval_requests — it is joined from
// pages — so it belongs to the one query that needs it rather than to
// ApprovalRequest, where every other producer would leave it blank.
//
// The embedded struct is flattened by encoding/json, so the wire shape
// is the ApprovalRequest one plus "space_id".
type PendingItem struct {
	ApprovalRequest
	SpaceID string `json:"space_id"`
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

// scanPendingItem reads requestCols followed by p.space_id — the
// SELECT list in ListPending, in that order. It is written out rather
// than layered on scanRequest because a Scan takes its destinations
// positionally: any drift between the two lists is a scan-count or
// type error on the first real row, which
// TestPending_CarriesTheSpaceThePageLivesIn_RealPG executes.
func scanPendingItem(s interface{ Scan(...any) error }) (*PendingItem, error) {
	var it PendingItem
	if err := s.Scan(
		&it.ID, &it.PageID, &it.WorkspaceID, &it.RequestedBy, &it.Reviewers,
		&it.Message, &it.DueDate, &it.Status, &it.CreatedAt, &it.UpdatedAt,
		&it.SpaceID,
	); err != nil {
		return nil, err
	}
	return &it, nil
}

func scanDecision(s interface{ Scan(...any) error }) (*ReviewDecision, error) {
	var d ReviewDecision
	if err := s.Scan(&d.ID, &d.RequestID, &d.ReviewerID, &d.Decision, &d.Comment, &d.CreatedAt); err != nil {
		return nil, err
	}
	return &d, nil
}

// ─── RequestApproval ─────────────────────────────────

// RequestApprovalInWorkspaces is the SEC-4 L2 gate for RequestApproval: it opens a review (and
// flips pages.doc_status → in-review) only if the page lives in one of the caller's verified
// workspaces (wsIDs = authz.WorkspaceIDs(ctx)). Foreign pageID → ErrNotFound (404, no oracle).
//
// This holds ON ITS OWN — the assert reads authz.WorkspaceIDs (the /v1 authz middleware's verified
// set), not the route enforcer. The bare-id doc_status flip inside RequestApproval was previously
// gated SOLELY by the route's pageEnf.Require wiring.
func (s *Store) RequestApprovalInWorkspaces(ctx context.Context, pageID, workspaceID, requestedBy string, reviewers []string, message string, dueDate *time.Time, wsIDs []string) (*ApprovalRequest, error) {
	if s.pool == nil {
		return nil, errors.New("approval: no pool")
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pages WHERE id = $1 AND workspace_id = ANY($2))`,
		pageID, wsIDs,
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("approval: scope check: %w", err)
	}
	if !exists {
		return nil, ErrNotFound
	}
	return s.RequestApproval(ctx, pageID, workspaceID, requestedBy, reviewers, message, dueDate)
}

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
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- GATED IN-METHOD: RequestApproval is reached from the handler only via RequestApprovalInWorkspaces (above), which asserts the page ∈ the caller's verified workspaces (pages.workspace_id = ANY) → ErrNotFound before this flip. Holds on its own, independent of the route enforcer. (Direct RequestApproval calls are server-side seeds only.)
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
//
// wsIDs is the caller's verified workspace set (SEC-4 L2). The whole
// operation is gated on the request living in one of those workspaces
// — if not, we return ErrNotFound BEFORE any decision write, so a
// caller can't drive a request they can't see. Once that check
// passes, the subsequent bare-id statements (page lookup, request +
// page flips) are safe: the request is confirmed in-workspace and its
// page_id is owned by the same request.
//
// ⚠⚠ AND THE PARAGRAPH ABOVE REASONS ABOUT THE OUTER RING ONLY — WHICH IS HOW THE INNER ONE STAYED
// OPEN. pageID is the page the ROUTE'S ENFORCER AUTHORIZED, and it is required for the same reason
// wsIDs is. `POST /spaces/{spaceID}/pages/{pageID}/approval/{requestID}/decide` is
// `.With(pageEnf.Require(AccessView))`, so the gate answers about {pageID}; this statement used to
// name only {requestID}, with the WORKSPACE — a ring both are already inside — the only thing
// between them. MEASURED through the real routes on real Postgres (crosspage_realpg_test.go): a
// named reviewer who is REFUSED at the request's own address, because he has no grant on the
// private space the page lives in, recorded his decision by naming a page of his own in the URL —
// and since he was the only reviewer it was final, so the request AND the private page both
// flipped to `approved`. The product's own position is that he may not act on that page; the
// borrowed address overrode it.
//
// Its sibling `PublishApproved` on the next handler down has always taken the page id from the
// URL. `Decide` was the outlier — the same shape as changelog (#82), permission (#83) and
// database rows/views (#84), and the first of the four found by a guard rather than by reading:
// internal/routeguard asserts that a handler gated on {P} reads {P}.
//
// page_id is NOT NULL with an FK to pages (migrations/0009), so scoping by it makes no request
// unreachable; every SPA caller already builds the URL from the page whose request it is showing.
func (s *Store) Decide(ctx context.Context, requestID, pageID, reviewerID, decision, comment string, wsIDs []string) error {
	if s.pool == nil {
		return errors.New("approval: no pool")
	}
	if !validDecisions[decision] {
		return fmt.Errorf("approval: invalid decision %q", decision)
	}

	// Scope gate: the request must be ON THE PAGE THE ROUTE AUTHORIZED, and in one of the
	// caller's workspaces, before we touch any decision row. A mismatched (page, request) pair
	// answers 404 and not 403: 403 would confirm the id exists somewhere the caller cannot reach.
	var inWorkspace bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM approval_requests
          WHERE id = $1 AND page_id = $2 AND workspace_id = ANY($3))`,
		requestID, pageID, wsIDs,
	).Scan(&inWorkspace); err != nil {
		return fmt.Errorf("approval: scope check: %w", err)
	}
	if !inWorkspace {
		return ErrNotFound
	}

	// ⚠ THE ROW COUNT IS THE ONLY REVIEWER TEST THIS METHOD HAS, AND IT USED TO BE DISCARDED.
	// The WHERE clause is what stops a non-reviewer WRITING — that much this package already had
	// written down, in crosspage_realpg_test.go's own words ("a stranger's decision matches no
	// row, so this is not 'anyone can decide anything'"). What nothing asked was what the stranger
	// is TOLD: zero rows fell straight through to aggregate() and the handler answered
	// 200 {"ok":true}. On the ordinary NON-PRIVATE space, resolveAccess hands AccessView to every
	// workspace member with no grant at all, so the route's own gate admits them — the success was
	// reachable by design, not by misconfiguration, and a quorum workflow whose failed click is
	// byte-identical to a successful one cannot be operated (the item stays in the caller's
	// inbox forever, because that inbox is these same rows).
	//
	// RETURNING BEFORE aggregate() IS PART OF THE FIX, not tidiness: aggregate + the two flips
	// below are the write half, and a call that recorded nothing must not re-drive the request's
	// or the page's state at all.
	tag, err := s.pool.Exec(ctx,
		`UPDATE review_decisions
        SET decision = $1, comment = $2
        WHERE request_id = $3 AND reviewer_id = $4`,
		decision, comment, requestID, reviewerID,
	)
	if err != nil {
		return fmt.Errorf("approval: update decision: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// 403 and not 404: the scope gate above has already established that this caller may reach
		// this request, and `GET …/approval` serves them the request WITH its reviewers array, so
		// naming the refusal discloses nothing they cannot already read. 404 here would also be a
		// lie of a different kind — the request is right where they asked.
		return ErrNotReviewer
	}

	agg, err := s.aggregate(ctx, requestID)
	if err != nil {
		return err
	}
	if agg == ApprovalPending {
		return nil
	}

	// Final state — flip the request + the page in lockstep.
	//
	// ⚠ THE `SELECT page_id FROM approval_requests WHERE id = $1` THAT USED TO STAND HERE IS
	// DELETED, and it is the scope gate above that earns the deletion, not tidiness: that gate now
	// returns ErrNotFound unless the request's page_id IS pageID, so the lookup could only ever
	// return the value already in hand. Control C6 removes the `page_id = $2` predicate and the
	// page-flip assertion reddens, which is the record that this deletion rests on the predicate
	// rather than on the parameter's name.
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- SCOPE-GATED upstream in Decide: the request is confirmed to be ON pageID and in the caller's workspace (SELECT ... WHERE id AND page_id AND workspace_id = ANY(wsIDs) → ErrNotFound) BEFORE any write; requestID is that request.
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
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- SCOPE-GATED upstream in Decide (see above): pageID is the page the ROUTE'S ENFORCER authorized, and the gate confirmed THIS request is on it and in the caller's workspace.
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
	// A STREAM THAT FAILED MID-FLIGHT IS NOT A COMPLETE COUNT, AND WITHOUT THIS IT IS
	// INDISTINGUISHABLE FROM ONE. When Postgres raises an error while rows are being produced,
	// pgx delivers every row made so far — each with a nil Scan error — and then Next() returns
	// false exactly as it does at a clean end of stream; the reason is only ever in rows.Err().
	// The counters below make that silence one-directional: a row that never arrived cannot be
	// `rejected` and cannot be `pending`, so a truncated read can only move the verdict TOWARD
	// approval. Measured on real Postgres through Decide before this line existed — two of four
	// rows read, a reviewer's `rejected` row unread, Decide returning nil, and both
	// approval_requests.status and pages.doc_status written as "approved".
	// See decisionstream_realpg_test.go.
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("approval: aggregate decisions: %w", err)
	}
	if rejected > 0 {
		return ApprovalRejected, nil
	}
	if pending == 0 && approved > 0 {
		return ApprovalApproved, nil
	}
	return ApprovalPending, nil
}

// ─── SEC-4 Layer 2: workspace-scoped by-id ops ─────────────
//
// ErrNotFound signals a by-id op resolved to no row IN THE CALLER'S
// WORKSPACES — the handler maps it to 404. Distinct from a raw DB
// error so a real failure is never masked as not-found. Each by-id
// method below takes the caller's verified workspace set (wsIDs,
// resolved from membership, never from a client header/body) and
// filters on it: approval_requests scopes directly on workspace_id;
// review_decisions has no workspace_id so it scopes through its parent
// request; page-touching ops scope on pages.workspace_id. wsIDs empty
// (caller has no membership) matches nothing → ErrNotFound / empty
// list. This is the IDOR cure.
var ErrNotFound = errors.New("approval: not found in workspace")

// ErrNotReviewer signals that Decide matched no review_decisions row: the caller is not an
// assigned reviewer on that request. The handler maps it to 403, DISTINCT from ErrNotFound's 404,
// because the two say different true things — ErrNotFound means "not here", this means "here, and
// not yours to decide". It exists because the alternative that shipped was a 200 for a write that
// never happened (nonreviewer_realpg_test.go).
var ErrNotReviewer = errors.New("approval: not an assigned reviewer on this request")

// PageInWorkspaces reports whether pageID belongs to one of the caller's verified workspaces —
// SEC-4 L2: the page-scoped read endpoints 404 a foreign page id (no existence oracle) rather than
// returning an empty 200.
func (s *Store) PageInWorkspaces(ctx context.Context, pageID string, wsIDs []string) (bool, error) {
	if s.pool == nil {
		return false, nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pages WHERE id = $1 AND workspace_id = ANY($2))`, pageID, wsIDs).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// ─── Lookups ─────────────────────────────────────────

func (s *Store) GetRequest(ctx context.Context, requestID string, wsIDs []string) (*ApprovalRequest, error) {
	if s.pool == nil {
		return nil, errors.New("approval: no pool")
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+requestCols+` FROM approval_requests WHERE id = $1 AND workspace_id = ANY($2)`,
		requestID, wsIDs)
	req, err := scanRequest(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return req, nil
}

func (s *Store) GetDecisions(ctx context.Context, requestID string, wsIDs []string) ([]ReviewDecision, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+decisionCols+` FROM review_decisions
        WHERE request_id = $1
          AND request_id IN (SELECT id FROM approval_requests WHERE workspace_id = ANY($2))
        ORDER BY created_at ASC`,
		requestID, wsIDs,
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

func (s *Store) ListByPage(ctx context.Context, pageID string, wsIDs []string) ([]ApprovalRequest, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+requestCols+` FROM approval_requests
        WHERE page_id = $1
          AND page_id IN (SELECT id FROM pages WHERE workspace_id = ANY($2))
        ORDER BY created_at DESC`,
		pageID, wsIDs,
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
//
// wsIDs is the caller's verified workspace set (SEC-4 L2). Previously
// this scoped on a single workspace id sourced from a URL param
// (chi.URLParam "wsID") — a deceptive shape that let a caller name a
// workspace they don't belong to. Now the scope is ANY($2) over the
// caller's membership set, so a foreign workspace simply yields no rows.
//
// It returns PendingItem, not ApprovalRequest: an inbox row has to be
// able to ADDRESS its page, and a page's address in this product is
// /spaces/{spaceID}/pages/{pageID}. The request row carries only
// page_id, so the space came back empty and the SPA's "Open" button
// navigated to a URL its own route table sends to Not found. The JOIN
// below is the whole fix (inbox_space_realpg_test.go).
func (s *Store) ListPending(ctx context.Context, reviewerID string, wsIDs []string) ([]PendingItem, error) {
	if s.pool == nil {
		return nil, nil
	}
	// The pages JOIN can neither drop a row nor invent one:
	// approval_requests.page_id is NOT NULL REFERENCES pages(id) ON
	// DELETE CASCADE (0009), and pages.space_id is NOT NULL REFERENCES
	// spaces(id) (0002). It is not a second tenancy gate either — the
	// scope is still a.workspace_id = ANY($2), unchanged.
	rows, err := s.pool.Query(ctx,
		`SELECT `+prefixed("a", requestCols)+`, p.space_id
        FROM approval_requests a
        JOIN review_decisions d ON d.request_id = a.id
        JOIN pages p ON p.id = a.page_id
        WHERE d.reviewer_id = $1 AND d.decision = 'pending'
          AND a.workspace_id = ANY($2) AND a.status = 'pending'
        ORDER BY a.created_at DESC`,
		reviewerID, wsIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("approval: list pending: %w", err)
	}
	defer rows.Close()
	var out []PendingItem
	for rows.Next() {
		it, err := scanPendingItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

// ─── PublishApproved ─────────────────────────────────

// PublishApproved confirms the approved page is live. It re-checks
// the doc_status as a guard against a stale UI fire-and-forget POST.
//
// wsIDs is the caller's verified workspace set (SEC-4 L2). Both the
// status read and the publish write scope on pages.workspace_id, so a
// page outside the caller's workspaces is invisible → ErrNotFound.
func (s *Store) PublishApproved(ctx context.Context, pageID string, wsIDs []string) error {
	if s.pool == nil {
		return errors.New("approval: no pool")
	}
	var status string
	if err := s.pool.QueryRow(ctx,
		`SELECT doc_status FROM pages WHERE id = $1 AND workspace_id = ANY($2)`, pageID, wsIDs,
	).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("approval: page not found: %w", err)
	}
	if status != string(DocApproved) {
		return errors.New("approval: page must be approved before publishing")
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE pages SET doc_status = $1 WHERE id = $2 AND workspace_id = ANY($3)`,
		string(DocApproved), pageID, wsIDs,
	); err != nil {
		return err
	}
	return nil
}

// SetStatus is the small write surface other packages use to
// reset a page back to draft (e.g. on edit). Centralised here so
// the state-machine logic stays in one place.
//
// wsIDs is the caller's verified workspace set (SEC-4 L2): the write
// only lands on a page in one of those workspaces.
func (s *Store) SetStatus(ctx context.Context, pageID string, status DocStatus, wsIDs []string) error {
	if s.pool == nil {
		return errors.New("approval: no pool")
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE pages SET doc_status = $1 WHERE id = $2 AND workspace_id = ANY($3)`,
		string(status), pageID, wsIDs,
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
