package customdomain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/model"
)

// contains reports whether v is present in xs. Used to authorize the
// caller-supplied path {wsID} against the verified membership set
// before it becomes a new domain's owner workspace.
func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// Handler covers two surfaces:
//   - admin CRUD for custom domains (mounted under /v1)
//   - the public read-only space view served by the DomainRouter
//     middleware on the custom hostname
type Handler struct {
	store *Store
	pages PublicPageLookup
}

// PublicPageLookup is the narrow page.Store surface the public
// renderer uses. Live deploys pass the real *page.Store; tests can
// stub it. Same shape as the sharing-handler loader to avoid a
// dep on the page package.
type PublicPageLookup interface {
	GetByID(ctx context.Context, id string) (*model.Page, error)
	GetBySlug(ctx context.Context, spaceID, slug string) (*model.Page, error)
	ListBySpace(ctx context.Context, spaceID string) ([]model.Page, error)
}

func NewHandler(store *Store, pages PublicPageLookup) *Handler {
	return &Handler{store: store, pages: pages}
}

// Mount registers the admin endpoints under /v1.
func (h *Handler) Mount(r chi.Router) {
	r.Post("/workspaces/{wsID}/custom-domains", h.Create)
	r.Get("/workspaces/{wsID}/custom-domains", h.List)
	r.Post("/workspaces/{wsID}/custom-domains/{id}/verify", h.Verify)
	r.Delete("/workspaces/{wsID}/custom-domains/{id}", h.Delete)
}

// ─── Admin ──────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type createBody struct {
	Domain  string  `json:"domain"`
	SpaceID *string `json:"space_id"`
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var in createBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	// A new domain may only be created inside a workspace the caller
	// is a verified member of. The path {wsID} is caller-supplied, so
	// authorize it against the membership set before it becomes the
	// new row's owner — otherwise the write lands in a foreign
	// workspace. Unauthorized → 404 (no existence oracle).
	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized write-target: contains(authz.WorkspaceIDs) below rejects non-members before Create owns the new domain
	if !contains(authz.WorkspaceIDs(r.Context()), wsID) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	cd, err := h.store.Create(r.Context(),
		wsID,
		in.Domain,
		authz.ActorOrEmpty(r.Context()),
		in.SpaceID,
	)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, cd)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.GetByWorkspace(r.Context(), authz.WorkspaceIDs(r.Context()))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out == nil {
		out = []CustomDomain{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	ok, err := h.store.Verify(r.Context(), chi.URLParam(r, "id"), authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	msg := "TXT record not found — DNS may still be propagating."
	if ok {
		msg = "Domain verified."
	}
	writeJSON(w, http.StatusOK, map[string]any{"verified": ok, "message": msg})
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	err := h.store.Delete(r.Context(), chi.URLParam(r, "id"), authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─── Public renderer ────────────────────────────────

// PublicHandler returns an http.Handler that the DomainRouter
// forwards requests to. Three routes:
//
//	GET /                — space index (page list)
//	GET /{slug}          — page by slug
//	GET /search?q=...    — basic search (delegates to upstream)
//
// All responses are read-only HTML — no JSON API, no admin UI.
func (h *Handler) PublicHandler() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.publicIndex)
	r.Get("/search", h.publicSearch)
	r.Get("/{slug}", h.publicPage)
	return r
}

const publicCSS = `body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif;max-width:780px;margin:32px auto;padding:0 16px;color:#1a1a1a;line-height:1.6}
nav.crumbs{font-size:0.85em;color:#777;margin-bottom:1.5em}
nav.crumbs a{color:#555;text-decoration:none}
h1,h2,h3{color:#111;line-height:1.25}
h1{font-size:2em;border-bottom:1px solid #eee;padding-bottom:0.2em}
a{color:#3a4ad9}
.page-list{list-style:none;padding:0;margin:0}
.page-list li{padding:8px 0;border-bottom:1px solid #f0f0f0}
.page-list li a{font-size:1.05em;font-weight:500}
footer{margin-top:4em;padding-top:1em;border-top:1px solid #eee;color:#999;font-size:0.8em;text-align:center}
`

func renderHTMLShell(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Powered-By", "Talyvor Docs")
	fmt.Fprintf(w,
		`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>%s</title><style>%s</style></head><body>%s<footer>Powered by Talyvor Docs</footer></body></html>`,
		html.EscapeString(title), publicCSS, body,
	)
}

func (h *Handler) publicIndex(w http.ResponseWriter, r *http.Request) {
	spaceID := SpaceFromContext(r.Context())
	if spaceID == "" || h.pages == nil {
		renderHTMLShell(w, "Docs", "<h1>Docs</h1><p>No space configured for this domain.</p>")
		return
	}
	pages, err := h.pages.ListBySpace(r.Context(), spaceID)
	if err != nil {
		renderHTMLShell(w, "Docs", "<h1>Docs</h1><p>Failed to load.</p>")
		return
	}
	var b strings.Builder
	b.WriteString(`<h1>Documentation</h1><ul class="page-list">`)
	for _, p := range pages {
		if p.IsTemplate {
			continue
		}
		fmt.Fprintf(&b, `<li><a href="/%s">%s</a></li>`,
			html.EscapeString(p.Slug),
			html.EscapeString(p.Title),
		)
	}
	b.WriteString(`</ul>`)
	renderHTMLShell(w, "Docs", b.String())
}

func (h *Handler) publicPage(w http.ResponseWriter, r *http.Request) {
	spaceID := SpaceFromContext(r.Context())
	if spaceID == "" || h.pages == nil {
		http.NotFound(w, r)
		return
	}
	slug := chi.URLParam(r, "slug")
	p, err := h.pages.GetBySlug(r.Context(), spaceID, slug)
	if err != nil || p == nil {
		http.NotFound(w, r)
		return
	}
	body := fmt.Sprintf(
		`<nav class="crumbs"><a href="/">← Docs</a></nav><h1>%s</h1><div class="page-content">%s</div>`,
		html.EscapeString(p.Title),
		// We deliberately render content_text as paragraphs — the
		// HTML projection of the full ProseMirror lives in
		// internal/export; the public view is intentionally minimal.
		paragraphsFromText(p.ContentText),
	)
	renderHTMLShell(w, p.Title, body)
}

func (h *Handler) publicSearch(w http.ResponseWriter, r *http.Request) {
	// Public search is deliberately out-of-scope for this minimal
	// surface — the spec leaves the depth to the operator. We
	// surface a "search disabled" page so the URL is still valid.
	renderHTMLShell(w, "Search", `<h1>Search</h1><p>Public search is not configured.</p>`)
}

// paragraphsFromText splits the dehydrated plain text into one
// <p> per blank-line block. Hardly Markdown but enough to make the
// public page legible without dragging in a renderer.
func paragraphsFromText(text string) string {
	if strings.TrimSpace(text) == "" {
		return "<p><em>No content yet.</em></p>"
	}
	var b strings.Builder
	for _, para := range strings.Split(strings.TrimSpace(text), "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		fmt.Fprintf(&b, "<p>%s</p>\n", html.EscapeString(para))
	}
	return b.String()
}
