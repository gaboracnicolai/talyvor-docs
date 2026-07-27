package trackintegration

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// service_handler.go — POST /v1/service/workspaces/{wsID}/member-sync.
//
// WHAT IT CLOSES. Membership arrives in Docs only when the periodic sweep runs, so a brand-new
// identity — Track has just minted their workspace, the BFF has just put them in a session — had no
// membership row until the next tick. Every Docs action 403s until then, and the first thing a new
// tester does is the thing that fails. This lets the BFF reconcile THAT ONE workspace at login, the
// same moment it bootstraps Track.
//
// AUTH IS THE EXISTING SCHEME, NOT A SECOND ONE. This route is mounted inside the /v1 group, so
// gatewayauth.Middleware has already proved transit (x-gateway-auth) before the handler runs — the
// same boundary every other /v1 route sits behind. A browser cannot reach it: the secret is
// server-side only. Nothing here reads an identity header, so there is no trusted-identity question
// to get wrong.
//
// WHY A CALLER CANNOT RECONCILE A WORKSPACE THEY DO NOT BELONG TO: the operation does not grant
// anything. It asks TRACK — the tenancy authority — who belongs to wsID, and writes exactly that
// answer. An id the caller has no business with returns Track's roster for it, which does not
// include the caller, so the caller gains nothing; a nonexistent id returns an empty roster and
// writes nothing. The BFF, the only thing holding the secret, sends only the session's own
// workspace.
//
// IDEMPOTENT: a full-pull upsert. A retry, or two logins racing, costs a round-trip and changes
// nothing — which is what makes best-effort calling safe on the BFF side.

// MountService registers the service route on r. Call it INSIDE the gatewayauth-protected group.
func (s *Syncer) MountService(r chi.Router) {
	r.Post("/service/workspaces/{wsID}/member-sync", s.handleMemberSync)
}

func (s *Syncer) handleMemberSync(w http.ResponseWriter, r *http.Request) {
	// NOT a store scope. This wsID is not used to scope a read or to decide what the caller may
	// see; it names WHICH workspace to re-pull from TRACK, the tenancy authority, and the roster
	// Track returns is written verbatim. The operation therefore GRANTS NOTHING: an id the caller
	// has no business with yields a roster that does not include them, and a nonexistent id yields
	// an empty roster and writes nothing. There is also no membership to authorize against —
	// reconciling membership is precisely what runs BEFORE any membership exists, which is the
	// deadlock this route closes. Reachable only behind gatewayauth (server-side secret), so the
	// caller is the BFF, which sends only the session's own workspace.
	// nosemgrep: docs-no-url-param-workspace-scope -- reconcile target, not a scope; see above.
	wsID := chi.URLParam(r, "wsID")
	if wsID == "" {
		http.Error(w, `{"error":"workspace id required"}`, http.StatusBadRequest)
		return
	}
	if !s.memberSyncOn() {
		// Member sync is not configured on this deployment. Say so rather than reporting a
		// success that reconciled nothing — a caller that believes membership landed will
		// diagnose the wrong thing when the next request 403s.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "member sync is not configured on this Docs deployment",
		})
		return
	}
	if err := s.SyncOneWorkspace(r.Context(), wsID); err != nil {
		// The detail is already logged with the workspace id; the body stays generic.
		slog.Warn("trackintegration: on-demand member sync failed", slog.String("workspace_id", wsID))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "member sync failed"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"workspace_id": wsID, "synced": true})
}
