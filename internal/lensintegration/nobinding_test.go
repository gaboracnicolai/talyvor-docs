package lensintegration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/lenscreds"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/page"
)

// "NOT OURS" MUST NOT REACH THE OPERATOR AS A FAILURE.
//
// page.ErrNoBinding is the sentinel for a Lens request Docs never bound to a page. Its whole
// reason to exist is that a caller can say "skip, not ours" without treating it as a fault —
// ai_spend.go says so in the declaration itself ("must not read as an error"). A sentinel that
// every caller then funnels into the generic `pErr != nil` branch keeps the promise in the
// comment and breaks it in the log.
//
// ⚠ IT IS REACHABLE, AND THE RACE IS ORDINARY. syncWorkspace pre-filters Lens's rows against its
// own unpriced binding ids, so a FOREIGN request never reaches PriceAISpend at all. But
// page_ai_spend_events cascades from pages: delete a document between UnpricedRequestIDs and the
// price call — a user emptying a space while the sweep runs — and an id that was ours a
// millisecond ago has no binding row. That is not a fault to page an operator about; it is the
// exact case the sentinel names.
//
// ⚠ THE ABSENCE IS FLOORED IN THE SAME RUN, because "no warning was logged" is also what a
// broken capture, a silenced logger and a sweep that never ran all look like. `req-broken`
// returns an ordinary error from the same loop iteration shape, so this test only passes when the
// warn channel is demonstrably live and carrying exactly the one request that deserves it.
func TestSweep_ANeverBoundRequestIsSkipped_NotLoggedAsAFailure(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	const ws = "ws-sweep"
	f := newFakeLensSpend(t)
	f.setRows(ws,
		map[string]any{"request_id": "req-gone", "feature": "docs-ai-write", "cost_usd": 1.25,
			"input_tokens": 10, "output_tokens": 20, "serve_source": "upstream"},
		map[string]any{"request_id": "req-broken", "feature": "docs-ai-write", "cost_usd": 2.50,
			"input_tokens": 10, "output_tokens": 20, "serve_source": "upstream"},
		map[string]any{"request_id": "req-live", "feature": "docs-ai-write", "cost_usd": 3.75,
			"input_tokens": 10, "output_tokens": 20, "serve_source": "upstream"},
	)

	st := &scriptedSpendStore{
		workspaces: []string{ws},
		unpriced:   map[string][]string{ws: {"req-gone", "req-broken", "req-live"}},
		answers: map[string]spendAnswer{
			"req-gone":   {err: page.ErrNoBinding},
			"req-broken": {err: errors.New("page: price ai spend: connection reset")},
			"req-live":   {landed: true},
		},
	}

	c := lensintegration.New(f.URL, "k1").WithTokenProvider(lenscreds.New(f.URL, "k1", lenscreds.Options{}))
	lensintegration.NewPageCostSyncer(c, st, 2).Sync(context.Background())

	// [SWEEP-RAN] — the premise. Every assertion below is trivially true of a sweep that made no
	// price calls at all.
	if len(st.called) != 3 {
		t.Fatalf("[SWEEP-RAN] the sweep priced %v, want all three request ids — nothing below is "+
			"about the sentinel if the loop never reached it", st.called)
	}

	warns := warnedRequestIDs(t, &buf)

	// [WARN-CHANNEL-LIVE] — the floor for the absence asserted next.
	if !warns["req-broken"] {
		t.Errorf("[WARN-CHANNEL-LIVE] an ordinary price failure produced no captured warning "+
			"(records: %q). The absence asserted below would then be green for a reason that has "+
			"nothing to do with ErrNoBinding.", buf.String())
	}

	// [NOT-A-FAULT] — the headline.
	if warns["req-gone"] {
		t.Errorf("[NOT-A-FAULT] the sweep logged a warning for a request that was never bound to " +
			"a page. page.ErrNoBinding is documented as \"NOT an error condition, just a request " +
			"that was not a page operation\"; routing it into the generic failure branch reports a " +
			"deleted page's leftover row as a broken money path.")
	}

	// [OTHERS-STILL-PRICED] — skipping must be a `continue`, not a `return`.
	if !warns["req-broken"] || st.called[len(st.called)-1] != "req-live" {
		t.Errorf("[OTHERS-STILL-PRICED] the sweep did not reach req-live after the two error "+
			"rows (called: %v)", st.called)
	}
}

type spendAnswer struct {
	landed bool
	err    error
}

// scriptedSpendStore is a PageSpendStore whose price answer is chosen per request id. A fake is
// the only way to reach ErrNoBinding from the sweep on demand: against real Postgres the case
// needs a page deleted inside the window between two of the sweep's own queries.
type scriptedSpendStore struct {
	workspaces []string
	unpriced   map[string][]string
	answers    map[string]spendAnswer
	called     []string
}

func (s *scriptedSpendStore) UnpricedWorkspaces(context.Context, int) ([]string, error) {
	return s.workspaces, nil
}

func (s *scriptedSpendStore) UnpricedRequestIDs(_ context.Context, wsID string, _ int) ([]string, error) {
	return s.unpriced[wsID], nil
}

func (s *scriptedSpendStore) PriceAISpendWithServeSource(_ context.Context, requestID string, _ float64, _ int, _ string) (bool, error) {
	s.called = append(s.called, requestID)
	a := s.answers[requestID]
	return a.landed, a.err
}

// warnedRequestIDs reads the captured records STRUCTURALLY — level and the request_id attribute —
// rather than grepping the buffer for a substring. A substring search would also match the id
// echoed inside another record's `err` string, which is precisely the shape that makes a log
// assertion agree with itself.
func warnedRequestIDs(t *testing.T, buf *bytes.Buffer) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("captured log line is not JSON (%q): %v", line, err)
		}
		if rec["level"] != "WARN" {
			continue
		}
		if id, ok := rec["request_id"].(string); ok {
			out[id] = true
		}
	}
	return out
}
