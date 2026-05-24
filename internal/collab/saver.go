package collab

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// SaveFunc persists a page snapshot. main.go wires this to a thin
// closure over page.Store.Update — the callback shape keeps this
// file free of an internal/page import and lets tests inject
// anything Go-callable.
type SaveFunc func(ctx context.Context, pageID, content string) error

// AutoSaver flushes the engine's in-memory page snapshots to disk
// on a periodic tick. The client always sends the full post-change
// ProseMirror JSON with every Change, so the snapshot the engine
// holds is the authoritative bytes-on-disk value — AutoSaver never
// has to replay ops.
type AutoSaver struct {
	engine *OTEngine
	save   SaveFunc
	mu     sync.Mutex
	// lastSavedVersion tracks the most recent doc version we
	// persisted per page. We only call the save callback when the
	// engine version moves past the saved one.
	lastSavedVersion map[string]int
	interval         time.Duration
}

func NewAutoSaver(engine *OTEngine, save SaveFunc) *AutoSaver {
	return &AutoSaver{
		engine:           engine,
		save:             save,
		lastSavedVersion: map[string]int{},
		interval:         5 * time.Second,
	}
}

// MarkDirty is retained as part of the documented API. The current
// implementation polls the engine directly so this is a no-op; we
// keep it so callers can flip to push-based saves later without
// breaking the surface.
func (s *AutoSaver) MarkDirty(_ string) {}

// Start runs the save loop until the context cancels. Each tick we
// ask the engine which pages have non-empty snapshots and flush
// every one whose version has advanced since the last save.
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
		snap, ver := s.engine.Snapshot(pageID)
		if snap == "" {
			continue
		}
		s.mu.Lock()
		lastVer := s.lastSavedVersion[pageID]
		s.mu.Unlock()
		if ver <= lastVer {
			continue
		}
		// content_text is left empty here; the page store extracts
		// it from the JSON on Update, so the field stays in sync.
		// Best-effort: a single transient failure shouldn't take
		// the save loop down for everyone.
		if err := s.save(ctx, pageID, snap); err != nil {
			slog.Warn("collab: autosave failed",
				slog.String("page_id", pageID),
				slog.String("err", err.Error()))
			continue
		}
		s.mu.Lock()
		s.lastSavedVersion[pageID] = ver
		s.mu.Unlock()
	}
}
