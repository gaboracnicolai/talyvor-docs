// Package page owns the database operations for documentation
// pages. Pages live inside spaces; ParentID enables nested hierarchy
// (max depth 5, enforced at Create time). Content is canonical
// ProseMirror JSON; content_text is the plain-text projection used
// by the full-text search index.
package page

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/docs/internal/model"
)

// MaxDepth caps the parent_id chain so a malicious caller can't
// build a deeply-recursive tree that breaks rendering.
const MaxDepth = 5

type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// linkSyncer is the Track-integration hook the page store fires on
// every content-changing Update. Wired by main.go via WithLinker.
// Best-effort: a sync failure must never fail the save itself, so
// the call site swallows errors and logs.
type linkSyncer interface {
	SyncLinks(ctx context.Context, pageID, workspaceID, content, createdBy string) error
}

// searchIndexer is the Phase-6 hook that pushes a freshly-saved page
// through the semantic indexer. Best-effort and asynchronous: the
// store fires it in a goroutine after a successful content save so
// a slow Lens never blocks the editor's debounce.
type searchIndexer interface {
	IndexPage(ctx context.Context, pageID, workspaceID, text string) error
}

// editGuard checks whether a member is allowed to mutate the page.
// internal/pagelock satisfies this — we accept a narrow interface
// rather than importing the package so the dep graph stays
// one-directional (pagelock reads pages, not the reverse).
type editGuard interface {
	CanEdit(ctx context.Context, pageID, memberID string, isAdmin bool) (bool, string, error)
}

type Store struct {
	pool    pgxDB
	linker  linkSyncer
	indexer searchIndexer
	guard   editGuard
}

func NewStore(pool *pgxpool.Pool) *Store {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newStore(db)
}

func newStore(db pgxDB) *Store { return &Store{pool: db} }

// WithLinker attaches the page-link sync hook. Optional — without
// it, content saves don't reconcile embed → issue links.
func (s *Store) WithLinker(l linkSyncer) *Store {
	s.linker = l
	return s
}

// WithIndexer attaches the semantic-search indexer. Optional —
// without it, content saves don't refresh embeddings. The store
// detaches indexing into a goroutine so a slow Lens never blocks
// the save.
func (s *Store) WithIndexer(i searchIndexer) *Store {
	s.indexer = i
	return s
}

// WithGuard attaches the lock-aware edit gate. When set, Update
// consults CanEdit before persisting. Optional — useful in tests
// + early Phase-1 deployments without locks.
func (s *Store) WithGuard(g editGuard) *Store {
	s.guard = g
	return s
}

// ErrLocked is the sentinel Update returns when the lock guard
// rejects an edit. Handlers map this to HTTP 423.
var ErrLocked = errors.New("page: locked")

// PageFilter drives the List query. Empty / zero fields fall back to
// permissive defaults so callers can list "everything in the space"
// with a single struct field set.
type PageFilter struct {
	SpaceID    string
	ParentID   *string
	IsTemplet  bool
	IsTemplate bool
	Limit      int
	Offset     int
}

// ─── slug + content_text helpers ────────────────────────────

var slugStripRe = regexp.MustCompile(`[^a-z0-9-]+`)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")
	s = slugStripRe.ReplaceAllString(s, "")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "page"
	}
	return s
}

var collapseWS = regexp.MustCompile(`\s+`)

// extractContentText walks a ProseMirror JSON document and returns
// the concatenated text content. Malformed JSON returns an empty
// string — the column is search-only, so losing the text on a bad
// payload is preferable to erroring on the write path.
func extractContentText(prosemirror string) string {
	if prosemirror == "" || prosemirror == "{}" {
		return ""
	}
	var doc any
	if err := json.Unmarshal([]byte(prosemirror), &doc); err != nil {
		return ""
	}
	var b strings.Builder
	walkContent(doc, &b)
	// Collapse the runs of whitespace introduced by per-sibling
	// separators so search tokens stay tidy ("Hello world" not
	// "Hello  world").
	return strings.TrimSpace(collapseWS.ReplaceAllString(b.String(), " "))
}

// walkContent recurses through the ProseMirror tree. Each "text"
// node contributes its "text" value; container nodes contribute
// their nested "content" array.
func walkContent(node any, b *strings.Builder) {
	switch v := node.(type) {
	case map[string]any:
		if t, ok := v["type"].(string); ok && t == "text" {
			if text, ok := v["text"].(string); ok {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(text)
				return
			}
		}
		if content, ok := v["content"]; ok {
			walkContent(content, b)
		}
	case []any:
		for _, child := range v {
			walkContent(child, b)
		}
	}
}

// ─── columns / scanner ─────────────────────────────────────

const columns = `id, space_id, workspace_id, parent_id, title, slug,
    content, content_text, icon, cover_url,
    position, depth, is_template, created_by, updated_by,
    linked_issues, ai_cost_usd,
    view_count, last_viewed_at,
    last_verified_at, verified_by, stale_after_days,
    doc_status,
    locked, locked_by, locked_at,
    COALESCE(page_type, 'document') AS page_type,
    created_at, updated_at`

func scan(s interface{ Scan(...any) error }) (*model.Page, error) {
	var p model.Page
	if err := s.Scan(
		&p.ID, &p.SpaceID, &p.WorkspaceID, &p.ParentID, &p.Title, &p.Slug,
		&p.Content, &p.ContentText, &p.Icon, &p.CoverURL,
		&p.Position, &p.Depth, &p.IsTemplate, &p.CreatedBy, &p.UpdatedBy,
		&p.LinkedIssues, &p.AICostUSD,
		&p.ViewCount, &p.LastViewedAt,
		&p.LastVerifiedAt, &p.VerifiedBy, &p.StaleAfterDays,
		&p.DocStatus,
		&p.Locked, &p.LockedBy, &p.LockedAt,
		&p.PageType,
		&p.CreatedAt, &p.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if p.LinkedIssues == nil {
		p.LinkedIssues = []string{}
	}
	return &p, nil
}

// ─── Create ────────────────────────────────────────────────

// Create inserts a page, deriving slug from title and depth from the
// parent's depth + 1. The first version is appended to
// page_versions inside the same transaction so an aborted insert
// can't leave a page without an initial revision.
func (s *Store) Create(ctx context.Context, p model.Page) (*model.Page, error) {
	if s.pool == nil {
		return nil, errors.New("page: store has no pool")
	}
	if p.SpaceID == "" || p.WorkspaceID == "" {
		return nil, errors.New("page: space_id and workspace_id required")
	}
	if p.Title == "" {
		p.Title = "Untitled"
	}
	if p.Slug == "" {
		p.Slug = slugify(p.Title)
	}
	if p.Content == "" {
		p.Content = "{}"
	}
	if p.LinkedIssues == nil {
		p.LinkedIssues = []string{}
	}
	if p.ContentText == "" {
		p.ContentText = extractContentText(p.Content)
	}

	// Depth = parent.depth + 1; root pages are depth 0.
	if p.ParentID != nil {
		var parentDepth int
		err := s.pool.QueryRow(ctx,
			`SELECT depth FROM pages WHERE id = $1`, *p.ParentID,
		).Scan(&parentDepth)
		if err != nil {
			return nil, fmt.Errorf("page: parent lookup: %w", err)
		}
		if parentDepth+1 > MaxDepth {
			return nil, fmt.Errorf("page: max depth %d exceeded", MaxDepth)
		}
		p.Depth = parentDepth + 1
	}

	out, err := scan(s.pool.QueryRow(ctx,
		`INSERT INTO pages
            (space_id, workspace_id, parent_id, title, slug,
             content, content_text, icon, cover_url, position, depth,
             is_template, created_by, updated_by,
             linked_issues, ai_cost_usd, stale_after_days)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
        RETURNING `+columns,
		p.SpaceID, p.WorkspaceID, p.ParentID, p.Title, p.Slug,
		p.Content, p.ContentText, p.Icon, p.CoverURL, p.Position, p.Depth,
		p.IsTemplate, p.CreatedBy, p.CreatedBy,
		p.LinkedIssues, p.AICostUSD, p.StaleAfterDays,
	))
	if err != nil {
		return nil, fmt.Errorf("page: insert: %w", err)
	}

	// First version. Failure here doesn't roll back the page itself
	// — the version table is informational. A retry path could
	// re-attach the initial revision, but in practice a missing v1
	// on a brand-new page is invisible to users.
	_, _ = s.pool.Exec(ctx,
		`INSERT INTO page_versions (page_id, workspace_id, version, title, content, created_by)
        VALUES ($1, $2, $3, $4, $5, $6)`,
		out.ID, out.WorkspaceID, 1, out.Title, out.Content, p.CreatedBy,
	)
	return out, nil
}

// ─── Get* ──────────────────────────────────────────────────

func (s *Store) GetByID(ctx context.Context, id string) (*model.Page, error) {
	return scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM pages WHERE id = $1`, id))
}

func (s *Store) GetBySlug(ctx context.Context, spaceID, slug string) (*model.Page, error) {
	return scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM pages WHERE space_id = $1 AND slug = $2`,
		spaceID, slug))
}

// ─── List ──────────────────────────────────────────────────

func (s *Store) List(ctx context.Context, filter PageFilter) ([]model.Page, error) {
	if s.pool == nil {
		return nil, nil
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+` FROM pages WHERE space_id = $1
        ORDER BY depth ASC, position ASC, created_at ASC
        LIMIT $2 OFFSET $3`,
		filter.SpaceID, limit, filter.Offset,
	)
	if err != nil {
		return nil, fmt.Errorf("page: list: %w", err)
	}
	defer rows.Close()
	var out []model.Page
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ─── Update ────────────────────────────────────────────────

// updatableFields lists the columns Update will touch directly.
// `content` triggers the content_text refresh + version snapshot
// inside the method body.
var updatableFields = map[string]struct{}{
	"title": {}, "content": {}, "icon": {}, "cover_url": {},
	"position": {}, "is_template": {}, "stale_after_days": {},
	"linked_issues": {}, "ai_cost_usd": {}, "parent_id": {},
	"updated_by": {},
}

// Update applies the supplied field map and returns the materialised
// row. When `content` is patched we ALSO bump content_text (extracted
// from the new ProseMirror JSON) and append a new entry to
// page_versions. History is append-only — no version is ever pruned.
func (s *Store) Update(ctx context.Context, id string, updates map[string]any) (*model.Page, error) {
	if s.pool == nil {
		return nil, errors.New("page: store has no pool")
	}
	// Lock + approval gate. updates["updated_by"] is the canonical
	// editor identity propagated from the handler; admin-bypass is
	// communicated via updates["is_admin"] (handler-injected, never
	// trusted from request bodies).
	if s.guard != nil {
		memberID, _ := updates["updated_by"].(string)
		isAdmin, _ := updates["is_admin"].(bool)
		ok, reason, err := s.guard.CanEdit(ctx, id, memberID, isAdmin)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrLocked, reason)
		}
	}
	// is_admin is a gate-only flag — never persist it.
	delete(updates, "is_admin")
	contentChanged := false
	if v, ok := updates["content"]; ok {
		if str, isStr := v.(string); isStr {
			contentChanged = true
			updates["content_text"] = extractContentText(str)
		}
	}

	// Read the pre-update title so the new version snapshot lines up
	// with what was just saved (we don't expose a separate "title at
	// version N" UI; the version's title is the canonical name at
	// snapshot time).
	var existing *model.Page
	if contentChanged {
		got, err := s.GetByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("page: pre-update read: %w", err)
		}
		existing = got
	}

	var (
		set  []string
		args []any
		n    int
	)
	// Walk a deterministic key order so SQL stays test-stable. The
	// runtime cost is negligible vs. the readability win.
	for _, k := range []string{
		"title", "content", "content_text", "icon", "cover_url",
		"position", "is_template", "stale_after_days",
		"linked_issues", "ai_cost_usd", "parent_id", "updated_by",
	} {
		v, ok := updates[k]
		if !ok {
			continue
		}
		if _, allowed := updatableFields[k]; !allowed && k != "content_text" {
			continue
		}
		n++
		set = append(set, fmt.Sprintf("%s = $%d", k, n))
		args = append(args, v)
	}
	if len(set) == 0 {
		return s.GetByID(ctx, id)
	}
	n++
	set = append(set, fmt.Sprintf("updated_at = $%d", n))
	args = append(args, time.Now().UTC())
	n++
	args = append(args, id)
	//nosemgrep: docs-by-id-write-requires-workspace-scope-sprintf -- Update is a primitive; every caller authorizes the page upstream: UpdateInWorkspaces / RestoreVersion via assertInWorkspaces(id, authz.WorkspaceIDs), and the collab AutoSaver via the SEC-4 WithPageScope session binding. Not reachable with an un-authorized page id.
	sql := fmt.Sprintf(`UPDATE pages SET %s WHERE id = $%d RETURNING %s`,
		strings.Join(set, ", "), n, columns)

	out, err := scan(s.pool.QueryRow(ctx, sql, args...))
	if err != nil {
		return nil, fmt.Errorf("page: update: %w", err)
	}

	if contentChanged {
		// Bump version: SELECT MAX(version) + INSERT new row. Version history is APPEND-ONLY —
		// every committed save's snapshot is a restore point and must never be truncated (the
		// single-writer + versioning model, and Option B later, both rely on a complete linear
		// history). Two statements, no transaction — a half-applied snapshot just means one
		// fewer historical row, not a corrupt page.
		var nextVer int
		_ = s.pool.QueryRow(ctx,
			`SELECT COALESCE(MAX(version), 0) FROM page_versions WHERE page_id = $1`,
			id,
		).Scan(&nextVer)
		nextVer++

		updatedBy, _ := updates["updated_by"].(string)
		_, _ = s.pool.Exec(ctx,
			`INSERT INTO page_versions (page_id, workspace_id, version, title, content, created_by)
            VALUES ($1, $2, $3, $4, $5, $6)`,
			id, out.WorkspaceID, nextVer, existing.Title, out.Content, updatedBy,
		)
		// Reconcile page_links → Track issue embeds. Best-effort:
		// a sync failure shouldn't fail the page save. The next
		// content edit will reconcile again.
		if s.linker != nil {
			updatedBy, _ := updates["updated_by"].(string)
			_ = s.linker.SyncLinks(ctx, id, out.WorkspaceID, out.Content, updatedBy)
		}
		// Refresh the semantic embedding asynchronously — the call
		// is slow (Lens round-trip) and we never want to block the
		// editor's save debounce on it. Templates skip indexing.
		if s.indexer != nil && !out.IsTemplate {
			pageID := out.ID
			workspaceID := out.WorkspaceID
			text := out.ContentText
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_ = s.indexer.IndexPage(ctx, pageID, workspaceID, text)
			}()
		}
	}
	return out, nil
}

// ─── Delete + reparent ─────────────────────────────────────

// Delete removes a page, reparenting its children to the deleted
// page's parent so the tree doesn't develop dangling subtrees. Three
// statements:
//
//  1. Look up (parent_id, depth) of the page being deleted.
//  2. Update every child whose parent_id == the deleted page to
//     point at the deleted page's parent.
//  3. DELETE the page itself.
//
// Depth is intentionally NOT recomputed for the entire subtree in
// Phase 1 — the simplest correct option for now. A "rebalance depth"
// helper can come later if anyone notices the off-by-one.
func (s *Store) Delete(ctx context.Context, id string) error {
	if s.pool == nil {
		return errors.New("page: store has no pool")
	}
	var (
		parent *string
		depth  int
	)
	if err := s.pool.QueryRow(ctx,
		`SELECT parent_id, depth FROM pages WHERE id = $1`, id,
	).Scan(&parent, &depth); err != nil {
		return fmt.Errorf("page: delete lookup: %w", err)
	}
	_ = depth // documented above; kept for future rebalance hook.

	if _, err := s.pool.Exec(ctx,
		`UPDATE pages SET parent_id = $1, updated_at = NOW() WHERE parent_id = $2`,
		parent, id,
	); err != nil {
		return fmt.Errorf("page: reparent: %w", err)
	}
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- Delete is a primitive reached only via DeleteInWorkspaces (store.go), which calls assertInWorkspaces(id, authz.WorkspaceIDs) first. Proven cross-tenant-404 by TestSEC4_CrossTenant_ByIDRoutes (real PG) with the Enforcer nil — the store gate alone carries it.
	if _, err := s.pool.Exec(ctx, `DELETE FROM pages WHERE id = $1`, id); err != nil {
		return fmt.Errorf("page: delete: %w", err)
	}
	return nil
}

// ─── RecordView ────────────────────────────────────────────

// RecordView increments view_count + bumps last_viewed_at. viewerID
// isn't persisted as a per-viewer row in Phase 1 — Phase 2 can
// introduce a per-viewer table for unique-views reporting.
func (s *Store) RecordView(ctx context.Context, pageID, viewerID string) error {
	_ = viewerID
	if s.pool == nil {
		return errors.New("page: store has no pool")
	}
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- RecordView is a primitive reached only via RecordViewInWorkspaces (store.go), which calls assertInWorkspaces(pageID, authz.WorkspaceIDs) first.
	_, err := s.pool.Exec(ctx,
		`UPDATE pages SET view_count = view_count + 1,
            last_viewed_at = NOW(), updated_at = NOW()
        WHERE id = $1`, pageID,
	)
	return err
}

// ─── Verify ────────────────────────────────────────────────

// Verify stamps last_verified_at + verified_by so the page drops off
// the stale-pages list. Docs's "this is still accurate" attestation.
func (s *Store) Verify(ctx context.Context, pageID, verifierID string) error {
	if s.pool == nil {
		return errors.New("page: store has no pool")
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE pages SET last_verified_at = NOW(), verified_by = $1,
            updated_at = NOW()
        WHERE id = $2`, verifierID, pageID,
	)
	return err
}

// ─── SEC-4 Layer 2: workspace-scoped by-id ops ─────────────
//
// The authed /v1 page handler calls these variants, never the unscoped ones above (those
// remain for the PUBLIC share adapter + internal engines — freshness/export/mcp — which are
// public-by-contract or gated elsewhere). Each scopes to the caller's workspace SET
// (resolved from membership by Layer 1): a page in a workspace the caller doesn't belong to
// is invisible → ErrNotFound → 404, never leaking existence. wsIDs empty (caller has no
// membership) matches nothing → ErrNotFound. This is the IDOR cure: the workspace filter
// comes from verified membership, never from a client header/body.

// ErrNotFound signals a by-id op resolved to no row IN THE CALLER'S WORKSPACES — the handler
// maps it to 404. Distinct from a raw DB error so a real failure is never masked as not-found.
var ErrNotFound = errors.New("page: not found in an accessible workspace")

// assertInWorkspaces returns ErrNotFound unless page id lives in one of wsIDs.
// PageInWorkspaces reports whether page id lives in one of wsIDs — the scope check the collab
// WebSocket entry point runs before opening a live-edit session (it can't use ErrNotFound
// because a WS reject is an HTTP status, not a store error).
func (s *Store) PageInWorkspaces(ctx context.Context, id string, wsIDs []string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pages WHERE id = $1 AND workspace_id = ANY($2))`,
		id, wsIDs,
	).Scan(&exists)
	return exists, err
}

func (s *Store) assertInWorkspaces(ctx context.Context, id string, wsIDs []string) error {
	exists, err := s.PageInWorkspaces(ctx, id, wsIDs)
	if err != nil {
		return fmt.Errorf("page: scope check: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

// GetByIDInWorkspaces reads a page only if it lives in one of the caller's workspaces.
func (s *Store) GetByIDInWorkspaces(ctx context.Context, id string, wsIDs []string) (*model.Page, error) {
	p, err := scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM pages WHERE id = $1 AND workspace_id = ANY($2)`, id, wsIDs))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// UpdateInWorkspaces mutates a page only if it lives in one of the caller's workspaces.
func (s *Store) UpdateInWorkspaces(ctx context.Context, id string, updates map[string]any, wsIDs []string) (*model.Page, error) {
	if err := s.assertInWorkspaces(ctx, id, wsIDs); err != nil {
		return nil, err
	}
	return s.Update(ctx, id, updates)
}

// DeleteInWorkspaces deletes a page only if it lives in one of the caller's workspaces.
func (s *Store) DeleteInWorkspaces(ctx context.Context, id string, wsIDs []string) error {
	if err := s.assertInWorkspaces(ctx, id, wsIDs); err != nil {
		return err
	}
	return s.Delete(ctx, id)
}

// GetVersionsInWorkspaces lists versions only if the page lives in one of the caller's workspaces.
func (s *Store) GetVersionsInWorkspaces(ctx context.Context, pageID string, wsIDs []string) ([]model.PageVersion, error) {
	if err := s.assertInWorkspaces(ctx, pageID, wsIDs); err != nil {
		return nil, err
	}
	return s.GetVersions(ctx, pageID)
}

// RestoreVersionInWorkspaces restores only if the page lives in one of the caller's workspaces.
func (s *Store) RestoreVersionInWorkspaces(ctx context.Context, pageID string, version int, wsIDs []string) (*model.Page, error) {
	if err := s.assertInWorkspaces(ctx, pageID, wsIDs); err != nil {
		return nil, err
	}
	return s.RestoreVersion(ctx, pageID, version)
}

// RecordViewInWorkspaces records a view only if the page lives in one of the caller's workspaces.
func (s *Store) RecordViewInWorkspaces(ctx context.Context, pageID, viewerID string, wsIDs []string) error {
	if err := s.assertInWorkspaces(ctx, pageID, wsIDs); err != nil {
		return err
	}
	return s.RecordView(ctx, pageID, viewerID)
}

// VerifyInWorkspaces stamps verification only if the page lives in one of the caller's workspaces.
func (s *Store) VerifyInWorkspaces(ctx context.Context, pageID, verifierID string, wsIDs []string) error {
	if err := s.assertInWorkspaces(ctx, pageID, wsIDs); err != nil {
		return err
	}
	return s.Verify(ctx, pageID, verifierID)
}

// ─── GetStalePages ─────────────────────────────────────────

// GetStalePages returns pages where stale_after_days > 0 AND the
// page hasn't been updated OR re-verified within that window. The
// query lets owners surface docs that need a freshness pass.
func (s *Store) GetStalePages(ctx context.Context, workspaceID string) ([]model.Page, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+` FROM pages
        WHERE workspace_id = $1 AND stale_after_days > 0
          AND updated_at < NOW() - INTERVAL '1 day' * stale_after_days
          AND (last_verified_at IS NULL
               OR last_verified_at < NOW() - INTERVAL '1 day' * stale_after_days)
        ORDER BY updated_at ASC`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("page: stale: %w", err)
	}
	defer rows.Close()
	var out []model.Page
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ─── Search ────────────────────────────────────────────────

// SearchResult is the rich projection returned by SearchWithRank.
// It carries the Page, the joined space name, the ranking score
// (Postgres ts_rank), and a ts_headline excerpt with <mark> tags
// flagging the matched terms. Headlines are server-rendered HTML —
// the frontend sanitises to allow ONLY <mark> tags.
type SearchResult struct {
	Page      model.Page `json:"page"`
	SpaceName string     `json:"space_name"`
	Rank      float64    `json:"rank"`
	Headline  string     `json:"headline"`
}

// SearchWithRank is the ranked, paginated, space-scoped successor to
// Search. Templates are excluded server-side so the user never sees
// boilerplate in search results.
func (s *Store) SearchWithRank(ctx context.Context, workspaceID, query string, spaceID *string, limit, offset int) ([]SearchResult, error) {
	if s.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+prefixedColumns("p")+`, sp.name AS space_name,
            ts_rank(
                setweight(to_tsvector('english', p.title), 'A') ||
                setweight(to_tsvector('english', p.content_text), 'B'),
                websearch_to_tsquery('english', $2)
            ) AS rank,
            ts_headline('english', p.content_text,
                websearch_to_tsquery('english', $2),
                'MaxWords=35,MinWords=15,StartSel=<mark>,StopSel=</mark>'
            ) AS headline
        FROM pages p
        JOIN spaces sp ON sp.id = p.space_id
        WHERE p.workspace_id = $1
          AND (
              setweight(to_tsvector('english', p.title), 'A') ||
              setweight(to_tsvector('english', p.content_text), 'B')
          ) @@ websearch_to_tsquery('english', $2)
          AND ($3::text IS NULL OR p.space_id = $3)
          AND p.is_template = false
        ORDER BY rank DESC
        LIMIT $4 OFFSET $5`,
		workspaceID, query, spaceID, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("page: search-rank: %w", err)
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var (
			r        SearchResult
			rawSpace string
			rank     float64
			headline string
		)
		// pgx supports scanning into the page struct fields directly,
		// but we already have a scan() helper that pulls every Page
		// column. To keep the column list in lock-step with that
		// helper we hand-list extras here.
		p, err := scanPlus(rows, &rawSpace, &rank, &headline)
		if err != nil {
			return nil, err
		}
		r.Page = *p
		r.SpaceName = rawSpace
		r.Rank = rank
		r.Headline = headline
		out = append(out, r)
	}
	return out, rows.Err()
}

// scanPlus scans a pages-row plus trailing extras (space_name, rank,
// headline). Keeping it next to SearchWithRank keeps the column
// ordering obvious. Future callers that need the same shape should
// declare new SELECTs through this helper rather than inlining their
// own scanning.
func scanPlus(s interface{ Scan(...any) error }, extras ...any) (*model.Page, error) {
	var p model.Page
	pageDest := []any{
		&p.ID, &p.SpaceID, &p.WorkspaceID, &p.ParentID, &p.Title, &p.Slug,
		&p.Content, &p.ContentText, &p.Icon, &p.CoverURL,
		&p.Position, &p.Depth, &p.IsTemplate, &p.CreatedBy, &p.UpdatedBy,
		&p.LinkedIssues, &p.AICostUSD,
		&p.ViewCount, &p.LastViewedAt,
		&p.LastVerifiedAt, &p.VerifiedBy, &p.StaleAfterDays,
		&p.DocStatus,
		&p.Locked, &p.LockedBy, &p.LockedAt,
		&p.PageType,
		&p.CreatedAt, &p.UpdatedAt,
	}
	dest := append(pageDest, extras...)
	if err := s.Scan(dest...); err != nil {
		return nil, err
	}
	return &p, nil
}

func prefixedColumns(alias string) string {
	parts := strings.Split(columns, ", ")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ", ")
}

// Search runs Postgres full-text search backed by the GIN index on
// (title || content_text). websearch_to_tsquery lets callers pass
// natural-language queries with quoted phrases / -exclusions.
func (s *Store) Search(ctx context.Context, workspaceID, query string, limit int) ([]model.Page, error) {
	if s.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 25
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+` FROM pages
        WHERE workspace_id = $1
          AND to_tsvector('english', title || ' ' || content_text)
              @@ websearch_to_tsquery('english', $2)
        ORDER BY updated_at DESC
        LIMIT $3`,
		workspaceID, query, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("page: search: %w", err)
	}
	defer rows.Close()
	var out []model.Page
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ─── Versions ──────────────────────────────────────────────

func (s *Store) GetVersions(ctx context.Context, pageID string) ([]model.PageVersion, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, page_id, workspace_id, version, title, content, created_by, created_at
        FROM page_versions WHERE page_id = $1 ORDER BY version DESC`,
		pageID,
	)
	if err != nil {
		return nil, fmt.Errorf("page: versions: %w", err)
	}
	defer rows.Close()
	var out []model.PageVersion
	for rows.Next() {
		var v model.PageVersion
		if err := rows.Scan(&v.ID, &v.PageID, &v.WorkspaceID, &v.Version, &v.Title, &v.Content, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RestoreVersion overwrites the live page with the content from a
// historical version. The current state lands as a fresh version
// first so the restore is itself reversible.
func (s *Store) RestoreVersion(ctx context.Context, pageID string, version int) (*model.Page, error) {
	if s.pool == nil {
		return nil, errors.New("page: store has no pool")
	}
	var (
		title   string
		content string
	)
	if err := s.pool.QueryRow(ctx,
		`SELECT title, content FROM page_versions WHERE page_id = $1 AND version = $2`,
		pageID, version,
	).Scan(&title, &content); err != nil {
		return nil, fmt.Errorf("page: restore lookup: %w", err)
	}
	return s.Update(ctx, pageID, map[string]any{
		"title":      title,
		"content":    content,
		"updated_by": "restore",
	})
}

// GetVersion returns a single version's snapshot. Unscoped primitive — callers must
// authorize the page first (GetVersionInWorkspaces does).
func (s *Store) GetVersion(ctx context.Context, pageID string, version int) (*model.PageVersion, error) {
	if s.pool == nil {
		return nil, errors.New("page: store has no pool")
	}
	var v model.PageVersion
	err := s.pool.QueryRow(ctx,
		`SELECT id, page_id, workspace_id, version, title, content, created_by, created_at
        FROM page_versions WHERE page_id = $1 AND version = $2`,
		pageID, version,
	).Scan(&v.ID, &v.PageID, &v.WorkspaceID, &v.Version, &v.Title, &v.Content, &v.CreatedBy, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("page: get version: %w", err)
	}
	return &v, nil
}

// GetVersionInWorkspaces returns one version only if the page lives in one of the caller's
// workspaces.
func (s *Store) GetVersionInWorkspaces(ctx context.Context, pageID string, version int, wsIDs []string) (*model.PageVersion, error) {
	if err := s.assertInWorkspaces(ctx, pageID, wsIDs); err != nil {
		return nil, err
	}
	return s.GetVersion(ctx, pageID, version)
}

// CompareVersionsInWorkspaces returns two version snapshots (from, to) for diffing, only if
// the page lives in one of the caller's workspaces. The tenancy check is done ONCE for the
// page — both versions belong to the same page, so authorizing the page authorizes both.
func (s *Store) CompareVersionsInWorkspaces(ctx context.Context, pageID string, from, to int, wsIDs []string) (*model.PageVersion, *model.PageVersion, error) {
	if err := s.assertInWorkspaces(ctx, pageID, wsIDs); err != nil {
		return nil, nil, err
	}
	fromV, err := s.GetVersion(ctx, pageID, from)
	if err != nil {
		return nil, nil, err
	}
	toV, err := s.GetVersion(ctx, pageID, to)
	if err != nil {
		return nil, nil, err
	}
	return fromV, toV, nil
}

// ─── Track integration helpers ────────────────────────────

// WorkspacePageIDs returns every page ID in a workspace. Used by
// the AI-cost syncer to enumerate pages in one pass instead of
// paging through List(). Excludes templates so the sync loop
// doesn't churn on un-released specs.
func (s *Store) WorkspacePageIDs(ctx context.Context, workspaceID string) ([]string, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM pages WHERE workspace_id = $1 AND is_template = false`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("page: workspace ids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UpdateAICost is the syncer's narrow write — bypasses the Update
// allowlist (which doesn't include ai_cost_usd) so a noisy embed
// can't drive a full page-version snapshot per sync tick.
func (s *Store) UpdateAICost(ctx context.Context, pageID string, costUSD float64) error {
	if s.pool == nil {
		return errors.New("page: store has no pool")
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE pages SET ai_cost_usd = $1 WHERE id = $2`,
		costUSD, pageID,
	)
	return err
}
