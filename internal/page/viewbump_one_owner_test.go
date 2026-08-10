package page_test

// THE VIEW BUMP HAD TWO WRITERS AND THEY DISAGREED ABOUT A COLUMN THAT DECIDES A FEATURE.
//
// `internal/analytics` owns "a page was viewed": handler.go:32 registers
// POST /spaces/{s}/pages/{p}/view, and internal/page/handler.go:53-59 says so in as many
// words — "this package's registration was shadowed dead code … One path, one owner: the
// duplicate meant a handler could look live while another one served."
//
// THAT FIXED THE ROUTE AND LEFT THE STORE. `page.Store.RecordView` survived with its own copy
// of the bump, and the two statements were NOT the same statement:
//
//	analytics: UPDATE pages SET view_count = view_count + 1, last_viewed_at = NOW() WHERE id = $1
//	page:      UPDATE pages SET view_count = view_count + 1, last_viewed_at = NOW(),
//	                            updated_at = NOW() WHERE id = $1     <-- and nothing else says so
//
// `updated_at` is what GetStalePages keys on (store.go: `updated_at < NOW() - INTERVAL '1 day'
// * stale_after_days`). MEASURED ON REAL POSTGRES, two identical pages both 30 days stale with
// stale_after_days = 7, one view each, both view_count 0 -> 1:
//
//	page.Store.RecordView(A)       -> A DROPPED OFF THE STALE LIST
//	analytics.Store.RecordView(B)  -> B STAYED ON IT
//
// So the shadowed copy does not merely duplicate the live one: reading a document would have
// reset its freshness clock, and the more a document is read — the more it matters — the less
// likely it would ever be flagged for review. Its own doc comment did not mention updated_at
// either ("increments view_count + bumps last_viewed_at"), so the divergence was invisible
// from every side: the comment, the route table, and the mock test that only matched
// `UPDATE pages SET view_count`.
//
// ⚠ IT WAS UNREACHABLE, AND THAT IS MEASURED, NOT ASSUMED. At 35ce495 a whole-tree census
// found ZERO callers of `RecordViewInWorkspaces` — no route, no other package, not one test —
// and exactly one caller of `RecordView`: the uncalled wrapper, plus a mock test of itself. So
// the honest answer to W3.1 finding (10)'s "page's RecordView view_count UPDATE is held by the
// mock and not at the product level" is stronger than the finding assumed: there was no
// product level to hold. A real-PG test for it would have been a test of code no request can
// reach — and it would have PINNED the staleness reset as correct.
//
// THIS TEST IS THE CENSUS THAT KEEPS IT AT ONE OWNER. Its companion,
// TestRecordedView_DoesNotResetTheStalenessClock_RealPG, is what says WHY one owner matters —
// the census cannot tell a correct copy from a divergent one, and the behaviour test cannot
// see a second copy that no request reaches yet.
//
// ⚠ SOURCE-DERIVED, SO IT CARRIES A FLOOR AND A PINNED OWNER. A regex census cannot notice the
// tree SHRINKING: delete the live bump too and a "no duplicates" check reports clean. The floor
// asserts the site still exists, and the owner is a hardcoded literal rather than a value read
// back from the same parse.
//
// ⚠ AND THE FLOOR IS EARNED BY A CONTROL THAT MUTATES THIS FILE, NOT THE PRODUCT — S7 in
// scripts/w31-viewbump-owner-controls.py blinds the predicate so it matches nothing. That is
// the only way to isolate a floor from the predicate it sits behind: S4 (the live statement
// deleted) fires the floor too, but it fires other assertions as well, so without S7 the floor
// could have been deleted and every control would still have read as predicted.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The one site, pinned as a literal. Read back from the parse it would compare to itself.
const viewBumpOwner = "internal/analytics/store.go"

func TestViewCountBump_HasExactlyOneWriter(t *testing.T) {
	root := filepath.Join("..", "..")
	var sites []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", ".git", "node_modules", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "UPDATE pages SET view_count") {
			rel, _ := filepath.Rel(root, path)
			sites = append(sites, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// FLOOR FIRST. An empty set is what a broken predicate produces, and it would otherwise
	// read as "no duplicates" — the strongest possible pass from an instrument that read
	// nothing.
	if len(sites) == 0 {
		t.Fatalf("census found NO `UPDATE pages SET view_count` site at all. Either the bump " +
			"was deleted — in which case a viewed page no longer counts — or this predicate " +
			"stopped matching the tree. Both are failures; neither is a clean run.")
	}
	if len(sites) != 1 || sites[0] != viewBumpOwner {
		t.Fatalf("the view-count bump must have exactly ONE writer, %s. Found %d: %v\n"+
			"A second copy is not a harmless duplicate: the one deleted in this commit also set "+
			"updated_at, which GetStalePages keys on, so a READ reset the page's freshness "+
			"clock. If a new owner is intended, move the statement and update this literal.",
			viewBumpOwner, len(sites), sites)
	}
}
