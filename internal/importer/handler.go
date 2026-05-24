package importer

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type Handler struct{ imp *ConfluenceImporter }

func NewHandler(imp *ConfluenceImporter) *Handler { return &Handler{imp: imp} }

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
	result, err := h.imp.ImportExport(r.Context(), wsID, spaceID, bytes.NewReader(body))
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
	result, err := h.imp.ImportFromNotion(r.Context(), wsID, spaceID, bytes.NewReader(body))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}
