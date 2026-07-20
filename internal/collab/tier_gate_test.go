package collab

// Collab `change`-frame TIER gate. The WS entry point gates on membership + (lock/approval), never the
// AccessEdit tier — so a view-only member could edit page content live and it PERSISTED (via the
// autosaver). This is the fourth tier-ungated write path, the same class as the DB-REST and MCP gates.
//
// The gate must be surgical: only `change` frames are refused; cursor + presence must keep flowing so a
// read-only member still sees live collaboration (handler.go's WithGuard already keeps those flowing for
// locked pages — the tier gate must not regress that). Fail-closed: if the tier can't be resolved,
// changes are refused.
//
// RED (dispatch does not check canEdit): the viewer's change is applied and PERSISTS to pages.content —
// (a) below FAILS. GREEN (dispatch refuses when !canEdit): the viewer is refused and nothing persists;
// the SAME viewer still receives presence + cursor (b); an edit-tier member's change persists (c); and
// with the gate neutered the viewer's change lands (d) — proving the gate, not a broken pipeline, is
// what blocks it. This is package `collab` (internal) so it can drive the real autosaver flush and
// assert on pages.content in the DB, not just the socket response.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

const tierSecret = "sec4-test-gateway-secret-0123456789"

// stubResolver forces the (inScope, actor, canEdit) triple — used to NEUTER the gate for the mutation
// proof (canEdit=true makes the same viewer's change land).
type stubResolver struct {
	inScope, canEdit bool
	actor            string
}

func (s stubResolver) ResolveSession(context.Context, string) (bool, string, bool) {
	return s.inScope, s.actor, s.canEdit
}

func tierPageLooker(d *testutil.DB) func(context.Context, string) (permission.PageMeta, error) {
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool)
	return func(ctx context.Context, id string) (permission.PageMeta, error) {
		pg, err := pageStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, pg.SpaceID, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		return permission.PageMeta{
			WorkspaceID: pg.WorkspaceID, SpaceID: pg.SpaceID, SpaceCreatedBy: sp.CreatedBy,
			SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
		}, nil
	}
}

// tierEnv wires a real collab chain (gatewayauth+authz+ServeWS) + a real autosaver over one engine, so a
// test can drive a WS and then flush snapshots to pages.content deterministically.
type tierEnv struct {
	url   string
	saver *AutoSaver
	d     *testutil.DB
}

func newTierEnv(t *testing.T, d *testutil.DB, resolver SessionResolver) *tierEnv {
	t.Helper()
	engine := NewOTEngine()
	pageStore := page.NewStore(d.Pool)
	// No WithGuard: nil guard skips the lock/approval check, isolating the TIER gate (lock behavior is
	// covered elsewhere). WithAccess wires the resolver under test.
	h := NewHandler(engine).WithAccess(resolver)
	saver := NewAutoSaver(engine, func(ctx context.Context, pageID, content string) error {
		_, err := pageStore.Update(ctx, pageID, map[string]any{"content": content})
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
	return &tierEnv{url: srv.URL, saver: saver, d: d}
}

func (e *tierEnv) dial(t *testing.T, pageID, clientID, email string) *websocket.Conn {
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

func (e *tierEnv) contentOf(t *testing.T, pageID string) string {
	t.Helper()
	var c string
	if err := e.d.Pool.QueryRow(context.Background(), `SELECT content FROM pages WHERE id=$1`, pageID).Scan(&c); err != nil {
		t.Fatalf("read content: %v", err)
	}
	return c
}

func readNext(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

func readUntil(t *testing.T, conn *websocket.Conn, wantType string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_ = conn.SetReadDeadline(deadline)
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read (want %q): %v", wantType, err)
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if m["type"] == wantType {
			return m
		}
	}
}

func sendChange(t *testing.T, conn *websocket.Conn, id, snapshot string, version int) {
	t.Helper()
	env := map[string]any{
		"type": "change",
		"change": map[string]any{
			"id": id, "version": version, "ops": []any{}, "snapshot": snapshot,
		},
	}
	b, _ := json.Marshal(env)
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatalf("send change: %v", err)
	}
}

func sendCursor(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"type": "cursor", "cursor": map[string]any{"from": 1, "to": 2}})
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatalf("send cursor: %v", err)
	}
}

func tierSeed(t *testing.T, d *testutil.DB) (ws, pageID, viewer, editor string) {
	t.Helper()
	ctx := context.Background()
	ws = d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	viewer = d.Member(t, ws, "viewer@corp.com")
	editor = d.Member(t, ws, "editor@corp.com")
	sp, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: ws, Name: "S", Slug: "s-" + owner[len(owner)-6:], CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	pg, err := page.NewStore(d.Pool).Create(ctx, model.Page{
		SpaceID: sp.ID, WorkspaceID: ws, Title: "Live doc", CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed page: %v", err)
	}
	grant := func(subject string, lvl permission.AccessLevel) {
		if err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
			ResourceType: permission.ResourcePage, ResourceID: pg.ID, SubjectType: "member",
			SubjectID: subject, Access: lvl, WorkspaceID: ws, GrantedBy: owner,
		}); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}
	grant(viewer, permission.AccessView)
	grant(editor, permission.AccessEdit)
	return ws, pg.ID, viewer, editor
}

const (
	viewerSentinel = `{"type":"doc","tag":"VIEWER-HACK-9f3a"}`
	editorSentinel = `{"type":"doc","tag":"EDITOR-OK-7b2c"}`
)

func TestSEC4_Collab_ChangeTierGate(t *testing.T) {
	d := testutil.New(t)
	_, pageID, _, _ := tierSeed(t, d)
	env := newTierEnv(t, d, NewPermissionSession(permission.NewStore(d.Pool), tierPageLooker(d)))

	vconn := env.dial(t, pageID, "v-client", "viewer@corp.com")
	readUntil(t, vconn, "init")
	econn := env.dial(t, pageID, "e-client", "editor@corp.com")
	readUntil(t, econn, "init")

	// (b) The view-only member still receives PRESENCE + CURSOR — read-only collaboration is intact.
	if m := readUntil(t, vconn, "presence"); m["event"] != "joined" {
		t.Errorf("(b) viewer presence event = %v, want joined", m["event"])
	}
	sendCursor(t, econn)
	if m := readUntil(t, vconn, "cursor"); m["type"] != "cursor" {
		t.Errorf("(b) viewer did not receive the editor's cursor frame")
	}

	// (a) The view-only member's CHANGE is REFUSED and does NOT persist. Assert on pages.content.
	sendChange(t, vconn, "v1", viewerSentinel, 0)
	if m := readNext(t, vconn); m["type"] != "change_rejected" {
		t.Errorf("(a) viewer change response = %v, want change_rejected (view-tier cannot mutate)", m["type"])
	}
	env.saver.flush(context.Background())
	if c := env.contentOf(t, pageID); strings.Contains(c, "VIEWER-HACK-9f3a") {
		t.Errorf("(a) view-only member's change PERSISTED to pages.content — tier bypass. content=%s", c)
	}

	// (c) An edit-tier member's change applies and persists.
	sendChange(t, econn, "e1", editorSentinel, 0)
	readUntil(t, econn, "ack")
	env.saver.flush(context.Background())
	if c := env.contentOf(t, pageID); !strings.Contains(c, "EDITOR-OK-7b2c") {
		t.Errorf("(c) edit-tier member's change did NOT persist. content=%s", c)
	}
}

// (d) MUTATION PROOF: with the gate neutered (a stub resolver forcing canEdit=true), the SAME view-only
// member's change lands and persists — so the pipeline works and the gate's canEdit=false in (a) is the
// only thing blocking it.
func TestSEC4_Collab_ChangeTierGate_NeuteredGateLetsItPersist(t *testing.T) {
	d := testutil.New(t)
	_, pageID, viewer, _ := tierSeed(t, d)
	env := newTierEnv(t, d, stubResolver{inScope: true, actor: viewer, canEdit: true})

	vconn := env.dial(t, pageID, "v-client", "viewer@corp.com")
	readUntil(t, vconn, "init")
	sendChange(t, vconn, "v1", viewerSentinel, 0)
	readUntil(t, vconn, "ack")
	env.saver.flush(context.Background())
	if c := env.contentOf(t, pageID); !strings.Contains(c, "VIEWER-HACK-9f3a") {
		t.Errorf("neutered gate: viewer change did not persist — the persistence pipeline is broken, "+
			"so (a) would be a false pass. content=%s", c)
	}
}
