package export

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
)

// fakePages stubs the page-store lookups Exporter needs. Each test
// hands in a fixed graph; the fake never hits the database.
type fakePages struct {
	byID    map[string]*model.Page
	bySpace map[string][]model.Page
}

func (f *fakePages) GetByID(_ context.Context, id string) (*model.Page, error) {
	return f.byID[id], nil
}
func (f *fakePages) List(_ context.Context, filter page.PageFilter) ([]model.Page, error) {
	return f.bySpace[filter.SpaceID], nil
}

type fakeSpaces struct{ byID map[string]*model.Space }

func (f *fakeSpaces) GetByID(_ context.Context, id string) (*model.Space, error) {
	return f.byID[id], nil
}

func makePage(id, title, prosemirror string, parent *string, position float64) *model.Page {
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	return &model.Page{
		ID:        id,
		SpaceID:   "sp-1",
		Title:     title,
		Slug:      strings.ReplaceAll(strings.ToLower(title), " ", "-"),
		Content:   prosemirror,
		ParentID:  parent,
		Position:  position,
		Icon:      "📄",
		CreatedAt: now.Add(-24 * time.Hour),
		UpdatedAt: now,
	}
}

const samplePM = `{"type":"doc","content":[
  {"type":"heading","attrs":{"level":1},"content":[{"type":"text","text":"Hello"}]},
  {"type":"paragraph","content":[{"type":"text","text":"World"}]}
]}`

// ─── ToMarkdown ───

func TestToMarkdown_IncludesYAMLFrontmatter(t *testing.T) {
	exp := newExporter(&fakePages{
		byID: map[string]*model.Page{"pg-1": makePage("pg-1", "Auth Flow", samplePM, nil, 1)},
	}, &fakeSpaces{})

	md, err := exp.ToMarkdown(context.Background(), "pg-1", ExportOptions{})
	if err != nil {
		t.Fatalf("ToMarkdown: %v", err)
	}
	if !strings.HasPrefix(md, "---\n") {
		t.Fatalf("expected YAML frontmatter, got: %q", md[:30])
	}
	if !strings.Contains(md, "title: Auth Flow") {
		t.Fatalf("title missing: %q", md)
	}
	if !strings.Contains(md, "# Hello") || !strings.Contains(md, "World") {
		t.Fatalf("body not rendered: %q", md)
	}
}

func TestToMarkdown_IncludesChildrenInPositionOrder(t *testing.T) {
	parent := "pg-1"
	child1 := makePage("pg-2", "Child 1", samplePM, &parent, 1)
	child2 := makePage("pg-3", "Child 2", samplePM, &parent, 2)
	exp := newExporter(&fakePages{
		byID: map[string]*model.Page{
			parent:  makePage(parent, "Parent", samplePM, nil, 1),
			"pg-2":  child1,
			"pg-3":  child2,
		},
		bySpace: map[string][]model.Page{
			"sp-1": {*child2, *child1}, // intentionally reversed
		},
	}, &fakeSpaces{})

	md, err := exp.ToMarkdown(context.Background(), parent, ExportOptions{IncludeChildren: true})
	if err != nil {
		t.Fatalf("ToMarkdown: %v", err)
	}
	// Both children must appear and Child 1 must come before Child 2.
	c1 := strings.Index(md, "Child 1")
	c2 := strings.Index(md, "Child 2")
	if c1 < 0 || c2 < 0 {
		t.Fatalf("children missing: c1=%d c2=%d", c1, c2)
	}
	if c1 > c2 {
		t.Fatalf("children out of order: c1=%d c2=%d", c1, c2)
	}
}

// ─── ToHTML ───

func TestToHTML_IncludesInlineCSSAndBody(t *testing.T) {
	exp := newExporter(&fakePages{
		byID: map[string]*model.Page{"pg-1": makePage("pg-1", "Deploy Guide", samplePM, nil, 1)},
	}, &fakeSpaces{})
	html, err := exp.ToHTML(context.Background(), "pg-1", ExportOptions{})
	if err != nil {
		t.Fatalf("ToHTML: %v", err)
	}
	if !strings.Contains(html, "<style") {
		t.Fatal("expected inline <style> block")
	}
	if !strings.Contains(html, "<h1>Hello</h1>") {
		t.Fatalf("heading missing: %s", html)
	}
	if !strings.Contains(html, "<title>Deploy Guide</title>") {
		t.Fatal("HTML title missing")
	}
}

func TestToHTML_IncludesTOCWhenRequested(t *testing.T) {
	pm := `{"type":"doc","content":[
		{"type":"heading","attrs":{"level":1},"content":[{"type":"text","text":"Intro"}]},
		{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Setup"}]},
		{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Usage"}]}
	]}`
	exp := newExporter(&fakePages{
		byID: map[string]*model.Page{"pg-1": makePage("pg-1", "Guide", pm, nil, 1)},
	}, &fakeSpaces{})
	html, err := exp.ToHTML(context.Background(), "pg-1", ExportOptions{IncludeTOC: true})
	if err != nil {
		t.Fatalf("ToHTML: %v", err)
	}
	if !strings.Contains(html, "Table of Contents") {
		t.Fatalf("TOC label missing: %s", html)
	}
	for _, h := range []string{"Intro", "Setup", "Usage"} {
		if !strings.Contains(html, h) {
			t.Fatalf("heading %q not in TOC", h)
		}
	}
}

func TestToHTML_EscapesAngleBracketsInContent(t *testing.T) {
	pm := `{"type":"doc","content":[
		{"type":"paragraph","content":[{"type":"text","text":"<script>alert(1)</script>"}]}
	]}`
	exp := newExporter(&fakePages{
		byID: map[string]*model.Page{"pg-1": makePage("pg-1", "Title", pm, nil, 1)},
	}, &fakeSpaces{})
	html, _ := exp.ToHTML(context.Background(), "pg-1", ExportOptions{})
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatalf("raw script tag leaked into HTML: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("script tag not escaped: %s", html)
	}
}

// ─── ToPDF ───

func TestToPDF_WritesPDFBytes(t *testing.T) {
	exp := newExporter(&fakePages{
		byID: map[string]*model.Page{"pg-1": makePage("pg-1", "Title", samplePM, nil, 1)},
	}, &fakeSpaces{})
	var buf bytes.Buffer
	if err := exp.ToPDF(context.Background(), "pg-1", ExportOptions{}, &buf); err != nil {
		t.Fatalf("ToPDF: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("PDF buffer empty")
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("%PDF-")) {
		t.Fatalf("not a PDF: %x", buf.Bytes()[:8])
	}
}

// ─── ToDocx ───

func TestToDocx_WritesValidZipWithDocumentXML(t *testing.T) {
	exp := newExporter(&fakePages{
		byID: map[string]*model.Page{"pg-1": makePage("pg-1", "Doc Title", samplePM, nil, 1)},
	}, &fakeSpaces{})
	var buf bytes.Buffer
	if err := exp.ToDocx(context.Background(), "pg-1", ExportOptions{}, &buf); err != nil {
		t.Fatalf("ToDocx: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("docx empty")
	}
	// docx is a zip — verify the central directory and find
	// word/document.xml inside.
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	var hasDoc bool
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			hasDoc = true
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("open doc: %v", err)
			}
			body, _ := io.ReadAll(rc)
			rc.Close()
			if !bytes.Contains(body, []byte("Hello")) {
				t.Fatalf("docx body missing content: %s", string(body))
			}
		}
	}
	if !hasDoc {
		t.Fatalf("word/document.xml missing from docx")
	}
}

// ─── Filename derivation ───

func TestSlugFilename_StripsUnsafeAndAddsExt(t *testing.T) {
	cases := []struct {
		title string
		ext   string
		want  string
	}{
		{"Hello World", "pdf", "hello-world.pdf"},
		{"Test: Page!", "docx", "test-page.docx"},
		{"  leading/trailing  ", "md", "leading-trailing.md"},
		{"", "html", "untitled.html"},
	}
	for _, c := range cases {
		if got := slugFilename(c.title, c.ext); got != c.want {
			t.Errorf("slugFilename(%q, %q) = %q, want %q", c.title, c.ext, got, c.want)
		}
	}
}
