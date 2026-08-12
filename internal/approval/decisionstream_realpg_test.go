package approval_test

import (
	"context"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/approval"
	"github.com/talyvor/docs/internal/testutil"
)

// A REVIEWER'S REJECTION WAS IN THE TABLE AND THE DOCUMENT WAS MARKED APPROVED.
//
// Store.aggregate counts the review_decisions rows for one request and returns the verdict
// Decide then WRITES to approval_requests.status and pages.doc_status:
//
//	rows, err := s.pool.Query(ctx, `SELECT decision FROM review_decisions WHERE request_id = $1`)
//	for rows.Next() { ... approved++ | rejected++ | pending++ }
//	if rejected > 0            { return ApprovalRejected }
//	if pending == 0 && approved > 0 { return ApprovalApproved }
//
// rows.Err() was never read. When Postgres raises an error WHILE the rows are streaming, pgx
// hands the caller every row produced so far, each with a nil Scan error, and then Next()
// returns false exactly as it does at a clean end of stream. The ONLY place the failure is
// visible is rows.Err(). So a truncated read is indistinguishable from a complete one — and
// the counters make that failure lean one way: rows that never arrive cannot be `rejected`
// and cannot be `pending`, so a partial stream can only ever move the verdict TOWARD approval.
// A rejection in the tail is a rejection that did not happen.
//
// MEASURED, NOT ARGUED, before the fix (real Postgres, the shipped Decide, four reviewers —
// two approved, one REJECTED, one deciding):
//
//	the SELECT delivered 2 rows, then rows.Err() = division by zero (SQLSTATE 22012)
//	Decide returned                 <nil>
//	approval_requests.status =      "approved"
//	pages.doc_status =              "approved"
//	the table still said            dave = rejected
//
// ⚠ WHAT THE FAULT INJECTION MODELS, said rather than implied. The fixture makes the error
// deterministic with a per-row expression that divides by zero on the rejected row (the
// divisor derives from the row's own data, so the planner cannot constant-fold it — a folded
// 1/0 raises at PLAN time, delivers zero rows, and would exercise a different path entirely).
// The production faults with this exact protocol shape are ordinary ones: statement_timeout
// firing mid-scan, the backend terminated by an admin or a failover, a connection reset, an
// I/O error on a heap page, or a recovery conflict on a replica. Every one of them ends the
// stream after N rows with the reason in rows.Err() and nowhere else. What is asserted below
// is the loop's response to that condition, which is identical for all of them.
//
// ⚠ [TRUNCATION-IS-REAL] IS THE VACUITY FLOOR AND IT IS NOT DECORATION. A fault that stops
// firing turns this whole file into a test that an ordinary approval works. Control E5 disarms
// the injected expression and leaves the production code untouched; the floor fires — AND SO
// DOES [NO-VERDICT-FROM-PARTIAL], which was predicted to stay green and did not. That is the
// useful part, recorded rather than tidied away: a complete stream is one Decide is RIGHT to
// answer nil for, so the error assertion cannot by itself tell a working fix from an absent
// fault. Only the floor can. The two firing together is the signature of a disarmed fixture.
// See scripts/w31-decisionstream-controls-6f3d.py.
func TestApprovalDecide_TruncatedDecisionStreamIsNotAVerdict_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	f := seedDecisionFixture(t, d)
	injectMidStreamFailure(t, d, ctx)

	// The vacuity floor: prove the fixture actually produces the fault being modelled —
	// rows delivered, every Scan clean, and the failure ONLY in rows.Err(). Read with the
	// shipped statement verbatim, against the pool, so this measures the wire and not a
	// re-description of it.
	delivered, scanErrs, streamErr := driveShippedSelect(t, d, ctx, f.requestID)
	if delivered == 0 {
		t.Errorf("[TRUNCATION-IS-REAL] fixture delivered 0 rows: the error is being raised before "+
			"any row streams, so nothing below exercises a PARTIAL read (scanErrs=%d err=%v)",
			scanErrs, streamErr)
	}
	if streamErr == nil {
		t.Errorf("[TRUNCATION-IS-REAL] fixture raised no stream error at all: rows.Err()=nil after "+
			"%d rows, so this file is asserting an ordinary approval and nothing more", delivered)
	}
	if scanErrs != 0 {
		t.Errorf("[TRUNCATION-IS-REAL] %d Scan error(s): the fault is arriving through Scan, which "+
			"the loop ALREADY handles — that is a different defect from the one under test", scanErrs)
	}

	// carol approves. The ordinary product action, through the shipped method the route calls.
	err := store(d).Decide(ctx, f.requestID, f.pageID, f.carol, "approved", "lgtm", []string{f.ws})

	if err == nil {
		t.Errorf("[NO-VERDICT-FROM-PARTIAL] Decide returned nil on a decision stream that FAILED "+
			"mid-flight: the same statement, run against the pool a moment earlier, delivered %d "+
			"of %d rows and ended with rows.Err() = %v. A verdict counted from rows that stopped "+
			"arriving is not a verdict", delivered, f.reviewers, streamErr)
	}

	// The two writes Decide makes on a final verdict. Read with independent SQL against the
	// base table — an oracle that shares a code path with its subject is not an oracle.
	if got := scalar(t, d, ctx,
		`SELECT status FROM approval_requests WHERE id=$1`, f.requestID); got == "approved" {
		t.Errorf("[REQUEST-NOT-FLIPPED] approval_requests.status = %q from a truncated count, "+
			"while review_decisions still holds a 'rejected' row for this request", got)
	}
	if got := scalar(t, d, ctx,
		`SELECT doc_status FROM pages WHERE id=$1`, f.pageID); got == "approved" {
		t.Errorf("[PAGE-NOT-FLIPPED] pages.doc_status = %q — the document is published-eligible "+
			"and a reviewer's rejection is sitting in the table unread", got)
	}
}

// The other direction: with the stream intact, the SAME four rows must still produce the verdict
// the rows say.
//
// ⚠ THIS TEST IS CORROBORATING, NOT LOAD-BEARING, AND SAYING SO IS THE POINT. It was written to
// stop "return an error always" passing as a fix — but control E4 does exactly that and reddens
// FIVE of this package's existing Decide tests as well, so no mutation in the set is caught here
// alone. What it still earns: it is the paired control on the SAME fixture as the test above —
// same four rows, same reviewers, fault vs no fault — which is what makes that test's verdict
// attributable to the injected failure rather than to anything about the fixture.
func TestApprovalDecide_IntactDecisionStreamStillDecides_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	f := seedDecisionFixture(t, d)
	// NO fault injected.

	if err := store(d).Decide(ctx, f.requestID, f.pageID, f.carol, "approved", "lgtm", []string{f.ws}); err != nil {
		t.Fatalf("[HAPPY-PATH-STILL-WORKS] Decide errored on a complete stream: %v", err)
	}
	// dave rejected, so the whole request is rejected however many others approved.
	if got := scalar(t, d, ctx,
		`SELECT status FROM approval_requests WHERE id=$1`, f.requestID); got != "rejected" {
		t.Errorf("[FULL-STREAM-SEES-REJECTION] approval_requests.status = %q, want \"rejected\": "+
			"the one rejection in the table did not reach the verdict", got)
	}
	if got := scalar(t, d, ctx,
		`SELECT doc_status FROM pages WHERE id=$1`, f.pageID); got != "rejected" {
		t.Errorf("[HAPPY-PATH-STILL-WORKS] pages.doc_status = %q, want \"rejected\"", got)
	}
}

// ─── fixture ───────────────────────────────────────────────────────────────

type decisionFixture struct {
	ws, pageID, requestID, carol string
	reviewers                    int
}

func store(d *testutil.DB) *approval.Store { return approval.NewStore(d.Pool) }

// seedDecisionFixture builds one request with four reviewers: two who have approved, one who
// has REJECTED, and carol who is still pending and about to decide.
//
// ⚠ THE PHYSICAL ORDER OF THE ROWS IS PART OF THE FIXTURE AND IS ASSERTED, NOT ASSUMED. The
// shipped SELECT names no ORDER BY, so the rows arrive in heap order; the rejected row must
// come AFTER at least one approved row or the stream breaks before delivering anything and
// the truncation is a different one (0 rows read → the verdict is Pending, not Approved).
// The approvals are written first and carol's row is rewritten LAST by Decide itself, which
// puts her new tuple after dave's. [TRUNCATION-IS-REAL] is what fails if that ever stops
// holding, rather than this file quietly testing a case it did not mean to.
func seedDecisionFixture(t *testing.T, d *testutil.DB) decisionFixture {
	t.Helper()
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	erin := d.Member(t, ws, "erin@example.com")
	frank := d.Member(t, ws, "frank@example.com")
	carol := d.Member(t, ws, "carol@example.com")
	dave := d.Member(t, ws, "dave@example.com")

	sp := seedSpaceA(t, d, ws, alice, "Approvals", false)
	pg := seedPageA(t, d, ws, sp, alice, "Board Memo")

	req, err := approval.NewStore(d.Pool).RequestApproval(
		ctx, pg, ws, alice, []string{erin, frank, carol, dave}, "please review", nil)
	if err != nil {
		t.Fatalf("seed request: %v", err)
	}
	for _, x := range []struct{ who, decision string }{
		{erin, "approved"}, {frank, "approved"}, {dave, "rejected"},
	} {
		if _, err := d.Pool.Exec(ctx,
			`UPDATE review_decisions SET decision=$1 WHERE request_id=$2 AND reviewer_id=$3`,
			x.decision, req.ID, x.who); err != nil {
			t.Fatalf("seed decision %s: %v", x.decision, err)
		}
	}
	return decisionFixture{ws: ws, pageID: pg, requestID: req.ID, carol: carol, reviewers: 4}
}

// injectMidStreamFailure replaces review_decisions with a view that raises an error while its
// rows stream: the `decision` expression divides by zero on the rejected row, and the divisor
// (position('r' in decision) - 1) comes from the row's own data so the planner cannot fold it
// away at plan time. An INSTEAD OF trigger keeps the relation writable, so Decide's own UPDATE
// still lands on the base table and the ONLY thing changed is how the read ends.
func injectMidStreamFailure(t *testing.T, d *testutil.DB, ctx context.Context) {
	t.Helper()
	for _, stmt := range []string{
		`ALTER TABLE review_decisions RENAME TO review_decisions_base`,
		`CREATE VIEW review_decisions AS
		   SELECT id, request_id, reviewer_id,
		          CASE WHEN decision = 'rejected'
		               THEN (1/(position('r' in decision) - 1))::text
		               ELSE decision END AS decision,
		          comment, created_at
		     FROM review_decisions_base`,
		`CREATE FUNCTION rd_upd() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   UPDATE review_decisions_base SET decision = NEW.decision, comment = NEW.comment
		    WHERE id = OLD.id;
		   RETURN NEW;
		 END $$`,
		`CREATE TRIGGER rd_upd_t INSTEAD OF UPDATE ON review_decisions
		   FOR EACH ROW EXECUTE FUNCTION rd_upd()`,
	} {
		if _, err := d.Pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("inject mid-stream failure (%.48s…): %v", strings.TrimSpace(stmt), err)
		}
	}
}

// driveShippedSelect runs aggregate's statement VERBATIM against the pool and reports what the
// caller of a rows.Next() loop can see: how many rows arrived, how many Scans failed, and what
// rows.Err() holds afterwards.
func driveShippedSelect(t *testing.T, d *testutil.DB, ctx context.Context, requestID string) (delivered, scanErrs int, streamErr error) {
	t.Helper()
	rows, err := d.Pool.Query(ctx,
		`SELECT decision FROM review_decisions WHERE request_id = $1`, requestID)
	if err != nil {
		t.Fatalf("[TRUNCATION-IS-REAL] the fault arrived from Query() itself, not from the row "+
			"stream — that error the code ALREADY returns: %v", err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			scanErrs++
		}
		delivered++
	}
	streamErr = rows.Err()
	rows.Close()
	return delivered, scanErrs, streamErr
}

func scalar(t *testing.T, d *testutil.DB, ctx context.Context, q, arg string) string {
	t.Helper()
	var out string
	if err := d.Pool.QueryRow(ctx, q, arg).Scan(&out); err != nil {
		t.Fatalf("oracle read (%s): %v", q, err)
	}
	return out
}
