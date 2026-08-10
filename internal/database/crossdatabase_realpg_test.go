package database_test

// THE INLINE-DATABASE BY-ID WRITES DID NOT NAME THE DATABASE THEIR ROUTE AUTHORIZED.
//
// Every /databases/{dbID}/* route is `.With(dbEnf.Require(...))`, and dbEnf's resolver reads
// {dbID} from the URL and inherits the OWNING PAGE's access (databases.page_id → pages). So the
// gate answers about the database IN THE URL. Three handlers then passed only the CHILD id to the
// store:
//
//	PATCH  /databases/{dbID}/rows/{rowID}    -> UpdateRow(rowID,  …, wsIDs)
//	DELETE /databases/{dbID}/rows/{rowID}    -> DeleteRow(rowID,  …, wsIDs)
//	PATCH  /databases/{dbID}/views/{viewID}  -> UpdateView(viewID, …, wsIDs)
//
// and each statement scoped by `database_id IN (SELECT id FROM databases WHERE workspace_id =
// ANY($n))` — the WORKSPACE, not the database the enforcer had just cleared. THE GATE AND THE
// STATEMENT WERE ABOUT TWO DIFFERENT OBJECTS, with only the ring both are already inside between
// them. Their five siblings on the same handler do name it: CreateRow/CreateView call
// assertDatabaseInWorkspaces on {dbID}, ListRows/ListViews/UpdateSchema put {dbID} in the WHERE.
// The three by-id writes were the outliers.
//
// ⚠ MEASURED THROUGH THE REAL ROUTES ON REAL POSTGRES BEFORE ANY CHANGE (this file, run against
// the unfixed store). One workspace W. alice owns a PRIVATE space with a page carrying an inline
// database. bob is a plain member of W with NO grant on that space, who has a page and a database
// of his own — nothing privileged, and `POST /spaces` is registered with no enforcer at all:
//
//	bob GET    /databases/{aliceDB}/rows                      -> 404   (he cannot even list them)
//	bob PATCH  /databases/{aliceDB}/rows/{aliceRow}           -> 404   (refused at its own address)
//	bob PATCH  /databases/{bobDB}/rows/{aliceRow}             -> 200   values rewritten
//	bob DELETE /databases/{bobDB}/rows/{aliceRow2}            -> 200   row gone
//	bob PATCH  /databases/{bobDB}/views/{aliceView}           -> 200   view rewritten
//
// THE SAME ROW, REFUSED AT 404 THROUGH ITS OWN ADDRESS AND SERVED AT 200 THROUGH A BORROWED ONE.
//
// ⚠⚠ THE WRITE IS ALSO A READ. UpdateRow is a read-modify-write that RETURNS the merged row, and
// UpdateView RETURNS the merged view — so a one-cell PATCH hands back every OTHER cell alice ever
// wrote, and a rename hands back the view's filters, sort and hidden columns.
// `{"values":{"note":"x"}}` against alice's row answers with her severance figure, which is why
// X-LEAK-ROW-READ and X-LEAK-VIEW-READ assert on the RESPONSE BODY and not only on the row on
// disk: the tamper and the disclosure are different harms and only one of them is visible in
// Postgres afterwards.
//
// ⚠ WHAT THAT DOES NOT MEAN, MEASURED RATHER THAN ASSUMED. The claim this work inherited was that
// "a write-shaped fix would miss half of it". It would not, in this shape, and the control set
// says so: C2 scopes the UPDATE and leaves the read-modify-write SELECT unscoped, and NO
// assertion here fires — the merged row only ever reaches the caller through the UPDATE's
// RETURNING, so a scoped UPDATE answers 404 and no cell escapes. C3 is the mirror and is also
// silent. The two predicates in UpdateRow are therefore individually redundant for anything this
// guard can observe; C4 (both removed) is what justifies that half of the fix, and
// X-LEAK-ROW-READ is claimed JOINTLY with X-LEAK-ROW-WRITE and by nothing else. Recorded here
// because an assertion no single control can isolate is a fact about the guard, not a detail.
//
// ⚠⚠ TWO GUARDS ARE GREEN OVER THIS AND BOTH BY CONSTRUCTION:
//   - sec4_l2_test.go (a) drives DELETE /databases/{dbID}/rows/{rowID} and asserts 404 for a
//     FOREIGN-TENANT caller. It is the OUTER RING and it passes throughout the leak above — it
//     always names the row's own database in the URL, so the mismatched pair is not a case it can
//     express.
//   - sec4_tier_test.go asserts a view-tier member is refused all six writes — same shape: every
//     path it builds is `base + "/rows/" + row.ID` where row belongs to base's database.
//
// ⚠ .semgrep/operate-by-id-tenancy.yml cannot see it either: its predicate is `workspace_id =
// ANY`, which all three statements have. The rule is satisfied by the very predicate that is the
// defect — the same sentence #82 (changelog) and #83 (permissions) recorded.
//
// This is finding (18)'s THIRD copy. `a86417b` (#82) closed the changelog copy, `227ac2a` (#83)
// the permission copy.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

// The secret cell alice's row carries and bob never sends. Distinctive enough that finding it in
// a response body is a fact, not a coincidence.
const aliceSecretCell = "SEVERANCE-USD-482000-BOARD-ONLY"

// The view fields alice's saved view carries. group_by and hidden_cols name COLUMNS of a schema
// bob cannot read, so the view read is a disclosure in its own right.
const (
	aliceViewName    = "Board comp — do not share"
	aliceViewGroupBy = "col-severance"
)

func TestDatabaseByID_MustNameTheDatabaseTheRouteAuthorized_RealPG(t *testing.T) {
	d := testutil.New(t)

	W := d.Workspace(t)
	alice := d.Member(t, W, "alice@corp.com")
	bob := d.Member(t, W, "bob@corp.com")

	// alice's PRIVATE space and its page. Seeded with SQL, never through the stores: a fixture
	// built by code under test moves when that code moves.
	alicePriv := seedSpaceX(t, d, W, alice, "Board Private", true)
	alicePage := seedPageX(t, d, W, alicePriv, alice, "Comp Plan")
	aliceDB := seedDatabaseX(t, d, W, alicePage, "Comp")
	aliceRow := seedRowX(t, d, aliceDB, map[string]any{"note": "approved", "severance": aliceSecretCell})
	aliceRow2 := seedRowX(t, d, aliceDB, map[string]any{"note": "second", "severance": aliceSecretCell})
	// ⚠ A THIRD ROW, USED ONLY BY X-HONEST-WRITE, AND THE FIRST RUN OF C13 IS WHY. That assertion
	// PATCHes at the honest address; when it pointed at aliceRow, removing the Edit gate let the
	// patch land — and X-LEAK-ROW-WRITE, which reads that same cell later, then fired on the
	// HONEST write rather than on any leak. A control caught by an earlier assertion's side effect
	// is a catch on the wrong branch. One row per assertion is what makes the verdict readable.
	aliceRow0 := seedRowX(t, d, aliceDB, map[string]any{"note": "untouched", "severance": aliceSecretCell})
	aliceView := seedViewX(t, d, aliceDB, aliceViewName, aliceViewGroupBy)

	// bob's own space, page and database. He is its creator, so resolveAccess's owner-is-admin arm
	// gives him Edit there — the only access this whole attack needs.
	bobSpace := seedSpaceX(t, d, W, bob, "Bobs Own", false)
	bobPage := seedPageX(t, d, W, bobSpace, bob, "Bobs Page")
	bobDB := seedDatabaseX(t, d, W, bobPage, "Bobs db")
	bobRow := seedRowX(t, d, bobDB, map[string]any{"note": "mine"})
	bobRow2 := seedRowX(t, d, bobDB, map[string]any{"note": "mine too"})
	bobRow3 := seedRowX(t, d, bobDB, map[string]any{"note": "deletable"})
	bobView := seedViewX(t, d, bobDB, "Bobs view", "col-b")

	// tierChain is main.go's wiring — pageEnf for the page-scoped create, dbEnf (db→page resolver)
	// for every /databases/{dbID}/* route — behind the real gatewayauth + authz middleware. Shared
	// with sec4_tier_test.go deliberately: it is the production chain, and a second copy would
	// drift from it silently.
	chain := tierChain(d)
	call := func(method, path, body string) (int, string) {
		t.Helper()
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, tierReq(method, path, "bob@corp.com", body))
		return rr.Code, strings.TrimSpace(rr.Body.String())
	}
	bobBase := "/v1/databases/" + bobDB
	aliceBase := "/v1/databases/" + aliceDB

	// ── X-PREMISE. bob's Edit on his OWN database is real and the PATCH route serves him. Without
	// it every refusal below would be a refusal of a route that never worked. Errorf, not Fatalf:
	// a control that kills the premise must not also hide which leak assertions it moved.
	if code, body := call(http.MethodPatch, bobBase+"/rows/"+bobRow, `{"values":{"note":"edited"}}`); code != http.StatusOK {
		t.Errorf("[X-PREMISE] PREMISE FAILED: bob cannot patch a row in HIS OWN database (%d %s) — "+
			"he has no edit anywhere, so nothing below means anything", code, body)
	}

	// ── X-HONEST-READ. The gate at alice's database's own address. This is what makes the leak a
	// statement about the PAIR rather than about bob's access: the product already knows bob may
	// not see this data.
	if code, body := call(http.MethodGet, aliceBase+"/rows", ""); code == http.StatusOK {
		t.Errorf("[X-HONEST-READ] bob listed the PRIVATE page's database rows at its own address "+
			"(%d %s) — the db enforcer is not refusing, so the leaks below are a broken gate and "+
			"the fix belongs somewhere else", code, body)
	}

	// ── X-HONEST-WRITE. The gate on the WRITE at that same honest address. X-HONEST-READ is a
	// read; this is what a deleted `.With(dbEnf.Require(AccessEdit))` on the PATCH route moves,
	// and no other assertion here can see that — the store scope would still be satisfied by the
	// honest URL.
	if code, body := call(http.MethodPatch, aliceBase+"/rows/"+aliceRow0, `{"values":{"note":"honest"}}`); code == http.StatusOK ||
		cellOf(t, d, aliceRow0, "note") != "untouched" {
		t.Errorf("[X-HONEST-WRITE] bob patched a row in the PRIVATE page's database at its OWN "+
			"address (%d %s) — the Edit gate on the row-patch route is not refusing", code, body)
	}

	// ── X-LEAK-ROW-WRITE. THE DEFECT. bob names HIS OWN database and alice's row.
	rowCode, rowBody := call(http.MethodPatch, bobBase+"/rows/"+aliceRow, `{"values":{"note":"pwned"}}`)
	if got := cellOf(t, d, aliceRow, "note"); got != "approved" {
		t.Errorf("[X-LEAK-ROW-WRITE] LEAK: bob rewrote a row in ALICE'S PRIVATE database by naming "+
			"HIS OWN database in the URL (%d %s) — the cell now reads %q, and bob cannot even list "+
			"that database's rows", rowCode, rowBody, got)
	}

	// ── X-LEAK-ROW-READ. The disclosure half: UpdateRow returns the MERGED row, so a one-cell
	// PATCH answers with every cell bob never sent. Asserted on the RESPONSE, not on disk — the
	// tamper above is reversible and this is not. It fires only together with X-LEAK-ROW-WRITE
	// (see the header: C2/C3 are both silent, C4 fires both), and that is stated rather than left
	// to be inferred from a control set that never isolates it.
	if strings.Contains(rowBody, aliceSecretCell) {
		t.Errorf("[X-LEAK-ROW-READ] LEAK: the PATCH response handed bob a cell from ALICE'S PRIVATE "+
			"database that he never sent (%d) — the merge reads the whole row and returns it: %s",
			rowCode, rowBody)
	}

	// ── X-LEAK-ROW-DELETE. Destructive, on a SECOND row so a refusal here and a refusal above are
	// never the same object.
	delCode, delBody := call(http.MethodDelete, bobBase+"/rows/"+aliceRow2, "")
	if !rowExistsX(t, d, "database_rows", aliceRow2) {
		t.Errorf("[X-LEAK-ROW-DELETE] LEAK: bob DESTROYED a row in ALICE'S PRIVATE database by "+
			"naming HIS OWN database in the URL (%d %s) — the row is gone", delCode, delBody)
	}

	// ── X-LEAK-VIEW-WRITE. The same seam on the views route — a different table, the same handler
	// shape.
	viewCode, viewBody := call(http.MethodPatch, bobBase+"/views/"+aliceView, `{"name":"pwned"}`)
	if got := viewNameX(t, d, aliceView); got != aliceViewName {
		t.Errorf("[X-LEAK-VIEW-WRITE] LEAK: bob renamed a saved view in ALICE'S PRIVATE database by "+
			"naming HIS OWN database in the URL (%d %s) — the name now reads %q", viewCode, viewBody, got)
	}

	// ── X-LEAK-VIEW-READ. UpdateView RETURNS the merged view, so a rename hands back the fields
	// bob did not send — group_by and hidden_cols NAME COLUMNS of a schema he cannot read.
	if strings.Contains(viewBody, aliceViewGroupBy) {
		t.Errorf("[X-LEAK-VIEW-READ] LEAK: the PATCH response handed bob the group-by column of a "+
			"view in ALICE'S PRIVATE database (%d) — a column name from a schema he cannot read: %s",
			viewCode, viewBody)
	}

	// ── OVER-CORRECTION. The fix must not refuse the honest pair. Each on its OWN object so no
	// one mutation can claim two of them.
	if code, body := call(http.MethodPatch, bobBase+"/rows/"+bobRow2, `{"values":{"note":"ok"}}`); code != http.StatusOK {
		t.Errorf("[X-OWN-ROW-PATCH] OVER-CORRECTION: bob cannot patch a row in HIS OWN database (%d %s)", code, body)
	} else if got := cellOf(t, d, bobRow2, "note"); got != "ok" {
		t.Errorf("[X-OWN-ROW-PATCH] the route answered 200 and the cell still reads %q", got)
	}
	if code, body := call(http.MethodDelete, bobBase+"/rows/"+bobRow3, ""); code != http.StatusOK {
		t.Errorf("[X-OWN-ROW-DELETE] OVER-CORRECTION: bob cannot delete a row in HIS OWN database (%d %s)", code, body)
	} else if rowExistsX(t, d, "database_rows", bobRow3) {
		t.Errorf("[X-OWN-ROW-DELETE] the route answered 200 and the row is still on disk")
	}
	if code, body := call(http.MethodPatch, bobBase+"/views/"+bobView, `{"name":"renamed"}`); code != http.StatusOK {
		t.Errorf("[X-OWN-VIEW-PATCH] OVER-CORRECTION: bob cannot rename a view in HIS OWN database (%d %s)", code, body)
	} else if got := viewNameX(t, d, bobView); got != "renamed" {
		t.Errorf("[X-OWN-VIEW-PATCH] the route answered 200 and the view is still named %q", got)
	}
}

// ─── fixtures, written with SQL so the code under test cannot build its own subject ───

func seedSpaceX(t *testing.T, d *testutil.DB, wsID, creator, name string, private bool) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		 VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		wsID, name, "spx-"+name+"-"+wsID, creator, private).Scan(&id); err != nil {
		t.Fatalf("seed space %q: %v", name, err)
	}
	return id
}

func seedPageX(t *testing.T, d *testutil.DB, wsID, spaceID, author, title string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text, content)
		 VALUES ($1,$2,$3,$4,$5,'body','{"type":"doc","content":[]}') RETURNING id`,
		spaceID, wsID, title, "pgx-"+title+"-"+wsID, author).Scan(&id); err != nil {
		t.Fatalf("seed page %q: %v", title, err)
	}
	return id
}

func seedDatabaseX(t *testing.T, d *testutil.DB, wsID, pageID, name string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO databases (page_id, workspace_id, name, schema) VALUES ($1,$2,$3,'[]') RETURNING id`,
		pageID, wsID, name).Scan(&id); err != nil {
		t.Fatalf("seed database %q: %v", name, err)
	}
	return id
}

func seedRowX(t *testing.T, d *testutil.DB, dbID string, values map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("seed row: marshal: %v", err)
	}
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO database_rows (database_id, values, position) VALUES ($1,$2,0) RETURNING id`,
		dbID, encoded).Scan(&id); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	return id
}

func seedViewX(t *testing.T, d *testutil.DB, dbID, name, groupBy string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO database_views (database_id, name, type, group_by, hidden_cols)
		 VALUES ($1,$2,'table',$3,$4) RETURNING id`,
		dbID, name, groupBy, []string{groupBy}).Scan(&id); err != nil {
		t.Fatalf("seed view %q: %v", name, err)
	}
	return id
}

// ─── oracles, read straight from Postgres rather than through the store the same edit moves ───

func cellOf(t *testing.T, d *testutil.DB, rowID, key string) string {
	t.Helper()
	var v *string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT values ->> $2 FROM database_rows WHERE id = $1`, rowID, key).Scan(&v); err != nil {
		t.Fatalf("read cell %s.%s: %v", rowID, key, err)
	}
	if v == nil {
		return ""
	}
	return *v
}

func viewNameX(t *testing.T, d *testutil.DB, viewID string) string {
	t.Helper()
	var name string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT name FROM database_views WHERE id = $1`, viewID).Scan(&name); err != nil {
		t.Fatalf("read view %s: %v", viewID, err)
	}
	return name
}

func rowExistsX(t *testing.T, d *testutil.DB, table, id string) bool {
	t.Helper()
	var ok bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id=$1)`, id).Scan(&ok); err != nil {
		t.Fatalf("exists %s %s: %v", table, id, err)
	}
	return ok
}
