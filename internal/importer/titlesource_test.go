package importer

import (
	"context"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/model"
)

// The imported page's TITLE, by source, with the three sources telling DIFFERENT stories.
//
// ⚠ WHY THIS FILE EXISTS RATHER THAN ANOTHER CASE IN confluence_test.go. The existing
// coverage (TestImportExport_ParsesHTMLAndCreatesPages) feeds HTML whose <title> and <h1>
// are the SAME STRING — "Auth Flow" in both — so it asserts a title that BOTH sources
// produce and cannot say which one did. Every case below makes the three sources disagree,
// which is the only fixture shape in which the answer is attributable.
func importOneHTML(t *testing.T, filename, body string) string {
	t.Helper()
	pages := &fakePages{}
	spaces := &fakeSpaces{defaultSpace: &model.Space{ID: "sp-title", WorkspaceID: "ws-title"}}
	imp := newImporter(pages, spaces)
	z := buildZip(t, map[string]string{filename: body})
	result, err := imp.ImportExport(context.Background(), "ws-title", "sp-title", z)
	if err != nil {
		t.Fatalf("ImportExport: %v", err)
	}
	if result.Imported != 1 || len(pages.created) != 1 {
		t.Fatalf("want exactly 1 imported page, got imported=%d created=%d (%+v)",
			result.Imported, len(pages.created), result.Errors)
	}
	return pages.created[0].Title
}

// <title> is the FIRST rung of htmlToProseMirror's documented fallback chain, and
// htmlWalker's own comment says firstH1 is tracked "so a missing <title> still gives the
// page a reasonable name" — i.e. <h1> is the fallback, not the source.
func TestImportTitle_HeadTitleWinsOverH1(t *testing.T) {
	got := importOneHTML(t, "123456789.html",
		`<!doctype html><html><head><title>Deploy runbook</title></head>`+
			`<body><h1>Untitled draft</h1><p>body</p></body></html>`)
	if got != "Deploy runbook" {
		t.Fatalf("title = %q, want %q (the <title>, not the <h1>)", got, "Deploy runbook")
	}
}

// THE CASE THAT IS USER-VISIBLE. A Confluence HTML space export names its files by page id
// (123456789.html), so when the only title-bearing element is <title>, falling through to
// the filename does not produce a "reasonable name" — it produces the page id.
func TestImportTitle_HeadTitleUsedWhenNoH1(t *testing.T) {
	got := importOneHTML(t, "123456789.html",
		`<!doctype html><html><head><title>Deploy runbook</title></head>`+
			`<body><h2>Stage 1</h2><p>body</p></body></html>`)
	if got != "Deploy runbook" {
		t.Fatalf("title = %q, want %q (the <title>, not the filename)", got, "Deploy runbook")
	}
}

// MUST-STAY-GREEN COMPANION: no <title> at all, so the <h1> is the answer. Without this a
// change that simply preferred <h1> everywhere would look like a fix above and silently
// delete the fallback.
func TestImportTitle_FirstH1WhenNoHeadTitle(t *testing.T) {
	got := importOneHTML(t, "123456789.html",
		`<!doctype html><html><body><h1>Deploy runbook</h1><h1>Second</h1><p>body</p></body></html>`)
	if got != "Deploy runbook" {
		t.Fatalf("title = %q, want the FIRST <h1> %q", got, "Deploy runbook")
	}
}

// MUST-STAY-GREEN COMPANION: neither source present, so the filename is the last resort —
// and it must stay the LAST one.
func TestImportTitle_FilenameWhenNeitherPresent(t *testing.T) {
	got := importOneHTML(t, "deploy-runbook.html",
		`<!doctype html><html><body><h2>Stage 1</h2><p>body</p></body></html>`)
	if got != "deploy-runbook" {
		t.Fatalf("title = %q, want the filename %q", got, "deploy-runbook")
	}
}

// A <title> WRITTEN INSIDE <body> IS A DIFFERENT PARSE, AND THAT IS THE WHOLE DEFECT'S
// MECHANISM. golang.org/x/net/html puts a head-declared <title> under /html/head and a
// body-declared one under /html/body (measured both ways), so a walker that skips the <head>
// branch reaches the second and never the first. Pinning both placements means the title
// source cannot silently become "wherever the parser happened to put it" again.
func TestImportTitle_BodyDeclaredTitleAlsoCounts(t *testing.T) {
	got := importOneHTML(t, "123456789.html",
		`<!doctype html><html><body><title>Deploy runbook</title><h1>Untitled draft</h1></body></html>`)
	if got != "Deploy runbook" {
		t.Fatalf("title = %q, want the body-declared <title> %q", got, "Deploy runbook")
	}
}

// <head> IS STILL SKIPPED FOR CONTENT. Making <title> reachable must not drag the rest of
// head into the document: a <style> block's CSS text rendering as a paragraph is exactly the
// regression the blanket skip existed to prevent.
func TestImportTitle_HeadContentStillNotImportedAsBlocks(t *testing.T) {
	pages := &fakePages{}
	spaces := &fakeSpaces{defaultSpace: &model.Space{ID: "sp-title", WorkspaceID: "ws-title"}}
	imp := newImporter(pages, spaces)
	z := buildZip(t, map[string]string{"123456789.html": `<!doctype html><html><head>` +
		`<title>Deploy runbook</title><style>.x{color:red}</style>` +
		`<script>var noise="SCRIPTNOISE"</script></head>` +
		`<body><p>real body</p></body></html>`})
	if _, err := imp.ImportExport(context.Background(), "ws-title", "sp-title", z); err != nil {
		t.Fatalf("ImportExport: %v", err)
	}
	p := pages.created[0]
	for _, noise := range []string{"color:red", "SCRIPTNOISE"} {
		if strings.Contains(p.Content, noise) || strings.Contains(p.ContentText, noise) {
			t.Fatalf("head content leaked into the document: %q found in %s / %s",
				noise, p.Content, p.ContentText)
		}
	}
}
