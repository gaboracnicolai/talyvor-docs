package editsession_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/docs/internal/editsession"
	"github.com/talyvor/docs/internal/testutil"
)

func seed(t *testing.T) (*editsession.Store, *testutil.DB, string, string, string, []string) {
	t.Helper()
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	bob := d.Member(t, ws, "bob@corp.com")
	pageID := d.Page(t, ws, alice, "Shared doc")
	return editsession.NewStore(d.Pool), d, pageID, alice, bob, []string{ws}
}

// PHASE 2 — LIFECYCLE: acquire → get → heartbeat → release.
func TestEditSession_Lifecycle_RealPG(t *testing.T) {
	store, _, pageID, alice, _, ws := seed(t)
	ctx := context.Background()

	s, err := store.Acquire(ctx, pageID, ws, alice)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if s.Holder != alice || !s.Live {
		t.Fatalf("acquired session = %+v, want holder=alice live=true", s)
	}

	got, err := store.Get(ctx, pageID, ws)
	if err != nil || got == nil || got.Holder != alice || !got.Live {
		t.Fatalf("get = %+v, %v; want alice live", got, err)
	}

	hb, err := store.Heartbeat(ctx, pageID, ws, alice)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !hb.LastHeartbeat.After(s.LastHeartbeat) && !hb.LastHeartbeat.Equal(s.LastHeartbeat) {
		t.Fatalf("heartbeat did not advance last_heartbeat")
	}

	if err := store.Release(ctx, pageID, ws, alice); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got, _ := store.Get(ctx, pageID, ws); got != nil {
		t.Fatalf("after release, session = %+v, want nil", got)
	}
}

// PHASE 2 — SINGLE-WRITER POLICY (MayWrite + the CanEdit guard adapter): a live session held by
// someone else blocks a non-holder; the holder, no-session, and admin all pass.
func TestEditSession_SingleWriterPolicy_RealPG(t *testing.T) {
	store, _, pageID, alice, bob, ws := seed(t)
	ctx := context.Background()

	// No session yet → anyone may write.
	if ok, _, _ := store.MayWrite(ctx, pageID, ws, bob); !ok {
		t.Fatal("MayWrite with no session must allow")
	}

	if _, err := store.Acquire(ctx, pageID, ws, alice); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}

	// Holder writes; non-holder is rejected with a holder-named reason.
	if ok, _, _ := store.MayWrite(ctx, pageID, ws, alice); !ok {
		t.Error("holder MayWrite must allow")
	}
	ok, reason, _ := store.MayWrite(ctx, pageID, ws, bob)
	if ok {
		t.Error("non-holder MayWrite must be rejected while a live session is held")
	}
	if reason == "" {
		t.Error("rejection must name the holder in the reason")
	}

	// The CanEdit guard adapter (page-scoped, consumed by store.Update via Composite): same
	// rule, plus admin bypass.
	if ok, _, _ := store.CanEdit(ctx, pageID, bob, false); ok {
		t.Error("CanEdit(non-holder) must deny")
	}
	if ok, _, _ := store.CanEdit(ctx, pageID, alice, false); !ok {
		t.Error("CanEdit(holder) must allow")
	}
	if ok, _, _ := store.CanEdit(ctx, pageID, bob, true); !ok {
		t.Error("CanEdit(admin) must bypass the single-writer lock")
	}
}

// PHASE 2 — TAKEOVER only on expiry; a LIVE session is never stolen.
func TestEditSession_TakeoverOnlyWhenExpired_RealPG(t *testing.T) {
	store, d, pageID, alice, bob, ws := seed(t)
	ctx := context.Background()

	if _, err := store.Acquire(ctx, pageID, ws, alice); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}

	// LIVE session: neither Bob's Acquire nor Takeover may steal it.
	if _, err := store.Acquire(ctx, pageID, ws, bob); !errors.Is(err, editsession.ErrHeldByOther) {
		t.Errorf("Acquire on live foreign session = %v, want ErrHeldByOther", err)
	}
	if _, err := store.Takeover(ctx, pageID, ws, bob); !errors.Is(err, editsession.ErrHeldByOther) {
		t.Errorf("Takeover on LIVE session = %v, want ErrHeldByOther (live must not be stolen)", err)
	}
	if got, _ := store.Get(ctx, pageID, ws); got == nil || got.Holder != alice {
		t.Fatalf("after failed steal, holder = %+v, want alice", got)
	}

	// Expire Alice's session (backdate the heartbeat past any TTL), then Bob may take over.
	if _, err := d.Pool.Exec(ctx,
		`UPDATE page_edit_sessions SET last_heartbeat = now() - interval '1 hour' WHERE page_id = $1`, pageID); err != nil {
		t.Fatalf("backdate heartbeat: %v", err)
	}
	tk, err := store.Takeover(ctx, pageID, ws, bob)
	if err != nil {
		t.Fatalf("Takeover after expiry: %v", err)
	}
	if tk.Holder != bob || !tk.Live {
		t.Fatalf("after takeover, session = %+v, want holder=bob live=true", tk)
	}
}

// PHASE 2 — HEARTBEAT and RELEASE are holder-only (no hijack via heartbeat, no disturb via a
// stale releaser).
func TestEditSession_HeartbeatAndReleaseAreHolderOnly_RealPG(t *testing.T) {
	store, _, pageID, alice, bob, ws := seed(t)
	ctx := context.Background()
	if _, err := store.Acquire(ctx, pageID, ws, alice); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}

	if _, err := store.Heartbeat(ctx, pageID, ws, bob); !errors.Is(err, editsession.ErrHeldByOther) {
		t.Errorf("Bob heartbeat on Alice's session = %v, want ErrHeldByOther", err)
	}
	// Bob's release is a no-op; Alice still holds.
	if err := store.Release(ctx, pageID, ws, bob); err != nil {
		t.Fatalf("bob release (no-op) errored: %v", err)
	}
	if got, _ := store.Get(ctx, pageID, ws); got == nil || got.Holder != alice {
		t.Fatalf("after bob's no-op release, holder = %+v, want alice", got)
	}
}
