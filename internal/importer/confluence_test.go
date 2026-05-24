package importer

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/docs/internal/model"
)

// fakePages records every Create call so tests can assert on the
// imported page graph without a real database.
type fakePages struct {
	mu      sync.Mutex
	created []model.Page
	nextID  int
}

func (f *fakePages) Create(_ context.Context, p model.Page) (*model.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	p.ID = "pg-imported-" + strings.Repeat("x", 0) + itoa(f.nextID)
	f.created = append(f.created, p)
	return &p, nil
}

// fakeSpaces returns canned spaces; importer falls back to creating
// when a default space isn't supplied.
type fakeSpaces struct {
	defaultSpace *model.Space
	created      []model.Space
}

func (f *fakeSpaces) GetByID(_ context.Context, id string) (*model.Space, error) {
	if f.defaultSpace != nil && f.defaultSpace.ID == id {
		return f.defaultSpace, nil
	}
	return nil, nil
}
func (f *fakeSpaces) Create(_ context.Context, sp model.Space) (*model.Space, error) {
	sp.ID = "sp-new"
	f.created = append(f.created, sp)
	return &sp, nil
}

func itoa(n int) string {
	// Tiny stand-in to keep this file dep-free of strconv juggling.
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// buildZip is a test helper that builds an in-memory zip containing
// the given entries. The map key is the file path; the value is the
// file body.
func buildZip(t *testing.T, files map[string]string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return &buf
}

// ─── Notion ───

func TestImportFromNotion_ParsesMarkdownAndCreatesPages(t *testing.T) {
	pages := &fakePages{}
	spaces := &fakeSpaces{defaultSpace: &model.Space{ID: "sp-import", Name: "Imported", WorkspaceID: "ws-1"}}
	imp := newImporter(pages, spaces)

	z := buildZip(t, map[string]string{
		"Getting Started.md":  "# Getting Started\n\nWelcome to the team.",
		"Onboarding/Day 1.md": "# Day 1\n\n- Read CLAUDE.md\n- Set up env",
	})
	result, err := imp.ImportFromNotion(context.Background(), "ws-1", "sp-import", z)
	if err != nil {
		t.Fatalf("ImportFromNotion: %v", err)
	}
	if result.Imported != 2 {
		t.Fatalf("Imported = %d, want 2", result.Imported)
	}
	// Titles come from the first H1.
	var titles []string
	for _, p := range pages.created {
		titles = append(titles, p.Title)
	}
	if !contains(titles, "Getting Started") || !contains(titles, "Day 1") {
		t.Fatalf("expected titles 'Getting Started' + 'Day 1', got %v", titles)
	}
	// Content must be ProseMirror JSON.
	for _, p := range pages.created {
		if !strings.Contains(p.Content, `"type":"doc"`) {
			t.Fatalf("page %q content is not ProseMirror: %s", p.Title, p.Content)
		}
	}
}

func TestImportFromNotion_FolderStructureBecomesParentPages(t *testing.T) {
	pages := &fakePages{}
	spaces := &fakeSpaces{defaultSpace: &model.Space{ID: "sp-import", WorkspaceID: "ws-1"}}
	imp := newImporter(pages, spaces)

	z := buildZip(t, map[string]string{
		"Onboarding/Overview.md": "# Overview\n\nstart here",
		"Onboarding/Day 1.md":    "# Day 1\n\nfirst day",
	})
	_, err := imp.ImportFromNotion(context.Background(), "ws-1", "sp-import", z)
	if err != nil {
		t.Fatalf("ImportFromNotion: %v", err)
	}
	// The Onboarding folder should become a parent page that the
	// two .md files reference via ParentID.
	var parentID string
	for _, p := range pages.created {
		if p.Title == "Onboarding" {
			parentID = p.ID
		}
	}
	if parentID == "" {
		t.Fatalf("expected an Onboarding parent page to be created, got pages: %+v", pages.created)
	}
	children := 0
	for _, p := range pages.created {
		if p.ParentID != nil && *p.ParentID == parentID {
			children++
		}
	}
	if children != 2 {
		t.Fatalf("expected 2 children under Onboarding, got %d", children)
	}
}

func TestImportFromNotion_SkipsNonMarkdownFiles(t *testing.T) {
	pages := &fakePages{}
	spaces := &fakeSpaces{defaultSpace: &model.Space{ID: "sp-import", WorkspaceID: "ws-1"}}
	imp := newImporter(pages, spaces)

	z := buildZip(t, map[string]string{
		"Note.md":       "# Note\n\nbody",
		"image.png":     "not really a png",
		"index.txt":     "skip me",
		"_metadata.csv": "skip me too",
	})
	result, err := imp.ImportFromNotion(context.Background(), "ws-1", "sp-import", z)
	if err != nil {
		t.Fatalf("ImportFromNotion: %v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("Imported = %d, want 1", result.Imported)
	}
	if result.Skipped < 3 {
		t.Fatalf("Skipped = %d, want >= 3", result.Skipped)
	}
}

// ─── Confluence ───

func TestImportExport_ParsesHTMLAndCreatesPages(t *testing.T) {
	pages := &fakePages{}
	spaces := &fakeSpaces{defaultSpace: &model.Space{ID: "sp-import", WorkspaceID: "ws-1"}}
	imp := newImporter(pages, spaces)

	html := `<!doctype html><html><head><title>Auth Flow</title></head>
<body>
  <h1>Auth Flow</h1>
  <p>The auth flow has <strong>three</strong> stages.</p>
  <h2>Stage 1</h2>
  <ul><li>request token</li><li>verify</li></ul>
  <pre><code>curl -X POST /auth</code></pre>
</body></html>`

	z := buildZip(t, map[string]string{
		"page-1.html": html,
		"index.html":  "<html><body><h1>Index</h1></body></html>",
	})
	result, err := imp.ImportExport(context.Background(), "ws-1", "sp-import", z)
	if err != nil {
		t.Fatalf("ImportExport: %v", err)
	}
	if result.Imported != 2 {
		t.Fatalf("Imported = %d, want 2", result.Imported)
	}
	// Find the Auth Flow page and check its ProseMirror content.
	var auth *model.Page
	for i, p := range pages.created {
		if p.Title == "Auth Flow" {
			auth = &pages.created[i]
		}
	}
	if auth == nil {
		t.Fatalf("Auth Flow page not created: %+v", pages.created)
	}
	if !strings.Contains(auth.Content, `"type":"doc"`) {
		t.Fatalf("content not ProseMirror: %s", auth.Content)
	}
	// Should contain a heading and a list_item.
	if !strings.Contains(auth.Content, `"type":"heading"`) {
		t.Fatalf("heading missing in content: %s", auth.Content)
	}
	if !strings.Contains(auth.Content, `"type":"bullet_list"`) {
		t.Fatalf("bullet_list missing in content: %s", auth.Content)
	}
}

func TestImportExport_EmptyZipReturnsResult(t *testing.T) {
	pages := &fakePages{}
	spaces := &fakeSpaces{defaultSpace: &model.Space{ID: "sp-import", WorkspaceID: "ws-1"}}
	imp := newImporter(pages, spaces)

	result, err := imp.ImportExport(context.Background(), "ws-1", "sp-import", buildZip(t, map[string]string{}))
	if err != nil {
		t.Fatalf("ImportExport: %v", err)
	}
	if result.Imported != 0 || result.Skipped != 0 {
		t.Fatalf("expected zero counts, got %+v", result)
	}
}

func TestImportExport_SkipsNonHTMLFiles(t *testing.T) {
	pages := &fakePages{}
	spaces := &fakeSpaces{defaultSpace: &model.Space{ID: "sp-import", WorkspaceID: "ws-1"}}
	imp := newImporter(pages, spaces)

	z := buildZip(t, map[string]string{
		"page.html":  "<html><body><h1>Title</h1></body></html>",
		"image.png":  "binary",
		"styles.css": "ignored",
	})
	result, err := imp.ImportExport(context.Background(), "ws-1", "sp-import", z)
	if err != nil {
		t.Fatalf("ImportExport: %v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("Imported = %d, want 1", result.Imported)
	}
	if result.Skipped < 2 {
		t.Fatalf("Skipped = %d, want >= 2", result.Skipped)
	}
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
