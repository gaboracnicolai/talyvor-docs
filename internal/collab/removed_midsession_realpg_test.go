package collab

// THE RE-RESOLVE PER `change` FRAME ASKS THE DATABASE FOR THE TIER AND THE REQUEST CONTEXT FOR THE
// MEMBERSHIP — SO A MEMBER REMOVED FROM THE WORKSPACE KEEPS EDITING THROUGH THE SOCKET THEY HOLD.
//
// #123 closed the TIME axis for one table. `dispatch` now re-resolves on every `change` frame
// instead of trusting the bool captured before the upgrade, and its own comment says why: "the
// connect-time answer is a statement about one instant, and a `permissions` row is revocable at any
// moment after it." That is true of `permissions`. It is equally true of `workspace_members`, and
// the re-resolve does NOT re-read that one:
//
//	PermissionSession.ResolveSession → pageMeta(ctx, pageID)                → authz.WorkspaceIDs(ctx)
//	                                 → authz.MemberIDForWorkspace(ctx, ws)  → ctx
//	                                 → perm.CheckPage(ctx, …, authz.WorkspaceIDs(ctx))
//
// Every one of those reads the membership set out of the CONTEXT, and the context is the one
// `ServeWS` captured from `r.Context()` — installed once by authz.Middleware, from one
// `MembershipsByEmail` query, at connect. The permission ROWS are re-read; the roster the query is
// SCOPED BY, and the actor id it resolves, are a snapshot. So the per-frame re-resolve is current
// truth about the grant and captured truth about the membership, and nothing in the code says so.
//
// THIS IS THE HALF THE #115 CENSUS NAMED AND DID NOT ENUMERATE — "mutable state OUTSIDE those two
// allow-lists is untouched by it: `permissions` rows (a grant is revocable), `workspace_members`
// (membership ends)". `permissions` was taken by #123. This is `workspace_members`.
//
// THE REMOVAL IS A SHIPPED, AUTOMATIC PATH, NOT A HAND-WRITTEN DELETE. `workspace_members` has
// exactly one production writer — membership.Store.ReconcileWorkspace, called only by the
// trackintegration syncer — and its prune is what happens when an administrator removes somebody in
// Track: `DELETE FROM workspace_members WHERE workspace_id = $1 AND source = 'track' AND email <>
// ALL($2)`. This test removes the member THROUGH that writer, with a roster read out of the
// database and one email dropped, which is exactly the roster Track would return. No test-only
// SQL touches the table.
//
// RED (the roster is the connect-time snapshot): the removed member's next `change` frame is ACKed,
// broadcast, and reaches pages.content through the autosaver — while a NEW connection from the same
// person is refused 404 by the same server, in the same test, milliseconds later. GREEN: the frame
// is refused with `change_rejected`, pages.content never sees it, and cursor + presence keep
// flowing (the same surgical shape the tier gate and #123 both hold).
//
// CONTROLS, because a green run has to be able to mean something:
//   - [PRE-REMOVAL]     the SAME socket edits the document for real BEFORE the removal, so a green
//     cannot be a socket that never worked.
//   - [REMOVAL-REAL]    authz's own PGResolver, asked fresh, returns ZERO memberships for that
//     email, so a green cannot be a prune that did not land.
//   - [FRONT-DOOR-SHUT] a NEW dial by the removed member is refused 404 by the shipped entry point.
//     This is the control that makes the finding a contradiction rather than an opinion: the same
//     server, at the same instant, refuses to open a session it is still serving.
//   - [STILL-LIVE]      cursor frames still reach the removed member, so the refusal is a gate and
//     not a torn-down connection.
//   - [SNAPSHOT-STILL-WRITES] the mutation proof, below: the identical scenario with a resolver
//     that answers from a FIXED roster — i.e. precisely the connect-time snapshot — persists the
//     change. So the refusal is the roster re-read and nothing else, and the write path is not
//     merely broken by being consulted twice.
//   - [ROSTER-RE-READ-PER-FRAME] the lookup count goes UP ACROSS the `change` frame, so the
//     refusal is a re-read attributable to that frame and not to some other connect.
//
// The defect itself is [HELD-SOCKET] (the frame is ACKed) and [PERSISTED] (it reaches
// pages.content); [FAIL-CLOSED] / [FAIL-CLOSED-ORDER] cover the three ways the re-read can fail.
// Every tag is exercised in both directions by ~/talyvor-queue/w31-rostersnapshot-controls-2e9f.py.
//
// ⚠ THE READ HALF IS UNCHANGED AND STILL UNDECIDED. A removed member's socket stays open and still
// receives other clients' `change` broadcasts, which carry the document body — measured for the
// revoked-grant case in TestSEC_Collab_RevokedMidSession_ReadHalf_Measured, and identical here. It
// needs a revalidation TIMER and therefore an interval, which is a threshold, which is a decision.
// This file closes the WRITE half only, and says so rather than implying more.
//
// COST, MEASURED NOT ASSUMED, since this runs per keystroke.
//
//	STATEMENTS — counted from Postgres's own log (log_statement=all, 200 frames bracketed by two
//	marker SELECTs, per-test database): 1202 query lines between the markers, i.e. exactly 6 per
//	frame — 200 workspace_members-by-email (this change), 200 page, 200 space, 200 space-scope
//	EXISTS, 400 permissions. #123 measured 5; this is the sixth. Production adds pagelock's 1 via
//	WithGuard, which the test env does not wire, so the shipped per-frame count goes 6 → 7.
//
//	LATENCY — 200 frames ack-to-ack, two pre-built test binaries run INTERLEAVED (the first
//	attempt ran the two arms in blocks and the numbers moved 5x with machine load; alternating
//	them removes the drift): 0.646–0.697 ms/frame with the re-read, 0.545–0.598 without, across
//	6 pairs. Every pair positive, delta +0.099 to +0.113 ms. Dockerised pg16 on macOS — a
//	pessimistic upper bound versus a same-host production database — and off the user's critical
//	path, since the editor does not block on the ack.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/membership"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/testutil"
)

const (
	preRemovalSentinel  = `{"type":"doc","tag":"BEFORE-REMOVAL-6c02"}`
	postRemovalSentinel = `{"type":"doc","tag":"AFTER-REMOVAL-b7f4"}`
)

// rosterWithout reads the workspace's REAL roster and returns it with one email dropped — the
// payload Track's service-members endpoint returns after an administrator removes that person.
func rosterWithout(t *testing.T, d *testutil.DB, ws, dropEmail string) []membership.MemberRef {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(),
		`SELECT email, role, member_id FROM workspace_members WHERE workspace_id=$1 AND email <> $2`,
		ws, dropEmail)
	if err != nil {
		t.Fatalf("rosterWithout: %v", err)
	}
	defer rows.Close()
	var out []membership.MemberRef
	for rows.Next() {
		var m membership.MemberRef
		if err := rows.Scan(&m.Email, &m.Role, &m.MemberID); err != nil {
			t.Fatalf("rosterWithout: scan: %v", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rosterWithout: rows: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("rosterWithout: empty roster — ReconcileWorkspace's empty-pull guard would no-op " +
			"and the removal below would not happen at all")
	}
	return out
}

// removeFromWorkspace drives the ONE production writer of workspace_members with a roster that no
// longer names the member, and asserts the prune actually deleted their row.
func removeFromWorkspace(t *testing.T, d *testutil.DB, ws, email string) {
	t.Helper()
	_, pruned, err := membership.NewStore(d.Pool).ReconcileWorkspace(
		context.Background(), ws, rosterWithout(t, d, ws, email))
	if err != nil {
		t.Fatalf("ReconcileWorkspace: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("ReconcileWorkspace pruned %d rows, want 1 — the removal did not happen, so "+
			"everything measured below is about nothing", pruned)
	}
}

// dialStatus dials as email and returns the HTTP status of a REFUSED upgrade. It is a fatal error
// for the upgrade to SUCCEED — this helper exists only to assert the front door is shut.
func dialStatus(t *testing.T, e *tierEnv, pageID, clientID, email string) int {
	t.Helper()
	u := "ws" + strings.TrimPrefix(e.url, "http") + "/v1/collab/" + pageID + "/ws?client_id=" + clientID
	hd := http.Header{}
	hd.Set("X-Gateway-Auth", tierSecret)
	hd.Set("X-User-Email", email)
	conn, resp, err := websocket.DefaultDialer.Dial(u, hd)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("[FRONT-DOOR-SHUT] the removed member opened a NEW collab session — the SEC-4 " +
			"membership gate at the entry point is not refusing them, so this test's premise " +
			"(the front door is shut while the held socket is not) is wrong")
	}
	if resp == nil {
		t.Fatalf("[FRONT-DOOR-SHUT] dial failed with no HTTP response: %v", err)
	}
	return resp.StatusCode
}

// countingResolver wraps a real authz.Resolver and counts the lookups, so the per-frame re-read is
// observable rather than inferred.
type countingResolver struct {
	inner authz.Resolver
	mu    sync.Mutex
	calls int
}

func (c *countingResolver) MembershipsByEmail(ctx context.Context, email string) ([]authz.Membership, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.MembershipsByEmail(ctx, email)
}

func (c *countingResolver) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// staleRoster answers every lookup with a FIXED membership set: the connect-time snapshot, frozen.
// It is the mutation proof — wiring it reproduces the shipped-before behaviour exactly.
type staleRoster struct{ ms []authz.Membership }

func (s staleRoster) MembershipsByEmail(context.Context, string) ([]authz.Membership, error) {
	return s.ms, nil
}

// THE MEASUREMENT.
func TestSEC_Collab_RemovedFromWorkspaceMidSession_ChangeIsRefused(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws, pageID, _, editor := tierSeed(t, d)
	roster := &countingResolver{inner: authz.NewPGResolver(d.Pool)}
	env := newTierEnv(t, d, NewPermissionSession(permission.NewStore(d.Pool), tierPageLooker(d), roster))

	econn := env.dial(t, pageID, "e-client", "editor@corp.com")
	readUntil(t, econn, "init")

	// [PRE-REMOVAL] while they are a member this socket edits the document for real.
	sendChange(t, econn, "e1", preRemovalSentinel, 0)
	readUntil(t, econn, "ack")
	env.saver.flush(ctx)
	if c := env.contentOf(t, pageID); !strings.Contains(c, "BEFORE-REMOVAL-6c02") {
		t.Fatalf("[PRE-REMOVAL] the member's change did not persist BEFORE the removal — the "+
			"pipeline is broken, so anything below is vacuous. content=%s", c)
	}

	// The removal, through the shipped syncer's own writer.
	removeFromWorkspace(t, d, ws, "editor@corp.com")

	// [REMOVAL-REAL] authz's own resolver, asked fresh, no longer knows them.
	ms, err := authz.NewPGResolver(d.Pool).MembershipsByEmail(ctx, "editor@corp.com")
	if err != nil {
		t.Fatalf("[REMOVAL-REAL] resolve: %v", err)
	}
	if len(ms) != 0 {
		t.Fatalf("[REMOVAL-REAL] the removed member still resolves to %d membership(s) — the prune "+
			"did not take", len(ms))
	}

	// [FRONT-DOOR-SHUT] the same server refuses to OPEN a session for them, right now.
	if code := dialStatus(t, env, pageID, "e-client-2", "editor@corp.com"); code != http.StatusNotFound {
		t.Fatalf("[FRONT-DOOR-SHUT] a new dial by the removed member returned %d, want 404", code)
	}

	// THE ASSERTION. Same socket, no reconnect — what an open browser tab is.
	beforeFrame := roster.count()
	sendChange(t, econn, "e2", postRemovalSentinel, 1)
	if m := readNext(t, econn); m["type"] != "change_rejected" {
		t.Errorf("[HELD-SOCKET] post-removal change response = %v, want change_rejected — somebody removed from "+
			"the workspace is still editing through the session they already held, while a new "+
			"session for the same person is refused 404", m["type"])
	}
	env.saver.flush(ctx)
	if c := env.contentOf(t, pageID); strings.Contains(c, "AFTER-REMOVAL-b7f4") {
		t.Errorf("[PERSISTED] the REMOVED member's change PERSISTED to pages.content. content=%s", c)
	}

	// [ROSTER-RE-READ-PER-FRAME] the FRAME caused a lookup. Counted across the frame rather than
	// against a fixed number: an earlier version asserted "more than one lookup in total" and was
	// satisfied by the second DIAL two lines up, so it stayed green with the per-frame re-resolve
	// deleted. A count that any connect can satisfy is not a per-frame guard.
	if n := roster.count(); n <= beforeFrame {
		t.Errorf("[ROSTER-RE-READ-PER-FRAME] the roster was NOT re-read for the `change` frame "+
			"(%d lookups before it, %d after) — the membership under this socket is still the "+
			"connect-time snapshot, so any refusal above came from something else", beforeFrame, n)
	}

	// [STILL-LIVE] the socket was not torn down. (The READ half — that this member still receives
	// the body in those broadcasts — is the undecided leak; see the file header.)
	oconn := env.dial(t, pageID, "o-client", "owner@corp.com")
	readUntil(t, oconn, "init")
	sendCursor(t, oconn)
	if m := readUntil(t, econn, "cursor"); m["type"] != "cursor" {
		t.Errorf("[STILL-LIVE] the removed member stopped receiving cursor frames — the change gate " +
			"must be surgical, not a disconnect")
	}
	_ = editor
}

// [SNAPSHOT-STILL-WRITES] the mutation proof. Identical scenario, one difference: the roster comes
// from a FROZEN set instead of the database — which is exactly what reading it out of the request
// context was. The change is ACCEPTED and PERSISTS. So the refusal above is the roster re-read, and
// the write path is not broken by being consulted twice.
func TestSEC_Collab_RemovedFromWorkspaceMidSession_SnapshotRosterStillWrites(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws, pageID, _, editor := tierSeed(t, d)
	frozen := staleRoster{ms: []authz.Membership{{WorkspaceID: ws, MemberID: editor, Role: "member"}}}
	env := newTierEnv(t, d, NewPermissionSession(permission.NewStore(d.Pool), tierPageLooker(d), frozen))

	econn := env.dial(t, pageID, "e-client", "editor@corp.com")
	readUntil(t, econn, "init")

	removeFromWorkspace(t, d, ws, "editor@corp.com")

	sendChange(t, econn, "e1", postRemovalSentinel, 0)
	readUntil(t, econn, "ack")
	env.saver.flush(ctx)
	if c := env.contentOf(t, pageID); !strings.Contains(c, "AFTER-REMOVAL-b7f4") {
		t.Errorf("[SNAPSHOT-STILL-WRITES] a FROZEN roster did NOT persist the change — the "+
			"membership re-read broke the write path, so the refusal in the test above proves "+
			"nothing. content=%s", c)
	}
}

// THE THREE WAYS THE RE-READ CAN FAIL, AND THAT ALL THREE REFUSE.
//
// A roster read that cannot answer must not fall through to the snapshot — falling through is
// precisely the behaviour this change removed, and it would come back as an error-handling
// "convenience". No database: these assert that the refusal happens BEFORE any other resolution, by
// giving the session a page-meta looker that RECORDS being called and stores that are nil (a
// permissive path would dereference them and panic, which is itself a red).
type errRoster struct{}

func (errRoster) MembershipsByEmail(context.Context, string) ([]authz.Membership, error) {
	return nil, errors.New("workspace_members unreadable")
}

func TestSEC_Collab_RosterReReadFailures_AreFailClosed(t *testing.T) {
	verified := authz.WithMemberships(context.Background(), "editor@corp.com",
		[]authz.Membership{{WorkspaceID: "ws-1", MemberID: "mbr-1", Role: "member"}})

	cases := []struct {
		name   string
		roster authz.Resolver
		ctx    context.Context
	}{
		// A lookup error. The snapshot in this context is a PERFECTLY GOOD one — that is the point:
		// the old code would have served from it.
		{"lookup error", errRoster{}, verified},
		// No resolver wired at all. The constructor takes it positionally so this cannot happen by
		// omission, only by writing nil — and nil refuses rather than reverting to the snapshot.
		{"no resolver", nil, verified},
		// No verified identity to re-read FOR. authz.Middleware cannot produce this, but a future
		// caller that builds its own context can, and it must not resolve to a permissive default.
		{"no verified email", staleRoster{}, context.Background()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			looker := func(context.Context, string) (permission.PageMeta, error) {
				called = true
				return permission.PageMeta{}, nil
			}
			s := NewPermissionSession(nil, looker, tc.roster)
			inScope, actor, canEdit := s.ResolveSession(tc.ctx, "page-1")
			if inScope || actor != "" || canEdit {
				t.Errorf("[FAIL-CLOSED] ResolveSession = (%v, %q, %v), want (false, \"\", false) — a roster that "+
					"cannot be re-read must refuse, not serve the connect-time snapshot",
					inScope, actor, canEdit)
			}
			if called {
				t.Errorf("[FAIL-CLOSED-ORDER] the page-meta looker ran anyway — the refusal must come BEFORE any " +
					"resolution scoped by a roster we could not confirm")
			}
		})
	}
}
