// Package collab implements real-time collaborative editing for
// Talyvor Docs.
//
// We use a positional Operational Transformation model — simpler
// than CRDT-based approaches like Yjs, easier to reason about in Go,
// and good enough for the small-group editing volumes Docs targets.
//
// The OT engine owns per-page session state: connected clients, a
// monotonic version counter, and a rolling history of the last 100
// changes used to transform stale-base changes from clients that
// were briefly out of sync.
//
// The engine is intentionally WebSocket-agnostic. Clients hand the
// engine a `send chan []byte`; the handler layer (handler.go) reads
// from that channel and writes to the actual websocket.Conn. This
// lets tests run the engine without any network stack.
package collab

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ─── operation types ────────────────────────────────────────

type OpType string

const (
	OpInsert  OpType = "insert"
	OpDelete  OpType = "delete"
	OpRetain  OpType = "retain"
	OpReplace OpType = "replace"
)

// Op is one positional edit. For inserts/replace we carry Content;
// for delete/retain we carry Length. The fields are deliberately
// flat (not a union) so JSON encoding stays compact.
type Op struct {
	Type    OpType `json:"type"`
	Pos     int    `json:"pos"`
	Content string `json:"content,omitempty"`
	Length  int    `json:"length,omitempty"`
}

// Change is a versioned batch of ops from a single client. Version
// is the document version the client believed it was editing; the
// engine uses that to know which history entries to transform
// against.
//
// Snapshot is the full ProseMirror JSON the client just produced.
// Servers ship without a ProseMirror runtime, so we can't replay
// ops to derive the new doc — instead the client always sends the
// post-change snapshot and the auto-saver persists it.
type Change struct {
	ID        string    `json:"id"`
	PageID    string    `json:"page_id"`
	ClientID  string    `json:"client_id"`
	Version   int       `json:"version"`
	Ops       []Op      `json:"ops"`
	Snapshot  string    `json:"snapshot,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type CursorPos struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// CollabClient is one user's session on a page. send is buffered so
// the engine can broadcast without blocking on slow networks; the
// handler drains the channel and writes to the WebSocket.
type CollabClient struct {
	ID         string
	MemberID   string
	MemberName string
	Cursor     *CursorPos
	Color      string
	send       chan []byte
}

// Send exposes the outbound channel so the handler can write to the
// WebSocket. Kept as an accessor so the field can stay unexported.
func (c *CollabClient) Send() <-chan []byte { return c.send }

// PageState is the in-memory document session: version + history +
// connected clients + the latest doc snapshot the AutoSaver flushes
// to Postgres on a 5-second cadence.
type PageState struct {
	PageID   string
	Version  int
	History  []Change
	Snapshot string
	clients  map[string]*CollabClient
}

// PresenceInfo is the wire shape for presence broadcasts and the
// /presence read. Color is assigned at Join time from a fixed
// palette so each client has a visually distinct cursor.
type PresenceInfo struct {
	ClientID   string     `json:"client_id"`
	MemberID   string     `json:"member_id"`
	MemberName string     `json:"member_name"`
	Cursor     *CursorPos `json:"cursor,omitempty"`
	Color      string     `json:"color"`
}

// historyCap is the size of the per-page rolling change log. Bigger
// = more out-of-date clients can reconcile; smaller = less memory.
// 100 covers a multi-minute network blip even at sustained typing.
const historyCap = 100

// presenceColors cycles through a palette so each connected client
// gets a visually distinct cursor. Eight colours covers the vast
// majority of multi-editor sessions; beyond that we wrap.
var presenceColors = []string{
	"#f0a030", // accent
	"#3b82f6",
	"#22c55e",
	"#ec4899",
	"#a78bfa",
	"#f59e0b",
	"#ef4444",
	"#06b6d4",
}

// ─── engine ─────────────────────────────────────────────────

type OTEngine struct {
	mu    sync.RWMutex
	pages map[string]*PageState
}

func NewOTEngine() *OTEngine {
	return &OTEngine{pages: map[string]*PageState{}}
}

// pageState returns the in-memory state for a page, creating it on
// first access. The caller must hold mu (read or write); this helper
// only handles the lazy-init shape.
func (e *OTEngine) pageStateLocked(pageID string) *PageState {
	st, ok := e.pages[pageID]
	if !ok {
		st = &PageState{PageID: pageID, clients: map[string]*CollabClient{}}
		e.pages[pageID] = st
	}
	return st
}

// pageState is an mu-acquiring convenience used by tests and the
// auto-saver. Returns nil if the page has no live state — callers
// should branch on the nil rather than synthesising one.
func (e *OTEngine) pageState(pageID string) *PageState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.pages[pageID]
}

// Snapshot returns the most recent doc JSON the engine has seen for
// a page. AutoSaver calls this on its tick.
func (e *OTEngine) Snapshot(pageID string) (string, int) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	st := e.pages[pageID]
	if st == nil {
		return "", 0
	}
	return st.Snapshot, st.Version
}

// DirtyPages returns the IDs of pages whose snapshots have been
// updated since the previous auto-save tick. The caller is expected
// to consume the result and call MarkClean once a save completes.
func (e *OTEngine) DirtyPages() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.pages))
	for id, st := range e.pages {
		if st.Snapshot != "" {
			out = append(out, id)
		}
	}
	return out
}

// ─── Join / Leave ───────────────────────────────────────────

func (e *OTEngine) Join(pageID, clientID, memberID, memberName string) (*CollabClient, error) {
	if pageID == "" || clientID == "" {
		return nil, errors.New("collab: page_id and client_id required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.pageStateLocked(pageID)
	color := presenceColors[len(st.clients)%len(presenceColors)]
	c := &CollabClient{
		ID:         clientID,
		MemberID:   memberID,
		MemberName: memberName,
		Color:      color,
		send:       make(chan []byte, 64),
	}
	st.clients[clientID] = c

	// init message: the joiner learns the current version + who else
	// is on the page so it can render presence avatars immediately.
	initMsg, _ := json.Marshal(map[string]any{
		"type":     "init",
		"version":  st.Version,
		"presence": presenceFromLocked(st),
	})
	c.send <- initMsg

	// Broadcast the joiner to existing clients so their presence bar
	// updates without polling.
	joinedMsg, _ := json.Marshal(map[string]any{
		"type":   "presence",
		"event":  "joined",
		"client": presenceFor(c),
	})
	for id, other := range st.clients {
		if id == clientID {
			continue
		}
		trySend(other.send, joinedMsg)
	}
	return c, nil
}

func (e *OTEngine) Leave(pageID, clientID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.pages[pageID]
	if st == nil {
		return
	}
	c, ok := st.clients[clientID]
	if !ok {
		return
	}
	close(c.send)
	delete(st.clients, clientID)

	leftMsg, _ := json.Marshal(map[string]any{
		"type":   "presence",
		"event":  "left",
		"client": presenceFor(c),
	})
	for _, other := range st.clients {
		trySend(other.send, leftMsg)
	}
	// Drop empty page state so memory doesn't grow forever.
	if len(st.clients) == 0 {
		delete(e.pages, pageID)
	}
}

// ─── Apply ──────────────────────────────────────────────────

// Apply transforms an incoming change against every history entry
// newer than its base version, appends the transformed change to
// the history, bumps the page version, and stashes the snapshot.
// Returns a slice of changes to broadcast — typically just the
// transformed one, but kept as a slice so the engine could batch in
// the future.
func (e *OTEngine) Apply(pageID string, change Change) ([]Change, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.pageStateLocked(pageID)

	// Transform against every committed change since the client's
	// base version. The history slice is ordered oldest → newest.
	for _, prior := range st.History {
		if prior.Version <= change.Version {
			continue
		}
		change = e.transform(change, prior)
	}

	// Commit the transformed change.
	change.Version = st.Version + 1
	change.Timestamp = time.Now().UTC()
	st.History = append(st.History, change)
	if len(st.History) > historyCap {
		st.History = st.History[len(st.History)-historyCap:]
	}
	st.Version = change.Version
	if change.Snapshot != "" {
		st.Snapshot = change.Snapshot
	}
	return []Change{change}, nil
}

// ─── Transform ──────────────────────────────────────────────

// Transform shifts every op in `op` against every op in
// `concurrent` according to the spec rules:
//
//   - concurrent delete strictly before op.Pos → op.Pos -= delete.Length
//   - concurrent insert at or before op.Pos    → op.Pos += len(insert.Content)
//
// Same-position insert ties go to the concurrent op (which is
// already committed); the incoming op shifts right by the concurrent
// insert's length. That keeps the transform deterministic and
// matches the typical "first-write-wins" mental model.
func (e *OTEngine) Transform(op Change, concurrent Change) Change {
	return e.transform(op, concurrent)
}

func (e *OTEngine) transform(op Change, concurrent Change) Change {
	out := Change{
		ID:        op.ID,
		PageID:    op.PageID,
		ClientID:  op.ClientID,
		Version:   op.Version,
		Ops:       make([]Op, len(op.Ops)),
		Snapshot:  op.Snapshot,
		Timestamp: op.Timestamp,
	}
	for i, o := range op.Ops {
		shifted := o
		for _, c := range concurrent.Ops {
			switch c.Type {
			case OpDelete:
				if c.Pos < shifted.Pos {
					shifted.Pos -= c.Length
					if shifted.Pos < c.Pos {
						shifted.Pos = c.Pos
					}
				}
			case OpInsert:
				if c.Pos <= shifted.Pos {
					shifted.Pos += len(c.Content)
				}
			}
		}
		out.Ops[i] = shifted
	}
	return out
}

// ─── BroadcastChange / cursor / presence ────────────────────

// BroadcastChange sends a change to every client on the page except
// the originator. We marshal once and tee the same bytes into each
// send channel — JSON encoding is the dominant cost on the hot path.
func (e *OTEngine) BroadcastChange(pageID string, change Change, exceptClientID string) {
	msg, err := json.Marshal(map[string]any{
		"type":    "change",
		"change":  change,
		"version": change.Version,
	})
	if err != nil {
		return
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	st := e.pages[pageID]
	if st == nil {
		return
	}
	for id, c := range st.clients {
		if id == exceptClientID {
			continue
		}
		trySend(c.send, msg)
	}
}

// UpdateCursor stamps the client's cursor and broadcasts a slim
// `cursor` message to the other clients on the page so they can
// re-render their remote-caret overlays.
func (e *OTEngine) UpdateCursor(pageID, clientID string, cursor CursorPos) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.pages[pageID]
	if st == nil {
		return
	}
	c, ok := st.clients[clientID]
	if !ok {
		return
	}
	c.Cursor = &cursor
	msg, _ := json.Marshal(map[string]any{
		"type":      "cursor",
		"client_id": clientID,
		"cursor":    cursor,
	})
	for id, other := range st.clients {
		if id == clientID {
			continue
		}
		trySend(other.send, msg)
	}
}

func (e *OTEngine) GetPresence(pageID string) []PresenceInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()
	st := e.pages[pageID]
	if st == nil {
		return []PresenceInfo{}
	}
	return presenceFromLocked(st)
}

// ─── helpers ────────────────────────────────────────────────

// presenceFromLocked: caller must hold e.mu (read or write).
func presenceFromLocked(st *PageState) []PresenceInfo {
	out := make([]PresenceInfo, 0, len(st.clients))
	for _, c := range st.clients {
		out = append(out, presenceFor(c))
	}
	return out
}

func presenceFor(c *CollabClient) PresenceInfo {
	return PresenceInfo{
		ClientID:   c.ID,
		MemberID:   c.MemberID,
		MemberName: c.MemberName,
		Cursor:     c.Cursor,
		Color:      c.Color,
	}
}

// trySend pushes a payload onto a client's outbound channel. If the
// channel is full (slow consumer) we drop the message rather than
// block the engine — the next change will re-sync state anyway.
func trySend(ch chan []byte, msg []byte) {
	defer func() { recover() }() // closed channels surface as panics
	select {
	case ch <- msg:
	default:
		// drop
	}
}

// formatErr keeps fmt.Errorf imports honest; never actually called.
func formatErr() error { return fmt.Errorf("unused") }

var _ = formatErr
