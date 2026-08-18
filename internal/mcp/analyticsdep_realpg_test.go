package mcp_test

// THE MISSING GUARD BESIDE THE DEAD ONES: get_page_analytics DEREFERENCES deps.analytics BARE.
//
// #141 normalised mcp.New so that a nil concrete pointer becomes a genuine nil INTERFACE on
// deps, which made three dead `s.deps.X == nil` guards live. It named this one as measured and
// deliberately NOT fixed, because a MISSING guard is a different finding from a DEAD one:
//
//	server.go  stats, err := s.deps.analytics.GetReadStats(...)   ← no guard at all
//
// so get_page_analytics on a server built without an analytics store panics BEFORE #141 (nil
// receiver inside GetReadStats, which checks s.pool and cannot check its own receiver) and AFTER
// it (method call on a nil interface). The whole `if stats != nil` block below the call is
// unreachable by the failure it looks like it handles.
//
// ⚠ AND THE FALLBACK UNDERNEATH IS THE SAME LIE #141 REFUSED TO LEAVE IN PLACE. GetReadStats
// answers a POOL-less store with (nil, nil), and toolGetPageAnalytics renders that as
// {"total_views":0,"unique_viewers":0,"avg_duration_sec":0} — the positive claim "nobody has read
// this page", made by a service that cannot read. So the guard and its answer move together:
// an absent store is an ERROR here, exactly as an absent freshness engine and an absent AI engine
// already are (server.go:1071, server.go:1037).
//
// RED/GREEN, stated per test:
//   - NilAnalyticsStore_GetPageAnalytics   RED before (panic), GREEN after.
//   - PoollessAnalyticsStore_IsNotZeroViews  RED before ("0 views" served), GREEN after.
//   - WiredAnalyticsStore_StillReports     passes before AND after — the control that stops the
//     fix from being "return an error always", which would satisfy both tests above vacuously.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/analytics"
	"github.com/talyvor/docs/internal/mcp"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// RED BEFORE THE FIX: a panic, not an answer. The recover here is what turns a process-killing
// SIGSEGV into a readable failure; without it the whole package's test binary dies and every
// other test in it reports nothing.
func TestMCP_NilAnalyticsStore_GetPageAnalyticsDoesNotPanic(t *testing.T) {
	d := testutil.New(t)
	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")
	pgID := d.Page(t, W, owner, "Runbook")

	srv := mcp.New(page.NewStore(d.Pool), space.NewStore(d.Pool), nil, nil, nil, "test").
		WithAccess(mcpAccess(d))
	chain := chainForServer(d, srv)

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("[PANIC] get_page_analytics panicked with NO analytics store: %v\n"+
				"server.go calls s.deps.analytics.GetReadStats bare — there is no guard at this "+
				"call site, and GetReadStats checks s.pool and cannot check its own receiver", rec)
		}
	}()

	rr := callTool(chain, "owner@corp.com", true, "get_page_analytics", map[string]any{"page_id": pgID})
	result, errObj := rpcEnvelope(t, rr.Body.Bytes())
	if errObj != nil {
		return // an error is the correct answer for an unwired store
	}
	// [ANALYTICS-NOT-ZERO] the tool answered. Any answer is wrong for an absent store, but the
	// zero one is the dangerous one: it reads as a measured "nobody has read this page".
	t.Errorf("[ANALYTICS-NOT-ZERO] get_page_analytics with NO analytics store returned a RESULT, "+
		"not an error: %s\nan unwired store cannot report a readership count", contentText(t, result))
}

// RED BEFORE THE FIX: a pool-less store is a DIFFERENT arm from the nil one — it survives the
// `!= nil` normalisation in New and reaches GetReadStats, whose `if s.pool == nil { return nil,
// nil }` the tool then renders as three zeroes. A nil-receiver check alone would leave this arm
// serving the lie, which is why the guard belongs where the answer is written.
func TestMCP_PoollessAnalyticsStore_GetPageAnalyticsIsNotZeroViews(t *testing.T) {
	d := testutil.New(t)
	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")
	pgID := d.Page(t, W, owner, "Runbook")

	srv := mcp.New(page.NewStore(d.Pool), space.NewStore(d.Pool), analytics.NewStore(nil), nil, nil, "test").
		WithAccess(mcpAccess(d))
	chain := chainForServer(d, srv)

	rr := callTool(chain, "owner@corp.com", true, "get_page_analytics", map[string]any{"page_id": pgID})
	result, errObj := rpcEnvelope(t, rr.Body.Bytes())
	if errObj != nil {
		return // an error is the correct answer
	}
	text := contentText(t, result)
	var out struct {
		TotalViews    int `json:"total_views"`
		UniqueViewers int `json:"unique_viewers"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode get_page_analytics payload: %v (%s)", err, text)
	}
	// [POOLLESS-NOT-ZERO] the failure this guards: an unreachable database reported as a
	// readership of zero, which the caller cannot tell from a page nobody opened.
	t.Errorf("[POOLLESS-NOT-ZERO] get_page_analytics with a POOL-LESS analytics store returned "+
		"total_views=%d unique_viewers=%d instead of an error — an unreachable store must not "+
		"answer a readership question: %s", out.TotalViews, out.UniqueViewers, text)
}

// CONTROL (passes before AND after): a properly wired store must still report the number. Without
// this, "return an error whenever analytics is asked for" satisfies both tests above and the tool
// is dead rather than guarded.
func TestMCP_WiredAnalyticsStore_GetPageAnalyticsStillReports(t *testing.T) {
	d := testutil.New(t)
	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")
	pgID := d.Page(t, W, owner, "Runbook")

	store := analytics.NewStore(d.Pool)
	if _, err := d.Pool.Exec(t.Context(),
		`INSERT INTO page_views (id, page_id, workspace_id, viewer_id, viewer_name, duration_sec)
         VALUES ('pv-wired-1', $1, $2, $3, 'Owner', 42)`, pgID, W, owner); err != nil {
		t.Fatalf("seed page_views: %v", err)
	}

	srv := mcp.New(page.NewStore(d.Pool), space.NewStore(d.Pool), store, nil, nil, "test").
		WithAccess(mcpAccess(d))
	chain := chainForServer(d, srv)

	rr := callTool(chain, "owner@corp.com", true, "get_page_analytics", map[string]any{"page_id": pgID})
	result, errObj := rpcEnvelope(t, rr.Body.Bytes())
	if errObj != nil {
		t.Fatalf("[WIRED-SERVED] get_page_analytics errored with a REAL analytics store: %v", errObj)
	}
	text := contentText(t, result)
	var out struct {
		TotalViews int `json:"total_views"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode get_page_analytics payload: %v (%s)", err, text)
	}
	// [WIRED-SERVED] the number the seeded view implies. Asserting the VALUE and not merely
	// "no error" is what stops a guard that answers every call with an empty payload.
	if out.TotalViews != 1 {
		t.Errorf("[WIRED-SERVED] wired analytics store reported total_views=%d, want 1: %s",
			out.TotalViews, text)
	}
	if strings.TrimSpace(text) == "" {
		t.Errorf("[WIRED-SERVED] wired analytics store returned an empty payload")
	}
}
