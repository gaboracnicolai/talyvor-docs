package collab

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

// baseUpgrader carries the buffer sizes; CheckOrigin is installed per-Handler by
// originChecker so the policy is configurable rather than compiled in.
var baseUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

// originChecker builds the CheckOrigin policy from an allow-list.
//
// This replaces an unconditional `return true` whose comment claimed "production deployments
// tighten this via env" — no such env existed anywhere in the repo, so the claim was false
// and the policy was accept-every-origin in every deployment. Because the edge gateway
// injects x-gateway-auth on requests it has authenticated (from a session cookie), a
// cross-site page could open a socket that the gateway stamps with the victim's identity —
// classic CSWSH, giving the attacker the document stream and, at edit tier, mutation.
//
// Policy, secure by default:
//   - No Origin header ⇒ allow. Non-browser clients (the Go test clients, CLI tools) send
//     none, and a request with no Origin cannot be a cross-site browser request.
//   - allowed empty ⇒ SAME-ORIGIN only: Origin host must equal the request Host. This is
//     gorilla's own default and the right behaviour behind a reverse proxy, where the SPA is
//     served from the same hostname as the API.
//   - allowed non-empty ⇒ the Origin must match one entry exactly (scheme+host+port). This
//     is what a split-origin dev setup needs (SPA on :5174, API on :4000).
func originChecker(allowed []string) func(*http.Request) bool {
	set := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		if o = strings.TrimSpace(o); o != "" {
			set[strings.ToLower(strings.TrimRight(o, "/"))] = true
		}
	}
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		if len(set) == 0 {
			u, err := url.Parse(origin)
			if err != nil {
				return false
			}
			return strings.EqualFold(u.Host, r.Host)
		}
		return set[strings.ToLower(strings.TrimRight(origin, "/"))]
	}
}

const (
	// pingInterval is how often the server sends a Ping frame to keep
	// idle connections alive through middleboxes.
	pingInterval = 30 * time.Second
	// readWait caps how long a read can block before we assume the
	// peer is dead and tear down.
	readWait = 60 * time.Second
	// writeWait bounds per-message write timeouts.
	writeWait = 10 * time.Second
)

// LockGuard is the narrow lock-check the collab handler delegates
// to before persisting any inbound change. internal/pagelock
// satisfies this; tests can stub it.
type LockGuard interface {
	CanEdit(ctx context.Context, pageID, memberID string, isAdmin bool) (bool, string, error)
}

type Handler struct {
	engine   *OTEngine
	guard    LockGuard
	access   SessionResolver
	upgrader websocket.Upgrader
}

func NewHandler(engine *OTEngine) *Handler {
	h := &Handler{engine: engine}
	// Default to same-origin. A handler built without WithAllowedOrigins is therefore
	// restrictive, not permissive — the previous package-level upgrader defaulted open.
	h.upgrader = baseUpgrader
	h.upgrader.CheckOrigin = originChecker(nil)
	return h
}

// WithAllowedOrigins sets the WebSocket Origin allow-list (DOCS_ALLOWED_ORIGINS). Empty or
// unset keeps the same-origin default, which is correct for a reverse-proxied deployment
// where the SPA and API share a hostname. A split-origin dev setup lists its SPA origin.
func (h *Handler) WithAllowedOrigins(origins []string) *Handler {
	h.upgrader.CheckOrigin = originChecker(origins)
	return h
}

// WithAccess attaches the SEC-4 session resolver: the membership scope gate (404 for a page outside
// the caller's workspaces), the verified actor, and the edit-tier decision that gates `change` frames.
// REQUIRED for a gated deployment: with no resolver wired, ServeWS fails closed (rejects every session).
func (h *Handler) WithAccess(s SessionResolver) *Handler {
	h.access = s
	return h
}

// WithGuard attaches the lock-aware guard. When set, the WebSocket
// loop drops "change" frames from clients that can't edit (a
// foreign lock or approved doc_status). Cursor + presence frames
// still flow so viewers can see what's happening.
func (h *Handler) WithGuard(g LockGuard) *Handler {
	h.guard = g
	return h
}

// ServeWS upgrades the connection and runs the read+write pumps for
// one client. Each pump owns one goroutine; the engine sees an
// abstract send channel and never touches the WebSocket directly.
//
// SEC-4: this route lives inside the /v1 boundary, so gatewayauth (transit proof) and authz
// (membership) have already run. The caller's member id comes from the VERIFIED gateway
// context, never a ?member_id= query param (retired, like the REST X-Member-Id), and the page
// MUST be in the caller's workspace membership or the session is refused BEFORE the upgrade.
//
// URL pattern: /v1/collab/{pageID}/ws?client_id=&member_name=
func (h *Handler) ServeWS(w http.ResponseWriter, r *http.Request) {
	pageID := chi.URLParam(r, "pageID")
	q := r.URL.Query()
	clientID := q.Get("client_id")
	memberName := q.Get("member_name") // display label only — not an identity
	if pageID == "" || clientID == "" {
		http.Error(w, `{"error":"page_id and client_id required"}`, http.StatusBadRequest)
		return
	}

	// SEC-4 gate, resolved from the VERIFIED gateway context BEFORE the upgrade so a reject is a
	// clean HTTP status, not a half-open socket. The resolver yields all three at once:
	//   - inScope: the page is in the caller's workspaces (else 404 — no live channel into a page
	//     they can't see); fail-closed if no resolver is wired.
	//   - actor: the caller's member id IN THE PAGE'S workspace (authz.MemberIDForWorkspace), correct
	//     for ANY membership count — this REPLACES authz.ActorOrEmpty, which returned "" for any
	//     caller with != 1 memberships (the empty-actor residue: unlabelled presence + CanEdit not
	//     matching a multi-workspace member to their own lock). Nothing client-supplied names the
	//     actor; ?member_id= is ignored.
	//   - canEdit: whether the caller holds the edit tier — gates `change` frames below.
	inScope, actor, canEdit := false, "", false
	if h.access != nil {
		inScope, actor, canEdit = h.access.ResolveSession(r.Context(), pageID)
	}
	if !inScope {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("collab: upgrade failed", slog.String("err", err.Error()))
		return
	}
	client, err := h.engine.Join(pageID, clientID, actor, memberName)
	if err != nil {
		_ = conn.Close()
		return
	}

	// Two goroutines per session: one writes the engine's outbound
	// queue to the socket, the other reads inbound frames and
	// dispatches them into the engine. The pair exits on the first
	// io error. canEdit is resolved once at connect and gates every
	// `change` frame; cursor + presence flow regardless (read-only
	// members still see live collaboration).
	ctx, cancel := context.WithCancel(r.Context())
	go h.writePump(ctx, conn, client, cancel)
	h.readPump(ctx, conn, pageID, client, canEdit, cancel)

	h.engine.Leave(pageID, clientID)
	_ = conn.Close()
}

func (h *Handler) writePump(ctx context.Context, conn *websocket.Conn, c *CollabClient, cancel context.CancelFunc) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-c.Send():
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (h *Handler) readPump(ctx context.Context, conn *websocket.Conn, pageID string, c *CollabClient, canEdit bool, cancel context.CancelFunc) {
	defer cancel()
	_ = conn.SetReadDeadline(time.Now().Add(readWait))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(readWait))
		return nil
	})
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(readWait))
		if !h.dispatch(ctx, pageID, c, canEdit, raw) {
			return
		}
	}
}

// dispatch routes one inbound JSON frame into the engine. Returns
// false to signal the read pump to tear down (e.g. an unrecoverable
// protocol error); true to continue.
func (h *Handler) dispatch(ctx context.Context, pageID string, c *CollabClient, canEdit bool, raw []byte) bool {
	var env struct {
		Type   string          `json:"type"`
		Change *Change         `json:"change,omitempty"`
		Cursor *CursorPos      `json:"cursor,omitempty"`
		Extras json.RawMessage `json:"-"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return true
	}
	switch env.Type {
	case "change":
		if env.Change == nil {
			return true
		}
		// TIER gate. canEdit was resolved once at connect (SessionResolver): the caller holds
		// >= AccessEdit on this page. A view-only member may stay connected for cursor + presence
		// but must not mutate — refuse the change at the boundary so the engine never sees it, and
		// the autosaver never persists it. Fail-closed: canEdit is false unless the tier resolved to
		// edit, so a broken/absent resolver denies. Same non-disconnecting shape as the lock guard.
		if !canEdit {
			rejected, _ := json.Marshal(map[string]any{
				"type":   "change_rejected",
				"reason": "you do not have edit access to this page",
			})
			trySend(c.send, rejected)
			return true
		}
		// Lock guard. A foreign lock or approved doc_status blocks
		// the change at the WebSocket boundary so the engine never
		// even sees it. We don't disconnect the client — they may
		// still want to read + watch presence.
		if h.guard != nil {
			ok, reason, gErr := h.guard.CanEdit(ctx, pageID, c.MemberID, false)
			if gErr == nil && !ok {
				rejected, _ := json.Marshal(map[string]any{
					"type":   "change_rejected",
					"reason": reason,
				})
				trySend(c.send, rejected)
				return true
			}
		}
		env.Change.ClientID = c.ID
		env.Change.PageID = pageID
		out, err := h.engine.Apply(pageID, *env.Change)
		if err != nil || len(out) == 0 {
			return true
		}
		applied := out[0]
		// Acknowledge to the sender so it can advance its local
		// base version + drop the change from its pending queue.
		ack, _ := json.Marshal(map[string]any{
			"type":    "ack",
			"id":      applied.ID,
			"version": applied.Version,
		})
		trySend(c.send, ack)
		// Fan out to everyone else.
		h.engine.BroadcastChange(pageID, applied, c.ID)
	case "cursor":
		if env.Cursor != nil {
			h.engine.UpdateCursor(pageID, c.ID, *env.Cursor)
		}
	case "ping":
		pong, _ := json.Marshal(map[string]string{"type": "pong"})
		trySend(c.send, pong)
	}
	return true
}
