package permission_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/permission"
)

// A nil *Enforcer must FAIL CLOSED — deny with 404 (the no-oracle convention), never pass through
// to the handler. Require was pass-through on a nil receiver, so a dropped WithAccess line silently
// unguarded every route it wrapped. collab's WithPageScope proves the right shape: default deny.
//
// RED against the old pass-through: the wrapped handler runs and returns 200. GREEN once Require
// denies on nil.
func TestEnforcer_NilReceiver_FailsClosed(t *testing.T) {
	var e *permission.Enforcer // nil — the "WithAccess never called" state

	reached := false
	mw := e.Require(permission.AccessView)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/anything", nil))

	if reached {
		t.Error("nil Enforcer.Require passed the request THROUGH to the handler — must deny (fail-closed)")
	}
	if rr.Code != http.StatusNotFound {
		t.Errorf("nil Enforcer.Require = %d, want 404 (no-oracle deny, matching RequireAccess)", rr.Code)
	}
}
