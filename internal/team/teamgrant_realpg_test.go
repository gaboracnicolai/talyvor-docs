package team_test

// B28.446 — a team grant takes effect, through the real routes on real Postgres: a member of a team
// granted edit on a private page can edit it, a non-member cannot (and cannot put themselves on the
// team, or get a team of their own granted), and taking the member off the team takes the access away.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/team"
	"github.com/talyvor/docs/internal/testutil"
)

func TestTeamGrant_FollowsTeamMembership_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com") // creates the private space, so Admin of it
	bob := d.Member(t, ws, "bob@example.com")     // goes on the team
	carol := d.Member(t, ws, "carol@example.com") // never on the team

	var sp, pg string
	if err := d.Pool.QueryRow(ctx, `INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		VALUES ($1, 'Private', 'private', $2, true) RETURNING id`, ws, alice).Scan(&sp); err != nil {
		t.Fatalf("seed space: %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text, content)
		VALUES ($1, $2, 'Plan', 'plan', $3, 'body', '{"type":"doc","content":[]}') RETURNING id`, sp, ws, alice).Scan(&pg); err != nil {
		t.Fatalf("seed page: %v", err)
	}

	// Wired as cmd/docs/main.go wires it.
	permStore := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool)
	spaceLooker := func(ctx context.Context, id string) (permission.SpaceMeta, error) {
		s, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.SpaceMeta{}, err
		}
		return permission.SpaceMeta{WorkspaceID: s.WorkspaceID, Private: s.Private, CreatedBy: s.CreatedBy}, nil
	}
	pageLooker := func(ctx context.Context, id string) (permission.PageMeta, error) {
		p, err := pageStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		s, err := spaceStore.GetByIDInWorkspaces(ctx, p.SpaceID, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		return permission.PageMeta{WorkspaceID: p.WorkspaceID, SpaceID: p.SpaceID,
			SpaceCreatedBy: s.CreatedBy, SpacePrivate: s.Private, PageCreatedBy: p.CreatedBy}, nil
	}
	spaceEnf := permission.NewEnforcer(permStore, permission.SpaceResolverFromParam("spaceID", spaceLooker))
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))

	do := func(memberID, method, path, body string) (int, string) {
		t.Helper()
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(authz.WithMemberships(req.Context(), memberID+"@example.com",
					[]authz.Membership{{WorkspaceID: ws, MemberID: memberID}})))
			})
		})
		r.Route("/v1", func(r chi.Router) {
			team.NewHandler(team.NewStore(d.Pool)).Mount(r)
			permission.NewHandler(permStore).WithAccess(spaceEnf, pageEnf).Mount(r)
			page.NewHandler(pageStore, d.Pool).WithAccess(pageEnf, spaceEnf).Mount(r)
		})
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rr.Code, strings.TrimSpace(rr.Body.String())
	}
	edit := func(memberID, title string) int {
		t.Helper()
		code, _ := do(memberID, http.MethodPatch, "/v1/spaces/"+sp+"/pages/"+pg, `{"title":"`+title+`"}`)
		return code
	}
	title := func() string {
		t.Helper()
		var s string
		if err := d.Pool.QueryRow(ctx, `SELECT title FROM pages WHERE id = $1`, pg).Scan(&s); err != nil {
			t.Fatalf("read title: %v", err)
		}
		return s
	}

	// Premise: the space is private, so before any grant bob cannot edit.
	if c := edit(bob, "before"); c != http.StatusForbidden {
		t.Fatalf("[premise] bob edited the private page with no grant: %d", c)
	}

	code, body := do(alice, http.MethodPost, "/v1/workspaces/"+ws+"/teams", `{"name":"Design"}`)
	if code != http.StatusCreated {
		t.Fatalf("create team = %d %s", code, body)
	}
	var tm team.Team
	if err := json.Unmarshal([]byte(body), &tm); err != nil || tm.ID == "" {
		t.Fatalf("create team body %s: %v", body, err)
	}
	membersPath := "/v1/workspaces/" + ws + "/teams/" + tm.ID + "/members/"
	if code, body := do(alice, http.MethodPut, membersPath+bob, ""); code != http.StatusOK {
		t.Fatalf("add bob = %d %s", code, body)
	}
	code, body = do(alice, http.MethodPost, "/v1/spaces/"+sp+"/pages/"+pg+"/permissions",
		`{"subject_type":"team","subject_id":"`+tm.ID+`","access":"edit"}`)
	if code != http.StatusCreated {
		t.Fatalf("grant the team edit = %d %s", code, body)
	}

	// A member of the team can edit.
	if c := edit(bob, "Edited by Bob"); c != http.StatusOK {
		t.Fatalf("bob (on the team) edit = %d, want 200", c)
	}
	if got := title(); got != "Edited by Bob" {
		t.Fatalf("page title = %q after bob's edit", got)
	}

	// A non-member cannot edit, and cannot put herself on the team.
	if c := edit(carol, "Edited by Carol"); c != http.StatusForbidden {
		t.Fatalf("carol (not on the team) edit = %d, want 403", c)
	}
	if code, body := do(carol, http.MethodPut, membersPath+carol, ""); code != http.StatusForbidden {
		t.Fatalf("carol added herself to alice's team = %d %s, want 403", code, body)
	}
	// Nor can she route access through a team she manages: the page admin can grant only a team
	// the admin manages, so carol's roster never decides who reaches alice's page.
	code, body = do(carol, http.MethodPost, "/v1/workspaces/"+ws+"/teams", `{"name":"Carol's"}`)
	var own team.Team
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &own) != nil {
		t.Fatalf("carol create team = %d %s", code, body)
	}
	if code, body := do(carol, http.MethodPut, "/v1/workspaces/"+ws+"/teams/"+own.ID+"/members/"+carol, ""); code != http.StatusOK {
		t.Fatalf("carol joins her own team = %d %s", code, body)
	}
	if code, body := do(alice, http.MethodPost, "/v1/spaces/"+sp+"/pages/"+pg+"/permissions",
		`{"subject_type":"team","subject_id":"`+own.ID+`","access":"edit"}`); code != http.StatusBadRequest {
		t.Fatalf("alice granted carol's team = %d %s, want 400", code, body)
	}
	if c := edit(carol, "Edited by Carol"); c != http.StatusForbidden {
		t.Fatalf("carol edit after trying to join = %d, want 403", c)
	}

	// Taking bob off the team takes the access away.
	if code, body := do(alice, http.MethodDelete, membersPath+bob, ""); code != http.StatusOK {
		t.Fatalf("remove bob = %d %s", code, body)
	}
	if c := edit(bob, "Edited again"); c != http.StatusForbidden {
		t.Fatalf("bob edit after leaving the team = %d, want 403", c)
	}
	if got := title(); got != "Edited by Bob" {
		t.Fatalf("page title = %q, want the last permitted edit", got)
	}
}
