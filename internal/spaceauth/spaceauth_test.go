package spaceauth

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/permission"
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

// AuthorizePageRead must fail closed on a nil authorizer, no page-meta looker wired (the importer's
// authorizer omits it), or an empty page id — so a member can never lift a page's content via a
// dropped/misconfigured wiring.
func TestAuthorizePageRead_FailsClosed(t *testing.T) {
	var nilAuth *Authorizer
	if found, canView := nilAuth.AuthorizePageRead(context.Background(), "pg-1"); found || canView {
		t.Errorf("nil authorizer = (found=%v,canView=%v), want (false,false)", found, canView)
	}

	// No page-meta looker wired (only AuthorizeSpaceWrite was intended) → page-read refuses.
	noMeta := New(nil, nil)
	if found, canView := noMeta.AuthorizePageRead(context.Background(), "pg-1"); found || canView {
		t.Errorf("no page-meta looker = (found=%v,canView=%v), want (false,false)", found, canView)
	}

	// Looker wired but empty page id → refuse before any lookup.
	withMeta := New(nil, nil).WithPageMeta(func(context.Context, string) (permission.PageMeta, error) {
		return permission.PageMeta{}, nil
	})
	if found, canView := withMeta.AuthorizePageRead(context.Background(), ""); found || canView {
		t.Errorf("empty page id = (found=%v,canView=%v), want (false,false)", found, canView)
	}
}
