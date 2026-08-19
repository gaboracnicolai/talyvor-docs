// Package templatelib backs the workspace template gallery: a
// browsable library of pre-built page templates plus workspace-
// owned custom templates saved from existing pages.
//
// Two storage tiers:
//
//  1. Built-ins live in code (see builtins.go). They render
//     markdown to ProseMirror once at process start and never
//     mutate. Their use_count is a per-process counter — exact
//     across a single instance, approximate across replicas.
//  2. Custom templates live in the `library_templates` table.
//     Listing merges both tiers; deleting only touches the DB
//     tier (built-ins are immutable from the API's view).
package templatelib

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/docs/internal/model"
)

// ErrNotFound signals a by-id / scoped op resolved to no row IN THE CALLER'S
// WORKSPACES — the handler maps it to 404 so existence never leaks. Distinct
// from a raw DB error so a real failure isn't masked as not-found. Built-ins
// are workspace-less and short-circuit BEFORE any scoped query, so they never
// surface this error.
var ErrNotFound = errors.New("templatelib: not found in workspace")

type TemplateCategory string

const (
	CatEngineering TemplateCategory = "engineering"
	CatProduct     TemplateCategory = "product"
	CatHR          TemplateCategory = "hr"
	CatMarketing   TemplateCategory = "marketing"
	CatFinance     TemplateCategory = "finance"
	CatOperations  TemplateCategory = "operations"
	CatGeneral     TemplateCategory = "general"
)

type LibraryTemplate struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Category    TemplateCategory `json:"category"`
	Icon        string           `json:"icon"`
	Tags        []string         `json:"tags"`
	Content     string           `json:"content"`
	ContentText string           `json:"content_text"`
	IsBuiltIn   bool             `json:"is_built_in"`
	WorkspaceID *string          `json:"workspace_id,omitempty"`
	CreatedBy   *string          `json:"created_by,omitempty"`
	UseCount    int              `json:"use_count"`
	CreatedAt   time.Time        `json:"created_at"`
}

// pageCreator + pageFetcher decouple the store from the full
// page.Store. UseTemplate needs to mint pages; CreateFromPage needs
// to read content + content_text from an existing page.
type pageCreator interface {
	Create(ctx context.Context, p model.Page) (*model.Page, error)
}

type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct {
	pool  pgxDB
	pages pageCreator
	// builtinUses tracks per-process use counts for code-shipped
	// templates. atomic.Int64 keeps reads + bumps lock-free; a
	// sync.Map would do but the cardinality is bounded (= len(Builtins)).
	builtinUses sync.Map // map[string]*atomic.Int64
}

func NewStore(pool *pgxpool.Pool, pages pageCreator) *Store {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newStore(db, pages)
}

func newStore(db pgxDB, pages pageCreator) *Store {
	s := &Store{pool: db, pages: pages}
	// Eagerly hydrate the builtin slice so List doesn't pay the
	// markdown-render cost on the first request.
	_ = Builtins()
	return s
}

const cols = `id, name, description, category, icon, tags, content, content_text, is_built_in, workspace_id, created_by, use_count, created_at`

func scan(s interface{ Scan(...any) error }) (*LibraryTemplate, error) {
	var t LibraryTemplate
	if err := s.Scan(
		&t.ID, &t.Name, &t.Description, &t.Category, &t.Icon,
		&t.Tags, &t.Content, &t.ContentText, &t.IsBuiltIn,
		&t.WorkspaceID, &t.CreatedBy, &t.UseCount, &t.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &t, nil
}

// ─── List ────────────────────────────────────────────

// List returns built-ins + workspace customs, optionally narrowed
// by category and/or a free-text search over name+description+tags.
// Filters run in Go because the row count is bounded and we keep
// the rule engine testable without per-case SQL.
func (s *Store) List(ctx context.Context, wsIDs []string, category *TemplateCategory, search string) ([]LibraryTemplate, error) {
	out := Builtins()
	// Hydrate per-process use counts onto the returned built-ins.
	for i := range out {
		out[i].UseCount = s.useCountFor(out[i].ID)
	}
	if s.pool != nil {
		rows, err := s.pool.Query(ctx,
			`SELECT `+cols+` FROM library_templates WHERE workspace_id = ANY($1) ORDER BY created_at DESC`,
			wsIDs,
		)
		if err != nil {
			return nil, fmt.Errorf("templatelib: list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scan(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, *t)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if category != nil {
		out = filterCategory(out, *category)
	}
	if strings.TrimSpace(search) != "" {
		out = filterSearch(out, search)
	}
	return out, nil
}

func filterCategory(in []LibraryTemplate, cat TemplateCategory) []LibraryTemplate {
	out := in[:0]
	for _, t := range in {
		if t.Category == cat {
			out = append(out, t)
		}
	}
	return out
}

func filterSearch(in []LibraryTemplate, q string) []LibraryTemplate {
	needle := strings.ToLower(q)
	out := in[:0]
	for _, t := range in {
		hay := strings.ToLower(t.Name + " " + t.Description + " " + strings.Join(t.Tags, " "))
		if strings.Contains(hay, needle) {
			out = append(out, t)
		}
	}
	return out
}

// GetByID returns either a built-in (looked up by stable ID) or a
// DB-backed custom template.
func (s *Store) GetByID(ctx context.Context, id string, wsIDs []string) (*LibraryTemplate, error) {
	if t := builtinByID(id); t != nil {
		t.UseCount = s.useCountFor(id)
		return t, nil
	}
	if s.pool == nil {
		return nil, ErrNotFound
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+cols+` FROM library_templates WHERE id = $1 AND workspace_id = ANY($2)`,
		id, wsIDs,
	)
	t, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ─── CreateFromPage ──────────────────────────────────

// CreateFromPage templates an existing page. wsIDs is the caller's VERIFIED
// workspace set: the SOURCE page read is scoped to it so a member can't
// template-copy a foreign workspace's page (pgx.ErrNoRows → ErrNotFound).
// workspaceID is the NEW template's owner (the create-target).
func (s *Store) CreateFromPage(ctx context.Context, pageID, workspaceID, createdBy, name, description string, category TemplateCategory, wsIDs []string) (*LibraryTemplate, error) {
	if s.pool == nil {
		return nil, errors.New("templatelib: no pool")
	}
	if pageID == "" || name == "" {
		return nil, errors.New("templatelib: page_id and name required")
	}
	if category == "" {
		category = CatGeneral
	}
	var (
		content     string
		contentText string
	)
	if err := s.pool.QueryRow(ctx,
		`SELECT content, content_text FROM pages WHERE id = $1 AND workspace_id = ANY($2)`,
		pageID, wsIDs,
	).Scan(&content, &contentText); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("templatelib: source page not found: %w", err)
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO library_templates
        (name, description, category, icon, tags, content, content_text, is_built_in, workspace_id, created_by)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
        RETURNING `+cols,
		name, description, string(category), "📄", []string{},
		content, contentText, false, &workspaceID, &createdBy,
	)
	return scan(row)
}

// ─── UseTemplate ─────────────────────────────────────

// UseTemplate mints a page from the template and bumps the appropriate counter
// (in-memory for built-ins, DB for customs); it returns the freshly minted page.
// wsIDs is the caller's VERIFIED workspace set, scoping the TEMPLATE read +
// use_count bump so a member can't mint from a foreign workspace's custom
// template (0 rows → ErrNotFound). workspaceID is the TARGET space's workspace
// — the owner of the NEW page, distinct from the template-read scope.
func (s *Store) UseTemplate(ctx context.Context, templateID, spaceID, workspaceID, createdBy string, wsIDs []string) (*model.Page, error) {
	if s.pages == nil {
		return nil, errors.New("templatelib: no page creator")
	}

	// Built-in path — no DB round-trip needed.
	//
	// ⚠ THE BUMP IS AFTER THE CREATE, AND IT USED TO BE BEFORE IT — ON BOTH TIERS. A create that
	// failed still moved the counter, so `use_count` counted ATTEMPTS while the tile that renders it
	// (frontend/src/components/TemplateCard.tsx, `{use_count} uses` under a TrendingUp icon) says
	// uses. Measured through the shipped route on real Postgres: six uses of one template into one
	// space gave five pages and use_count = 6 (usecount_failedcreate_realpg_test.go).
	//
	// The sibling counter in this very call path already wrote the rule down, and this is the same
	// sentence one package over (page/store.go, beside metrics.PagesCreated.Inc()): "AFTER the insert
	// succeeded and NOT before: a refused or failed create is not a page created, and a counter that
	// moves on the attempt is a different, quieter lie."
	//
	// ⚠ WHAT IS *NOT* FIXED HERE, because both are product decisions and the queue holds them (W3.1):
	// this counter is keyed on template id ALONE, so it is CROSS-TENANT — one workspace is served
	// every workspace's uses — and it is PER-PROCESS, resetting to zero on each deploy. Whether a
	// built-in's number should be global popularity or this workspace's usage decides which of those
	// is the defect. The ordering is the one half that is wrong under EITHER reading.
	if t := builtinByID(templateID); t != nil {
		pg, err := s.pages.Create(ctx, model.Page{
			SpaceID:     spaceID,
			WorkspaceID: workspaceID,
			Title:       t.Name,
			Content:     t.Content,
			ContentText: t.ContentText,
			Icon:        t.Icon,
			CreatedBy:   createdBy,
		})
		if err != nil {
			return nil, err
		}
		s.bumpBuiltin(templateID)
		return pg, nil
	}

	// Custom path — read content, bump use_count, create page.
	if s.pool == nil {
		return nil, ErrNotFound
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+cols+` FROM library_templates WHERE id = $1 AND workspace_id = ANY($2)`,
		templateID, wsIDs,
	)
	t, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("templatelib: not found: %w", err)
	}
	pg, err := s.pages.Create(ctx, model.Page{
		SpaceID:     spaceID,
		WorkspaceID: workspaceID,
		Title:       t.Name,
		Content:     t.Content,
		ContentText: t.ContentText,
		Icon:        t.Icon,
		CreatedBy:   createdBy,
	})
	if err != nil {
		return nil, err
	}
	// Same ordering rule as the built-in tier above, and the bump keeps its wsIDs scope so a member
	// still cannot move a foreign workspace's counter.
	//
	// ⚠ IT NO LONGER FAILS THE REQUEST, AND THAT IS THE COST OF THE REORDER, STATED RATHER THAN
	// HIDDEN. Before, a bump error returned an error and no page existed, so the error was true.
	// After, the page IS committed by the line above — returning an error would tell the caller their
	// page was not created while it sits in their space, which is a worse lie than an undercounted
	// tile. So it logs and serves the page, exactly as page.Store.Create does for the version
	// snapshot it cannot write ("page: version snapshot NOT written"). The counter is wrong in the
	// SAFE direction here: it undercounts a use that happened, rather than claiming one that did not.
	if _, err := s.pool.Exec(ctx,
		`UPDATE library_templates SET use_count = use_count + 1 WHERE id = $1 AND workspace_id = ANY($2)`,
		templateID, wsIDs,
	); err != nil {
		slog.Error("templatelib: use_count NOT bumped — the page was created and this use is uncounted",
			slog.String("template_id", templateID),
			slog.String("page_id", pg.ID),
			slog.String("workspace_id", workspaceID),
			slog.String("err", err.Error()),
			slog.String("effect", "the template gallery under-reports this template's uses by one"))
	}
	return pg, nil
}

// UseCountForBuiltin exposes the per-process counter so tests can
// assert on bumps without poking package-private state.
func (s *Store) UseCountForBuiltin(id string) int {
	return s.useCountFor(id)
}

func (s *Store) useCountFor(id string) int {
	if v, ok := s.builtinUses.Load(id); ok {
		return int(v.(*atomic.Int64).Load())
	}
	return 0
}

func (s *Store) bumpBuiltin(id string) {
	v, _ := s.builtinUses.LoadOrStore(id, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// ─── Delete ──────────────────────────────────────────

// Delete removes a workspace template. Built-ins are immutable and
// can't be deleted — we surface a typed error so the handler can
// map it to a 400/403.
func (s *Store) Delete(ctx context.Context, id string, wsIDs []string) error {
	if builtinByID(id) != nil {
		return errors.New("templatelib: built-in templates cannot be deleted")
	}
	if s.pool == nil {
		return errors.New("templatelib: no pool")
	}
	// wsIDs is an AUTHORIZED scope, never a raw path param: the handler runs
	// {wsID} through authz.AuthorizeWorkspace (handler.go#scopeFor) before it
	// arrives here. A template outside that scope is invisible (0 rows affected
	// → ErrNotFound → 404), never deleted. It used to be the caller's whole
	// membership SET, which refused the stranger but not the second workspace
	// the same caller belonged to — see wsidscope_realpg_test.go.
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM library_templates WHERE id = $1 AND workspace_id = ANY($2)`,
		id, wsIDs,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
