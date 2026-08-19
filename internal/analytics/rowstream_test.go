package analytics

// A RESULT STREAM THAT STOPPED EARLY WAS REPORTED AS A COMPLETE ANSWER.
//
// `GetReadStats` runs three statements and iterates two of them. Both loops ran to
// `rows.Next() == false` and then returned `&out, nil` WITHOUT CONSULTING `rows.Err()` — and
// `rows.Next()` returns false for two different reasons: the rows ran out, or the stream broke.
// A broken stream therefore left a SHORT `views_by_day` series and a SHORT `top_viewers` list in
// a 200 response, which the SPA's per-page Analytics screen paints as the readership of the
// document.
//
// ⚠ THE MECHANISM IS MEASURED, NOT ASSUMED. On real Postgres (pgvector/pgvector:pg16, pgx v5,
// the driver this repo ships), a stream that fails part-way delivers its prefix and reports the
// failure only through `Err()`:
//
//	SELECT i, 1/(3-i) FROM generate_series(1,5) i        -> 2 rows delivered, Err() = 22012
//	SET statement_timeout='150ms';
//	SELECT i, pg_sleep(0.05) FROM generate_series(1,20)  -> 2 rows delivered, Err() = 57014
//
// 57014 is the ordinary operational one — a server-side statement timeout, a cancelled query, a
// connection reset mid-send. `Query` had ALREADY returned nil, and every `Scan` had already
// succeeded, so the only thing that ever knew was `Err()`.
//
// ⚠ THIS FILE IS A MOCK GUARD AND SAYS SO. The two shipped statements both end in an aggregate
// with an ORDER BY, which Postgres materialises before sending — measured: 2000 rows delivered,
// Err() nil — so there is no deterministic way to break THESE statements mid-stream on a real
// pool. What is pinned here is the code's RESPONSE to a truncated stream, using pgxmock's
// `CloseError`, whose semantics were checked against the real driver's: rows are delivered, then
// `Next()` is false, then `Err()` is non-nil. The real-Postgres half of the claim is the
// measurement above, which establishes that the response is one this code can actually be given.
//
// ⚠ THE REPOSITORY'S OWN CONVENTION IS THE CONTROL, and it is why these are oversights rather
// than choices. A census of every `for rows.Next()` loop in non-test `internal/` at 3bdb186:
// 36 loops, 33 consult `rows.Err()`. The three that did not are the two here and
// `pagelink.SyncLinks` — whose neighbour twenty lines up, `IssueIDsForPage`, reads the SAME
// table for the SAME roll-up and ends `return out, rows.Err()`.
//
// ⚠ THAT CENSUS IS CLOSED AND RE-MEASURED AT d35f640: **37 loops, 37 consult `rows.Err()`** —
// zero omissions repo-wide. So what follows is not a second copy of the defect above. It is the
// other half of THIS FILE.
//
// ⚠⚠ THIS FILE COVERED THE PER-PAGE ROUTE AND NEITHER LOOP OF THE WORKSPACE ROLL-UP, AND THE
// UNCOVERED HALF IS THE ONE WHERE A TRUNCATION MOVES A FIGURE. `GetWorkspaceStats` iterates two
// streams — the ranked window and the never-read ids — and both DO consult `rows.Err()` today.
// Nothing asserted it. MEASURED by mutation over the FULL suite on real Postgres
// (~/talyvor-queue/w31-mutation-sweep-6d81.py), each check replaced by `if false`:
//
//	GetReadStats      day-bucket loop   -> CAUGHT (TestGetReadStats_ATruncatedDayStreamIsNotAChart)
//	GetReadStats      top-viewers loop  -> CAUGHT (…ATruncatedViewerStreamIsNotARoster)
//	GetWorkspaceStats ranked loop       -> NOT CAUGHT — whole suite green
//	GetWorkspaceStats never-read loop   -> NOT CAUGHT — whole suite green
//
// 2 of 4, in the file whose own subject is exactly this failure. The same shape this repository
// has now learned three times: a guard written against one instance of a class, over a surface
// that had two.
//
// ⚠ AND THE ROLL-UP HALF IS STRICTLY WORSE THAN THE HALF THAT WAS COVERED. Above, a truncated
// stream shortens a CHART and a ROSTER — a list, where an absence at least has somewhere to be
// noticed. In `GetWorkspaceStats` the ranked stream is what `out.TotalViews` is SUMMED FROM
// (`out.TotalViews += r.TotalViews`, one line per surviving row), and the never-read stream is
// what `out.NeverRead` is COUNTED FROM. A prefix therefore does not shorten a list: it produces a
// LOWER NUMBER, and Analytics.tsx paints those two numbers as the tiles "Views (30d)" and
// "Never read". A workspace with 81 views reported as 8 is byte-identical, in a 200, to a
// workspace that had 8 — which is the distinction this package's own `withEmptyLists` note and
// search.Result's three cost fields both exist to hold.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// errStreamBroke stands for the 57014 measured above: the rows the server had already sent are
// in hand, and the failure arrives only through Err().
var errStreamBroke = errors.New("canceling statement due to statement timeout (SQLSTATE 57014)")

// TestGetReadStats_ATruncatedDayStreamIsNotAChart drives the day-bucket read to a prefix.
//
// The fixture is the honest shape of the hazard: the caller asked for 30 days, TWO buckets were
// delivered, and the stream then failed. Nothing downstream can tell that series apart from a
// page that genuinely had traffic on two days.
func TestGetReadStats_ATruncatedDayStreamIsNotAChart(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()

	pool.ExpectQuery(`COUNT.*page_views.*page_id`).
		WithArgs("pg-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{
			"total_views", "unique_viewers", "avg_duration_sec", "last_viewed_at",
		}).AddRow(int(42), int(7), int(95), now))

	pool.ExpectQuery(`DATE_TRUNC.*FROM page_views`).
		WithArgs("pg-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{"date", "count"}).
			AddRow(now.Truncate(24*time.Hour), int(5)).
			AddRow(now.Add(-24*time.Hour).Truncate(24*time.Hour), int(3)).
			CloseError(errStreamBroke))

	// ⚠ NO EXPECTATION FOR THE TOP-VIEWERS STATEMENT, AND THAT IS AN ASSERTION. A read that
	// discovered its own stream was truncated must not go on to run the next one and assemble a
	// half-answer; the ExpectationsWereMet cleanup in newMockStore turns a third statement into
	// a mismatch rather than letting it pass unnoticed.
	got, err := store.GetReadStats(context.Background(), "pg-1", 30)

	// ── [DAY-STREAM-TRUNCATION-IS-AN-ERROR] the finding. ──────────────────────────────
	//
	// ⚠ HOW THIS TAG READS RED BEFORE THE FIX IS NOT `err == nil`, AND SAYING SO IS THE POINT.
	// The unfixed loop sails past the truncation and runs the third statement, which the mock
	// has no expectation for — so it DOES return an error, naming the wrong cause ("top viewers:
	// call to method Query() was not expected"). The assertion is therefore about the error's
	// IDENTITY, not its presence: the read must stop at the loop that broke and report THAT.
	if err == nil {
		t.Fatalf("[DAY-STREAM-TRUNCATION-IS-AN-ERROR] GetReadStats returned nil error with a "+
			"%d-bucket series after the stream broke — a short chart and a complete one are the "+
			"same 200 to the Analytics screen", len(got.ViewsByDay))
	}
	if !errors.Is(err, errStreamBroke) {
		t.Errorf("[DAY-STREAM-TRUNCATION-IS-AN-ERROR] err = %v, want it to wrap the stream "+
			"failure — the day loop must not carry on into the next statement and then blame it",
			err)
	}
	// ⚠ THE STATS MUST NOT COME BACK BESIDE THE ERROR. handler.PageStats maps a non-nil error to
	// 500 and never reads the value, but a returned struct invites the next caller to.
	if got != nil {
		t.Errorf("[DAY-STREAM-TRUNCATION-IS-AN-ERROR] stats = %+v, want nil — a truncated read "+
			"must not hand back the prefix it managed to collect", got)
	}
}

// TestGetReadStats_ATruncatedViewerStreamIsNotARoster is the same failure one statement later.
// It is a SEPARATE loop with a SEPARATE omission, so one fix does not stand in for the other —
// the control harness reverts them independently and each reddens only its own tag.
func TestGetReadStats_ATruncatedViewerStreamIsNotARoster(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()

	pool.ExpectQuery(`COUNT.*page_views.*page_id`).
		WithArgs("pg-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{
			"total_views", "unique_viewers", "avg_duration_sec", "last_viewed_at",
		}).AddRow(int(42), int(7), int(95), now))

	// The day stream is WHOLE here, so this test cannot pass on the previous fix.
	pool.ExpectQuery(`DATE_TRUNC.*FROM page_views`).
		WithArgs("pg-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{"date", "count"}).
			AddRow(now.Truncate(24*time.Hour), int(5)))

	pool.ExpectQuery(`viewer_id.*page_views.*GROUP BY`).
		WithArgs("pg-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{
			"viewer_id", "viewer_name", "view_count", "last_viewed",
		}).
			AddRow("u-1", "Alice", int(12), now).
			CloseError(errStreamBroke))

	got, err := store.GetReadStats(context.Background(), "pg-1", 30)

	// ── [VIEWER-STREAM-TRUNCATION-IS-AN-ERROR] ────────────────────────────────────────
	if err == nil {
		t.Fatalf("[VIEWER-STREAM-TRUNCATION-IS-AN-ERROR] GetReadStats returned nil error with "+
			"%d of the page's viewers after the stream broke — 'who read this document' is "+
			"exactly the list a missing name is invisible in", len(got.TopViewers))
	}
	if !errors.Is(err, errStreamBroke) {
		t.Errorf("[VIEWER-STREAM-TRUNCATION-IS-AN-ERROR] err = %v, want it to wrap the stream "+
			"failure", err)
	}
	if got != nil {
		t.Errorf("[VIEWER-STREAM-TRUNCATION-IS-AN-ERROR] stats = %+v, want nil", got)
	}
}

// TestGetReadStats_AWholeStreamStillAnswers is the vacuity floor under both tags above.
//
// ⚠ WITHOUT IT THE CHEAPEST WAY TO GO GREEN IS `return nil, errors.New("no")`. Every assertion in
// this file is that a read FAILS; this one is the only assertion that it ever SUCCEEDS, and it
// asserts the payload, not just the nil error — a fix that erred on the last row of a healthy
// stream (`if rows.Err() != nil` written as `if rows.Err() == nil`) would pass a bare nil-error
// check on the mock's first statement and be caught here by the missing buckets.
func TestGetReadStats_AWholeStreamStillAnswers(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()

	pool.ExpectQuery(`COUNT.*page_views.*page_id`).
		WithArgs("pg-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{
			"total_views", "unique_viewers", "avg_duration_sec", "last_viewed_at",
		}).AddRow(int(42), int(7), int(95), now))
	pool.ExpectQuery(`DATE_TRUNC.*FROM page_views`).
		WithArgs("pg-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{"date", "count"}).
			AddRow(now.Truncate(24*time.Hour), int(5)).
			AddRow(now.Add(-24*time.Hour).Truncate(24*time.Hour), int(3)))
	pool.ExpectQuery(`viewer_id.*page_views.*GROUP BY`).
		WithArgs("pg-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{
			"viewer_id", "viewer_name", "view_count", "last_viewed",
		}).
			AddRow("u-1", "Alice", int(12), now).
			AddRow("u-2", "Bob", int(8), now))

	got, err := store.GetReadStats(context.Background(), "pg-1", 30)
	if err != nil {
		t.Fatalf("[UNBROKEN-STREAM-STILL-ANSWERS] GetReadStats: %v — an unbroken read must not "+
			"be turned into a 500 by the truncation check", err)
	}
	if got == nil {
		t.Fatalf("[UNBROKEN-STREAM-STILL-ANSWERS] stats = nil for a healthy read")
	}
	if len(got.ViewsByDay) != 2 {
		t.Errorf("[UNBROKEN-STREAM-STILL-ANSWERS] views_by_day = %d buckets, want 2 — the whole "+
			"series must survive the check", len(got.ViewsByDay))
	}
	if len(got.TopViewers) != 2 {
		t.Errorf("[UNBROKEN-STREAM-STILL-ANSWERS] top_viewers = %d, want 2", len(got.TopViewers))
	}
}

// TestGetReadStats_TruncationDoesNotBorrowTheScanCheck is the blindness control, in the file
// rather than only in the harness.
//
// `rows.Scan` failing and `rows.Err()` being set are DIFFERENT failures with different causes,
// and the loop already handled the first. If a reader ever concludes "Scan covers it", this is
// the row that says otherwise: a value the destination cannot take must still be refused, and
// refused as ITSELF rather than as the stream error.
//
// ⚠⚠ IT BREAKS THE **LAST** STATEMENT, AND THAT IS NOT COSMETIC — THE FIRST VERSION OF THIS TEST
// COULD NOT FAIL. It fed the bad row to the day-bucket read, which is the SECOND of three
// statements. Delete the loop's Scan branch and the bad row is silently absorbed, the read
// carries on to the third statement, the mock has no expectation for it, and an error comes back
// anyway — from the harness, not from the code. Control F7 in
// ~/talyvor-queue/w31-rowstream-controls-7a2c.py deleted that branch and this test STAYED GREEN.
// Breaking the top-viewers read instead leaves nothing after it to manufacture an error, so
// `err == nil` is reachable and F7 now reddens this tag alone.
func TestGetReadStats_TruncationDoesNotBorrowTheScanCheck(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()

	pool.ExpectQuery(`COUNT.*page_views.*page_id`).
		WithArgs("pg-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{
			"total_views", "unique_viewers", "avg_duration_sec", "last_viewed_at",
		}).AddRow(int(42), int(7), int(95), now))
	pool.ExpectQuery(`DATE_TRUNC.*FROM page_views`).
		WithArgs("pg-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{"date", "count"}).
			AddRow(now.Truncate(24*time.Hour), int(5)))
	// view_count is an int column and this row carries text. The stream is WHOLE — no
	// CloseError — so the only thing that can refuse it is the Scan branch.
	pool.ExpectQuery(`viewer_id.*page_views.*GROUP BY`).
		WithArgs("pg-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{
			"viewer_id", "viewer_name", "view_count", "last_viewed",
		}).AddRow("u-1", "Alice", "not-an-int", now))

	_, err := store.GetReadStats(context.Background(), "pg-1", 30)
	if err == nil {
		t.Fatalf("[SCAN-IS-A-DIFFERENT-FAILURE] a row the destination cannot take was accepted " +
			"into top_viewers — the rows.Err() check does not stand in for the Scan check")
	}
	if errors.Is(err, errStreamBroke) || strings.Contains(err.Error(), "57014") {
		t.Errorf("[SCAN-IS-A-DIFFERENT-FAILURE] a Scan failure surfaced as the stream error — "+
			"the two must not be conflated: %v", err)
	}
}

// ─── the workspace roll-up: the half this file did not cover ──────────────────────────────
//
// TestGetWorkspaceStats_ATruncatedRankedStreamIsNotARollUp drives the ranked window to a prefix.
//
// The ranked stream is the FIRST of the roll-up's three statements and the one `out.TotalViews`
// is summed from, so a prefix understates the headline tile by exactly the rows that never
// arrived.
//
// ⚠ NO EXPECTATION IS REGISTERED FOR THE TWO STATEMENTS AFTER IT, AND THAT IS AN ASSERTION —
// the same one the day-bucket test above makes. A read that discovered its own stream was
// truncated must stop, not carry on and assemble a roll-up out of what it managed to collect.
//
// ⚠⚠ HOW THIS TAG READS RED IS `errors.Is`, NOT `err == nil`, AND THE DISTINCTION IS THE WHOLE
// LESSON OF THIS FILE (see TruncationDoesNotBorrowTheScanCheck). With the check removed, the loop
// sails past the truncation into the unique-viewers statement, which the mock has no expectation
// for — so an error DOES come back, from the HARNESS, naming the wrong cause ("analytics: unique
// viewers: call to method Query() was not expected"). A test that only asked "was there an error"
// would be GREEN over the defect. Measured, not reasoned: control C1 in
// ~/talyvor-queue/w31-rollupstream-controls-6d81.py reddens exactly this tag and reddens it on
// the identity assertion.
func TestGetWorkspaceStats_ATruncatedRankedStreamIsNotARollUp(t *testing.T) {
	store, pool := newMockStore(t)
	store.WithPageRead(allowAllPages{})
	lastSeen := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	// Two rows delivered, then the stream fails. 50 + 30 = 80 is the total a caller would be
	// handed; nothing downstream can tell it from a workspace that genuinely saw 80 views.
	pool.ExpectQuery(`(?i)page_id.*group by.*order by count.*desc`).
		WithArgs("ws-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{
			"page_id", "title", "view_count", "unique_viewers", "avg_duration_sec", "last_viewed",
		}).
			AddRow("pg-1", "Top", int(50), int(7), int(41), lastSeen).
			AddRow("pg-2", "Second", int(30), int(5), int(33), lastSeen.Add(-time.Hour)).
			CloseError(errStreamBroke))

	got, err := store.GetWorkspaceStats(context.Background(), "ws-1", 30)

	// ── [RANKED-STREAM-TRUNCATION-IS-AN-ERROR] ────────────────────────────────────────
	if err == nil {
		t.Fatalf("[RANKED-STREAM-TRUNCATION-IS-AN-ERROR] GetWorkspaceStats returned nil error "+
			"with total_views = %d after the ranked stream broke — the SPA paints that number as "+
			"\"Views (30d)\", and an understated one is a measurement, not an absence",
			got.TotalViews)
	}
	if !errors.Is(err, errStreamBroke) {
		t.Errorf("[RANKED-STREAM-TRUNCATION-IS-AN-ERROR] err = %v, want it to wrap the stream "+
			"failure — the ranked loop must not carry on into the unique-viewers statement and "+
			"then blame it", err)
	}
	if got != nil {
		t.Errorf("[RANKED-STREAM-TRUNCATION-IS-AN-ERROR] stats = %+v, want nil — a truncated "+
			"roll-up must not hand back the prefix it managed to sum", got)
	}
}

// TestGetWorkspaceStats_ATruncatedNeverReadStreamIsNotACount is the same failure two statements
// later, and it is a SEPARATE loop with a SEPARATE check — one fix does not stand in for the
// other. The ranked and unique-viewer streams are WHOLE here, so this cannot pass on the fix above.
//
// ⚠ THIS ONE READS RED ON `err == nil` FOR REAL, and it is the only place in this file where that
// is true. The never-read stream is the LAST statement, so there is nothing after it for the mock
// to manufacture an error out of: with the check removed the method returns a fully-formed
// `*WorkspaceReadStats` and a nil error — the actual 200 a caller receives. The tag therefore
// prints the fabricated figure, which is the finding stated in the units the screen uses.
//
// ⚠ `never_read_count` IS THE TILE THE UI MARKS `tone="warning"` (Analytics.tsx). Truncation
// pushes it DOWN — toward "nothing needs attention" — so the failure is silent in the direction a
// reader is least likely to question.
func TestGetWorkspaceStats_ATruncatedNeverReadStreamIsNotACount(t *testing.T) {
	store, pool := newMockStore(t)
	store.WithPageRead(allowAllPages{})
	lastSeen := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	pool.ExpectQuery(`(?i)page_id.*group by.*order by count.*desc`).
		WithArgs("ws-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{
			"page_id", "title", "view_count", "unique_viewers", "avg_duration_sec", "last_viewed",
		}).AddRow("pg-1", "Top", int(50), int(7), int(41), lastSeen))

	pool.ExpectQuery(`(?i)count\(distinct viewer_id\).*page_id = any`).
		WithArgs("ws-1", 30, []string{"pg-1"}).
		WillReturnRows(pgxmock.NewRows([]string{"unique_viewers"}).AddRow(int(15)))

	// Two ids delivered, then the stream fails. The workspace's real never-read cohort is
	// unknowable from here — which is the point: 2 is not it, and 2 is what gets counted.
	pool.ExpectQuery(`(?i)select p\.id from pages p.*left join page_views`).
		WithArgs("ws-1").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).
			AddRow("pg-a").
			AddRow("pg-b").
			CloseError(errStreamBroke))

	got, err := store.GetWorkspaceStats(context.Background(), "ws-1", 30)

	// ── [NEVER-READ-STREAM-TRUNCATION-IS-AN-ERROR] ────────────────────────────────────
	if err == nil {
		t.Fatalf("[NEVER-READ-STREAM-TRUNCATION-IS-AN-ERROR] GetWorkspaceStats returned nil error "+
			"with never_read_count = %d after the id stream broke — that tile is rendered as a "+
			"warning, and a truncated stream moves it DOWN, toward \"nothing needs attention\"",
			got.NeverRead)
	}
	if !errors.Is(err, errStreamBroke) {
		t.Errorf("[NEVER-READ-STREAM-TRUNCATION-IS-AN-ERROR] err = %v, want it to wrap the stream "+
			"failure", err)
	}
	if got != nil {
		t.Errorf("[NEVER-READ-STREAM-TRUNCATION-IS-AN-ERROR] stats = %+v, want nil — a roll-up "+
			"whose last read broke must not answer with numbers", got)
	}
}
