package importer

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/spaceauth"
)

type Handler struct {
	imp    *ConfluenceImporter
	access *spaceauth.Authorizer // gates import on the TARGET space's edit tier (space_id is in the form)
}

func NewHandler(imp *ConfluenceImporter) *Handler { return &Handler{imp: imp} }

// WithAccess wires the space-write authorizer that gates import on the target space's AccessEdit tier.
// Without it import fails closed (a nil authorizer refuses). space_id arrives in the multipart form, not
// the URL, so SpaceResolverFromParam can't gate this route.
func (h *Handler) WithAccess(a *spaceauth.Authorizer) *Handler {
	h.access = a
	return h
}

// maxUploadBytes caps any single import to a manageable size so a
// malicious zip can't exhaust the box's memory. 200MB matches the
// largest reasonable Confluence space export observed in the wild.
const maxUploadBytes = 200 << 20

func (h *Handler) Mount(r chi.Router) {
	r.Post("/import/confluence", h.Confluence)
	r.Post("/import/notion", h.Notion)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// readUpload pulls the workspace_id, space_id, and zip file off a
// multipart upload. Both endpoints share this preamble.
func readUpload(r *http.Request) (workspaceID, spaceID string, body []byte, err error) {
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		return "", "", nil, err
	}
	workspaceID = r.FormValue("workspace_id")
	spaceID = r.FormValue("space_id")
	file, _, err := r.FormFile("file")
	if err != nil {
		return "", "", nil, err
	}
	defer file.Close()
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 32<<10)
	for {
		n, rerr := file.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	return workspaceID, spaceID, buf, nil
}

func (h *Handler) Confluence(w http.ResponseWriter, r *http.Request) {
	wsID, spaceID, body, err := readUpload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "upload failed: "+err.Error())
		return
	}
	if wsID == "" || spaceID == "" {
		writeErr(w, http.StatusBadRequest, "workspace_id and space_id are required")
		return
	}
	// Import creates page content in the FORM-named space, so it must clear THAT space's AccessEdit
	// tier — the same page.Create enforces. Membership alone (AuthorizeWorkspace) let a view-tier member
	// bulk-plant pages into a space they may only view. spaceauth resolves the space scoped to the
	// caller's workspaces and tier-checks it; fail-closed (foreign → 404, under-edit → 403). The pages'
	// workspace comes from the resolved SPACE (d.WorkspaceID), never the client-supplied workspace_id.
	d := h.access.AuthorizeSpaceWrite(r.Context(), spaceID)
	if !d.Found {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if !d.CanEdit {
		writeErr(w, http.StatusForbidden, "insufficient access: importing requires edit access on the space")
		return
	}
	result, err := h.imp.ImportExport(r.Context(), d.WorkspaceID, spaceID, bytes.NewReader(body))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (h *Handler) Notion(w http.ResponseWriter, r *http.Request) {
	wsID, spaceID, body, err := readUpload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "upload failed: "+err.Error())
		return
	}
	if wsID == "" || spaceID == "" {
		writeErr(w, http.StatusBadRequest, "workspace_id and space_id are required")
		return
	}
	// See Confluence: gate on the FORM-named space's AccessEdit tier (not just workspace membership),
	// fail-closed, and take the pages' workspace from the resolved SPACE.
	d := h.access.AuthorizeSpaceWrite(r.Context(), spaceID)
	if !d.Found {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if !d.CanEdit {
		writeErr(w, http.StatusForbidden, "insufficient access: importing requires edit access on the space")
		return
	}
	result, err := h.imp.ImportFromNotion(r.Context(), d.WorkspaceID, spaceID, bytes.NewReader(body))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}
