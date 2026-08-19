package templatelib

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/spaceauth"
)

type Handler struct {
	store  *Store
	access *spaceauth.Authorizer // gates Use on the TARGET space's edit tier (space_id is in the body)
}

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// WithAccess wires the space-write authorizer that gates Use on the target space's AccessEdit tier.
// Without it Use fails closed (a nil authorizer refuses every write). SpaceResolverFromParam can't gate
// this route because space_id arrives in the request body, not the URL.
func (h *Handler) WithAccess(a *spaceauth.Authorizer) *Handler {
	h.access = a
	return h
}

// scopeFor authorizes the caller-supplied {wsID} path param against the verified membership set
// and returns the ONE-workspace scope every store call on these routes must filter by.
//
// ⚠ WHY THE SET IS NOT THE SCOPE HERE, THOUGH IT IS EVERYWHERE ELSE IN DOCS. authz's package doc
// says it "does NOT authorize a single path workspace" because "Docs's by-id routes are flat
// (/v1/spaces/{spaceID}/pages/{pageID}), not workspace-in-path like Track". These four routes are
// the exception that premise excludes — they ARE workspace-in-path. Handing List, Use and Delete
// authz.WorkspaceIDs made the {wsID} they were asked about decorative: a caller in workspaces A
// and B was served, could instantiate, and could DELETE B's custom templates through A's URL.
// FromPage alone had noticed (it authorizes {wsID} before that workspace owns the new template).
// Measured on real Postgres through the shipped chain in wsidscope_realpg_test.go. This is the
// same repair #166 made one package over in internal/customdomain, whose scopeFor this mirrors.
//
// FAIL-CLOSED: a caller with no memberships, an empty {wsID}, or a workspace the caller does not
// belong to all return ok=false. Callers answer 404 rather than 403 — the no-existence-oracle
// convention ErrNotFound already sets for this package.
//
// ⚠ IT RETURNS THE ID AND THE SCOPE SEPARATELY rather than just the slice, for the reason
// customdomain's copy records: a caller that reads scope[0] turns an over-refusal control into a
// PANIC instead of a catch. Nothing here indexes it today; the signature keeps it that way.
func scopeFor(r *http.Request) (wsID string, scope []string, ok bool) {
	wsID = chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized here: authz.AuthorizeWorkspace rejects non-members before this id becomes a query scope
	if _, member := authz.AuthorizeWorkspace(r.Context(), wsID); !member {
		return "", nil, false
	}
	return wsID, []string{wsID}, true
}

func (h *Handler) Mount(r chi.Router) {
	r.Get("/workspaces/{wsID}/template-library", h.List)
	r.Post("/workspaces/{wsID}/template-library/from-page", h.FromPage)
	r.Post("/workspaces/{wsID}/template-library/{templateID}/use", h.Use)
	r.Delete("/workspaces/{wsID}/template-library/{templateID}", h.Delete)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	// Scope to the {wsID} the caller ASKED ABOUT, after authorizing it against the verified
	// membership set. The path workspace is spoofable, which is why it is checked rather than
	// trusted — but the whole membership set is not the answer to a one-workspace question:
	// it served a caller in A and B every workspace's custom templates under A's URL.
	_, wsIDs, ok := scopeFor(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	var cat *TemplateCategory
	if c := strings.TrimSpace(r.URL.Query().Get("category")); c != "" {
		t := TemplateCategory(c)
		cat = &t
	}
	search := r.URL.Query().Get("search")
	out, err := h.store.List(r.Context(), wsIDs, cat, search)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out == nil {
		out = []LibraryTemplate{}
	}
	writeJSON(w, http.StatusOK, out)
}

type fromPageBody struct {
	PageID      string           `json:"page_id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Category    TemplateCategory `json:"category"`
}

func (h *Handler) FromPage(w http.ResponseWriter, r *http.Request) {
	var in fromPageBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	// A template COPIES the source page's content verbatim into the library (readable via List), so
	// creating one requires VIEW on that page — not just workspace membership. Otherwise a member could
	// templatize a page in a private space they cannot read and then read its content from the library.
	// page_id is in the body, so SpaceResolverFromParam can't gate it; spaceauth resolves + tier-checks
	// it in-handler via permission.CheckPage. Fail-closed: a foreign/unresolvable page → 404 (no oracle);
	// resolvable but under View → 403.
	if found, canView := h.access.AuthorizePageRead(r.Context(), in.PageID); !found {
		writeErr(w, http.StatusNotFound, "not found")
		return
	} else if !canView {
		writeErr(w, http.StatusForbidden, "insufficient access: creating a template requires view access to the source page")
		return
	}
	// The SOURCE page read is scoped to the caller's VERIFIED set. The NEW
	// template's owner is the {wsID} path workspace, but ONLY if the caller
	// is a verified member of it — otherwise a raw path param could name a
	// foreign workspace to plant a template into. Reject with 404 (no leak).
	wsIDs := authz.WorkspaceIDs(r.Context())
	target := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized write-target: authz.AuthorizeWorkspace below rejects non-members before this workspace owns the new object
	// AuthorizeWorkspace hands back the caller's Membership — use ITS member id. It is the
	// caller's id in THIS workspace, correct for any membership count, unlike ActorOrEmpty
	// which is "" for anyone with != 1 memberships (leaving the object unattributed).
	m, ok := authz.AuthorizeWorkspace(r.Context(), target)
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	creator := m.MemberID
	tmpl, err := h.store.CreateFromPage(r.Context(),
		in.PageID, target, creator,
		in.Name, in.Description, in.Category, wsIDs,
	)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, tmpl)
}

type useBody struct {
	SpaceID string `json:"space_id"`
}

func (h *Handler) Use(w http.ResponseWriter, r *http.Request) {
	templateID := chi.URLParam(r, "templateID")
	var in useBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if in.SpaceID == "" {
		writeErr(w, http.StatusBadRequest, "space_id is required")
		return
	}
	// TEMPLATE read is scoped to the caller's VERIFIED set (wsIDs). The NEW page is created in the
	// BODY-named space, so its creation must clear THAT space's AccessEdit tier — the same page.Create
	// enforces at the canonical door. The {wsID} path param cannot authorize a body-named space, and
	// SpaceResolverFromParam only reads chi URL params, so spaceauth resolves + tier-checks it in-handler.
	// Fail-closed: a foreign/unresolvable space → 404 (no oracle); an under-edit tier → 403. The page's
	// workspace + author come from the resolved SPACE, never a client-supplied id.
	//
	// The TEMPLATE is addressed by the {wsID} in the path, so that is its scope (authorized, see
	// scopeFor) — not the caller's whole membership set, which let A's URL instantiate B's
	// template. The TARGET space keeps its own, separate authorization: templating one workspace's
	// template into another workspace's space is still possible, through the OWNING workspace's URL.
	_, wsIDs, ok := scopeFor(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	d := h.access.AuthorizeSpaceWrite(r.Context(), in.SpaceID)
	if !d.Found {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if !d.CanEdit {
		writeErr(w, http.StatusForbidden, "insufficient access: creating a page requires edit access on the space")
		return
	}
	page, err := h.store.UseTemplate(r.Context(), templateID, in.SpaceID, d.WorkspaceID, d.Actor, wsIDs)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"page_id":  page.ID,
		"page_url": "/spaces/" + page.SpaceID + "/pages/" + page.ID,
	})
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	templateID := chi.URLParam(r, "templateID")
	// Scope to the {wsID} the caller asked about, authorized against the verified membership set.
	// A template outside that workspace → 404, never deleted (the SEC-4 L2 "deceptive shape" fix
	// covered the STRANGER; this covers the second workspace the same caller happens to be in,
	// whose rows the gallery renders under A's heading with a delete button and no owner label).
	_, wsIDs, ok := scopeFor(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	err := h.store.Delete(r.Context(), templateID, wsIDs)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		// Built-in deletion returns a typed error; map to 400 so
		// the UI can disable the trash icon for built-ins instead
		// of treating it as a server fault.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
