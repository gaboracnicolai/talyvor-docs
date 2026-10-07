package collab

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// SaveFunc persists a page snapshot ON BEHALF OF the member whose change produced it. main.go wires
// this to a thin closure over page.Store.Update — the callback shape keeps this file free of an
// internal/page import and lets tests inject anything Go-callable.
//
// memberID IS NOT OPTIONAL BOOKKEEPING. page.Store.Update runs the lock + approval + single-writer
// gate against updates["updated_by"] — what its own comment calls "the canonical editor identity".
// This callback used to pass content alone, so that gate was asked whether the EMPTY STRING may
// write, and pagelock answers "Locked by <holder>" to that question for every held lock, the
// holder's own included. The result was a change the socket had ACKed and broadcast that never
// reached pages.content, retried every 5s until OTEngine.Leave freed the snapshot and there was
// nothing left to retry. Measured in lockedsave_realpg_test.go.
//
// It is also what attributes the write: updated_by is the author recorded on the page_versions row
// (page.Store.appendVersion) and the actor passed to SyncLinks, both of which took "" for every
// collab-authored save.
type SaveFunc func(ctx context.Context, pageID, content, memberID string) error

// idleSaveTimeout bounds the save made when the last client leaves a page. That save runs on the
// departing socket's goroutine with a context of its own: the closed socket's request context is no
// use to a database call.
const idleSaveTimeout = 10 * time.Second

// AutoSaver flushes the engine's in-memory page snapshots to disk
// on a periodic tick, and once more when the last client leaves a
// page. The client always sends the full post-change ProseMirror
// JSON with every Change, so the snapshot the engine holds is the
// authoritative bytes-on-disk value — AutoSaver never has to replay
// ops.
type AutoSaver struct {
	engine *OTEngine
	save   SaveFunc
	// mu serialises saves, so the disconnect-time save and a tick that lands at the same moment
	// do not both write the same snapshot.
	mu       sync.Mutex
	interval time.Duration
}

// NewAutoSaver also installs itself as the engine's idle hook, so the edit a client made inside the
// autosave window is written when its socket closes rather than dropped with the page state.
func NewAutoSaver(engine *OTEngine, save SaveFunc) *AutoSaver {
	s := &AutoSaver{
		engine:   engine,
		save:     save,
		interval: 5 * time.Second,
	}
	engine.mu.Lock()
	engine.onIdle = s.saveIdle
	engine.mu.Unlock()
	return s
}

// MarkDirty is retained as part of the documented API. The current
// implementation polls the engine directly so this is a no-op; we
// keep it so callers can flip to push-based saves later without
// breaking the surface.
func (s *AutoSaver) MarkDirty(_ string) {}

// Start runs the save loop until the context cancels. Each tick we
// ask the engine which pages have unsaved snapshots and flush each.
func (s *AutoSaver) Start(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.flush(ctx)
		}
	}
}

func (s *AutoSaver) flush(ctx context.Context) {
	for _, pageID := range s.engine.DirtyPages() {
		if err := s.savePage(ctx, pageID); err != nil && s.engine.DropIdle(pageID) {
			// Nobody is connected, so this save was already tried when the last client left and has
			// now failed on the retry too. A page that cannot be saved (deleted, or locked by someone
			// else) would fail the same way for ever, so release it and say what was lost.
			slog.Warn("collab: unsaved edit discarded after its last editor left",
				slog.String("page_id", pageID))
		}
	}
}

// saveIdle is the engine's idle hook: the last client has left pageID with an unsaved snapshot.
// A failure leaves the page in the engine for the next tick to retry.
func (s *AutoSaver) saveIdle(pageID string) {
	ctx, cancel := context.WithTimeout(context.Background(), idleSaveTimeout)
	defer cancel()
	_ = s.savePage(ctx, pageID)
}

func (s *AutoSaver) savePage(ctx context.Context, pageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ver, by, ok := s.engine.unsavedSnapshot(pageID)
	if !ok {
		return nil
	}
	// content_text is left empty here; the page store extracts
	// it from the JSON on Update, so the field stays in sync.
	// Best-effort: a single transient failure shouldn't take
	// the save loop down for everyone.
	if err := s.save(ctx, pageID, snap, by); err != nil {
		slog.Warn("collab: autosave failed",
			slog.String("page_id", pageID),
			slog.String("err", err.Error()))
		return err
	}
	s.engine.MarkSaved(pageID, ver)
	return nil
}
