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
