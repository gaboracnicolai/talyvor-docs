package trackintegration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/membership"
)

// A FULL PAGE IS NOT A ROSTER, AND THE NEXT THING THAT HAPPENS TO IT IS A DELETE.
//
// ⚠ THE MECHANISM, MEASURED ACROSS TWO REPOSITORIES. This client asks Track for a workspace's
// roster with `limit=500` and treats what comes back as the complete membership. Track's
// `internal/member/handler.go` (read at talyvor-track 7ad05ce3) has `maxLimit = 500` with the
// comment "hard cap — the roster read can never return more per page", and answers 200 with no
// "there is more" signal of any kind. So at 500 members the two numbers meet and the response is
// indistinguishable from a complete roster of exactly 500.
//
// What happens to it next is `membership.Store.ReconcileWorkspace`, whose second statement is:
//
//	DELETE FROM workspace_members
//	WHERE workspace_id = $1 AND source = 'track' AND email <> ALL($2::text[])
//
// — every Track-sourced membership not in the pulled set. **A workspace with more than 500 Track
// members would therefore have every membership past the first page deleted on each sync**, and
// the next sync would delete the next lot, silently, with a 200 at every step.
//
// ⚠⚠ THE ADJACENT BOUNDARY WAS ALREADY DEFENDED AND THIS ONE WAS NOT. ReconcileWorkspace opens with
// `if len(refs) == 0 { return }` — "empty-pull safety — never prune a roster to zero". The empty
// page was recognised as unsafe evidence; the FULL page, which is the same kind of unsafe evidence,
// was not. This client's own docstring even names the gap — "rosters are assumed < 500; a workspace
// exceeding that would need offset pagination (deferred, flagged)" — but nothing DETECTS the
// condition, so the flag has never been raised by anything.
//
// ⚠ THE REPAIR REUSES A DOCUMENTED PROPERTY RATHER THAN INVENTING ONE. `SyncOneWorkspace` already
// treats an error from this call as "skip this workspace, leave the existing roster intact" — its
// comment says "a pull that failed is not evidence that nobody is a member". A truncated pull is
// not evidence that nobody is a member either, so it returns an error and takes the same path.

// fullPage builds a roster of exactly n members, as Track would serialise it.
func fullPage(n int) []membership.MemberRef {
	out := make([]membership.MemberRef, n)
	for i := range out {
		out[i] = membership.MemberRef{
			MemberID: "m" + strconv.Itoa(i),
			Email:    fmt.Sprintf("user%d@example.com", i),
			Role:     "member",
		}
	}
	return out
}

func rosterServer(t *testing.T, n int) *Client {
	t.Helper()
	srv := httpFixture(t, map[string]http.HandlerFunc{
		"/v1/service/members": func(w http.ResponseWriter, r *http.Request) {
			// The server caps at whatever the caller asked for, exactly as Track's pageParams does.
			lim := 500
			if v := r.URL.Query().Get("limit"); v != "" {
				if p, err := strconv.Atoi(v); err == nil && p > 0 {
					lim = p
				}
			}
			body := fullPage(n)
			if len(body) > lim {
				body = body[:lim]
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
		},
	})
	return New(srv.URL, "k").WithMemberSyncSecret("s")
}

func TestGetWorkspaceMembers_RefusesAFullPage(t *testing.T) {
	// A roster that fills the page exactly cannot be distinguished from a truncated one, and the
	// caller's next act is a DELETE of everything absent from it.
	_, err := rosterServer(t, memberPageLimit).GetWorkspaceMembers(context.Background(), "ws-1")
	if err == nil {
		t.Fatalf("a full page (%d members) was accepted as a complete roster — "+
			"ReconcileWorkspace would then DELETE every Track-sourced membership beyond it",
			memberPageLimit)
	}
	if !strings.Contains(err.Error(), "truncat") {
		t.Errorf("the error must say WHY it refused, so an operator can act on it; got %q", err)
	}
}

func TestGetWorkspaceMembers_AcceptsAShortPage(t *testing.T) {
	// ⚠ THE OTHER DIRECTION, AND IT IS NOT DECORATION: a refusal that fired on every pull would
	// stop member sync entirely and would look exactly like a working guard until someone noticed
	// nobody was being synced.
	got, err := rosterServer(t, memberPageLimit-1).GetWorkspaceMembers(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("a short page is a complete roster and must be accepted: %v", err)
	}
	if len(got) != memberPageLimit-1 {
		t.Fatalf("got %d members, want %d", len(got), memberPageLimit-1)
	}
}

func TestGetWorkspaceMembers_AcceptsAnEmptyRoster(t *testing.T) {
	// The already-defended boundary, pinned here so a fix to the full-page one cannot break it:
	// an empty roster is ([], nil), and ReconcileWorkspace's own len(refs)==0 guard skips pruning.
	got, err := rosterServer(t, 0).GetWorkspaceMembers(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("an empty roster is a legitimate answer, not an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d members, want 0", len(got))
	}
}

// TestMemberPageLimit_IsTheValueOnTheWire keeps the check and the request on ONE number.
//
// ⚠ A SECOND SOURCE OF TRUTH HERE WOULD BE THE SAME DEFECT ONE LEVEL UP: if the query said 500 and
// the guard compared against 250, every pull of 250+ would be refused; if the query said 1000 and
// the guard compared against 500, a real truncation at Track's cap would sail through. The request
// is built from memberPageLimit and so is the comparison — this asserts the constant reaches the
// wire rather than trusting that it does.
func TestMemberPageLimit_IsTheValueOnTheWire(t *testing.T) {
	var seen string
	srv := httpFixture(t, map[string]http.HandlerFunc{
		"/v1/service/members": func(w http.ResponseWriter, r *http.Request) {
			seen = r.URL.Query().Get("limit")
			_ = json.NewEncoder(w).Encode([]membership.MemberRef{})
		},
	})
	c := New(srv.URL, "k").WithMemberSyncSecret("s")
	if _, err := c.GetWorkspaceMembers(context.Background(), "ws-1"); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if seen != strconv.Itoa(memberPageLimit) {
		t.Fatalf("the wire carried limit=%q but the guard compares against %d", seen, memberPageLimit)
	}
}
