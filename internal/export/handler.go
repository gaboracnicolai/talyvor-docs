package export

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
)

// MaxExportBytes caps a single export at 50MB. The constraint is
// enforced by buffering into a bytes.Buffer up to the cap before
// flushing to the http.ResponseWriter — so a runaway export can't
// stream tens of GB. PDF and DOCX both need their full byte length
// to be known up-front (so we'd buffer anyway), so the cap is
// honest about the trade-off rather than pretending to stream.
const MaxExportBytes = 50 << 20

type Handler struct {
	exp     *Exporter
	pageEnf *permission.Enforcer // A3: by-page access (view)
}

func NewHandler(exp *Exporter) *Handler { return &Handler{exp: exp} }

// WithAccess wires the A3 access enforcer. Without it the routes mount unguarded (tests).
func (h *Handler) WithAccess(pageEnf *permission.Enforcer) *Handler {
	h.pageEnf = pageEnf
	return h
}

func (h *Handler) Mount(r chi.Router) {
	// Exporting the full page content is a read → View (a member with no
	// grant on a private page must not export it).
	r.With(h.pageEnf.Require(permission.AccessView)).Get("/spaces/{spaceID}/pages/{pageID}/export", h.Export)
}

func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	pageID := chi.URLParam(r, "pageID")
	q := r.URL.Query()
	format := Format(q.Get("format"))
	if format == "" {
		writeErr(w, http.StatusBadRequest, "format is required")
		return
	}
	opts := ExportOptions{
		Format:          format,
		IncludeTOC:      q.Get("include_toc") == "true",
		IncludeChildren: q.Get("include_children") == "true",
		PageBreaks:      q.Get("page_breaks") == "true",
		PageTitle:       q.Get("page_title") != "false",
		Watermark:       q.Get("watermark"),
	}

	// SEC-4 L2: the source read is scoped to the caller's verified
	// workspace set (resolved from membership, never from a client
	// header/body). A page in a workspace the caller doesn't belong to
	// resolves to page.ErrNotFound → 404, so a member of workspace A
	// can't export workspace B's private page.
	wsIDs := authz.WorkspaceIDs(r.Context())

	// Look up the page so we can derive a filename. Scoped to the same
	// workspace set — a foreign id yields page.ErrNotFound → 404 here
	// before any content is streamed.
	rootTitle := "untitled"
	if e, ok := h.exp.pages.(interface {
		GetByIDInWorkspaces(context.Context, string, []string) (*model.Page, error)
	}); ok {
		p, err := e.GetByIDInWorkspaces(r.Context(), pageID, wsIDs)
		if err != nil {
			if errors.Is(err, page.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "page not found")
				return
			}
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if p != nil {
			rootTitle = p.Title
		}
	}

	// Buffer into the cap. Anything past MaxExportBytes returns 413.
	var buf limitedBuffer
	buf.cap = MaxExportBytes
	if err := h.exp.ExportPage(r.Context(), pageID, wsIDs, opts, &buf); err != nil {
		if err == errExceedsLimit {
			writeErr(w, http.StatusRequestEntityTooLarge, "export exceeds 50MB cap")
			return
		}
		if errors.Is(err, page.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "page not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	ct, ext := contentTypeAndExt(format)
	filename := slugFilename(rootTitle, ext)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", buf.Len()))
	_, _ = w.Write(buf.Bytes())
}

func contentTypeAndExt(f Format) (string, string) {
	switch f {
	case FormatPDF:
		return "application/pdf", "pdf"
	case FormatDocx:
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "docx"
	case FormatHTML:
		return "text/html; charset=utf-8", "html"
	case FormatMD:
		return "text/markdown; charset=utf-8", "md"
	}
	return "application/octet-stream", "bin"
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// errExceedsLimit signals the 50MB cap was hit during export.
var errExceedsLimit = fmt.Errorf("export: exceeds size cap")

// limitedBuffer wraps bytes.Buffer with a hard cap. Once the cap is
// hit subsequent writes return errExceedsLimit, which the handler
// translates to a 413.
type limitedBuffer struct {
	bytes.Buffer
	cap int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.cap {
		return 0, errExceedsLimit
	}
	return b.Buffer.Write(p)
}

// page.PageFilter import shim — keeps go vet happy when a future
// edit adds a filter-based variant. Compile-time no-op.
var _ = page.PageFilter{}
