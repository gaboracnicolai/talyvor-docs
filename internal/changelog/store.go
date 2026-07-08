// Package changelog backs Talyvor's specialised changelog pages.
// One row per release entry; entries can be auto-generated from
// Track issues (label-based grouping) or written by hand. Published
// entries surface in a public RSS feed; drafts stay private.
package changelog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/docs/internal/trackintegration"
)

type EntryType string

const (
	EntryFeature     EntryType = "feature"
	EntryBugfix      EntryType = "bugfix"
	EntryBreaking    EntryType = "breaking"
	EntryImprovement EntryType = "improvement"
	EntryDeprecated  EntryType = "deprecated"
	EntrySecurity    EntryType = "security"
)

var validEntryTypes = map[EntryType]bool{
	EntryFeature: true, EntryBugfix: true, EntryBreaking: true,
	EntryImprovement: true, EntryDeprecated: true, EntrySecurity: true,
}

type ChangelogEntry struct {
	ID          string     `json:"id"`
	PageID      string     `json:"page_id"`
	WorkspaceID string     `json:"workspace_id"`
	Version     string     `json:"version"`
	Title       string     `json:"title"`
	Summary     string     `json:"summary"`
	Type        EntryType  `json:"type"`
	IssueIDs    []string   `json:"issue_ids"`
	Content     string     `json:"content"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// issueLookup is the narrow Track surface — what we actually need
// from trackintegration.Client. Tests stub this directly.
type issueLookup interface {
	IsConfigured() bool
	GetIssue(ctx context.Context, workspaceID, issueID string) (*trackintegration.IssueRef, error)
}

type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct {
	pool  pgxDB
	track issueLookup
}

func NewStore(pool *pgxpool.Pool, track *trackintegration.Client) *Store {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	var lookup issueLookup
	if track != nil {
		lookup = track
	}
	return newStore(db, lookup)
}

func newStore(db pgxDB, track issueLookup) *Store {
	return &Store{pool: db, track: track}
}

const cols = `id, page_id, workspace_id, version, title, summary, type, issue_ids, content, published_at, created_by, created_at, updated_at`

func scan(s interface{ Scan(...any) error }) (*ChangelogEntry, error) {
	var e ChangelogEntry
	if err := s.Scan(
		&e.ID, &e.PageID, &e.WorkspaceID, &e.Version, &e.Title, &e.Summary,
		&e.Type, &e.IssueIDs, &e.Content, &e.PublishedAt,
		&e.CreatedBy, &e.CreatedAt, &e.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if e.IssueIDs == nil {
		e.IssueIDs = []string{}
	}
	return &e, nil
}

// ─── Validation ─────────────────────────────────────

// versionRE accepts semver (v1.2.3, 1.2.3, v0.1.0-rc1) and ISO
// dates (2026-05-23). Strict enough to reject typos, loose enough
// for the two formats teams actually use.
var versionRE = regexp.MustCompile(`^(v?\d+\.\d+\.\d+(?:[-+][\w.]+)?|\d{4}-\d{2}-\d{2})$`)

func isValidVersion(v string) bool {
	if v == "" {
		return false
	}
	return versionRE.MatchString(v)
}

// ─── CRUD ───────────────────────────────────────────

func (s *Store) CreateEntry(ctx context.Context, e ChangelogEntry) (*ChangelogEntry, error) {
	if s.pool == nil {
		return nil, errors.New("changelog: no pool")
	}
	if !isValidVersion(e.Version) {
		return nil, fmt.Errorf("changelog: invalid version %q", e.Version)
	}
	if e.Title == "" {
		return nil, errors.New("changelog: title required")
	}
	if e.Type == "" {
		e.Type = EntryImprovement
	}
	if !validEntryTypes[e.Type] {
		return nil, fmt.Errorf("changelog: invalid type %q", e.Type)
	}
	if e.IssueIDs == nil {
		e.IssueIDs = []string{}
	}
	if e.Content == "" {
		e.Content = "{}"
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO changelog_entries
        (page_id, workspace_id, version, title, summary, type, issue_ids, content, created_by)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
        RETURNING `+cols,
		e.PageID, e.WorkspaceID, e.Version, e.Title, e.Summary,
		string(e.Type), e.IssueIDs, e.Content, e.CreatedBy,
	)
	return scan(row)
}

// ErrNotFound signals a by-id op resolved to no row IN THE CALLER'S WORKSPACES — the handler
// maps it to 404. Distinct from a raw DB error so a real failure is never masked as not-found.
// wsIDs empty (caller has no membership) matches nothing → ErrNotFound (fail-closed).
var ErrNotFound = errors.New("changelog: not found in workspace")

func (s *Store) GetEntry(ctx context.Context, id string, wsIDs []string) (*ChangelogEntry, error) {
	if s.pool == nil {
		return nil, errors.New("changelog: no pool")
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+cols+` FROM changelog_entries WHERE id = $1 AND workspace_id = ANY($2)`, id, wsIDs)
	e, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// UpdateEntry applies the allow-listed updates and re-stamps
// updated_at. Allow-listing keeps internal columns (id, created_at,
// page_id) immutable.
func (s *Store) UpdateEntry(ctx context.Context, id string, updates map[string]any, wsIDs []string) (*ChangelogEntry, error) {
	if s.pool == nil {
		return nil, errors.New("changelog: no pool")
	}
	allowed := map[string]bool{
		"version": true, "title": true, "summary": true, "type": true,
		"issue_ids": true, "content": true,
	}
	var (
		setParts []string
		args     []any
	)
	idx := 1
	for k, v := range updates {
		if !allowed[k] {
			continue
		}
		setParts = append(setParts, fmt.Sprintf("%s = $%d", k, idx))
		args = append(args, v)
		idx++
	}
	if len(setParts) == 0 {
		return nil, errors.New("changelog: no updatable fields")
	}
	setParts = append(setParts, fmt.Sprintf("updated_at = NOW()"))
	// id is $idx; the workspace scope set is $idx+1.
	args = append(args, id, wsIDs)
	row := s.pool.QueryRow(ctx,
		fmt.Sprintf(`UPDATE changelog_entries SET %s WHERE id = $%d AND workspace_id = ANY($%d) RETURNING %s`,
			strings.Join(setParts, ", "), idx, idx+1, cols),
		args...,
	)
	e, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

func (s *Store) PublishEntry(ctx context.Context, id string, wsIDs []string) (*ChangelogEntry, error) {
	if s.pool == nil {
		return nil, errors.New("changelog: no pool")
	}
	row := s.pool.QueryRow(ctx,
		`UPDATE changelog_entries SET published_at = NOW(), updated_at = NOW()
        WHERE id = $1 AND workspace_id = ANY($2) RETURNING `+cols,
		id, wsIDs,
	)
	e, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

func (s *Store) DeleteEntry(ctx context.Context, id string, wsIDs []string) error {
	if s.pool == nil {
		return errors.New("changelog: no pool")
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM changelog_entries WHERE id = $1 AND workspace_id = ANY($2)`, id, wsIDs)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ─── ListEntries ─────────────────────────────────────

func (s *Store) ListEntries(ctx context.Context, pageID string, entryType *EntryType, limit, offset int, wsIDs []string) ([]ChangelogEntry, error) {
	if s.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var rows pgx.Rows
	var err error
	if entryType != nil {
		rows, err = s.pool.Query(ctx,
			`SELECT `+cols+` FROM changelog_entries
            WHERE page_id = $1 AND type = $2 AND workspace_id = ANY($3)
            ORDER BY published_at DESC NULLS LAST, created_at DESC
            LIMIT $4 OFFSET $5`,
			pageID, string(*entryType), wsIDs, limit, offset,
		)
	} else {
		rows, err = s.pool.Query(ctx,
			`SELECT `+cols+` FROM changelog_entries
            WHERE page_id = $1 AND workspace_id = ANY($2)
            ORDER BY published_at DESC NULLS LAST, created_at DESC
            LIMIT $3 OFFSET $4`,
			pageID, wsIDs, limit, offset,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("changelog: list: %w", err)
	}
	defer rows.Close()
	var out []ChangelogEntry
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// ─── GenerateFromIssues ──────────────────────────────

// labelToType maps Track issue labels to changelog buckets. Labels
// not in the map fall through to EntryImprovement so unfamiliar
// vocab doesn't lose information.
var labelToType = map[string]EntryType{
	"bug":             EntryBugfix,
	"bugfix":          EntryBugfix,
	"feature":         EntryFeature,
	"breaking":        EntryBreaking,
	"breaking-change": EntryBreaking,
	"deprecated":      EntryDeprecated,
	"security":        EntrySecurity,
}

func issueType(ref *trackintegration.IssueRef) EntryType {
	if ref == nil {
		return EntryImprovement
	}
	for _, label := range ref.Labels {
		if t, ok := labelToType[strings.ToLower(label)]; ok {
			return t
		}
	}
	return EntryImprovement
}

// groupTitles maps the EntryType bucket to its markdown heading +
// emoji. Stable ordering for the generated output.
var groupOrder = []EntryType{EntryBreaking, EntryFeature, EntryBugfix, EntryImprovement, EntryDeprecated, EntrySecurity}

var groupTitles = map[EntryType]string{
	EntryBreaking:    "⚠️ Breaking Changes",
	EntryFeature:     "✨ New Features",
	EntryBugfix:      "🐛 Bug Fixes",
	EntryImprovement: "🔧 Improvements",
	EntryDeprecated:  "🗄️ Deprecated",
	EntrySecurity:    "🔒 Security",
}

// buildContent emits a ProseMirror JSON doc from the issue list.
// Issues are bucketed by label-derived type; each group renders
// as a heading-2 followed by a bullet list of identifier + title.
// When Track is unconfigured, ref will be nil and we emit a
// fallback "From issues" group with just the IDs.
func buildContent(track issueLookup, issueIDs []string) string {
	// bucket: ordered map from EntryType → list of "[ENG-1]: Title"
	bucket := map[EntryType][]string{}
	for _, id := range issueIDs {
		var ref *trackintegration.IssueRef
		if track != nil && track.IsConfigured() {
			ref, _ = track.GetIssue(nil, "", id)
		}
		t := issueType(ref)
		line := id
		if ref != nil {
			line = fmt.Sprintf("[%s]: %s", ref.Identifier, ref.Title)
		}
		bucket[t] = append(bucket[t], line)
	}

	var doc struct {
		Type    string `json:"type"`
		Content []any  `json:"content"`
	}
	doc.Type = "doc"

	emitted := 0
	for _, t := range groupOrder {
		lines := bucket[t]
		if len(lines) == 0 {
			continue
		}
		// Stable order within a bucket.
		sort.Strings(lines)
		// Heading.
		doc.Content = append(doc.Content, map[string]any{
			"type":  "heading",
			"attrs": map[string]any{"level": 2},
			"content": []map[string]any{
				{"type": "text", "text": groupTitles[t]},
			},
		})
		// Bullet list.
		items := make([]map[string]any, 0, len(lines))
		for _, ln := range lines {
			items = append(items, map[string]any{
				"type": "list_item",
				"content": []map[string]any{
					{"type": "paragraph", "content": []map[string]any{{"type": "text", "text": ln}}},
				},
			})
		}
		doc.Content = append(doc.Content, map[string]any{
			"type":    "bullet_list",
			"content": items,
		})
		emitted++
	}

	if emitted == 0 {
		// Nothing bucketed — emit a friendly fallback so the entry
		// isn't a blank doc.
		doc.Content = append(doc.Content, map[string]any{
			"type": "paragraph",
			"content": []map[string]any{
				{"type": "text", "text": "No issues."},
			},
		})
	}
	out, _ := json.Marshal(doc)
	return string(out)
}

// GenerateFromIssues creates an entry from a list of issue IDs.
// Track-down case: still creates an entry, just with the bare IDs
// and the improvement bucket — the spec asks for graceful
// degradation, not a hard failure.
func (s *Store) GenerateFromIssues(ctx context.Context, workspaceID, pageID, createdBy string, issueIDs []string, version string) (*ChangelogEntry, error) {
	content := buildContent(s.track, issueIDs)
	// The overall entry's "type" is the dominant bucket — most
	// teams ship a mix and want one badge. Pick the highest-rank
	// bucket present, falling back to improvement.
	pickedType := EntryImprovement
	if s.track != nil && s.track.IsConfigured() {
		counts := map[EntryType]int{}
		for _, id := range issueIDs {
			ref, _ := s.track.GetIssue(ctx, workspaceID, id)
			counts[issueType(ref)]++
		}
		// Breaking > Security > Feature > Bugfix > Improvement > Deprecated.
		for _, t := range []EntryType{EntryBreaking, EntrySecurity, EntryFeature, EntryBugfix, EntryImprovement, EntryDeprecated} {
			if counts[t] > 0 {
				pickedType = t
				break
			}
		}
	}
	if issueIDs == nil {
		issueIDs = []string{}
	}
	return s.CreateEntry(ctx, ChangelogEntry{
		PageID:      pageID,
		WorkspaceID: workspaceID,
		Version:     version,
		Title:       version,
		Summary:     fmt.Sprintf("Generated from %d issues", len(issueIDs)),
		Type:        pickedType,
		IssueIDs:    issueIDs,
		Content:     content,
		CreatedBy:   createdBy,
	})
}

// ─── GetPublicFeed ───────────────────────────────────

// GetPublicFeed lists published entries scoped to the caller's VERIFIED workspace set
// (SEC-4 L2 DECEPTIVE shape). The Feed handler previously fed workspaceID straight from
// chi.URLParam("wsID"), so a caller could name any workspace's id in the URL and read its
// published feed. wsIDs now comes from authz.WorkspaceIDs (verified membership), never the URL.
func (s *Store) GetPublicFeed(ctx context.Context, wsIDs []string, limit int) ([]ChangelogEntry, error) {
	if s.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+cols+` FROM changelog_entries
        WHERE workspace_id = ANY($1) AND published_at IS NOT NULL
        ORDER BY published_at DESC
        LIMIT $2`,
		wsIDs, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("changelog: feed: %w", err)
	}
	defer rows.Close()
	var out []ChangelogEntry
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}
