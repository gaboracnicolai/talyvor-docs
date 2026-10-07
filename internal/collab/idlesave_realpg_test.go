package collab

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

const (
	inWindowEdit  = `{"type":"doc","tag":"TYPED-INSIDE-THE-AUTOSAVE-WINDOW-7e31"}`
	reopenedEdit  = `{"type":"doc","tag":"FIRST-EDIT-AFTER-REOPENING-2b90"}`
	firstSessionA = `{"type":"doc","tag":"FIRST-SESSION-A"}`
	firstSessionB = `{"type":"doc","tag":"FIRST-SESSION-B"}`
)

// An edit acked inside the autosave window, with no tick before the socket closes, reaches
// pages.content. flush is never called here: the save the last disconnect makes is the only one.
func TestCollab_EditInAutosaveWindow_SurvivesDisconnect(t *testing.T) {
	d := testutil.New(t)
	pageID, _, _ := lockSeed(t, d)
	env := newLockEnv(t, d)

	conn := env.dial(t, pageID, "a-client", "alice@corp.com")
	readUntil(t, conn, "init")
	sendChange(t, conn, "a1", inWindowEdit, 0)
	readUntil(t, conn, "ack")
	_ = conn.Close()
	waitForEngineRelease(t, env.engine, pageID)

	if c := env.contentOf(t, pageID); !strings.Contains(c, "TYPED-INSIDE-THE-AUTOSAVE-WINDOW-7e31") {
		t.Errorf("an edit acked before the socket closed was lost: pages.content=%s", c)
	}
}

// A page opened again after its state was released starts back at version 1, and its first edit is
// saved by the next tick. When the saver kept the old session's version, it skipped every save of
// the new session until its version overtook the old one.
func TestCollab_ReopenedPage_FirstEditIsSaved(t *testing.T) {
	d := testutil.New(t)
	pageID, _, _ := lockSeed(t, d)
	env := newLockEnv(t, d)

	first := env.dial(t, pageID, "a-client", "alice@corp.com")
	readUntil(t, first, "init")
	sendChange(t, first, "a1", firstSessionA, 0)
	readUntil(t, first, "ack")
	sendChange(t, first, "a2", firstSessionB, 1)
	readUntil(t, first, "ack")
	env.saver.flush(context.Background())
	_ = first.Close()
	waitForEngineRelease(t, env.engine, pageID)

	second := env.dial(t, pageID, "b-client", "bob@corp.com")
	readUntil(t, second, "init")
	sendChange(t, second, "b1", reopenedEdit, 0)
	readUntil(t, second, "ack")
	env.saver.flush(context.Background())

	if c := env.contentOf(t, pageID); !strings.Contains(c, "FIRST-EDIT-AFTER-REOPENING-2b90") {
		t.Errorf("the first edit after reopening the page was not saved by the tick: pages.content=%s", c)
	}
}

// A page whose save fails when its last client leaves stays for one retry on the next tick, and is
// released if the retry fails too, so a page that can never be saved is not held in memory for ever.
func TestCollab_UnsavableIdlePage_IsReleasedAfterOneRetry(t *testing.T) {
	engine := NewOTEngine()
	calls := 0
	saver := NewAutoSaver(engine, func(context.Context, string, string, string) error {
		calls++
		return errors.New("page is gone")
	})
	if _, err := engine.Join("pg-1", "c-1", "m-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply("pg-1", Change{ClientID: "c-1", Snapshot: `{"type":"doc"}`}); err != nil {
		t.Fatal(err)
	}
	engine.Leave("pg-1", "c-1")
	if snap, _, _ := engine.Snapshot("pg-1"); snap == "" || calls != 1 {
		t.Fatalf("after a failed disconnect save: snapshot kept=%v, save calls=%d; want kept, 1", snap != "", calls)
	}
	saver.flush(context.Background())
	if snap, _, _ := engine.Snapshot("pg-1"); snap != "" || calls != 2 {
		t.Fatalf("after the failed retry: snapshot kept=%v, save calls=%d; want released, 2", snap != "", calls)
	}
}
