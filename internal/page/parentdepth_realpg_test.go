package page_test

// MaxDepth IS NOT A CAP ON THE TREE. IT IS A CAP ON ONE DOOR, AND THE OTHER DOOR MOVES THE
// NUMBER THAT DOOR IS CHECKED AGAINST.
//
// `depth` is derived in Create — `parent.depth + 1`, refused above MaxDepth — and nowhere else.
// `parent_id` is in Update's allowlist and Update did not recompute it. So a PATCH that moves a
// page under a deep parent left the moved page reporting the depth it had somewhere else, and
// Create then read THAT number to decide whether the next child was legal.
//
// ⚠⚠ THE CAP IS NOT BYPASSED BY ONE LEVEL. IT IS BYPASSED WITHOUT BOUND, THROUGH THE SHIPPED
// API, USING NOTHING BUT CREATE AND PATCH. Measured on real Postgres at ceb9dab before a line
// was written (probe log, and [UNBOUNDED] below re-runs the same sequence as an assertion):
//
//	Create refuses a 6th level:            "page: max depth 5 exceeded"   ← the cap works HERE
//	PATCH X under the depth-5 page:        accepted; X reports depth 0, TRUE depth 6
//	X's untouched child:                   reports depth 1, TRUE depth 7
//	Create a child under X:                gets depth 1 (parent said 0), TRUE depth 7
//	five more Creates:                     reports depth 5, TRUE depth 11
//	one more PATCH + five more Creates:    reports depth 5, TRUE depth 17
//
// Each lap adds MaxDepth levels for one PATCH, and the reported depth never exceeds 5, so
// nothing that reads the column can see it happening. `store.go`'s own sentence — "MaxDepth caps
// the parent_id chain so a malicious caller can't build a deeply-recursive tree that breaks
// rendering" — is false as written, and it is false in the direction the sentence is about.
//
// ⚠ WHAT IS *NOT* CLAIMED. No walker in this repo recurses the parent chain today (export
// gathers one level, internal/mcp's byParent is a one-level map, Create reads the parent once),
// so this is not a hang. It is a cap that does not cap, a column whose value is wrong on the
// wire (`Page.depth` is in the API type, the MCP page projection and List's ORDER BY), and the
// arithmetic every future recursive read will inherit.
//
// ⚠ THE FIX REFUSES, AND WHAT IT REFUSES IS THE RESULT, NOT THE STATE. A move is judged by where
// the moved page AND EVERY PAGE UNDER IT would land — [SUBTREE-CAP] is a move whose own new
// depth is legal and whose grandchild's is not. Unparenting is NEVER refused ([UNPARENT],
// [DISMANTLE]): parent_id → NULL puts every node in the subtree at or above where it already
// was, so it cannot be the operation that breaks the cap, and it is the only repair available
// for the trees the defect already wrote to disk.
//
// ⚠ AND THE DEPTH REWRITE MUST NOT TOUCH `updated_at` — [CLOCK]. Writing a descendant's row is
// exactly the shape that made Delete's re-parent the fourth copy of the staleness seam
// (reparent_staleness_realpg_test.go, and the RecordView / PriceAISpend / analytics.RecordView
// blocks before it). A move re-dating every page under the moved one would be the fifth, and it
// would be introduced BY THE FIX. The moved page itself still bumps the column — that is
// pre-existing and deliberate; its descendants, whose authors did nothing, must not.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// depthFixture holds the per-test scaffolding the cases share.
type depthFixture struct {
	t     *testing.T
	d     *testutil.DB
	ctx   context.Context
	ws    string
	space string
	who   string
	store *page.Store
	n     int // slug uniqueness: pages carry a UNIQUE (space_id, slug)
}

func newDepthFixture(t *testing.T) *depthFixture {
	t.Helper()
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	who := d.Member(t, ws, "alice@corp.com")
	var space string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO spaces (workspace_id, name, slug, created_by)
		 VALUES ($1, 'Depth', 'depth-'||md5(random()::text), $2) RETURNING id`,
		ws, who).Scan(&space); err != nil {
		t.Fatalf("seed space: %v", err)
	}
	return &depthFixture{t: t, d: d, ctx: ctx, ws: ws, space: space, who: who, store: page.NewStore(d.Pool)}
}

func (f *depthFixture) create(title string, parent *string) *model.Page {
	f.t.Helper()
	f.n++
	p, err := f.store.Create(f.ctx, model.Page{
		SpaceID: f.space, WorkspaceID: f.ws, Title: fmt.Sprintf("%s %d", title, f.n),
		CreatedBy: f.who, ParentID: parent,
	})
	if err != nil {
		f.t.Fatalf("create %s: %v", title, err)
	}
	return p
}

// chain builds n levels under the space root and returns every page, deepest last.
func (f *depthFixture) chain(n int) []*model.Page {
	f.t.Helper()
	out := []*model.Page{f.create("root", nil)}
	for i := 1; i <= n; i++ {
		out = append(out, f.create(fmt.Sprintf("c%d", i), &out[len(out)-1].ID))
	}
	return out
}

func (f *depthFixture) move(id, parent string) error {
	f.t.Helper()
	_, err := f.store.UpdateInWorkspaces(f.ctx, id, map[string]any{"parent_id": parent}, []string{f.ws})
	return err
}

// trueDepthOf walks parent_id upward and reports the REAL number of hops to a root, which is
// the number `depth` is supposed to be and the number no code in this repo consults.
func (f *depthFixture) trueDepthOf(id string) int {
	f.t.Helper()
	var n int
	if err := f.d.Pool.QueryRow(f.ctx, `
		WITH RECURSIVE up AS (
			SELECT id, parent_id, 0 AS hops FROM pages WHERE id = $1
			UNION ALL
			SELECT p.id, p.parent_id, up.hops + 1 FROM pages p JOIN up ON up.parent_id = p.id
		) CYCLE id SET is_cycle USING path
		SELECT max(hops) FROM up`, id).Scan(&n); err != nil {
		f.t.Fatalf("trueDepth(%s): %v", id, err)
	}
	return n
}

func (f *depthFixture) recordedDepthOf(id string) int {
	f.t.Helper()
	var n int
	if err := f.d.Pool.QueryRow(f.ctx, `SELECT depth FROM pages WHERE id = $1`, id).Scan(&n); err != nil {
		f.t.Fatalf("recordedDepth(%s): %v", id, err)
	}
	return n
}

func (f *depthFixture) parentOf(id string) *string {
	f.t.Helper()
	var p *string
	if err := f.d.Pool.QueryRow(f.ctx, `SELECT parent_id FROM pages WHERE id = $1`, id).Scan(&p); err != nil {
		f.t.Fatalf("parentOf(%s): %v", id, err)
	}
	return p
}

// ── [CAP-ON-MOVE] ───────────────────────────────────────────────────────────────────────────
// The move door must apply the cap the create door applies.
func TestMovingAPageUnderADeepParent_IsRefused_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	c := f.chain(page.MaxDepth) // root(0) … c5(5)
	deepest := c[len(c)-1]
	x := f.create("X", nil)

	err := f.move(x.ID, deepest.ID)
	if err == nil {
		t.Errorf("[CAP-ON-MOVE] moving a page under a depth-%d parent was ACCEPTED; true depth now %d, cap %d",
			deepest.Depth, f.trueDepthOf(x.ID), page.MaxDepth)
	} else if !errors.Is(err, page.ErrMaxDepth) {
		t.Errorf("[CAP-ON-MOVE] refused, but not with ErrMaxDepth: %v", err)
	}
	// A refusal must not half-apply: the link is not written.
	if p := f.parentOf(x.ID); p != nil {
		t.Errorf("[CAP-ON-MOVE] refused the move and wrote the link anyway: parent_id = %q", *p)
	}
}

// ── [SUBTREE-CAP] ───────────────────────────────────────────────────────────────────────────
// A move whose OWN new depth is legal and whose descendant's is not. This is the case a check
// written only about the moved page cannot see.
func TestMovingASubtree_IsJudgedByItsDeepestPage_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	c := f.chain(page.MaxDepth) // root(0) … c5(5)

	// X with two levels under it: X, XC, XGC — height 2.
	x := f.create("X", nil)
	xc := f.create("XC", &x.ID)
	xgc := f.create("XGC", &xc.ID)

	// Under c4 (depth 4): X would be 5 — legal on its own — and XGC would be 7.
	if err := f.move(x.ID, c[4].ID); err == nil {
		t.Errorf("[SUBTREE-CAP] accepted a move that puts a grandchild at depth %d (cap %d); X itself would be legal at 5",
			f.trueDepthOf(xgc.ID), page.MaxDepth)
	} else if !errors.Is(err, page.ErrMaxDepth) {
		t.Errorf("[SUBTREE-CAP] refused, but not with ErrMaxDepth: %v", err)
	}

	// The boundary, from the other side: under c2 (depth 2) the deepest lands exactly on the cap.
	if err := f.move(x.ID, c[2].ID); err != nil {
		t.Fatalf("[SUBTREE-CAP] refused a move whose deepest page lands exactly on the cap: %v", err)
	}
	if got, want := f.recordedDepthOf(xgc.ID), page.MaxDepth; got != want {
		t.Errorf("[SUBTREE-CAP] boundary move: grandchild records depth %d, want %d", got, want)
	}
}

// ── [DEPTH-REWRITTEN] ───────────────────────────────────────────────────────────────────────
// A legal move must leave every page under it reporting the truth, or the next Create inherits
// the lie.
func TestALegalMove_RewritesTheWholeSubtreesDepth_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	c := f.chain(2) // root(0), c1(1), c2(2)
	x := f.create("X", nil)
	xc := f.create("XC", &x.ID)
	xgc := f.create("XGC", &xc.ID)

	if err := f.move(x.ID, c[2].ID); err != nil {
		t.Fatalf("[DEPTH-REWRITTEN] legal move refused: %v", err)
	}
	for _, tc := range []struct {
		name string
		id   string
		want int
	}{
		{"the moved page", x.ID, 3},
		{"its child", xc.ID, 4},
		{"its grandchild", xgc.ID, 5},
	} {
		if got := f.recordedDepthOf(tc.id); got != tc.want {
			t.Errorf("[DEPTH-REWRITTEN] %s records depth %d, want %d (true depth %d)",
				tc.name, got, tc.want, f.trueDepthOf(tc.id))
		}
	}
	// And the returned row agrees with the table — a caller that trusts the response must not
	// be handed the pre-move number.
	got, err := f.store.UpdateInWorkspaces(f.ctx, x.ID, map[string]any{"title": "same place"}, []string{f.ws})
	if err != nil {
		t.Fatalf("[DEPTH-REWRITTEN] follow-up update: %v", err)
	}
	if got.Depth != 3 {
		t.Errorf("[DEPTH-REWRITTEN] the materialised row reports depth %d, want 3", got.Depth)
	}
}

// ── [UNBOUNDED] ─────────────────────────────────────────────────────────────────────────────
// The headline, as an assertion: drive the exact Create+PATCH sequence that reached true depth
// 17 at ceb9dab and assert that nothing in the space ever ends up deeper than the cap.
func TestTheCapCannotBeDrivenPastItselfWithCreateAndPatch_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	c := f.chain(page.MaxDepth)
	tip := c[len(c)-1]

	// Two laps of "move a fresh root under the tip, then create under it until the cap".
	for lap := 1; lap <= 2; lap++ {
		fresh := f.create(fmt.Sprintf("lap%d", lap), nil)
		if err := f.move(fresh.ID, tip.ID); err == nil {
			// If the move is allowed at all, the fix must have made room for it.
			if d := f.trueDepthOf(fresh.ID); d > page.MaxDepth {
				t.Fatalf("[UNBOUNDED] lap %d: move accepted and landed at true depth %d, cap %d", lap, d, page.MaxDepth)
			}
		}
		// Whatever the move did, keep creating as deep as Create will allow.
		cur := fresh
		for i := 0; i < page.MaxDepth+2; i++ {
			p, err := f.store.Create(f.ctx, model.Page{
				SpaceID: f.space, WorkspaceID: f.ws, Title: fmt.Sprintf("l%d-%d", lap, i),
				CreatedBy: f.who, ParentID: &cur.ID,
			})
			if err != nil {
				break // Create hit the cap, which is the point.
			}
			cur = p
		}
		tip = cur
	}

	var deepest int
	if err := f.d.Pool.QueryRow(f.ctx, `
		WITH RECURSIVE up AS (
			SELECT id, parent_id, 0 AS hops FROM pages WHERE space_id = $1
			UNION ALL
			SELECT u.id, p.parent_id, u.hops + 1 FROM pages p JOIN up u ON u.parent_id = p.id
		) CYCLE id SET is_cycle USING path
		SELECT coalesce(max(hops), 0) FROM up`, f.space).Scan(&deepest); err != nil {
		t.Fatalf("[UNBOUNDED] measure deepest chain: %v", err)
	}
	if deepest > page.MaxDepth {
		t.Errorf("[UNBOUNDED] deepest TRUE chain in the space is %d with a cap of %d — Create and PATCH together defeat it",
			deepest, page.MaxDepth)
	}
}

// ── [CLOCK] ─────────────────────────────────────────────────────────────────────────────────
// The depth rewrite writes rows belonging to pages nobody edited. It must not re-date them:
// GetStalePages keys on pages.updated_at and feeds the stale screen, the freshness digest,
// GET /workspaces/{wsID}/stale and the MCP get_stale_pages tool.
func TestMovingAPage_DoesNotResetItsDescendantsStalenessClock_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	c := f.chain(2)
	x := f.create("X", nil)
	xc := f.create("XC", &x.ID)

	old := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	if _, err := f.d.Pool.Exec(f.ctx, `UPDATE pages SET updated_at = $1 WHERE id = $2`, old, xc.ID); err != nil {
		t.Fatalf("[CLOCK] seed: %v", err)
	}
	if err := f.move(x.ID, c[2].ID); err != nil {
		t.Fatalf("[CLOCK] legal move refused: %v", err)
	}
	var after time.Time
	if err := f.d.Pool.QueryRow(f.ctx, `SELECT updated_at FROM pages WHERE id = $1`, xc.ID).Scan(&after); err != nil {
		t.Fatalf("[CLOCK] read back: %v", err)
	}
	if !after.UTC().Equal(old) {
		t.Errorf("[CLOCK] moving the PARENT re-dated the child: %s -> %s; its freshness clock was reset by somebody else's move",
			old.Format(time.RFC3339), after.UTC().Format(time.RFC3339))
	}
	// …and the child's depth was still corrected, so [CLOCK] cannot be satisfied by doing nothing.
	if got := f.recordedDepthOf(xc.ID); got != 4 {
		t.Errorf("[CLOCK] child records depth %d, want 4 — the rewrite has to happen, just not through updated_at", got)
	}
}

// ── [UNPARENT] ──────────────────────────────────────────────────────────────────────────────
// parent_id → NULL is never refused and re-bases the whole subtree at 0.
//
// ⚠ THE SUBTREE IS PLACED BY Create, NOT BY A MOVE, AND THAT IS WHAT MAKES THIS CASE RED-FIRST.
// Its first version built the same shape with a PATCH, so both pages were ALREADY sitting on
// the stale depths the unparent was supposed to produce and the case passed at ceb9dab without
// an unparent doing anything at all. Create derives depth correctly, so seeding through it is
// the only way to ask this question with a wrong answer available.
func TestUnparenting_IsNeverRefusedAndRebasesTheSubtree_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	c := f.chain(3)
	x := f.create("X", &c[3].ID) // depth 4, derived by Create
	xc := f.create("XC", &x.ID)  // depth 5, derived by Create
	if _, err := f.store.UpdateInWorkspaces(f.ctx, x.ID, map[string]any{"parent_id": nil}, []string{f.ws}); err != nil {
		t.Fatalf("[UNPARENT] unparent refused: %v", err)
	}
	if got := f.recordedDepthOf(x.ID); got != 0 {
		t.Errorf("[UNPARENT] the unparented page records depth %d, want 0", got)
	}
	if got := f.recordedDepthOf(xc.ID); got != 1 {
		t.Errorf("[UNPARENT] its child records depth %d, want 1", got)
	}
}

// ── [DISMANTLE] ─────────────────────────────────────────────────────────────────────────────
// The trees the defect already wrote are on disk and no fix repairs them retroactively. The
// refusal must not TRAP them: unparenting is always available, so a too-deep subtree can be
// taken apart. Seeded with raw SQL because the store can no longer build one.
func TestATreeAlreadyDeeperThanTheCap_CanStillBeTakenApart_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	root := f.create("root", nil)
	// Nine levels, every row claiming depth 0 — exactly what the defect left behind.
	ids := []string{root.ID}
	for i := 1; i <= 9; i++ {
		var id string
		if err := f.d.Pool.QueryRow(f.ctx,
			`INSERT INTO pages (space_id, workspace_id, parent_id, title, slug, created_by, depth)
			 VALUES ($1, $2, $3, $4, $5, $6, 0) RETURNING id`,
			f.space, f.ws, ids[len(ids)-1], fmt.Sprintf("legacy%d", i),
			fmt.Sprintf("legacy-%d", i), f.who).Scan(&id); err != nil {
			t.Fatalf("[DISMANTLE] seed: %v", err)
		}
		ids = append(ids, id)
	}
	mid := ids[5] // true depth 5, with four levels under it
	if _, err := f.store.UpdateInWorkspaces(f.ctx, mid, map[string]any{"parent_id": nil}, []string{f.ws}); err != nil {
		t.Fatalf("[DISMANTLE] a too-deep subtree could not be unparented — the refusal traps the data it inherited: %v", err)
	}
	if got := f.trueDepthOf(ids[9]); got > page.MaxDepth {
		t.Errorf("[DISMANTLE] after unparenting the middle, the deepest legacy page is still at true depth %d", got)
	}
	// ⚠ THIS ASSERTION EXISTS BECAUSE A CONTROL SAID THE CASE WAS NOT EARNING ITS NAME. C5 drops
	// the unparent re-base entirely and [DISMANTLE] stayed SILENT: the two assertions above ask
	// only "was it refused" and "what does parent_id say", and neither can see a stale depth. The
	// legacy rows all claim depth 0, so the repair is only a repair if the column is corrected
	// too — otherwise the next Create under them derives from 0 again and rebuilds the tree the
	// unparent just took apart.
	if got := f.recordedDepthOf(ids[9]); got != 4 {
		t.Errorf("[DISMANTLE] the unparented legacy subtree still records depth %d for its deepest page, want 4 — taken apart on disk, not in the column", got)
	}
}

// ── [NO-HANG] ───────────────────────────────────────────────────────────────────────────────
// Rings written before #118's guard are still on disk, and a ring member is the one page whose
// descendant walk can revisit itself. Re-parenting a ring member is also the operation that
// REPAIRS a ring, so it must terminate. (parentcycle_realpg_test.go makes the same argument
// about the cycle predicate; this is the depth walk, which carries a per-row counter and would
// therefore never dedupe.)
func TestMovingARingMember_Terminates_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	root := f.create("root", nil)
	ringX := f.create("ringX", nil)
	ringY := f.create("ringY", &ringX.ID)
	if _, err := f.d.Pool.Exec(f.ctx, `UPDATE pages SET parent_id = $1 WHERE id = $2`, ringY.ID, ringX.ID); err != nil {
		t.Fatalf("[NO-HANG] seed ring: %v", err)
	}
	deadline, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancel()
	if _, err := f.store.UpdateInWorkspaces(deadline, ringX.ID, map[string]any{"parent_id": root.ID}, []string{f.ws}); err != nil {
		t.Fatalf("[NO-HANG] moving a ring member out did not complete: %v", err)
	}
	if p := f.parentOf(ringX.ID); p == nil || *p != root.ID {
		t.Errorf("[NO-HANG] the repair did not land")
	}
}

// ── [SHALLOW-MOVE-STILL-WORKS] / [CREATE-DOOR-STILL-CAPPED] ─────────────────────────────────
// The vacuity floor. A gate that refuses every move would pass every case above.
func TestOrdinaryMovesAndCreatesAreUnaffected_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	c := f.chain(2)
	x := f.create("X", nil)

	got, err := f.store.UpdateInWorkspaces(f.ctx, x.ID, map[string]any{"parent_id": c[1].ID}, []string{f.ws})
	if err != nil {
		t.Fatalf("[SHALLOW-MOVE-STILL-WORKS] an ordinary move was refused: %v", err)
	}
	if got.Depth != 2 {
		t.Errorf("[SHALLOW-MOVE-STILL-WORKS] moved page reports depth %d, want 2", got.Depth)
	}

	// The create door's cap has never been pinned by anything in this repo — a whole-tree grep
	// for MaxDepth at ceb9dab found the const, its one use, and a comment. It is pinned here so
	// a change to the shared refusal cannot silently open the door this test is about.
	deep := f.chain(page.MaxDepth)
	if _, err := f.store.Create(f.ctx, model.Page{
		SpaceID: f.space, WorkspaceID: f.ws, Title: "one too far", CreatedBy: f.who,
		ParentID: &deep[len(deep)-1].ID,
	}); !errors.Is(err, page.ErrMaxDepth) {
		t.Errorf("[CREATE-DOOR-STILL-CAPPED] Create at depth %d returned %v, want ErrMaxDepth", page.MaxDepth+1, err)
	}
}
