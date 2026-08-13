package mcp_test

// THE list_pages TOOL DOCUMENTS A parent_id SCOPE AND THE STORE THREW IT AWAY.
//
// The tool's own advertised schema says it in two places (server.go): the description is "List
// pages in a space, optionally scoped to children of a specific parent_id", and the argument is
// documented as "Restrict to children of this page". toolListPages honours that faithfully — it
// sets page.PageFilter.ParentID — and page.Store.List's SQL was
//
//	SELECT ... FROM pages WHERE space_id = $1 ORDER BY ... LIMIT $2 OFFSET $3
//
// with no reference to the field at all. The argument was accepted, carried through two layers,
// and dropped at the query.
//
// ⚠ MEASURED THROUGH THE SHIPPED JSON-RPC CHAIN, at a68fe86, before this guard existed. A space
// holding Anchor, Parent, Child (parent=Parent) and Not A Child; list_pages{space_id, parent_id:
// Parent} returned FOUR rows:
//
//	[{"title":"Anchor"},{"title":"Parent"},{"title":"Not A Child"},{"title":"Child"}]
//
// ⚠ WHY THIS IS WORSE THAN A MISSING FEATURE, AND WHY IT IS THE READER WHO PAYS. The caller is an
// AI agent. An unscoped answer to a scoped question is not silence it can notice — it is a
// well-formed list of pages that looks exactly like a correctly-scoped one, and the agent has no
// second surface to check it against. "What lives under this page?" was answered with "everything
// in the space", and the answer includes the parent itself, so even the shape of the reply does
// not give it away. A refused argument, or an empty list, would each have been honest.
//
// ⚠ THE INDEX FOR THE MISSING PREDICATE HAS SHIPPED SINCE MIGRATION 0002:
// `idx_pages_parent ON pages(parent_id) WHERE parent_id IS NOT NULL`. The query this file
// restores is the one that index was created for.
//
// WHY THE GUARD IS HERE AND NOT ONLY AT THE STORE. The store is where the predicate lives and
// listfilter_realpg_test.go pins it there, both directions. This file exists because the PROMISE
// is made here — in a tool schema an agent reads — and a store test cannot see whether the tool
// still passes the argument down. Both halves have to hold for the sentence in the schema to be
// true, and they are held by different packages.
//
// ⚠ [UNSCOPED] IS NOT DECORATION. Without it the whole file passes against a List that returns
// nothing, or one that filters when it should not; with the parent arm alone, `WHERE false` is
// green. It is the negative direction of the same predicate and it is why an always-on filter
// cannot hide here.

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

func TestMCPListPages_ParentIDScopesToChildren_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	bob := d.Member(t, ws, "bob@corp.com")
	pages := page.NewStore(d.Pool)

	// One space, seeded through the real store so depth is derived the way Create derives it.
	// d.Page mints a space per page, so it is used once, for the space.
	anchor := d.Page(t, ws, bob, "Anchor")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id = $1`, anchor).Scan(&spaceID); err != nil {
		t.Fatalf("read seed space: %v", err)
	}
	mk := func(title string, parent *string) string {
		t.Helper()
		p, err := pages.Create(ctx, model.Page{
			SpaceID: spaceID, WorkspaceID: ws, Title: title, ParentID: parent, CreatedBy: bob,
		})
		if err != nil {
			t.Fatalf("seed %q: %v", title, err)
		}
		return p.ID
	}
	parent := mk("Parent", nil)
	mk("Child", &parent)
	// A SECOND parent with its own child. Without it, "return the children of the FIRST parent
	// you can find" and "return the children of the parent you were asked about" are the same
	// answer, and so are "return every page that has any parent" and the correct one.
	other := mk("Other Parent", nil)
	mk("Other Child", &other)
	// Childless, and not a leaf of anything: the row that says an unmatched scope is EMPTY
	// rather than falling back to the space.
	childless := mk("Childless", nil)

	chain := newMCPChain(t, d)

	// The oracle is the shipped tool's own payload, decoded as titles.
	titles := func(t *testing.T, args map[string]any) []string {
		t.Helper()
		args["space_id"] = spaceID
		rr := callTool(chain, "bob@corp.com", true, "list_pages", args)
		if rr.Code != http.StatusOK {
			t.Fatalf("list_pages %v: HTTP %d — %s", args, rr.Code, rr.Body.String())
		}
		var env struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("list_pages %v: decode envelope: %v — %s", args, err, rr.Body.String())
		}
		if len(env.Error) > 0 {
			t.Fatalf("list_pages %v: rpc error %s", args, env.Error)
		}
		if len(env.Result.Content) == 0 {
			t.Fatalf("list_pages %v: no tool content — %s", args, rr.Body.String())
		}
		var rows []struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal([]byte(env.Result.Content[0].Text), &rows); err != nil {
			t.Fatalf("list_pages %v: decode payload: %v — %s", args, err, env.Result.Content[0].Text)
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Title)
		}
		sort.Strings(out)
		return out
	}
	same := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// PRECONDITION. Every assertion below is about which of these six rows come back; if the
	// fixture never landed, an empty answer would satisfy the scoped cases for the wrong reason.
	all := []string{"Anchor", "Child", "Childless", "Other Child", "Other Parent", "Parent"}
	if got := titles(t, map[string]any{}); !same(got, all) {
		t.Fatalf("fixture is wrong: the unscoped tool call returned %v, want %v", got, all)
	}

	// [SCOPED] — the sentence in the tool schema. The parent itself is deliberately in the want
	// list's ABSENCE: a page is not its own child, and `WHERE id = $n OR parent_id = $n` is the
	// near-miss this names.
	if got, want := titles(t, map[string]any{"parent_id": parent}), []string{"Child"}; !same(got, want) {
		t.Errorf("[SCOPED] list_pages(parent_id=Parent) = %v, want %v — the documented scope was dropped", got, want)
	}

	// [OTHER-PARENT] — the scope is the parent that was ASKED for, not whichever one has children.
	if got, want := titles(t, map[string]any{"parent_id": other}), []string{"Other Child"}; !same(got, want) {
		t.Errorf("[OTHER-PARENT] list_pages(parent_id=Other Parent) = %v, want %v", got, want)
	}

	// [EMPTY] — a scope that matches nothing is an empty list, never the whole space. This is the
	// assertion that separates a real predicate from "filter, but fall back if it is unhelpful".
	if got := titles(t, map[string]any{"parent_id": childless}); len(got) != 0 {
		t.Errorf("[EMPTY] list_pages(parent_id=Childless) = %v, want no rows", got)
	}

	// [UNSCOPED] — no parent_id still lists the space. Re-asserted AFTER the scoped calls so a
	// filter that latches on once it has been used cannot pass.
	if got := titles(t, map[string]any{}); !same(got, all) {
		t.Errorf("[UNSCOPED] list_pages(no parent_id) = %v, want %v — the space listing must not be filtered", got, all)
	}
}
