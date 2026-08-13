package collab

// THE TIME AXIS ON A COLLAB SESSION: THE GATE RAN AT CONNECT, THE SERVING RUNS FOR AS LONG AS THE
// SOCKET LIVES.
//
// `ServeWS` resolves (inScope, actor, canEdit) ONCE, before the upgrade, and hands `canEdit` to
// `readPump` as a plain bool that gates every subsequent `change` frame. handler.go says so in its
// own words — "canEdit is resolved once at connect and gates every `change` frame" — as a
// description of the design, with nothing said about what happens when the grant behind it is
// withdrawn. Nothing re-reads the permission rows for the life of the connection, and a connection
// has no lifetime bound: `readWait` is reset by every Pong, and `writePump` pings every 30s, so an
// idle tab holds its session open indefinitely.
//
// This is the same shape as #114/#115 one table over — the thing authorized and the thing served
// are separated, there by SCOPE and here by TIME — and it is the half the #115 census explicitly
// did not enumerate: "a decision captured while a GRANT was live, served after it was revoked."
// `permissions` rows are revocable (permission.Store.Revoke is a shipped route), so the premise
// `canEdit` captured is one an administrator can withdraw at any moment.
//
// TWO HALVES, MEASURED SEPARATELY BECAUSE THEY FAIL DIFFERENTLY:
//
//	(A) WRITE. Revoke the edit grant of a member who is mid-session. Their next `change` frame is
//	    still applied, still broadcast, and still PERSISTED to pages.content by the autosaver.
//	(B) READ. Revoke a member's access ENTIRELY (a private space, so no grant = AccessNone). Their
//	    socket keeps receiving every other client's `change` broadcast — and a `change` carries the
//	    full `snapshot`, i.e. the document body. Revoking access does not stop the document being
//	    delivered.
//
// RED (canEdit captured at connect, never re-read): (A)'s post-revoke change is ACKed and the
// sentinel reaches pages.content. GREEN: it is refused with `change_rejected` and pages.content
// never sees it, while cursor + presence keep flowing on the same socket — the surgical shape the
// tier gate already holds for a view-only member.
//
// The controls are what make a green run mean something:
//   - [PRE-REVOKE] the SAME socket's change is accepted and persists BEFORE the revoke, so a green
//     (A) cannot be a socket that was never working.
//   - [REVOKE-REAL] a direct CheckPage after the revoke resolves to AccessView, so a green (A)
//     cannot be a revoke that did not land.
//   - [STILL-LIVE] cursor + presence still reach the revoked member afterwards, so a green (A)
//     cannot be a torn-down connection.
//   - [RE-RESOLVE-IS-THE-GATE] a stub that answers yes ONCE and no afterwards refuses the change
//     AND records >1 call, so the refusal is the re-resolve and not something incidental. Its
//     opposite (a stub that keeps saying yes) still persists, so the write path was not simply
//     broken by being consulted twice. Neither control needs the database.
//
// ⚠ THE COST WAS MEASURED, NOT ASSUMED, BECAUSE THIS RUNS PER KEYSTROKE. `sendChange` is NOT
// debounced: Editor.tsx's handleChange calls it on every editor change event and the 2000ms timer
// beside it debounces only the REST autosave. So the re-resolve is per keystroke, per editor.
//
//	QUERIES — counted from Postgres's own statement log (log_statement=all, per-test database,
//	21 frames + 1 connect = 22 resolves): 22 page SELECT + 22 space SELECT + 22 space-scope
//	SELECT EXISTS + 44 permissions SELECT = 110, i.e. exactly 5 statements per frame. (Counting
//	the call chain by eye said 4 — GetByIDInWorkspaces' scope check is the fifth. Measured.)
//	Production already ran 1 per frame here: pagelock.Store.read, one SELECT, via WithGuard —
//	which is wired alongside WithAccess in cmd/docs/main.go. So the per-frame statement count
//	goes 1 → 6, and the shape (a DB round trip per change frame) is one this path already had.
//
//	LATENCY — 300 frames, ack-to-ack, same harness, Dockerised pg16 on macOS (a pessimistic
//	upper bound versus a same-host production database): 0.089 ms/frame with a no-DB resolver,
//	0.597 ms/frame with the real permission chain. +0.5 ms of server-side work per keystroke,
//	off the user's critical path — the editor does not block on the ack.
//
// Whoever wants that back: the four statements behind pageMeta are the expensive half and are
// nearly static — but only NEARLY, because spaces.private is exactly the mutable field #115 was
// about, so caching it re-opens the hole one table over. Do not cache it without re-reading #115.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

const (
	preRevokeSentinel  = `{"type":"doc","tag":"BEFORE-REVOKE-4a11"}`
	postRevokeSentinel = `{"type":"doc","tag":"AFTER-REVOKE-91c7"}`
	ownerSentinel      = `{"type":"doc","tag":"OWNER-BODY-3d5e"}`
)

// (A) WRITE HALF — a revoked editor keeps editing through the socket they already hold.
func TestSEC_Collab_RevokedMidSession_ChangeIsRefused(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	_, pageID, _, editor := tierSeed(t, d)
	perms := permission.NewStore(d.Pool)
	env := newTierEnv(t, d, NewPermissionSession(perms, tierPageLooker(d)))

	econn := env.dial(t, pageID, "e-client", "editor@corp.com")
	readUntil(t, econn, "init")

	// [PRE-REVOKE] control: while the grant is live this socket edits the document for real.
	sendChange(t, econn, "e1", preRevokeSentinel, 0)
	readUntil(t, econn, "ack")
	env.saver.flush(ctx)
	if c := env.contentOf(t, pageID); !strings.Contains(c, "BEFORE-REVOKE-4a11") {
		t.Fatalf("[PRE-REVOKE] the editor's change did not persist BEFORE the revoke — the pipeline "+
			"is broken, so anything this test says afterwards is vacuous. content=%s", c)
	}

	// The withdrawal. The space is PUBLIC, so with the page grant gone the member falls to the
	// workspace-member default of AccessView — edit is genuinely lost, view is genuinely kept.
	if err := perms.Revoke(ctx, permission.ResourcePage, pageID, "member", editor); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// [REVOKE-REAL] control: the rule engine, asked fresh, now says view. A green assertion below
	// cannot be a revoke that silently did nothing.
	if lvl := resolveTierNow(t, d, editor, pageID); lvl != permission.AccessView {
		t.Fatalf("[REVOKE-REAL] after Revoke the tier resolves to %q, want %q — the revoke did not "+
			"take, so the measurement below is about nothing", lvl, permission.AccessView)
	}

	// THE MEASUREMENT. Same socket, no reconnect — exactly what an open browser tab is.
	sendChange(t, econn, "e2", postRevokeSentinel, 1)
	if m := readNext(t, econn); m["type"] != "change_rejected" {
		t.Errorf("(A) post-revoke change response = %v, want change_rejected — a member whose edit "+
			"grant was withdrawn is still editing through the session they already held", m["type"])
	}
	env.saver.flush(ctx)
	if c := env.contentOf(t, pageID); strings.Contains(c, "AFTER-REVOKE-91c7") {
		t.Errorf("(A) the REVOKED member's change PERSISTED to pages.content. content=%s", c)
	}

	// [STILL-LIVE] control: the socket was not torn down — cursor still flows to the revoked
	// member, so (A) green is a refused frame and not a dead connection.
	oconn := env.dial(t, pageID, "o-client", "owner@corp.com")
	readUntil(t, oconn, "init")
	sendCursor(t, oconn)
	if m := readUntil(t, econn, "cursor"); m["type"] != "cursor" {
		t.Errorf("[STILL-LIVE] the revoked member stopped receiving cursor frames — the change gate " +
			"must be surgical, not a disconnect")
	}
}

// (B) READ HALF — a member with NO access left keeps receiving the document body.
//
// Not asserted as a defect here: it is MEASURED and reported, and this test PASSES on the leak. The
// write half had a per-frame hook to hang a re-check on — an inbound frame — and the numbers above
// say what using it costs. The read half has no inbound frame: the revoked member is receiving, not
// sending, so closing it means revalidating on a TIMER and picking the interval, i.e. choosing how
// many seconds a revoked member may keep reading. That is a threshold, and this session may not
// pick one. FOR NICOLAI — the three candidate answers and what each costs are in the queue entry.
//
// ⚠ IF THIS TEST EVER FAILS, THE LEAK WAS CLOSED AND THIS FILE IS STALE: delete this test and the
// paragraph above rather than "fixing" the assertion.
func TestSEC_Collab_RevokedMidSession_ReadHalf_Measured(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws, pageID, owner, member := privateSeed(t, d)
	_ = ws
	perms := permission.NewStore(d.Pool)
	env := newTierEnv(t, d, NewPermissionSession(perms, tierPageLooker(d)))

	mconn := env.dial(t, pageID, "m-client", "member@corp.com")
	readUntil(t, mconn, "init")
	oconn := env.dial(t, pageID, "o-client", "owner@corp.com")
	readUntil(t, oconn, "init")

	// Withdraw the member's ONLY grant. The space is private, so nothing backstops it.
	if err := perms.Revoke(ctx, permission.ResourcePage, pageID, "member", member); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if lvl := resolveTierNow(t, d, member, pageID); lvl != permission.AccessNone {
		t.Fatalf("[REVOKE-REAL] after Revoke the tier resolves to %q, want %q", lvl, permission.AccessNone)
	}
	_ = owner

	// The owner types. Does the member who may no longer read this page receive the body?
	sendChange(t, oconn, "o1", ownerSentinel, 0)
	readUntil(t, oconn, "ack")

	got := readUntil(t, mconn, "change")
	ch, _ := got["change"].(map[string]any)
	snap, _ := ch["snapshot"].(string)
	if strings.Contains(snap, "OWNER-BODY-3d5e") {
		t.Logf("MEASURED: a member whose access was revoked mid-session still received the "+
			"document body over the socket they already held. snapshot=%s", snap)
	} else {
		t.Errorf("expected to MEASURE the read-half leak (the broadcast carrying the body); got "+
			"snapshot=%q — if this stops being true the comment above is stale", snap)
	}
}

// flipResolver answers canEdit=true on the FIRST call and false on every call after it, and counts
// the calls. It is the MUTATION PROOF for the re-resolve, and it needs no database: the first call
// is the one ServeWS makes before the upgrade, so
//
//	dispatch re-resolves      → the 2nd call answers false → the change is REFUSED, calls > 1
//	dispatch trusts the bool  → there is no 2nd call       → the change is APPLIED, calls == 1
//
// so the two implementations are distinguishable by this stub alone, in both directions.
type flipResolver struct {
	mu    sync.Mutex
	calls int
	actor string
}

func (f *flipResolver) ResolveSession(context.Context, string) (bool, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return true, f.actor, f.calls == 1
}

func (f *flipResolver) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// [RE-RESOLVE-IS-THE-GATE] the change is refused ONLY because dispatch asked again.
func TestSEC_Collab_RevokedMidSession_ReResolveIsWhatRefuses(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	_, pageID, _, editor := tierSeed(t, d)
	fr := &flipResolver{actor: editor}
	env := newTierEnv(t, d, fr)

	econn := env.dial(t, pageID, "e-client", "editor@corp.com")
	readUntil(t, econn, "init")
	if n := fr.count(); n != 1 {
		t.Fatalf("expected exactly 1 resolve at connect, got %d — the stub's premise is wrong", n)
	}

	sendChange(t, econn, "e1", postRevokeSentinel, 0)
	if m := readNext(t, econn); m["type"] != "change_rejected" {
		t.Errorf("[RE-RESOLVE-IS-THE-GATE] response = %v, want change_rejected", m["type"])
	}
	if n := fr.count(); n < 2 {
		t.Errorf("[RE-RESOLVE-IS-THE-GATE] resolver called %d time(s) — dispatch did NOT ask again, "+
			"so any refusal above came from something other than the re-resolve", n)
	}
	env.saver.flush(ctx)
	if c := env.contentOf(t, pageID); strings.Contains(c, "AFTER-REVOKE-91c7") {
		t.Errorf("[RE-RESOLVE-IS-THE-GATE] the change persisted anyway. content=%s", c)
	}
}

// The other direction: a resolver that keeps saying yes lets the SAME frame through, so the refusal
// above is the resolver's answer and not a pipeline that stopped working when it was consulted twice.
func TestSEC_Collab_RevokedMidSession_StillYesStillPersists(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	_, pageID, _, editor := tierSeed(t, d)
	env := newTierEnv(t, d, stubResolver{inScope: true, actor: editor, canEdit: true})

	econn := env.dial(t, pageID, "e-client", "editor@corp.com")
	readUntil(t, econn, "init")
	sendChange(t, econn, "e1", postRevokeSentinel, 0)
	readUntil(t, econn, "ack")
	env.saver.flush(ctx)
	if c := env.contentOf(t, pageID); !strings.Contains(c, "AFTER-REVOKE-91c7") {
		t.Errorf("a resolver that still says yes did NOT persist the change — the re-resolve broke "+
			"the write path, so the refusals above prove nothing. content=%s", c)
	}
}

// resolveTierNow asks the same rule engine the resolver uses, with a freshly built context, what
// the member's tier on the page is RIGHT NOW. Used as the [REVOKE-REAL] control.
func resolveTierNow(t *testing.T, d *testutil.DB, memberID, pageID string) permission.AccessLevel {
	t.Helper()
	ctx := context.Background()
	pg, err := page.NewStore(d.Pool).GetByID(ctx, pageID)
	if err != nil {
		t.Fatalf("resolveTierNow: page: %v", err)
	}
	sp, err := space.NewStore(d.Pool).GetByID(ctx, pg.SpaceID)
	if err != nil {
		t.Fatalf("resolveTierNow: space: %v", err)
	}
	md := permission.PageMeta{
		WorkspaceID: pg.WorkspaceID, SpaceID: pg.SpaceID, SpaceCreatedBy: sp.CreatedBy,
		SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
	}
	lvl, err := permission.NewStore(d.Pool).CheckPage(ctx, memberID, pageID, md, []string{pg.WorkspaceID})
	if err != nil {
		t.Fatalf("resolveTierNow: check: %v", err)
	}
	return lvl
}

// privateSeed is tierSeed's private-space sibling: with Private=true there is no workspace-member
// default, so revoking the one grant takes the member to AccessNone rather than AccessView.
func privateSeed(t *testing.T, d *testutil.DB) (ws, pageID, owner, member string) {
	t.Helper()
	ctx := context.Background()
	ws = d.Workspace(t)
	owner = d.Member(t, ws, "owner@corp.com")
	member = d.Member(t, ws, "member@corp.com")
	sp, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: ws, Name: "P", Slug: "p-" + owner[len(owner)-6:], CreatedBy: owner, Private: true,
	})
	if err != nil {
		t.Fatalf("seed private space: %v", err)
	}
	pg, err := page.NewStore(d.Pool).Create(ctx, model.Page{
		SpaceID: sp.ID, WorkspaceID: ws, Title: "Private doc", CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed page: %v", err)
	}
	if err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
		ResourceType: permission.ResourcePage, ResourceID: pg.ID, SubjectType: "member",
		SubjectID: member, Access: permission.AccessEdit, WorkspaceID: ws, GrantedBy: owner,
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	return ws, pg.ID, owner, member
}
