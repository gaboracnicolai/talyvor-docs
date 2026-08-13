package page

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/docs/internal/testutil"
)

// A COMMITTED SAVE WITH NO RESTORE POINT, UNDER ORDINARY OVERLAP, REPORTED AS SUCCESS.
//
// The snapshot block used to read the next version into Go and then insert it:
//
//	_ = pool.QueryRow(`SELECT COALESCE(MAX(version),0) FROM page_versions WHERE page_id=$1`).Scan(&nextVer)
//	nextVer++
//	_, _ = pool.Exec(`INSERT INTO page_versions (...) VALUES ($1,$2,$3,...)`, id, ws, nextVer, ...)
//
// Saves of one page that overlap all read the same MAX, all pick the same number, and every loser
// violates `UNIQUE (page_id, version)` — which `_, _ =` discarded. Update returned nil, the page
// was saved, and the state it was saved in is in no row of its history. The comment three lines
// above called that history APPEND-ONLY.
//
// ⚠⚠ THE INTERLEAVING IS FORCED, NOT HOPED FOR, AND THAT IS THE WHOLE DESIGN OF THIS TEST. The
// natural race was measured first — 8 unsynchronised saves left 4-6 version rows, 10 runs out of
// 10 losing at least one — and a test built on that measurement PASSED against the unfixed store on
// its first run. A guard whose red depends on scheduling is not a guard; it is a coin. So the store
// is driven through a pgxDB wrapper that holds the FIRST `savers` snapshot writes at a barrier
// until all of them have arrived. Pre-fix that makes every writer read the same MAX before any
// INSERT runs, so the collision is certain rather than likely. The barrier fires ONCE, so the
// fixed store's retries pass straight through it.
//
// ⚠ WHY OVERLAP IS NOT A STRESS CASE HERE: PageView.tsx flushes a title mutation SEPARATELY from
// the content mutation (#99's census of Update's five callers), the collab AutoSaver saves on its
// own debounce, and MCP toolUpdatePage is a third writer. Two of those landing together is a
// Tuesday, not a load test.
func TestConcurrentSaves_EveryCommittedSaveKeepsARestorePoint_RealPG(t *testing.T) {
	const savers = 4

	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pageID := d.Page(t, ws, alice, "Runbook")

	gate := newSnapshotBarrier(d.Pool, savers)
	store := newStore(gate)

	// THE DETERMINISTIC FLOOR RUNS FIRST, AND ITS POSITION IS A CONTROL RESULT RATHER THAN A STYLE
	// CHOICE. It was written last, after the concurrent assertions, and control C3 (no snapshot
	// written at all) showed it never executed: with no INSERT anywhere, the barrier records zero
	// arrivals and the precondition below aborts the test before the floor is reached. A floor that
	// the mutation it exists for cannot reach is decoration. One serial save, asserted before any
	// concurrency, must append exactly one row.
	beforeSerial := versionCount(t, d, pageID)
	if _, err := store.Update(ctx, pageID, map[string]any{
		"content":      `{"type":"doc","rev":"serial"}`,
		"content_text": "serial",
		"updated_by":   alice,
	}); err != nil {
		t.Fatalf("serial save: %v", err)
	}
	if got := versionCount(t, d, pageID) - beforeSerial; got != 1 {
		t.Errorf("[SNAPSHOT-STILL-TAKEN] one serial content save appended %d version rows, want exactly 1", got)
	}

	// Measured, not assumed: the baseline includes whatever the fixture and the serial save above
	// left behind, and a baseline taken on faith would mis-count every assertion below.
	base := versionCount(t, d, pageID)

	gate.arm()
	var wg sync.WaitGroup
	errs := make([]error, savers)
	for i := 0; i < savers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.Update(ctx, pageID, map[string]any{
				"content":      fmt.Sprintf(`{"type":"doc","rev":%d}`, i),
				"content_text": fmt.Sprintf("rev %d", i),
				"updated_by":   alice,
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()

	committed := 0
	for i, err := range errs {
		if err == nil {
			committed++
			continue
		}
		// A save that FAILED owes no restore point. Counting only committed saves keeps this an
		// assertion about history and not about the save path.
		t.Logf("save %d returned an error (it owes no snapshot): %v", i, err)
	}
	if committed == 0 {
		t.Fatalf("precondition: no save committed, so this proves nothing about their snapshots")
	}
	if n := gate.arrivals(); n < savers {
		t.Fatalf("precondition: the barrier saw %d snapshot writes, want %d — the interleaving this "+
			"test depends on did not happen, so a green result would mean nothing", n, savers)
	}

	if got := versionCount(t, d, pageID) - base; got != committed {
		t.Errorf("[NO-LOST-RESTORE-POINT] %d of %d overlapping saves committed and reported success, "+
			"but the append-only history grew by %d — %d committed save(s) left NO restore point. The "+
			"version number was derived in Go, a second writer took it, and the unique violation was "+
			"discarded.", committed, savers, got, committed-got)
	}

	// A repair that dodged the collision by skipping numbers would satisfy the count above and
	// leave holes a "previous version" walk trips on.
	if versions := versionList(t, d, pageID); !dense(versions) {
		t.Errorf("[VERSIONS-DENSE] page_versions holds %v — a history numbered by the table must be "+
			"1..N with no gaps and no repeats", versions)
	}

}

// ─── the forced interleaving ──────────────────────────────

// snapshotBarrier is a pgxDB that delegates everything to a real pool, except that the first `n`
// statements writing into page_versions are held until all `n` have arrived — then all are released
// and the barrier is spent. Blocking happens BEFORE the inner call, so a waiting goroutine holds no
// pooled connection and the pool cannot be starved by the wait.
type snapshotBarrier struct {
	inner pgxDB
	n     int

	mu      sync.Mutex
	armed   bool
	seen    int
	release chan struct{}
	spent   bool
}

// arm opens the barrier for the next cohort. UNARMED IS THE DEFAULT AND THAT IS NOT DEFENSIVE
// PADDING: the deterministic floor above saves the page ONCE, and an always-on barrier held that
// single save waiting for a cohort of four that was never coming. The test hung for five minutes.
func (b *snapshotBarrier) arm() {
	b.mu.Lock()
	b.armed = true
	b.mu.Unlock()
}

func newSnapshotBarrier(inner pgxDB, n int) *snapshotBarrier {
	return &snapshotBarrier{inner: inner, n: n, release: make(chan struct{})}
}

func (b *snapshotBarrier) arrivals() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seen
}

// hold blocks a snapshot write until the cohort is complete. Returns immediately once spent, so the
// fixed store's retry of a contested version number is not re-held.
func (b *snapshotBarrier) hold(sql string) {
	if !strings.Contains(sql, "INSERT INTO page_versions") {
		return
	}
	b.mu.Lock()
	if b.spent || !b.armed {
		b.mu.Unlock()
		return
	}
	b.seen++
	if b.seen >= b.n {
		b.spent = true
		close(b.release)
		b.mu.Unlock()
		return
	}
	ch := b.release
	b.mu.Unlock()
	<-ch
}

func (b *snapshotBarrier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	b.hold(sql)
	return b.inner.Exec(ctx, sql, args...)
}

func (b *snapshotBarrier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return b.inner.Query(ctx, sql, args...)
}

func (b *snapshotBarrier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return b.inner.QueryRow(ctx, sql, args...)
}

// ─── helpers ──────────────────────────────────────────────

func versionCount(t *testing.T, d *testutil.DB, pageID string) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*)::int FROM page_versions WHERE page_id = $1`, pageID).Scan(&n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	return n
}

func versionList(t *testing.T, d *testutil.DB, pageID string) []int {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(),
		`SELECT version FROM page_versions WHERE page_id = $1 ORDER BY version`, pageID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list versions: %v", err)
	}
	return out
}

func dense(vs []int) bool {
	for i, v := range vs {
		if v != i+1 {
			return false
		}
	}
	return len(vs) > 0
}
