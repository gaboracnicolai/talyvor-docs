package analytics

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/permission"
)

type Handler struct {
	store    *Store
	pageEnf  *permission.Enforcer // A3: by-page access (view/edit)
	spaceEnf *permission.Enforcer // A3: by-space access — the SPACE roll-up only
}

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// WithAccess wires the A3 access enforcer. Without it those routes FAIL CLOSED:
// Enforcer.Require on a NIL receiver denies with 404 and never reaches the handler, so a
// test that skips WithAccess sees 404s, not an open door
// (permission.TestEnforcer_NilReceiver_FailsClosed).
func (h *Handler) WithAccess(pageEnf *permission.Enforcer) *Handler {
	h.pageEnf = pageEnf
	return h
}

// WithSpaceAccess wires the by-SPACE enforcer that gates the space roll-up.
//
// ⚠ A SEPARATE METHOD RATHER THAN A SECOND PARAMETER ON `WithAccess`, and the reason is not
// taste: `WithAccess(pageEnf)` has nineteen call sites across this repo, all but one of them in
// tests that build their own router. Widening its signature would edit nineteen files to add a
// gate eighteen of them do not use, and every one of those edits is a chance to pass the wrong
// enforcer to a route that then looks gated.
//
// ⚠ AND NOT WIRING IT FAILS CLOSED, which is what makes an additive method safe here.
// `Enforcer.Require` on a NIL receiver denies with 404 and never reaches the handler
// (permission.TestEnforcer_NilReceiver_FailsClosed), so a router that forgets this call serves a
// dead route rather than an open one. `mainwiring_test.go` is the tripwire on the production call
// site, because a dead route is still a feature that looks shipped.
func (h *Handler) WithSpaceAccess(spaceEnf *permission.Enforcer) *Handler {
	h.spaceEnf = spaceEnf
	return h
}

func (h *Handler) Mount(r chi.Router) {
	r.With(h.pageEnf.Require(permission.AccessView)).Post("/spaces/{spaceID}/pages/{pageID}/view", h.RecordView)
	r.With(h.pageEnf.Require(permission.AccessView)).Get("/spaces/{spaceID}/pages/{pageID}/analytics", h.PageStats)
	r.Get("/workspaces/{wsID}/analytics/pages", h.WorkspaceStats)
	// THE SPACE ROLL-UP — the third of the three scopes the product page sells ("PAGE, SPACE AND
	// ORG ROLLUPS") and the one that did not exist. Gated by the SPACE enforcer, unlike the org
	// route above, which authorizes its URL workspace in the handler: here the resource in the
	// path IS a space, so the repo's own space gate is the right one and a private space the
	// caller has no grant on is refused before any aggregation happens.
	r.With(h.spaceEnf.Require(permission.AccessView)).Get("/spaces/{spaceID}/analytics/pages", h.SpaceStats)
}

// SpaceStats serves the readership roll-up for ONE space.
//
// ⚠ IT IS CHECKED TWICE, AND THE SECOND CHECK IS NOT THE FIRST ONE REPEATED. The route's space
// enforcer answers "may this caller VIEW this space" — a question about GRANTS. The workspace
// assertion below answers "is that space in a workspace this session is a verified member of" —
// a question about TENANCY. Different subsystems, different failure modes; the org route already
// applies the tenancy half to its own URL param and this is the same rule one level down.
//
// The per-page visibility filter inside the roll-up is the third, and it is the one that actually
// protects private CONTENT — see main.go's note on the defect where the org route "authorized the
// workspace and stopped".
func (h *Handler) SpaceStats(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "spaceID")
	wsID, err := h.store.WorkspaceOfSpace(r.Context(), spaceID)
	if err != nil {
		// A space that does not exist and a space in someone else's workspace answer the same
		// way, on purpose: the alternative tells an unauthorized caller which space ids are real.
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeErr(w, http.StatusForbidden, "forbidden")
		return
	}
	stats, err := h.store.GetSpaceStats(r.Context(), wsID, spaceID, daysParam(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stats failed")
		return
	}
	if stats == nil {
		stats = withEmptyCohorts(&WorkspaceReadStats{})
	}
	writeJSON(w, http.StatusOK, stats)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// daysParam parses the ?days= query param with a sane default + cap.
// 30 days is the spec default; we cap at 365 to avoid pathological
// aggregate scans.
func daysParam(r *http.Request) int {
	d, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if d <= 0 {
		d = 30
	}
	if d > 365 {
		d = 365
	}
	return d
}

func (h *Handler) RecordView(w http.ResponseWriter, r *http.Request) {
	// No viewer_id / workspace_id: both are derived from the verified caller and the
	// authorized page. viewer_name is display text only, not identity.
	var in struct {
		ViewerName string `json:"viewer_name"`
		Duration   int    `json:"duration_sec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	// SEC: the workspace AND the viewer both come from the resource this route already
	// authorized — never the body.
	//
	// workspace_id was overridden here before, but via authz.WorkspaceOrEmpty, which
	// returns "" for a multi-workspace caller and so silently NO-OP'd for exactly those
	// callers, leaving the client's value. viewer_id was not overridden at all, and it
	// feeds COUNT(DISTINCT viewer_id) / GROUP BY viewer_id — so the body could forge who
	// read a page. WorkspaceFromContext / ActorFromContext are correct for any membership
	// count.
	ws, ok := permission.WorkspaceFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the workspace for this page")
		return
	}
	viewer, ok := permission.ActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the viewing member for this page")
		return
	}
	// RecordViewInWorkspaces scopes the write to the caller's VERIFIED membership set — so the
	// page bump is gated in-method, not solely by this route's pageEnf wiring. A foreign pageID
	// resolves to 404 (no cross-tenant existence oracle).
	err := h.store.RecordViewInWorkspaces(r.Context(), PageView{
		PageID:      chi.URLParam(r, "pageID"),
		WorkspaceID: ws,
		ViewerID:    viewer,
		ViewerName:  in.ViewerName,
		Duration:    in.Duration,
	}, authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "record failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) PageStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.store.GetReadStats(r.Context(), chi.URLParam(r, "pageID"), daysParam(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stats failed")
		return
	}
	if stats == nil {
		// The no-pool fallback builds its own value, so it needs the same normalisation the
		// store return already has — a zero ReadStats has nil lists and would put `null` back
		// on the wire down exactly this branch. See withEmptyLists.
		stats = withEmptyLists(&ReadStats{PageID: chi.URLParam(r, "pageID")})
	}
	writeJSON(w, http.StatusOK, stats)
}

func (h *Handler) WorkspaceStats(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized below by AuthorizeWorkspace, before any store read
	// A4D: authorize the URL workspace against the caller's verified memberships.
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeErr(w, http.StatusForbidden, "forbidden")
		return
	}
	stats, err := h.store.GetWorkspaceStats(r.Context(), wsID, daysParam(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stats failed")
		return
	}
	if stats == nil {
		stats = withEmptyCohorts(&WorkspaceReadStats{})
	}
	writeJSON(w, http.StatusOK, stats)
}
