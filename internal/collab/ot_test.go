package collab

import (
	"encoding/json"
	"testing"
	"time"
)

// drainSend consumes whatever's already on a client's send channel
// so a follow-up assertion can wait for the next message. Tests use
// this to skip the initial "init" + "presence" broadcasts.
func drainSend(c *CollabClient) {
	for {
		select {
		case <-c.send:
		default:
			return
		}
	}
}

func readMsg(t *testing.T, c *CollabClient) map[string]any {
	t.Helper()
	select {
	case raw := <-c.send:
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return m
	default:
		t.Fatal("expected a message, none queued")
	}
	return nil
}

// ─── Transform ─────────────────────────────────────────────

func TestTransform_ShiftsInsertAfterConcurrentDelete(t *testing.T) {
	e := NewOTEngine()
	// op: insert "x" at position 10
	// concurrent: delete 3 chars at position 5
	// Result: insert should now happen at position 7 (10 - 3).
	op := Change{Ops: []Op{{Type: OpInsert, Pos: 10, Content: "x"}}}
	concurrent := Change{Ops: []Op{{Type: OpDelete, Pos: 5, Length: 3}}}
	out := e.Transform(op, concurrent)
	if out.Ops[0].Pos != 7 {
		t.Errorf("pos = %d, want 7", out.Ops[0].Pos)
	}
}

func TestTransform_ShiftsDeleteAfterConcurrentInsert(t *testing.T) {
	e := NewOTEngine()
	// op: delete 2 chars at position 8
	// concurrent: insert "hello" (5 chars) at position 3
	// Result: delete now at position 13.
	op := Change{Ops: []Op{{Type: OpDelete, Pos: 8, Length: 2}}}
	concurrent := Change{Ops: []Op{{Type: OpInsert, Pos: 3, Content: "hello"}}}
	out := e.Transform(op, concurrent)
	if out.Ops[0].Pos != 13 {
		t.Errorf("pos = %d, want 13", out.Ops[0].Pos)
	}
}

func TestTransform_OperationsAtSamePositionAreLeftAlone(t *testing.T) {
	e := NewOTEngine()
	// Two inserts at the same exact position. We use a stable rule
	// (the concurrent op wins the tie and the incoming op shifts
	// right) so the result is deterministic.
	op := Change{Ops: []Op{{Type: OpInsert, Pos: 5, Content: "a"}}}
	concurrent := Change{Ops: []Op{{Type: OpInsert, Pos: 5, Content: "b"}}}
	out := e.Transform(op, concurrent)
	if out.Ops[0].Pos != 6 {
		t.Errorf("pos = %d, want 6 (shift by len(\"b\"))", out.Ops[0].Pos)
	}
}

// ─── Apply ────────────────────────────────────────────────

func TestApply_AppliesAndAdvancesVersion(t *testing.T) {
	e := NewOTEngine()
	pageID := "p-1"
	c1, _ := e.Join(pageID, "client-1", "m-1", "Alice")
	drainSend(c1)

	out, err := e.Apply(pageID, Change{
		ClientID: "client-1",
		Version:  0,
		Ops:      []Op{{Type: OpInsert, Pos: 0, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("returned %d changes, want 1", len(out))
	}
	st := e.pageState(pageID)
	if st.Version != 1 {
		t.Errorf("page version = %d, want 1", st.Version)
	}
}

func TestApply_TransformsAgainstNewerHistory(t *testing.T) {
	e := NewOTEngine()
	pageID := "p-1"
	c1, _ := e.Join(pageID, "client-1", "m-1", "Alice")
	c2, _ := e.Join(pageID, "client-2", "m-2", "Bob")
	drainSend(c1)
	drainSend(c2)

	// client-1 inserts 3 characters at position 5 from version 0.
	if _, err := e.Apply(pageID, Change{
		ClientID: "client-1", Version: 0,
		Ops: []Op{{Type: OpInsert, Pos: 5, Content: "abc"}},
	}); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// client-2 inserts 1 character at position 10, still based on
	// version 0. Engine should transform: shift right by 3.
	out, err := e.Apply(pageID, Change{
		ClientID: "client-2", Version: 0,
		Ops: []Op{{Type: OpInsert, Pos: 10, Content: "z"}},
	})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if out[0].Ops[0].Pos != 13 {
		t.Errorf("transformed pos = %d, want 13", out[0].Ops[0].Pos)
	}
}

// ─── BroadcastChange ──────────────────────────────────────

func TestBroadcastChange_SkipsSender(t *testing.T) {
	e := NewOTEngine()
	pageID := "p-1"
	c1, _ := e.Join(pageID, "c-1", "m-1", "Alice")
	c2, _ := e.Join(pageID, "c-2", "m-2", "Bob")
	drainSend(c1)
	drainSend(c2)

	e.BroadcastChange(pageID, Change{
		ClientID: "c-1", Version: 1,
		Ops: []Op{{Type: OpInsert, Pos: 0, Content: "hi"}},
	}, "c-1")

	if _, ok := waitForMsg(c1, 50*time.Millisecond); ok {
		t.Error("sender should not receive the broadcast")
	}
	msg, ok := waitForMsg(c2, 200*time.Millisecond)
	if !ok {
		t.Fatal("expected the other client to receive a message")
	}
	if msg["type"] != "change" {
		t.Errorf("msg type = %v, want change", msg["type"])
	}
}

// ─── Presence / Join / Leave ──────────────────────────────

func TestJoin_AddsClient(t *testing.T) {
	e := NewOTEngine()
	pageID := "p-1"
	if _, err := e.Join(pageID, "c-1", "m-1", "Alice"); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if len(e.GetPresence(pageID)) != 1 {
		t.Errorf("expected 1 client in presence")
	}
}

func TestLeave_RemovesClient(t *testing.T) {
	e := NewOTEngine()
	pageID := "p-1"
	_, _ = e.Join(pageID, "c-1", "m-1", "Alice")
	_, _ = e.Join(pageID, "c-2", "m-2", "Bob")
	e.Leave(pageID, "c-1")
	pres := e.GetPresence(pageID)
	if len(pres) != 1 || pres[0].ClientID != "c-2" {
		t.Errorf("expected c-2 alone, got %+v", pres)
	}
}

func TestGetPresence_AssignsUniqueColors(t *testing.T) {
	e := NewOTEngine()
	pageID := "p-1"
	_, _ = e.Join(pageID, "c-1", "m-1", "Alice")
	_, _ = e.Join(pageID, "c-2", "m-2", "Bob")
	pres := e.GetPresence(pageID)
	if pres[0].Color == "" || pres[1].Color == "" || pres[0].Color == pres[1].Color {
		t.Errorf("colors not unique: %v / %v", pres[0].Color, pres[1].Color)
	}
}

// ─── helpers ──────────────────────────────────────────────

// waitForMsg blocks up to timeout for the next message on a client.
// Returns (msg, true) on success, (nil, false) on timeout. Used
// instead of bare channel receives so a failing test bounds itself
// instead of hanging the whole package.
func waitForMsg(c *CollabClient, timeout time.Duration) (map[string]any, bool) {
	select {
	case raw, ok := <-c.send:
		if !ok {
			return nil, false
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return m, true
	case <-time.After(timeout):
		return nil, false
	}
}

// silence unused: readMsg is defined for the benefit of debugging
// runs where tests want to inspect specific messages.
var _ = readMsg
