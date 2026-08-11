package search

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/lenscreds"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/testutil"
)

// THE SEMANTIC HALF TURNS ITSELF OFF WITH NO SIGNAL ANYWHERE, AND THE DOOR THE ERROR COMES
// THROUGH IS NOT THE ONE THE SCAN BRANCH WATCHES.
//
// `SemanticSearch.Search` ended its loop `return out, nil` with `rows.Err()` NEVER READ. pgx
// delivers an error raised DURING row production only through `rows.Err()`: `Query()` returns
// nil, `rows.Next()` returns false on the first call, and `rows.Scan` is NEVER REACHED. So the
// function returned zero rows and a nil error, and the sibling `slog.Warn` six lines above —
// the one the Query() door already had — could not fire.
//
// MEASURED THROUGH THIS FUNCTION ON REAL POSTGRES BEFORE THE FIX, three indexed pages seeded:
//
//	honest 1536-dim query vector -> rows=3  Scan err=<nil>  rows.Err()=<nil>
//	3-dim query vector           -> rows=0  Scan err=<nil>  rows.Err()=ERROR: different vector
//	                                                        dimensions 1536 and 3 (SQLSTATE 22000)
//	Search(...)                  -> ([], nil) and slog output ""
//
// ⚠ THE TRIGGER IS NOT EXOTIC, WHICH IS WHY THIS IS A GUARD AND NOT A NOTE. `page_embeddings.
// embedding` is `vector(1536)` (0004_search.sql:12) but the query vector is an UNCONSTRAINED
// `$1::vector` built from whatever Lens returns. The dimension is a property of the model Lens
// is configured with, NOT of this schema — `embeddingModel` names text-embedding-3-small here,
// and Lens proxies to "whichever upstream it has configured" (see the const's own comment). A
// model swap on the Lens side therefore turns semantic search off across every workspace,
// permanently, with an empty log — while full-text keeps serving, so the surface still answers
// 200 with results and nobody has a reason to look.
//
// ⚠ WHAT IS NOT CLAIMED: this does not make the failure LOUD to a caller, and it must not. The
// graceful-degradation contract is deliberate and documented on `Search` and again in the
// handler (Lens down ⇒ empty semantic results, full-text still serves; only ErrTokenUnavailable
// is fail-closed). Erroring here would turn a Lens hiccup into a 500 for the WHOLE search,
// including the full-text half that worked. The defect was never the degradation — it was that
// the degradation was INVISIBLE. This asserts the operator can see it, nothing more.
//
// ⚠ AND THE PARTIAL SET IS RETURNED AS EMPTY, DELIBERATELY: on `rows.Err()` the accumulated
// `out` is DISCARDED rather than returned. A truncated result set handed back with a nil error
// is an incomplete answer presented as a complete one, which is the same silence one layer up.
func TestSemanticSearch_ARowsErrIsNotSilent_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	sp := seedSpace(t, d, ws, alice, "Runbooks", false)

	const seeded = 3
	for _, title := range []string{"Deploy runbook", "Rollback runbook", "Oncall runbook"} {
		id := seedPage(t, d, ws, sp, alice, title, "the deployment runbook")
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO page_embeddings (page_id, embedding) VALUES ($1, $2::vector)`,
			id, vecAt(0.05)); err != nil {
			t.Fatalf("seed embedding: %v", err)
		}
	}

	lens := newFakeLens(t)
	defer lens.Close()
	sem := newSemanticSearch(lensintegration.New(lens.URL, "k1"), d.Pool).
		WithLensURL(lens.URL).
		WithTokenProvider(lenscreds.New(lens.URL, "k1", lenscreds.Options{}))

	// ── 1. THE HONEST PATH. Two things are asserted here and they are two rather than one
	// written twice: the query FINDS the rows, and it says NOTHING while doing so.
	lens.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":` + vecAt(0) + `}]}`))
	}
	okRows, okLog, okErr := searchCapturingLog(t, sem, ctx, ws)
	if okErr != nil {
		t.Fatalf("honest Search returned an error: %v", okErr)
	}
	//
	// [BASELINE] IS THE FLOOR AND IT IS LOAD-BEARING. Every assertion below is about the
	// WRONG-DIMENSION call returning nothing and logging something. "Returns nothing" is
	// satisfied by a workspace with no pages in it, by a threshold above 1.0, and by a fixture
	// whose embeddings were never inserted — so without this line a green below could mean the
	// query never found anything in the first place, and the guard would be measuring an empty
	// table rather than a silenced failure.
	if len(okRows) != seeded {
		t.Fatalf("[BASELINE] an honest semantic search returned %d rows, want %d — the fixture is "+
			"not the populated one this guard needs, so nothing below would mean anything",
			len(okRows), seeded)
	}
	// [NO-FALSE-WARN] keeps the log assertion a DIFFERENCE detector rather than a presence
	// detector: a `slog.Warn` on every successful search would satisfy [SILENT-ROWS] forever.
	if len(okLog) != 0 {
		t.Errorf("[NO-FALSE-WARN] a SUCCESSFUL semantic search logged %d record(s): %v. The warn "+
			"must mark a failure; one on the happy path makes it noise an operator learns to "+
			"scroll past, and makes the assertion below unfalsifiable", len(okLog), okLog)
	}

	// ── 2. THE DEFECT. Lens answers with a vector of the wrong dimension — which is exactly
	// what this package's own newFakeLens returns by default, and what a model change upstream
	// produces in production. pgvector raises during row production, so the error reaches
	// NEITHER `Query()` NOR `rows.Scan`.
	lens.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}
	_, badLog, badErr := searchCapturingLog(t, sem, ctx, ws)

	// The CONTRACT half, asserted so the fix cannot quietly become a fail-closed one: the
	// caller still sees graceful degradation.
	if badErr != nil {
		t.Errorf("[DEGRADES] Search errored (%v) on a pgvector failure. The documented contract is "+
			"empty semantic results with the full-text half still serving; erroring here 500s the "+
			"whole search", badErr)
	}
	//
	// ⚠ THERE IS NO ROW-COUNT ASSERTION ON THIS CALL, AND ITS ABSENCE IS DELIBERATE AND MEASURED.
	// The obvious companion — "a failed statement must not hand back the rows it had already
	// accumulated" — CANNOT FAIL HERE: pgvector raises before pgx delivers a single row, so `out`
	// is nil and returning it is byte-identical to returning `[]SemanticResult{}`. No mutation of
	// the product can move that number on this fixture, so the assertion would be earned by
	// nothing. The property is real and it IS asserted — in semanticscan_test.go, on a mock row
	// set that delivers one good row and then fails, which is the only instrument here that can
	// construct a partial set at all.

	// [SILENT-ROWS] IS THE FINDING, AND IT SELECTS ON LEVEL **AND** THE EXACT MESSAGE.
	//
	// ⚠ THE EXACT MESSAGE, NOT A PREFIX. `Search` has FOUR warn sites and `strings.HasPrefix(msg,
	// "search:")` is satisfied by every one of them. Measured on the sibling scan guard: written
	// with a prefix check, that test passed IN FULL against a log line from a completely
	// different branch, and only an unmet pgxmock expectation said so. The literal is hardcoded
	// rather than read from the product, so it cannot be compared to itself.
	//
	// ⚠ AND THE LEVEL IS PART OF THE SELECTOR RATHER THAN A SECOND `if`. Downgrading the line to
	// INFO buries a dead subsystem in request noise and upgrading it to ERROR overstates a
	// degradation the product is designed to survive — but as a separate assertion neither could
	// be told from "the line is missing", and one control would have fired two assertions. Folded
	// in, the count below is the single thing that moves, and ONE control moves it.
	var matched []map[string]any
	for _, rec := range badLog {
		lvl, _ := rec["level"].(string)
		msg, _ := rec["msg"].(string)
		if lvl == "WARN" && msg == "search: pgvector rows" {
			matched = append(matched, rec)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("[SILENT-ROWS] the pgvector statement FAILED (`different vector dimensions 1536 "+
			"and 3`) and Search emitted %d WARN records reading \"search: pgvector rows\", want "+
			"exactly 1. It returned ([], nil). rows.Err() is the ONLY door this class of error "+
			"comes through — Query() returned nil and rows.Scan was never reached, so the Scan "+
			"branch cannot see it. Semantic search is off across every workspace and no operator "+
			"has a signal. Records seen: %v", len(matched), badLog)
	}

	// [ROWS-CAUSE] is a SECOND assertion with its own failure mode: a warn that fires and says
	// nothing about WHY is barely better than the silence. The driver's own text is what
	// distinguishes a dimension mismatch from a timeout from a dropped connection — three
	// different operator actions behind one identical empty result.
	if cause, _ := matched[0]["err"].(string); strings.TrimSpace(cause) == "" {
		t.Errorf("[ROWS-CAUSE] the warn carries no `err` attribute (%v). Every other failure path "+
			"in this file attaches the driver's own message; without it the line says a statement "+
			"failed and gives an operator nothing to act on", matched[0])
	}
}

// searchCapturingLog runs the SHIPPED Search with the default slog logger swapped for a JSON
// handler, and returns the rows, the decoded records and the error.
//
// ⚠ IT SWAPS A PROCESS GLOBAL, which is safe here and is asserted rather than assumed: this
// repository has ZERO `t.Parallel()` calls, so tests within a package run one at a time. The
// restore is a `defer` so a t.Fatalf inside the call cannot leave the fleet's logger redirected
// into a dead buffer. `internal/freshness/privatespace_realpg_test.go` reads a real log line the
// same way; this is a deliberate re-implementation rather than a shared helper, because a helper
// carries the evidence of the test it was written for and not of this one.
func searchCapturingLog(t *testing.T, sem *SemanticSearch, ctx context.Context, ws string) ([]SemanticResult, []map[string]any, error) {
	t.Helper()
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	rows, err := sem.Search(ctx, ws, "runbook", nil, 10, 0)

	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if e := json.Unmarshal([]byte(line), &m); e != nil {
			t.Fatalf("log line is not JSON (%v): %q", e, line)
		}
		recs = append(recs, m)
	}
	return rows, recs, err
}
