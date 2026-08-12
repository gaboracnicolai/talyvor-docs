// Package model defines the shared types for Talyvor Docs.
//
// Spaces are the top-level container; pages nest under spaces with
// a parent_id chain (max depth: 5). Blocks are an alternate, finer-
// grained content representation that Phase 2's real-time editor
// will populate — Phase 1 stores them but doesn't actively use them
// in the page-render path.
package model

import "time"

// Space is the top-level container for documentation. workspace_id
// links to Talyvor Track; the field is intentionally a plain string
// so this service can stand alone without a foreign-key dependency
// on Track's workspaces table.
type Space struct {
	ID          string    `json:"id"           db:"id"`
	WorkspaceID string    `json:"workspace_id" db:"workspace_id"`
	Name        string    `json:"name"         db:"name"`
	Slug        string    `json:"slug"         db:"slug"`
	Description string    `json:"description"  db:"description"`
	Icon        string    `json:"icon"         db:"icon"`
	Color       string    `json:"color"        db:"color"`
	Private     bool      `json:"private"      db:"private"`
	CreatedBy   string    `json:"created_by"   db:"created_by"`
	CreatedAt   time.Time `json:"created_at"   db:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"   db:"updated_at"`
}

// Page is one document inside a space. ParentID enables nested
// pages (depth capped at 5 by the store). Content is the canonical
// ProseMirror JSON; ContentText is the plain-text projection used
// by the GIN-backed full-text search index. The denormalisation
// keeps Search a pure index lookup — no JSON parsing per row.
type Page struct {
	ID          string  `json:"id"           db:"id"`
	SpaceID     string  `json:"space_id"     db:"space_id"`
	WorkspaceID string  `json:"workspace_id" db:"workspace_id"`
	ParentID    *string `json:"parent_id,omitempty" db:"parent_id"`
	Title       string  `json:"title"        db:"title"`
	Slug        string  `json:"slug"         db:"slug"`
	Content     string  `json:"content"      db:"content"`
	ContentText string  `json:"content_text" db:"content_text"`
	Icon        string  `json:"icon"         db:"icon"`
	CoverURL    string  `json:"cover_url"    db:"cover_url"`
	Position    float64 `json:"position"     db:"position"`
	Depth       int     `json:"depth"        db:"depth"`
	IsTemplate  bool    `json:"is_template"  db:"is_template"`
	CreatedBy   string  `json:"created_by"   db:"created_by"`
	UpdatedBy   string  `json:"updated_by"   db:"updated_by"`

	// Track integration.
	LinkedIssues []string `json:"linked_issues,omitempty" db:"linked_issues"`
	// AICostUSD is the cost of the Track ISSUES linked to this page — NOT the cost of AI work on
	// the document, which is OwnAICostUSD. Derived: recomputed from a complete set of links on
	// every sweep and overwritten (see migration 0018).
	AICostUSD float64 `json:"ai_cost_usd"             db:"ai_cost_usd"`

	// OwnAICostUSD is the cost of AI operations performed ON this document (write, summarize,
	// translate, title …), accumulated exactly-once from page_ai_spend_events.
	//
	// ⚠ IT IS A LOWER BOUND, and the field name cannot say that, so this comment and the API
	// response do. Two Docs AI operations have no single page — docs-ai-ask answers across many
	// pages, docs-search spans the workspace — and are deliberately attributed to none. Their cost
	// is visible in Lens under its operation feature.
	OwnAICostUSD float64 `json:"own_ai_cost_usd" db:"own_ai_cost_usd"`

	// TotalAICostUSD is AICostUSD + OwnAICostUSD, computed on read and never stored. It exists so
	// a caller that wants "everything this page cost" does not add the two itself and get it
	// subtly wrong — and so neither underlying number had to quietly widen to mean the sum.
	TotalAICostUSD float64 `json:"total_ai_cost_usd" db:"-"`

	// Analytics.
	ViewCount    int        `json:"view_count"               db:"view_count"`
	LastViewedAt *time.Time `json:"last_viewed_at,omitempty" db:"last_viewed_at"`

	// Freshness — Docs's "this page is stale" feature lets owners
	// declare a TTL (StaleAfterDays). Pages older than that AND not
	// re-verified surface in GetStalePages so doc owners can
	// re-attest or rewrite.
	LastVerifiedAt *time.Time `json:"last_verified_at,omitempty" db:"last_verified_at"`
	VerifiedBy     *string    `json:"verified_by,omitempty"      db:"verified_by"`
	StaleAfterDays int        `json:"stale_after_days"           db:"stale_after_days"`

	// Approval workflow. Owned by internal/approval; page.Store does
	// not include this column in its primary SELECT/UPDATE allow-list
	// — approval is the single writer.
	DocStatus string `json:"doc_status,omitempty" db:"doc_status"`

	// Soft locks. Owned by internal/pagelock — page.Store reads the
	// columns but won't mutate them outside the lock path.
	Locked   bool       `json:"locked"                db:"locked"`
	LockedBy *string    `json:"locked_by,omitempty"   db:"locked_by"`
	LockedAt *time.Time `json:"locked_at,omitempty"   db:"locked_at"`

	// PageType switches the frontend surface: "document" (the default) or "changelog", which
	// is what routes PageView to ChangelogView and picks the Sidebar icon. Backed by a column
	// on `pages`; the closed set and both write paths live in page.Store (validatePageType).
	//
	// ⚠ THE PREVIOUS COMMENT HERE NAMED A CONSUMER THAT DID NOT EXIST and a value nothing
	// produced: it claimed "the changelog package reads this when generating entries" — that
	// package has never mentioned page_type, it keys entries off page_id — and listed
	// "template" as a third value, when templates are the separate `is_template` boolean.
	// Worse, NOTHING WROTE THE COLUMN AT ALL: no INSERT column, no Update allowlist entry, no
	// API field. So every row was 'document' forever and the changelog surface was unreachable
	// while the comment described it as working. The writer is the fix; this note is the
	// record of why a column's stated consumer has to be checked rather than believed.
	PageType string `json:"page_type,omitempty" db:"page_type"`

	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// Block is the granular content node. Pages render from Content (the
// JSON blob) today; Phase 2's collaborative editor will move to a
// per-block CRDT model, at which point Block becomes load-bearing.
type Block struct {
	ID        string    `json:"id"                  db:"id"`
	PageID    string    `json:"page_id"             db:"page_id"`
	Type      string    `json:"type"                db:"type"`
	Content   string    `json:"content"             db:"content"`
	Position  float64   `json:"position"            db:"position"`
	ParentID  *string   `json:"parent_id,omitempty" db:"parent_id"`
	CreatedAt time.Time `json:"created_at"          db:"created_at"`
	UpdatedAt time.Time `json:"updated_at"          db:"updated_at"`
}

// PageVersion captures the state of a page each time its TITLE OR its
// CONTENT changes — both, because RestoreVersion writes both back, and
// a column that is restored but not recorded is a column a restore can
// overwrite with no copy left. History is APPEND-ONLY — every committed save's snapshot is
// a restore point and is never pruned/truncated (the single-writer +
// versioning model relies on a complete linear history). WorkspaceID is
// the version's own tenant (denormalized from the page) so a row is
// self-describing and reads can scope directly on the versions table.
type PageVersion struct {
	ID          string    `json:"id"           db:"id"`
	PageID      string    `json:"page_id"      db:"page_id"`
	WorkspaceID string    `json:"workspace_id" db:"workspace_id"`
	Version     int       `json:"version"      db:"version"`
	Title       string    `json:"title"        db:"title"`
	Content     string    `json:"content"      db:"content"`
	CreatedBy   string    `json:"created_by"   db:"created_by"`
	CreatedAt   time.Time `json:"created_at"   db:"created_at"`
}

// Comment is a page-level or inline (BlockID set) discussion thread.
// Resolved comments stay in the table so the audit trail is intact;
// the UI filters them by default.
type Comment struct {
	ID         string    `json:"id"                    db:"id"`
	PageID     string    `json:"page_id"               db:"page_id"`
	BlockID    *string   `json:"block_id,omitempty"    db:"block_id"`
	AuthorID   string    `json:"author_id"             db:"author_id"`
	Content    string    `json:"content"               db:"content"`
	Resolved   bool      `json:"resolved"              db:"resolved"`
	ResolvedBy *string   `json:"resolved_by,omitempty" db:"resolved_by"`
	CreatedAt  time.Time `json:"created_at"            db:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"            db:"updated_at"`
}
