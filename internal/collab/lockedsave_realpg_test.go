package collab

// THE WEBSOCKET BOUNDARY AND THE AUTOSAVER ASK THE SAME GUARD TWO DIFFERENT QUESTIONS, AND ONLY THE
// FIRST ONE NAMES THE EDITOR.
//
// dispatch gates a `change` frame with h.guard.CanEdit(ctx, pageID, c.MemberID, false) — the ACTUAL
// member on the socket. The autosaver, which is the ONLY thing that ever moves that change to disk,
// calls page.Store.Update with map[string]any{"content": snap} and NOTHING ELSE, so Update's own gate
// (store.go:542, `memberID, _ := updates["updated_by"].(string)`) reads the EMPTY STRING and asks the
// composite guard whether "" may write. pagelock answers "Locked by <someone>" for every held lock,
// including the lock held by the very member whose frame was just accepted; editsession answers
// "<holder> is editing" for every live session.
//
// So the lock holder — the person who took the lock precisely so they could edit safely — gets their
// change APPLIED by the engine, ACKed back to their own editor, and BROADCAST to everyone else, and
// it never reaches pages.content. The only trace is a slog.Warn in the save loop.
//
// AND THE LOSS IS PERMANENT, NOT DEFERRED. AutoSaver.flush leaves lastSavedVersion unadvanced and
// retries every 5s, so "it'll save when the lock lifts" sounds plausible — but OTEngine.Leave does
// `delete(e.pages, pageID)` when the last client disconnects. Close the tab while still holding your
// own lock and the snapshot is freed with the edit still only in memory. [SURVIVES-DISCONNECT] holds
// that: it waits for the engine to release the page as a PREMISE, then reads the database, which by
// then is the only place the work could be.
//
// WHY NO EXISTING TEST SEES IT: the divergence IS the two guards, and no harness wired both. This
// file's env wires BOTH of them the way cmd/docs/main.go wires them —
// NewHandler(engine).WithGuard(lockStore) (main.go:520) AND
// pageStore.WithGuard(editsession.Compose(lockStore, editSessionStore)) (main.go:324) AND the saver
// closure (main.go:523). It is not main.go verbatim: WithAllowedOrigins is omitted because httptest
// dials same-origin, and that middleware is upstream of everything measured here.
// tier_gate_test.go says so in its own words at
// newTierEnv: "No WithGuard: nil guard skips the lock/approval check ... lock behavior is covered
// elsewhere". It is not covered elsewhere; nothing wired the save-side guard at all.
//
// RED on the unmodified tree: [LOCK-HOLDER-PERSISTED] and [SESSION-HOLDER-PERSISTED] fail —
// pages.content still holds the seed body after a flush. [ACCEPTED] and [ACCEPTED-SESSION] are GREEN
// before AND after, and that is the point: they are what make the failure a CONTRADICTION (the server
// told the author yes) rather than a refusal the product could defend.
//
// GREEN: the saver propagates the change's own author as "updated_by", the key store.go already calls
// "the canonical editor identity", so Update's gate evaluates the member who actually made the edit —
// the same member dispatch already cleared. A foreign lock is still refused at the socket, so nothing
// this widens was ever reachable; [FOREIGN-LOCK-REFUSED] pins that the fix did not open that door.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/editsession"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelock"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// lockEnv is cmd/docs/main.go's collab wiring, not a reduction of it: BOTH guards are attached, the
// same way and in the same order main.go attaches them.
type lockEnv struct {
	url    string
	saver  *AutoSaver
	engine *OTEngine
	d      *testutil.DB
}

func newLockEnv(t *testing.T, d *testutil.DB) *lockEnv {
	t.Helper()
	engine := NewOTEngine()
	pageStore := page.NewStore(d.Pool)
	lockStore := pagelock.NewStore(d.Pool)
	editSessionStore := editsession.NewStore(d.Pool)
	// main.go:324 — the REST save guard is approvalOK AND manualLockOK AND editSessionOK.
	pageStore = pageStore.WithGuard(editsession.Compose(lockStore, editSessionStore))
	// main.go:520 — the collab handler keeps its OWN lockStore guard.
	h := NewHandler(engine).
		WithGuard(lockStore).
		WithAccess(NewPermissionSession(permission.NewStore(d.Pool), tierPageLooker(d), authz.NewPGResolver(d.Pool)))
	// main.go:523 — the autosaver closure, verbatim.
	saver := NewAutoSaver(engine, func(ctx context.Context, pageID, content, memberID string) error {
		_, err := pageStore.Update(ctx, pageID, map[string]any{
			"content":    content,
			"updated_by": memberID,
		})
		return err
	})
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(string) bool { return false }
		r.Use(gatewayauth.Middleware(tierSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		r.Get("/collab/{pageID}/ws", h.ServeWS)
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &lockEnv{url: srv.URL, saver: saver, engine: engine, d: d}
}

func (e *lockEnv) dial(t *testing.T, pageID, clientID, email string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(e.url, "http") + "/v1/collab/" + pageID + "/ws?client_id=" + clientID
	hd := http.Header{}
	hd.Set("X-Gateway-Auth", tierSecret)
	hd.Set("X-User-Email", email)
	conn, resp, err := websocket.DefaultDialer.Dial(u, hd)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status=%d)", email, err, code)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func (e *lockEnv) contentOf(t *testing.T, pageID string) string {
	t.Helper()
	var c string
	if err := e.d.Pool.QueryRow(context.Background(), `SELECT content FROM pages WHERE id=$1`, pageID).Scan(&c); err != nil {
		t.Fatalf("read content: %v", err)
	}
	return c
}

// lockSeed returns a page plus two EDIT-tier members, so the tier gate (#126's neighbour) is never
// what refuses anything here — every refusal in this file is the lock/session guard or nothing.
func lockSeed(t *testing.T, d *testutil.DB) (pageID, alice, bob string) {
	t.Helper()
	ctx := context.Background()
	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	alice = d.Member(t, ws, "alice@corp.com")
	bob = d.Member(t, ws, "bob@corp.com")
	sp, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: ws, Name: "S", Slug: "s-" + owner[len(owner)-6:], CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	pg, err := page.NewStore(d.Pool).Create(ctx, model.Page{
		SpaceID: sp.ID, WorkspaceID: ws, Title: "Live doc", CreatedBy: owner, Content: seedBody,
	})
	if err != nil {
		t.Fatalf("seed page: %v", err)
	}
	for _, m := range []string{alice, bob} {
		if _, err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
			ResourceType: permission.ResourcePage, ResourceID: pg.ID, SubjectType: "member",
			SubjectID: m, Access: permission.AccessEdit, WorkspaceID: ws, GrantedBy: owner,
		}); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}
	return pg.ID, alice, bob
}

// workspaceOf reads the page's workspace straight from the row, so the editsession calls below are
// scoped the way the REST handler scopes them (wsIDs from the caller's verified memberships).
func workspaceOf(t *testing.T, d *testutil.DB, pageID string) string {
	t.Helper()
	var ws string
	if err := d.Pool.QueryRow(context.Background(), `SELECT workspace_id FROM pages WHERE id=$1`, pageID).Scan(&ws); err != nil {
		t.Fatalf("workspace of page: %v", err)
	}
	return ws
}

// waitForEngineRelease establishes the PREMISE of the durability test — that OTEngine.Leave has run
// and freed the page, so the database is the only place the edit could still be. It is a precondition,
// not the assertion: if it times out the test proved nothing and says so, rather than passing.
// Leave runs on the ServeWS goroutine after the read pump returns, so a client-side Close is not
// synchronous with it; this polls the real condition instead of sleeping a guessed interval.
func waitForEngineRelease(t *testing.T, e *OTEngine, pageID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if snap, _, _ := e.Snapshot(pageID); snap == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PREMISE FAILED: engine still holds page %s after 3s, so this test cannot say whether the edit survived", pageID)
}

const (
	seedBody     = `{"type":"doc","tag":"SEED-BEFORE-ANY-EDIT"}`
	lockedByMine = `{"type":"doc","tag":"MINE-UNDER-MY-OWN-LOCK-4d21"}`
	sessionMine  = `{"type":"doc","tag":"MINE-UNDER-MY-OWN-SESSION-8c07"}`
)

// TestCollab_OwnLock_ChangeIsAckedThenSilentlyDropped is the write half. Alice takes the manual lock
// through pagelock.Store.Lock — the production writer behind PUT /pages/{id}/lock — and then edits
// over the socket the lock exists to protect.
func TestCollab_OwnLock_ChangeIsAckedAtTheSocket(t *testing.T) {
	d := testutil.New(t)
	pageID, alice, _ := lockSeed(t, d)
	env := newLockEnv(t, d)
	if _, err := pagelock.NewStore(d.Pool).Lock(context.Background(), pageID, alice); err != nil {
		t.Fatalf("alice takes her own lock: %v", err)
	}

	conn := env.dial(t, pageID, "a-client", "alice@corp.com")
	readUntil(t, conn, "init")
	sendChange(t, conn, "a1", lockedByMine, 0)

	// [ACCEPTED] — the server tells the author the change is in. Green before and after the fix; it is
	// what makes the persistence failure a contradiction rather than a documented refusal.
	if m := readNext(t, conn); m["type"] != "ack" {
		t.Fatalf("[ACCEPTED] lock holder's own change response = %v, want ack", m["type"])
	}
}

func TestCollab_OwnLock_ChangeReachesPagesContent(t *testing.T) {
	d := testutil.New(t)
	pageID, alice, _ := lockSeed(t, d)
	env := newLockEnv(t, d)
	if _, err := pagelock.NewStore(d.Pool).Lock(context.Background(), pageID, alice); err != nil {
		t.Fatalf("alice takes her own lock: %v", err)
	}

	conn := env.dial(t, pageID, "a-client", "alice@corp.com")
	readUntil(t, conn, "init")
	sendChange(t, conn, "a1", lockedByMine, 0)
	readUntil(t, conn, "ack")
	env.saver.flush(context.Background())

	// [LOCK-HOLDER-PERSISTED] — RED before the fix: Update's gate was asked about "" and answered
	// "Locked by <alice>", so the ACKed change never left memory.
	if c := env.contentOf(t, pageID); !strings.Contains(c, "MINE-UNDER-MY-OWN-LOCK-4d21") {
		t.Errorf("[LOCK-HOLDER-PERSISTED] the lock holder's own ACKed change did NOT reach pages.content. content=%s", c)
	}
}

// TestCollab_OwnLock_EditSurvivesDisconnect is why the retry loop is not a mitigation. AutoSaver
// leaves lastSavedVersion unadvanced and retries every 5s, so "it will save once the lock lifts"
// sounds plausible — but OTEngine.Leave does `delete(e.pages, pageID)` when the last client goes, and
// the retry then has nothing to retry. Once the engine has released the page (the premise below), the
// database is the only copy of the author's work that can exist.
func TestCollab_OwnLock_EditSurvivesDisconnect(t *testing.T) {
	d := testutil.New(t)
	pageID, alice, _ := lockSeed(t, d)
	env := newLockEnv(t, d)
	if _, err := pagelock.NewStore(d.Pool).Lock(context.Background(), pageID, alice); err != nil {
		t.Fatalf("alice takes her own lock: %v", err)
	}

	conn := env.dial(t, pageID, "a-client", "alice@corp.com")
	readUntil(t, conn, "init")
	sendChange(t, conn, "a1", lockedByMine, 0)
	readUntil(t, conn, "ack")
	env.saver.flush(context.Background())
	_ = conn.Close()
	waitForEngineRelease(t, env.engine, pageID)
	env.saver.flush(context.Background()) // the retry the save loop would make — nothing left to save

	// [SURVIVES-DISCONNECT] — RED before the fix. The engine has freed the page (premise above), so
	// this read is the last place the author's work could be. Before the fix it is neither here nor
	// there: the page still holds the seed body and the only record of the edit was the ack.
	if c := env.contentOf(t, pageID); !strings.Contains(c, "MINE-UNDER-MY-OWN-LOCK-4d21") {
		t.Errorf("[SURVIVES-DISCONNECT] after the engine released the page the edit exists NOWHERE — pages.content=%s", c)
	}
}

// TestCollab_OwnEditSession_* is the second guard in the composite, and the sharper half: main.go:320
// says in its own words that the single-writer policy "governs only the REST save path" and that
// "collab (the multi-writer OT path) keeps its own lockStore guard". It governs the collab path too —
// at the point of persistence, where its refusal is invisible.
func TestCollab_OwnEditSession_ChangeIsAckedAtTheSocket(t *testing.T) {
	d := testutil.New(t)
	pageID, alice, _ := lockSeed(t, d)
	env := newLockEnv(t, d)
	ws := workspaceOf(t, d, pageID)
	if _, err := editsession.NewStore(d.Pool).Acquire(context.Background(), pageID, []string{ws}, alice); err != nil {
		t.Fatalf("alice acquires her own edit session: %v", err)
	}

	conn := env.dial(t, pageID, "a-client", "alice@corp.com")
	readUntil(t, conn, "init")
	sendChange(t, conn, "a1", sessionMine, 0)

	// [ACCEPTED-SESSION] — the socket never consults editsession at all, so it always says yes here.
	if m := readNext(t, conn); m["type"] != "ack" {
		t.Fatalf("[ACCEPTED-SESSION] session holder's own change response = %v, want ack", m["type"])
	}
}

func TestCollab_OwnEditSession_ChangeReachesPagesContent(t *testing.T) {
	d := testutil.New(t)
	pageID, alice, _ := lockSeed(t, d)
	env := newLockEnv(t, d)
	ws := workspaceOf(t, d, pageID)
	if _, err := editsession.NewStore(d.Pool).Acquire(context.Background(), pageID, []string{ws}, alice); err != nil {
		t.Fatalf("alice acquires her own edit session: %v", err)
	}

	conn := env.dial(t, pageID, "a-client", "alice@corp.com")
	readUntil(t, conn, "init")
	sendChange(t, conn, "a1", sessionMine, 0)
	readUntil(t, conn, "ack")
	env.saver.flush(context.Background())

	// [SESSION-HOLDER-PERSISTED] — RED before the fix.
	if c := env.contentOf(t, pageID); !strings.Contains(c, "MINE-UNDER-MY-OWN-SESSION-8c07") {
		t.Errorf("[SESSION-HOLDER-PERSISTED] the session holder's own ACKed change did NOT reach pages.content. content=%s", c)
	}
}

// TestCollab_ForeignLock_StillRefusedAtTheSocket is the door the fix must NOT open. Bob edits a page
// Alice has locked: dispatch's own guard refuses the frame with c.MemberID=bob BEFORE the engine sees
// it, so nothing ever enters the snapshot and there is nothing for a widened save gate to let through.
func TestCollab_ForeignLock_StillRefusedAtTheSocket(t *testing.T) {
	d := testutil.New(t)
	pageID, alice, _ := lockSeed(t, d)
	env := newLockEnv(t, d)
	if _, err := pagelock.NewStore(d.Pool).Lock(context.Background(), pageID, alice); err != nil {
		t.Fatalf("alice locks: %v", err)
	}

	conn := env.dial(t, pageID, "b-client", "bob@corp.com")
	readUntil(t, conn, "init")
	sendChange(t, conn, "b1", `{"type":"doc","tag":"BOB-THROUGH-ALICES-LOCK"}`, 0)

	// [FOREIGN-LOCK-REFUSED] — green before and after; the assertion exists so a regression that
	// removes the socket-side guard shows up here rather than as a quiet write.
	if m := readNext(t, conn); m["type"] != "change_rejected" {
		t.Fatalf("[FOREIGN-LOCK-REFUSED] bob's change through alice's lock = %v, want change_rejected", m["type"])
	}
}

func TestCollab_ForeignLock_NothingPersists(t *testing.T) {
	d := testutil.New(t)
	pageID, alice, _ := lockSeed(t, d)
	env := newLockEnv(t, d)
	if _, err := pagelock.NewStore(d.Pool).Lock(context.Background(), pageID, alice); err != nil {
		t.Fatalf("alice locks: %v", err)
	}

	conn := env.dial(t, pageID, "b-client", "bob@corp.com")
	readUntil(t, conn, "init")
	sendChange(t, conn, "b1", `{"type":"doc","tag":"BOB-THROUGH-ALICES-LOCK"}`, 0)
	// Drain ONE frame, whatever it is, rather than readUntil("change_rejected"). readUntil Fatalfs
	// when the frame never comes, which made this assertion unreachable in exactly the scenario it
	// exists for: control C4 deletes dispatch's guard, bob gets an ack instead, and the test died at
	// the premise instead of reporting on persistence. An assertion you cannot reach when the thing
	// it guards is broken is not an assertion.
	readNext(t, conn)
	env.saver.flush(context.Background())

	// [FOREIGN-LOCK-NOT-PERSISTED] — the lock still means what it meant.
	if c := env.contentOf(t, pageID); strings.Contains(c, "BOB-THROUGH-ALICES-LOCK") {
		t.Errorf("[FOREIGN-LOCK-NOT-PERSISTED] a change through someone else's lock reached pages.content. content=%s", c)
	}
}
