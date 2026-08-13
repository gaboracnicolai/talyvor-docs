package importer_test

// THE IMPORT DOOR INTO page.Store.Create WAS NOT IN docs_pages_created_total.
//
// The full account of the defect is in internal/page/pagescreated_metric_realpg_test.go. This
// is the door where the under-count is largest by orders of magnitude: one request creates one
// page per file in the archive, and internal/importer's own upload cap calls 200MB "the largest
// reasonable Confluence space export". A bulk import moved the only page-creation signal this
// service exports by ZERO.
//
// ⚠ THE ARCHIVE HOLDS TWO FILES SO THE ASSERTION IS +2, NOT +1. A counter incremented once per
// REQUEST — the obvious wrong fix at a handler — passes a +1 test on every one of these four
// surfaces and still mis-counts the only one where a call and a page differ. Two is the
// smallest number that tells those apart.
//
// RED at a59c424 (before the Inc moved into the store): moved by 0, want 2.

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// twoPageNotionZip is notionZip with a second document. Both at the archive ROOT: a file in a
// folder would make the importer mint a folder page too, and this case is about the count being
// per PAGE, which a third page of a different kind would blur rather than sharpen.
func twoPageNotionZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, doc := range []struct{ name, body string }{
		{"first.md", "# First Doc\n\nbody text"},
		{"second.md", "# Second Doc\n\nmore body text"},
	} {
		f, err := zw.Create(doc.name)
		if err != nil {
			t.Fatalf("zip create %s: %v", doc.name, err)
		}
		if _, err := f.Write([]byte(doc.body)); err != nil {
			t.Fatalf("zip write %s: %v", doc.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestImport_CountsEveryPageInPagesCreatedTotal_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")
	editor := d.Member(t, W, "editor@corp.com")

	targetSpace, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: W, Name: "Target", Slug: "target-" + owner[len(owner)-6:], Private: true, CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed target space: %v", err)
	}
	if err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
		ResourceType: permission.ResourceSpace, ResourceID: targetSpace.ID, SubjectType: "member",
		SubjectID: editor, Access: permission.AccessEdit, WorkspaceID: W, GrantedBy: owner,
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	chain := impChain(d)
	before := testutil.ScrapeCounter(t, "docs_pages_created_total")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, importReq(t, "editor@corp.com", W, targetSpace.ID, twoPageNotionZip(t)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("import: HTTP %d — %s", rr.Code, rr.Body.String())
	}
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pages WHERE space_id=$1`, targetSpace.ID).Scan(&n); err != nil {
		t.Fatalf("count pages: %v", err)
	}
	if n != 2 {
		t.Fatalf("precondition: the import landed %d pages, want 2 — %s", n, rr.Body.String())
	}
	if got := testutil.ScrapeCounter(t, "docs_pages_created_total") - before; got != 2 {
		t.Errorf("[IMPORT-COUNTED] docs_pages_created_total moved by %v after an import landed 2 "+
			"pages, want 2 — the bulk door into page.Store.Create is not in the metric", got)
	}
}
