package page_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/spaceauth"
	"github.com/talyvor/docs/internal/testutil"
)

// A CALLER-SUPPLIED `limit` REACHES THE STORE WITH NO LOWER BOUND, AND THE STORE'S OWN
// `limit <= 0` CORRECTION IS THE ONLY THING THAT KEEPS `LIMIT -1` OUT OF POSTGRES.
//
// `handler.go`'s search route parses `?limit=` with `strconv.Atoi` and applies an UPPER bound only
// (`fetchLimit > searchMaxFetchRows`). There is no lower bound anywhere on the route, so
// `?limit=-1` arrives at `Store.Search` as -1 (or as -4, through the ×searchFetchFactor over-fetch,
// when the access gate is wired — both non-positive, so the correction is what matters either way).
//
// ⚠ MEASURED, NOT REASONED. Postgres refuses a negative LIMIT outright:
//
//	postgres=# SELECT i FROM t ORDER BY i LIMIT -1;
//	ERROR:  LIMIT must not be negative
//
// and `LIMIT 0` is legal but returns NOTHING, so a zero would silently empty the answer rather
// than error. `Store.Search`'s `if limit <= 0 { limit = 25 }` absorbs both.
//
// ⚠ WHY THIS TEST EXISTS: that correction was measured UNTESTED (W3.58, arm B4 — neuter it and the
// whole suite stays green), while being REACHABLE from this shipping route. A defensive correction
// nothing can reach is a true row and not a finding; this one is reached by a query parameter.
//
// ⚠ THE ARM MUST BE LINE-PRESERVING AND THAT IS NOT A DETAIL. Deleting the correction's three
// lines reds `internal/sqlguard` on its own account — its `unreconstructable` exemption list is
// keyed by file:line, so ANY line-count change in store.go reds it for free. Scored as a catch,
// that artefact reports an undefended correction as defended.
func TestPageSearch_NegativeLimit_IsCorrectedBeforeSQL_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	// content_text must be seeded EXPLICITLY: testutil's Page() leaves it NULL, and this route's
	// SQL matches on `to_tsvector(title || ' ' || content_text)`, which is NULL for a NULL operand
	// and therefore matches nothing. The premise check below is what surfaced that.
	for _, title := range []string{"Runbook alpha", "Runbook beta", "Runbook gamma"} {
		id := d.Page(t, ws, alice, title)
		if _, err := d.Pool.Exec(context.Background(),
			`UPDATE pages SET content_text = $2 WHERE id = $1`, id, title+" body text"); err != nil {
			t.Fatalf("seed content_text: %v", err)
		}
	}

	// THE ACCESS GATE MUST BE WIRED, AND THAT IS NOT BOILERPLATE. `visibleTo` FAILS CLOSED when
	// `h.access == nil` — it returns nil, so an unwired handler answers `[]` for every input and
	// every assertion below would pass on an empty route. (The premise check found this too.)
	// Wiring it is also the PRODUCTION path and the harder one: with the gate on, the route
	// over-fetches `limit * searchFetchFactor`, so `?limit=-1` reaches the store as -4.
	store := page.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)
	pageLooker := func(ctx context.Context, id string) (permission.PageMeta, error) {
		pg, err := store.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, pg.SpaceID, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		return permission.PageMeta{
			WorkspaceID: pg.WorkspaceID, SpaceID: pg.SpaceID,
			SpaceCreatedBy: sp.CreatedBy, SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
		}, nil
	}

	get := func(query string) (int, []model.Page, string) {
		t.Helper()
		h := page.NewHandler(store, d.Pool).
			WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(authz.WithMemberships(req.Context(),
					"alice@example.com", []authz.Membership{{WorkspaceID: ws, MemberID: alice}})))
			})
		})
		r.Route("/v1", func(r chi.Router) { h.Mount(r) })
		req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/pages/search?q=Runbook&"+query, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		var out []model.Page
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out, rr.Body.String()
	}

	// PREMISE + NON-VACUITY. An ordinary limit must find the seeded pages. Without this, every
	// assertion below is satisfied by a route that returns nothing for any input, and "no error"
	// would be indistinguishable from "no search".
	if code, rows, body := get("limit=10"); code != http.StatusOK || len(rows) != 3 {
		t.Fatalf("PREMISE FAILED: limit=10 returned %d with %d rows — the instrument cannot see "+
			"the three seeded pages at all, so nothing below would mean anything: %s", code, len(rows), body)
	}

	// THE ARGUMENT IS NOT REJECTED UPSTREAM, so the store correction is genuinely load-bearing
	// rather than unreachable. A 400 here would be a perfectly good product answer — and would
	// mean this test is pinning the wrong layer. It is not what the route does today.
	for _, tc := range []struct {
		name  string
		query string
		// A negative limit truncates to nothing (`limit > 0 && len(hits) >= limit` never fires
		// on the handler's own truncation), so the row count is the corrected fetch, not `limit`.
		wantRows int
	}{
		{"negative limit", "limit=-1", 3},
		{"more negative limit", "limit=-5", 3},
		{"zero limit", "limit=0", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, rows, body := get(tc.query)
			if code != http.StatusOK {
				t.Fatalf("%s: status %d, want 200 — with the store's `limit <= 0` correction gone "+
					"this is a 500 carrying Postgres' own \"LIMIT must not be negative\" text: %s",
					tc.query, code, body)
			}
			if len(rows) != tc.wantRows {
				t.Fatalf("%s: %d rows, want %d — a status-only assertion would pass on `LIMIT 0`, "+
					"which is legal SQL that returns nothing, so the count is the half that "+
					"separates \"corrected\" from \"silently emptied\"", tc.query, len(rows), tc.wantRows)
			}
		})
	}
}
