package spaceauth

import (
	"context"
	"testing"
)

// The security-critical property the handler tests can't show (they always wire a real authorizer):
// a nil authorizer or an empty space id must FAIL CLOSED — Found=false and CanEdit=false — so a handler
// whose WithAccess wiring was dropped refuses (404) instead of creating a page.
func TestAuthorizeSpaceWrite_FailsClosed(t *testing.T) {
	var nilAuth *Authorizer
	if d := nilAuth.AuthorizeSpaceWrite(context.Background(), "sp-1"); d.Found || d.CanEdit {
		t.Errorf("nil authorizer = %+v, want Found=false CanEdit=false (fail-closed)", d)
	}

	// A real authorizer with an empty space id also refuses before any lookup.
	a := New(nil, nil)
	if d := a.AuthorizeSpaceWrite(context.Background(), ""); d.Found || d.CanEdit {
		t.Errorf("empty space_id = %+v, want Found=false CanEdit=false", d)
	}
}
