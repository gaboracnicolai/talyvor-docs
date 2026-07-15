package changelog

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/permission"
)

type Handler struct {
	store   *Store
	pageEnf *permission.Enforcer // A3: by-page access (view/edit)
}

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// WithAccess wires the A3 access enforcer. Without it the routes mount unguarded (tests).
func (h *Handler) WithAccess(pageEnf *permission.Enforcer) *Handler {
	h.pageEnf = pageEnf
	return h
}

func (h *Handler) Mount(r chi.Router) {
	// Per-page: read=View, entry mutation (create/update/delete/publish/generate)=Edit.
	r.With(h.pageEnf.Require(permission.AccessEdit)).Post("/spaces/{spaceID}/pages/{pageID}/changelog/entries", h.Create)
	r.With(h.pageEnf.Require(permission.AccessView)).Get("/spaces/{spaceID}/pages/{pageID}/changelog/entries", h.List)
	r.With(h.pageEnf.Require(permission.AccessView)).Get("/spaces/{spaceID}/pages/{pageID}/changelog/entries/{id}", h.Get)
	r.With(h.pageEnf.Require(permission.AccessEdit)).Patch("/spaces/{spaceID}/pages/{pageID}/changelog/entries/{id}", h.Update)
	r.With(h.pageEnf.Require(permission.AccessEdit)).Delete("/spaces/{spaceID}/pages/{pageID}/changelog/entries/{id}", h.Delete)
	r.With(h.pageEnf.Require(permission.AccessEdit)).Post("/spaces/{spaceID}/pages/{pageID}/changelog/entries/{id}/publish", h.Publish)
	r.With(h.pageEnf.Require(permission.AccessEdit)).Post("/spaces/{spaceID}/pages/{pageID}/changelog/generate", h.Generate)
	r.Get("/workspaces/{wsID}/changelog/feed", h.Feed)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var in ChangelogEntry
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	in.PageID = chi.URLParam(r, "pageID")
	// SEC: this used to be an INVERTED fallback — `if in.CreatedBy == "" { ...verified }`
	// PREFERRED the client's value, so any caller authored an entry as anyone, with no
	// precondition. The workspace override next to it (WorkspaceOrEmpty) silently no-op'd
	// for multi-workspace callers, leaving the body's workspace_id. Both now derive from
	// the parent page, which pageEnf.Require already authorized.
	ws, ok := permission.WorkspaceFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the workspace for this page")
		return
	}
	actor, ok := permission.ActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the acting member for this page")
		return
	}
	in.WorkspaceID = ws
	in.CreatedBy = actor
	out, err := h.store.CreateEntry(r.Context(), in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	var typ *EntryType
	if t := r.URL.Query().Get("type"); t != "" {
		et := EntryType(t)
		typ = &et
	}
	wsIDs := authz.WorkspaceIDs(r.Context())
	out, err := h.store.ListEntries(r.Context(), chi.URLParam(r, "pageID"), typ, limit, offset, wsIDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out == nil {
		out = []ChangelogEntry{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	wsIDs := authz.WorkspaceIDs(r.Context())
	e, err := h.store.GetEntry(r.Context(), chi.URLParam(r, "id"), wsIDs)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	var updates map[string]any
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	wsIDs := authz.WorkspaceIDs(r.Context())
	e, err := h.store.UpdateEntry(r.Context(), chi.URLParam(r, "id"), updates, wsIDs)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	wsIDs := authz.WorkspaceIDs(r.Context())
	err := h.store.DeleteEntry(r.Context(), chi.URLParam(r, "id"), wsIDs)
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

func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	wsIDs := authz.WorkspaceIDs(r.Context())
	e, err := h.store.PublishEntry(r.Context(), chi.URLParam(r, "id"), wsIDs)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, e)
}

type generateBody struct {
	Version     string   `json:"version"`
	IssueIDs    []string `json:"issue_ids"`
	WorkspaceID string   `json:"workspace_id"`
}

func (h *Handler) Generate(w http.ResponseWriter, r *http.Request) {
	var in generateBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	// SEC: same shape as Create above. WorkspaceOrEmpty no-ops for a multi-workspace
	// caller, leaving the BODY's workspace_id as the tenancy key; ActorOrEmpty is "" for
	// them, leaving the entry unattributed. Derive both from the parent page.
	ws, ok := permission.WorkspaceFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the workspace for this page")
		return
	}
	createdBy, ok := permission.ActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the acting member for this page")
		return
	}
	in.WorkspaceID = ws
	out, err := h.store.GenerateFromIssues(r.Context(),
		in.WorkspaceID, chi.URLParam(r, "pageID"), createdBy,
		in.IssueIDs, in.Version)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ─── RSS feed ────────────────────────────────────────

type rssChannel struct {
	XMLName xml.Name  `xml:"channel"`
	Title   string    `xml:"title"`
	Link    string    `xml:"link"`
	Desc    string    `xml:"description"`
	Items   []rssItem `xml:"item"`
}

type rssItem struct {
	Title   string `xml:"title"`
	Link    string `xml:"link"`
	Desc    string `xml:"description"`
	Guid    string `xml:"guid"`
	PubDate string `xml:"pubDate"`
}

type rss struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}

func (h *Handler) Feed(w http.ResponseWriter, r *http.Request) {
	// SEC-4 L2 DECEPTIVE shape: the feed is scoped to the caller's VERIFIED workspace set,
	// never the {wsID} URL param — otherwise any caller could read another workspace's
	// published feed by naming its id in the path.
	wsIDs := authz.WorkspaceIDs(r.Context())
	entries, err := h.store.GetPublicFeed(r.Context(), wsIDs, 50)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]rssItem, 0, len(entries))
	for _, e := range entries {
		pub := e.CreatedAt
		if e.PublishedAt != nil {
			pub = *e.PublishedAt
		}
		items = append(items, rssItem{
			Title:   e.Version + " — " + e.Title,
			Link:    "/changelog/" + e.ID,
			Desc:    e.Summary,
			Guid:    e.ID,
			PubDate: pub.Format(time.RFC1123Z),
		})
	}
	feed := rss{
		Version: "2.0",
		Channel: rssChannel{
			Title: "Changelog",
			Link:  "/changelog",
			Desc:  "Latest changes",
			Items: items,
		},
	}
	w.Header().Set("Content-Type", "application/rss+xml")
	w.Header().Set("X-Powered-By", "Talyvor Docs")
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(feed)
}
