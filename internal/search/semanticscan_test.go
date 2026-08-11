package search

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// THE SCAN BRANCH IS THE OTHER HALF OF THE SAME SILENCE, AND IT IS A DIFFERENT DOOR — WHICH IS
// WHY IT IS A SECOND GUARD AND NOT A SECOND ASSERTION IN THE FIRST ONE.
//
// `rows.Scan` fails when the SELECT list and the destination list disagree — exactly what adding
// a column edits, and exactly what this package's pgxmock fixtures did the moment #91 grew the
// row from three fields to five. The branch returned `[]SemanticResult{}, nil` with no log, so a
// programming error was laundered through the same path as an infrastructure one.
//
// ⚠ THIS CANNOT BE DRIVEN FROM REAL POSTGRES AND THAT IS THE POINT. The Scan destinations are a
// source-level fact of `Search`; no query a test can send makes the shipped SELECT list and the
// shipped Scan disagree. A mock is the only instrument that can hand this function a row shape
// it did not ask for — the one shape every other test in this package is built never to have.
//
// ⚠ AND IT IS NOT THE BRANCH THE REALISTIC FAILURE USES. Measured on real Postgres: a pgvector
// error raised during row production reaches `rows.Err()` ONLY — Scan is never called. So a warn
// added here alone would have been INERT for the failure that actually happens in production.
// The pair is the fix; see semanticsilence_realpg_test.go for the other half and the measurement.
func TestSemanticSearch_AScanMismatchIsNotSilent(t *testing.T) {
	srv := newFakeLens(t)
	defer srv.Close()
	s, pool := newSemantic(t, srv.URL)

	// A row set the shipped Scan CANNOT consume: it names five destinations, this supplies two.
	// pgxmock reports the arity mismatch through Scan, which is the branch under test.
	//
	// ⚠ WithArgs IS NOT DECORATION. pgxmock's default is "expects a call with NO arguments", so
	// an expectation written without it never matches and Search returns before the row set is
	// ever handed over — see the note on the message literal below for what that cost.
	pool.ExpectQuery(`page_embeddings.*<=>`).
		WithArgs(pgxmock.AnyArg(), "ws-1", 10, (*string)(nil), 0).
		WillReturnRows(pgxmock.NewRows([]string{"page_id", "space_id"}).
			AddRow("pg-1", "sp-1"))

	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	_, err := s.Search(context.Background(), "ws-1", "runbook", nil, 10, 0)
	slog.SetDefault(prev)

	// [SCAN-DEGRADES] — the contract half, so the fix cannot quietly become a fail-closed one.
	// There is no row-count assertion beside it: this fixture delivers no scannable row at all,
	// so `out` is nil and no mutation can move that number. The partial-set property has its own
	// test below, on the only fixture here that can construct a partial set.
	if err != nil {
		t.Errorf("[SCAN-DEGRADES] Search errored (%v) on a scan mismatch; the contract is empty "+
			"semantic results with the full-text half still serving", err)
	}

	// [SILENT-SCAN] — the branch the finding named. Selects on level AND the exact message, for
	// the reason recorded in semanticsilence_realpg_test.go and proved by this very test.
	//
	// ⚠ THE EXACT MESSAGE, NOT A PREFIX, AND THE REASON IS A MEASUREMENT OF THIS GUARD ITSELF.
	// The first draft asserted `strings.HasPrefix(msg, "search:")` — and `Search` has FOUR warn
	// sites that all satisfy that (`search: query embed`, `search: pgvector query`, and the two
	// added here). On its first run the expectation above did not match, Search failed EARLIER,
	// and every assertion in this test passed against a completely different log line; only the
	// unmet-expectation cleanup said so. A guard for one branch that any other branch can
	// satisfy is not a guard for that branch. The literal is hardcoded rather than read from a
	// product constant, so comparing it to itself is impossible.
	matched := warnsMatching(t, buf.String(), "search: pgvector scan")
	if len(matched) != 1 {
		t.Fatalf("[SILENT-SCAN] the row scan FAILED and Search emitted %d WARN records reading "+
			"\"search: pgvector scan\", want exactly 1 — it returned ([], nil). A SELECT list and "+
			"a Scan that disagree is a programming error, and it turns the semantic half off for "+
			"every workspace with no signal anywhere. Log was: %q", len(matched), buf.String())
	}
	// [SCAN-CAUSE] — its own failure mode, exactly as [ROWS-CAUSE] is on the other door. The
	// driver's message is what names WHICH column set disagreed; without it the line reports
	// that scanning failed and leaves an operator to guess the shape.
	if cause, _ := matched[0]["err"].(string); strings.TrimSpace(cause) == "" {
		t.Errorf("[SCAN-CAUSE] the warn carries no `err` attribute (%v) — an operator is told a "+
			"scan failed and nothing about which fields disagreed", matched[0])
	}
}

// A PARTIAL RESULT SET MUST NOT BE HANDED BACK AS A COMPLETE ONE.
//
// This is the half the real-Postgres guard CANNOT assert: pgvector raises before pgx delivers a
// single row, so there `out` is always nil and "return out" and "return []SemanticResult{}" are
// the same value — an assertion no mutation could move. pgxmock's RowError delivers one good row
// and THEN fails, which is the only fixture available here that makes the difference visible.
//
// ⚠ WHY IT MATTERS MORE THAN THE EMPTY CASE: an empty semantic half is the documented
// degradation and the full-text half covers for it. A set that is silently SHORT is a different
// harm — the caller is told these are the results, and the missing ones are indistinguishable
// from documents that did not match.
func TestSemanticSearch_APartialScanIsNotReturnedAsComplete(t *testing.T) {
	srv := newFakeLens(t)
	defer srv.Close()
	s, pool := newSemantic(t, srv.URL)

	// Row 0 scans cleanly and is accumulated into `out`; row 1 fails. Both are above the 0.75
	// similarity threshold, so nothing here is dropped for relevance.
	pool.ExpectQuery(`page_embeddings.*<=>`).
		WithArgs(pgxmock.AnyArg(), "ws-1", 10, (*string)(nil), 0).
		WillReturnRows(pgxmock.NewRows([]string{"page_id", "space_id", "title", "space_name", "similarity"}).
			AddRow("pg-1", "sp-1", "One", "Space One", float64(0.93)).
			AddRow("pg-2", "sp-1", "Two", "Space One", float64(0.92)).
			RowError(1, errors.New("connection reset mid-stream")))

	out, _ := s.Search(context.Background(), "ws-1", "runbook", nil, 10, 0)
	//
	// ⚠ THE RETURNED ERROR IS DELIBERATELY NOT ASSERTED HERE, AND A CONTROL IS WHY. The first
	// draft also checked `err != nil` under this same tag. [SCAN-DEGRADES] in the test above
	// already holds that exact property on that exact branch, so the copy could not be breached
	// by any mutation of its own — control C6 (the scan branch made fail-closed) fired BOTH tags
	// at once, which means either one could have been deleted or made constant-true with the
	// campaign still reading as predicted. An invariant held twice is held once and counted
	// twice. The copy goes; this test asserts only the thing no other test can see.
	if len(out) != 0 {
		t.Errorf("[SCAN-PARTIAL] Search accumulated %d row(s) before the failure and RETURNED "+
			"them with a nil error: %+v. The caller cannot tell a short answer from a complete "+
			"one, so the documents the statement never reached read as documents that did not "+
			"match. The failed set must degrade to empty, the way every other failure here does",
			len(out), out)
	}
}

// warnsMatching decodes the captured JSON log and returns the records that are BOTH level WARN
// and exactly the given message. The level is folded into the selector rather than asserted
// separately so that one control moves one number — as a separate `if`, a downgrade to INFO and
// a missing line would fire two assertions and neither would be independently earned.
func warnsMatching(t *testing.T, log, msg string) []map[string]any {
	t.Helper()
	var matched []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON (%v): %q", err, line)
		}
		lvl, _ := m["level"].(string)
		got, _ := m["msg"].(string)
		if lvl == "WARN" && got == msg {
			matched = append(matched, m)
		}
	}
	return matched
}
