package page

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/permission"
)

// Comments moved to internal/comment in the threaded-comments
// rework. The page handler retains the constructor signature for
// backwards compatibility — pool is no longer used here but main
// still hands it in for symmetry with other handlers.
type Handler struct {
	store    *Store
	pageEnf  *permission.Enforcer // A3: by-page access (view/edit)
	spaceEnf *permission.Enforcer // A3: by-space access for the space-scoped create/list routes
	// access gates the two workspace-scoped list routes (search / stale), whose target is a
	// query rather than an id. nil FAILS CLOSED — see WithPageRead.
	access pageReader
}

func NewHandler(store *Store, _ *pgxpool.Pool) *Handler {
	return &Handler{store: store}
}

// WithAccess wires the A3 access enforcers. Without it those routes FAIL CLOSED:
// Enforcer.Require on a NIL receiver denies with 404 and never reaches the handler, so a
// test that skips WithAccess sees 404s, not an open door
// (permission.TestEnforcer_NilReceiver_FailsClosed).
func (h *Handler) WithAccess(pageEnf, spaceEnf *permission.Enforcer) *Handler {
	h.pageEnf, h.spaceEnf = pageEnf, spaceEnf
	return h
}

// pageReader authorizes a READ of ONE page for the verified caller. *spaceauth.Authorizer
// satisfies it — the same shipped primitive internal/search uses, so this package introduces NO
// new access model.
//
// THE TWO ROUTES IT EXISTS FOR ARE THE ONES NO ENFORCER CAN REACH. Both /workspaces/{wsID}/pages/
// search and /stale name a QUERY or a PREDICATE, not an id, so no chi URL-param resolver
// (permission.RequireAccess) can gate them — exactly the reason internal/search needed the same
// primitive. Before this they authorized the WORKSPACE and stopped, and each selects the full
// column list, so a workspace member with no grant on a PRIVATE space received that space's pages
// WHOLE: title, space id, author, and `content` — the entire ProseMirror document. See
// privatespace_realpg_test.go for the measurement.
type pageReader interface {
	AuthorizePageRead(ctx context.Context, pageID string) (found, canView bool)
}

// WithPageRead attaches the per-page read gate used by the two workspace-scoped list routes.
//
// ⚠ NIL FAILS CLOSED — visibleTo returns NOTHING — AND THAT DELIBERATELY DIFFERS FROM
// internal/search, WHOSE nil MEANS UNFILTERED. That convention is why its whole fix hangs on one
// main.go line, and a route that looks gated but is not is the defect this method exists to close.
// It mirrors permission.Enforcer.Require's nil receiver, this repo's established direction for a
// missing gate, and it is why no mainwiring tripwire is needed here: an unwired gate is an empty
// endpoint, which a test asserting rows already fails on.
//
// ⚠ AND THAT LAST CLAIM IS A MEASUREMENT, NOT A HOPE — IT FALSIFIED MY FIRST ONE. I chose
// fail-closed after grepping for tests that assert rows on these routes and concluding there were
// none. There was one: TestSEC4_WorkspaceRoutes_SearchAndStale_CrossTenant, whose ANCHOR reads
// Bob's own page back before asserting the cross-tenant denials. It went red immediately, on its
// own premise assertion, and newV1Chain now wires the gate. A grep for callers is a census of what
// a name is SPELLED in, not of what a route is ASSERTED about.
func (h *Handler) WithPageRead(a pageReader) *Handler {
	h.access = a
	return h
}

// visibleTo drops every row the caller may not VIEW, asking the same engine the by-id page routes
// ask. Callers are already workspace-authorized; this is the SPACE/PAGE tier they never consulted.
func (h *Handler) visibleTo(ctx context.Context, rows []model.Page) []model.Page {
	if h.access == nil {
		return nil // fail closed — see WithPageRead
	}
	out := make([]model.Page, 0, len(rows))
	for _, p := range rows {
		found, canView := h.access.AuthorizePageRead(ctx, p.ID)
		if !found || !canView {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Mount registers every page-scoped route under /v1. Comments,
// versions, view, verify, search, and stale all live under the
// same handler so the page surface is one chi sub-router.
func (h *Handler) Mount(r chi.Router) {
	r.Route("/spaces/{spaceID}/pages", func(r chi.Router) {
		// Create a page in / list a space's pages → space-level (Edit to add, View to list).
		r.With(h.spaceEnf.Require(permission.AccessEdit)).Post("/", h.Create)
		r.With(h.spaceEnf.Require(permission.AccessView)).Get("/", h.List)
		// Per-page: read=View, content mutation=Edit.
		r.With(h.pageEnf.Require(permission.AccessView)).Get("/{pageID}", h.Get)
		r.With(h.pageEnf.Require(permission.AccessEdit)).Patch("/{pageID}", h.Update)
		r.With(h.pageEnf.Require(permission.AccessEdit)).Delete("/{pageID}", h.Delete)

		// POST /{pageID}/view is deliberately NOT registered here — it is owned by
		// internal/analytics, which registers the same absolute path and mounts AFTER this
		// handler in main.go. analytics has therefore always served it; this package's
		// registration was shadowed dead code, and analytics.RecordView subsumes it (it
		// inserts page_views AND bumps view_count/last_viewed_at). One path, one owner:
		// the duplicate meant a handler could look live while another one served.

		r.With(h.pageEnf.Require(permission.AccessEdit)).Post("/{pageID}/verify", h.Verify)

		r.With(h.pageEnf.Require(permission.AccessView)).Get("/{pageID}/versions", h.GetVersions)
		// ⚠ NOT `/{pageID}/versions/cost`, AND THE REASON IS ONE ROUTE UP. `/{pageID}/versions/
		// {version}` is registered on the next line, so a sibling under the same prefix would sit
		// beside a wildcard and its resolution would depend on chi's static-over-param precedence
		// — true today, invisible at the call site, and a 400 BAD_VERSION rather than a 404 the
		// day it stopped being true. A distinct segment cannot be shadowed by anything.
		r.With(h.pageEnf.Require(permission.AccessView)).Get("/{pageID}/version-cost", h.GetVersionCostSplit)
		r.With(h.pageEnf.Require(permission.AccessView)).Get("/{pageID}/versions/{version}", h.GetVersion)
		r.With(h.pageEnf.Require(permission.AccessView)).Get("/{pageID}/versions/{version}/diff/{other}", h.DiffVersions)
		r.With(h.pageEnf.Require(permission.AccessEdit)).Post("/{pageID}/versions/{version}/restore", h.RestoreVersion)

		// Comment routes live in internal/comment as of the threaded-
		// comments rework. The legacy handlers below stay for the
		// /pages/{pageID}/comments/{commentID} update path, but the
		// list/create/resolve trio is owned by the new package.
	})

	r.Get("/workspaces/{wsID}/pages/search", h.Search)
	r.Get("/workspaces/{wsID}/pages/stale", h.Stale)
}

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: msg, Code: code})
}

// ─── page CRUD ────────────────────────────────────────────

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "spaceID")
	var in model.Page
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	in.SpaceID = spaceID
	// SEC-4: model.Page carries workspace_id and created_by, and this decodes the whole
	// struct from the body — so both used to be caller-supplied, and Store.Create REQUIRED
	// a workspace_id rather than deriving one. A caller could create in a space they
	// legitimately admin while naming ANOTHER tenant's workspace, landing the row in that
	// tenant: every L2 query filters on workspace_id, so it surfaced in the victim's
	// search/stale reports, attacker-authored and falsely attributed.
	//
	// Both values now come from the resource the route already authorized: the workspace
	// is the PARENT SPACE's (resolved by spaceEnf's RequireAccess), and created_by is the
	// caller's member id in that workspace. This mirrors space/handler.go's Create, which
	// was fixed the same way. Deriving the workspace is also what lets an honest client
	// stop sending one at all.
	ws, ok := permission.WorkspaceFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "cannot resolve the workspace for this space")
		return
	}
	in.WorkspaceID = ws
	actor, ok := permission.ActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "cannot resolve the acting member for this space")
		return
	}
	in.CreatedBy = actor
	out, err := h.store.Create(r.Context(), in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "CREATE_FAILED", err.Error())
		return
	}
	// docs_pages_created_total IS INCREMENTED IN Store.Create, NOT HERE. This was the only
	// call site, and it is one of SIX doors into that store method — see the block above the
	// INSERT in store.go, and pagescreated_metric_realpg_test.go.
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "spaceID")
	filter := PageFilter{SpaceID: spaceID}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Limit = n
		}
	}
	out, err := h.store.List(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if out == nil {
		out = []model.Page{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	// SEC-4: scope to the caller's verified workspace membership — a page in another
	// workspace is not-found (404), never leaked.
	out, err := h.store.GetByIDInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), authz.WorkspaceIDs(r.Context()))
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	var updates map[string]any
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	// SEC-4: the editor identity for the lock guard is the VERIFIED member id, never a
	// client-supplied header or body field. Overwrite any caller-provided updated_by.
	// ActorFromContext (not authz.SingleMemberID) — the latter returns "" for any caller
	// with != 1 memberships, which silently DROPPED updated_by for every multi-workspace
	// member, leaving the column stale and the lock guard unable to recognise the locker.
	if mid, ok := permission.ActorFromContext(r.Context()); ok {
		updates["updated_by"] = mid
	} else {
		delete(updates, "updated_by")
	}
	// Same rule for the lock guard's admin-bypass. `updates` IS the decoded request body,
	// so a caller could send {"is_admin": true} and Store.Update would hand it to
	// guard.CanEdit, which returns true outright for an admin — bypassing another
	// member's page lock. Overwrite it with the level RequireAccess resolved from the
	// gateway-verified identity. Assigning unconditionally (rather than deleting) also
	// restores the override for REAL admins, who previously could not use it without
	// asserting a flag the honest client never sent.
	updates["is_admin"] = permission.IsAdminFromContext(r.Context())
	out, err := h.store.UpdateInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), updates, authz.WorkspaceIDs(r.Context()))
	if err != nil {
		// Cross-tenant / unknown page → 404 (never leak existence).
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
			return
		}
		// 423 Locked is the precise signal for lock conflicts so the
		// frontend can render a specific banner; everything else is
		// a generic 400 because it's caller-supplied bad input.
		if errors.Is(err, ErrLocked) {
			writeErr(w, http.StatusLocked, "LOCKED", err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, "UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	err := h.store.DeleteInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─── view / verify ────────────────────────────────────────

func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	verifier, _ := permission.ActorFromContext(r.Context()) // verified actor, resolved in THIS page's workspace
	err := h.store.VerifyInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), verifier, authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "VERIFY_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─── versions ─────────────────────────────────────────────

func (h *Handler) GetVersions(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.GetVersionsInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "VERSIONS_FAILED", err.Error())
		return
	}
	if out == nil {
		out = []model.PageVersion{}
	}
	writeJSON(w, http.StatusOK, out)
}

// versionCostSplitBody is the WIRE shape of a page's AI spend split. It is declared here rather
// than by tagging `Store.VersionCostSplit` because the store's field names are free to change and
// these four keys are a contract with the SPA — and because every one of them is money, where a
// silently renamed key reads to a client as an absent value and to a reader as a missing bucket.
//
// ⚠ THE UNITS ARE IN THE NAMES. The store calls them Attributed/Pending/Unattributable/PageTotal;
// on the wire they carry `_usd`, matching `own_ai_cost_usd` and `total_ai_cost_usd`, the two money
// keys this API already serves. A bare `attributed` beside those would be the only money field in
// the surface that does not say what it is denominated in.
type versionCostSplitBody struct {
	Attributed     float64 `json:"attributed_usd"`
	Pending        float64 `json:"pending_usd"`
	Unattributable float64 `json:"unattributable_usd"`
	PageTotal      float64 `json:"page_total_usd"`
}

// GetVersionCostSplit serves the reconciliation between what version history SHOWS and what the
// page has actually spent.
//
// ⚠ IT EXISTS BECAUSE THE FUNCTION UNDER IT HAD NO CALLER. `Store.VersionCostSplit` landed with
// #190 arguing, in its own docstring, that "a per-revision figure that does not add up is a lie
// about money that looks like a feature" — and then nothing served it. MEASURED at merged main
// `f1ad4db`: zero production callers, only its own unit test and two comments naming it. The
// reconciliation was guaranteed in Go and unreachable from the product, which is the same
// distance from a reader as not existing.
//
// ⚠ AND THE GAP IS ALREADY ON SCREEN, which is what makes this a route rather than a report.
// `frontend/src/pages/PageView.tsx` renders `<VersionHistory>` and, directly beneath it, the panel
// printing `own_ai_cost_usd`. A reader therefore already sees both numbers; when a page carries
// pending or pre-0021 spend they do not agree, and until now nothing could say why.
//
// Workspace-scoped like every other per-page read here: the store's `assertInWorkspaces` does the
// refusing, and it is handed the CALLER's verified workspaces — a route that passed the page's own
// would defeat it without changing a line of that function.
func (h *Handler) GetVersionCostSplit(w http.ResponseWriter, r *http.Request) {
	split, err := h.store.VersionCostSplit(r.Context(), chi.URLParam(r, "pageID"), authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "VERSION_COST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, versionCostSplitBody{
		Attributed:     split.Attributed,
		Pending:        split.Pending,
		Unattributable: split.Unattributable,
		// ⚠ PASSED THROUGH, NEVER RECOMPUTED AS THE SUM OF THE OTHER THREE. The store reads this
		// from `pages.own_ai_cost_usd` precisely so the whole and its parts are two independent
		// numbers that can be COMPARED; deriving it here would make the reconciliation true by
		// construction and blind — the arithmetic would balance in exactly the case where the
		// buckets had lost money.
		PageTotal: split.PageTotal,
	})
}

// GetVersion returns a single historical snapshot. Workspace-scoped: a page outside the
// caller's verified workspaces resolves to 404 (no cross-tenant version read).
func (h *Handler) GetVersion(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_VERSION", "version must be int")
		return
	}
	out, err := h.store.GetVersionInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), n, authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "VERSION_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// DiffVersions returns two snapshots ({from, to}) for a version comparison. Workspace-scoped:
// a page outside the caller's verified workspaces resolves to 404 (no cross-tenant diff).
func (h *Handler) DiffVersions(w http.ResponseWriter, r *http.Request) {
	from, err1 := strconv.Atoi(chi.URLParam(r, "version"))
	to, err2 := strconv.Atoi(chi.URLParam(r, "other"))
	if err1 != nil || err2 != nil {
		writeErr(w, http.StatusBadRequest, "BAD_VERSION", "versions must be ints")
		return
	}
	fromV, toV, err := h.store.CompareVersionsInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), from, to, authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DIFF_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": fromV, "to": toV})
}

func (h *Handler) RestoreVersion(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_VERSION", "version must be int")
		return
	}
	out, err := h.store.RestoreVersionInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), n, authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "RESTORE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── search + stale ───────────────────────────────────────

func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	// SEC-4: {wsID} comes from the URL — attacker-controlled — so scoping the store op to it
	// "looks scoped but isn't". Authorize it against the caller's VERIFIED membership set
	// first, or a member of any workspace could read another workspace's document body text.
	// Mirrors internal/search/handler.go's guard on the sibling /workspaces/{wsID}/search.
	// Authorize BEFORE validating params so a foreign workspace cannot be probed via the
	// difference between a 400 and a 403.
	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized by AuthorizeWorkspace on the next line, before any store op
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "not a member of this workspace")
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		writeErr(w, http.StatusBadRequest, "BAD_PARAMS", "q required")
		return
	}
	limit := 25
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	// OVER-FETCH, BECAUSE THE ROWS THE CALLER MAY NOT READ ARE DROPPED AFTER THE SQL LIMIT.
	// Measured: with limit=1 and one hidden page sorting above a readable one, the store returns
	// the hidden row alone, visibleTo drops it, and the caller is told NOTHING MATCHED for a
	// document they may open. It is a mitigation, not a guarantee — a run of more than
	// (searchFetchFactor-1)×limit consecutive hidden rows still under-fills — and the response has
	// always been a bare list with no total, so nothing here newly misreports a count.
	fetchLimit := limit
	if h.access != nil {
		fetchLimit = limit * searchFetchFactor
		if fetchLimit > searchMaxFetchRows {
			fetchLimit = searchMaxFetchRows
		}
	}
	rows, err := h.store.Search(r.Context(), wsID, q, fetchLimit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "SEARCH_FAILED", err.Error())
		return
	}
	// Drop rows the caller may not VIEW *before* truncating to `limit` — a page they cannot read
	// must not consume one of their result slots.
	out := h.visibleTo(r.Context(), rows)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	if out == nil {
		out = []model.Page{}
	}
	writeJSON(w, http.StatusOK, out)
}

// searchFetchFactor / searchMaxFetchRows size the window Store.Search is asked for when the access
// gate is wired. searchMaxFetchRows is the store's OWN ceiling (Search clamps limit to 100), named
// here so the two numbers cannot silently disagree.
const (
	searchFetchFactor  = 4
	searchMaxFetchRows = 100
)

func (h *Handler) Stale(w http.ResponseWriter, r *http.Request) {
	// SEC-4: same deceptive shape as Search above — {wsID} is attacker-controlled, so
	// authorize it against the verified membership set before the store op.
	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized on the next line before any store op
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "not a member of this workspace")
		return
	}
	rows, err := h.store.GetStalePages(r.Context(), wsID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "STALE_FAILED", err.Error())
		return
	}
	// GetStalePages has no LIMIT, so unlike Search there is no truncation to race and the filter
	// is complete rather than a mitigation.
	out := h.visibleTo(r.Context(), rows)
	if out == nil {
		out = []model.Page{}
	}
	writeJSON(w, http.StatusOK, out)
}
