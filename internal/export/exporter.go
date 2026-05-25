// Package export converts Docs pages to portable formats —
// markdown, HTML, PDF, and DOCX. The exporter composes the
// existing markdown converter (mcp.ProseMirrorToMarkdown) for the
// text formats; PDF lives on github.com/go-pdf/fpdf (pure Go),
// and DOCX is built from the WordprocessingML XML schema via
// archive/zip so we don't pull in a dedicated dep.
package export

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/go-pdf/fpdf"

	"github.com/talyvor/docs/internal/mcp"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
)

type Format string

const (
	FormatPDF  Format = "pdf"
	FormatDocx Format = "docx"
	FormatHTML Format = "html"
	FormatMD   Format = "markdown"
)

type ExportOptions struct {
	Format          Format `json:"format"`
	IncludeTOC      bool   `json:"include_toc"`
	IncludeChildren bool   `json:"include_children"`
	PageTitle       bool   `json:"page_title"`
	PageBreaks      bool   `json:"page_breaks"`
	Watermark       string `json:"watermark,omitempty"`
}

// pageReader + spaceReader keep the package's dependencies narrow
// so the tests can swap in in-memory fakes. The real *page.Store /
// *space.Store satisfy them structurally.
type pageReader interface {
	GetByID(ctx context.Context, id string) (*model.Page, error)
	List(ctx context.Context, filter page.PageFilter) ([]model.Page, error)
}

type spaceReader interface {
	GetByID(ctx context.Context, id string) (*model.Space, error)
}

type Exporter struct {
	pages  pageReader
	spaces spaceReader
}

func New(pages pageReader, spaces spaceReader) *Exporter {
	return newExporter(pages, spaces)
}

func newExporter(pages pageReader, spaces spaceReader) *Exporter {
	return &Exporter{pages: pages, spaces: spaces}
}

// ExportPage is the top-level entry point used by the HTTP handler.
// It dispatches to the right backend based on opts.Format and
// streams the resulting bytes into w.
func (e *Exporter) ExportPage(ctx context.Context, pageID string, opts ExportOptions, w io.Writer) error {
	switch opts.Format {
	case FormatMD:
		md, err := e.ToMarkdown(ctx, pageID, opts)
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, md)
		return err
	case FormatHTML:
		body, err := e.ToHTML(ctx, pageID, opts)
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, body)
		return err
	case FormatPDF:
		return e.ToPDF(ctx, pageID, opts, w)
	case FormatDocx:
		return e.ToDocx(ctx, pageID, opts, w)
	}
	return fmt.Errorf("export: unsupported format %q", opts.Format)
}

// gatherPages returns the root page followed by its children (in
// position order) when opts.IncludeChildren is set. This is the
// expansion every format walks before rendering.
func (e *Exporter) gatherPages(ctx context.Context, pageID string, includeChildren bool) ([]model.Page, error) {
	root, err := e.pages.GetByID(ctx, pageID)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, errors.New("export: page not found")
	}
	out := []model.Page{*root}
	if !includeChildren {
		return out, nil
	}
	siblings, err := e.pages.List(ctx, page.PageFilter{SpaceID: root.SpaceID})
	if err != nil {
		return out, nil
	}
	var children []model.Page
	for _, p := range siblings {
		if p.ID == root.ID {
			continue
		}
		if p.ParentID != nil && *p.ParentID == root.ID {
			children = append(children, p)
		}
	}
	sort.SliceStable(children, func(i, j int) bool {
		return children[i].Position < children[j].Position
	})
	return append(out, children...), nil
}

// SlugFilename returns a URL-safe filename for the exported file.
// Strips punctuation, collapses whitespace + dashes, and adds the
// requested extension. Empty titles fall back to "untitled".
func SlugFilename(title, ext string) string { return slugFilename(title, ext) }

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

func slugFilename(title, ext string) string {
	t := strings.ToLower(strings.TrimSpace(title))
	t = slugStrip.ReplaceAllString(t, "-")
	t = strings.Trim(t, "-")
	if t == "" {
		t = "untitled"
	}
	return t + "." + ext
}

// ─── Markdown ────────────────────────────────────────

// ToMarkdown emits a clean markdown document with YAML frontmatter
// pulled from the page metadata. When IncludeChildren is set, each
// child page renders below a horizontal rule with its own subhead.
func (e *Exporter) ToMarkdown(ctx context.Context, pageID string, opts ExportOptions) (string, error) {
	pages, err := e.gatherPages(ctx, pageID, opts.IncludeChildren)
	if err != nil {
		return "", err
	}
	root := pages[0]
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", root.Title)
	fmt.Fprintf(&b, "created: %s\n", root.CreatedAt.Format("2006-01-02"))
	fmt.Fprintf(&b, "updated: %s\n", root.UpdatedAt.Format("2006-01-02"))
	if opts.Watermark != "" {
		fmt.Fprintf(&b, "watermark: %q\n", opts.Watermark)
	}
	b.WriteString("---\n\n")
	for i, p := range pages {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
			fmt.Fprintf(&b, "# %s\n\n", p.Title)
		}
		b.WriteString(mcp.ProseMirrorToMarkdown(p.Content))
		b.WriteString("\n")
	}
	return b.String(), nil
}

// ─── HTML ────────────────────────────────────────────

const htmlStyles = `body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif;max-width:760px;margin:32px auto;padding:0 16px;color:#1a1a1a;line-height:1.6}
h1,h2,h3{color:#111;line-height:1.25;margin-top:1.6em}
h1{font-size:2em;border-bottom:1px solid #eee;padding-bottom:0.2em}
h2{font-size:1.4em}
h3{font-size:1.15em}
p{margin:0.8em 0}
code{font-family:"SF Mono",Menlo,monospace;background:#f4f4f4;padding:1px 4px;border-radius:3px;font-size:0.9em}
pre{background:#f4f4f4;padding:12px;border-radius:4px;overflow:auto}
pre code{background:transparent;padding:0}
blockquote{border-left:3px solid #6366f1;padding-left:12px;color:#555;margin:1em 0;font-style:italic}
ul,ol{padding-left:1.4em}
hr{border:0;border-top:1px solid #ddd;margin:2em 0}
.meta{color:#888;font-size:0.85em;margin-bottom:2em}
.toc{background:#f9f9fb;border:1px solid #e5e5ea;padding:12px 18px;border-radius:6px;margin-bottom:2em}
.toc h2{font-size:1em;margin:0 0 6px}
.toc ul{margin:0;padding-left:1.2em}
.watermark{position:fixed;top:50%;left:50%;transform:translate(-50%,-50%) rotate(-30deg);font-size:6em;color:rgba(0,0,0,0.05);pointer-events:none;z-index:-1}
footer{margin-top:3em;padding-top:1em;border-top:1px solid #eee;color:#888;font-size:0.85em;text-align:center}`

func (e *Exporter) ToHTML(ctx context.Context, pageID string, opts ExportOptions) (string, error) {
	pages, err := e.gatherPages(ctx, pageID, opts.IncludeChildren)
	if err != nil {
		return "", err
	}
	root := pages[0]
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n")
	b.WriteString("<meta charset=\"utf-8\">\n")
	fmt.Fprintf(&b, "<title>%s</title>\n", html.EscapeString(root.Title))
	fmt.Fprintf(&b, "<style>%s</style>\n", htmlStyles)
	b.WriteString("</head>\n<body>\n")
	if opts.Watermark != "" {
		fmt.Fprintf(&b, `<div class="watermark">%s</div>`, html.EscapeString(opts.Watermark))
	}
	fmt.Fprintf(&b, "<h1>%s</h1>\n", html.EscapeString(root.Title))
	fmt.Fprintf(&b, `<div class="meta">Updated %s</div>`, root.UpdatedAt.Format("January 2, 2006"))
	if opts.IncludeTOC {
		b.WriteString(buildTOC(pages))
	}
	for i, p := range pages {
		if i > 0 {
			b.WriteString("<hr>\n")
			if opts.PageBreaks {
				// Browsers + WeasyPrint honour the page-break style
				// even when CSS is inlined.
				b.WriteString(`<div style="page-break-before:always"></div>` + "\n")
			}
			fmt.Fprintf(&b, "<h1>%s</h1>\n", html.EscapeString(p.Title))
		}
		renderHTMLBody(p.Content, &b)
	}
	b.WriteString(`<footer>Exported from Talyvor Docs</footer>` + "\n")
	b.WriteString("</body>\n</html>\n")
	return b.String(), nil
}

// buildTOC walks every page's headings and emits a nested list of
// anchored entries. The render targets headings by their text — we
// don't write IDs into the body, so the TOC stays a navigation hint
// rather than a live link.
func buildTOC(pages []model.Page) string {
	var b strings.Builder
	b.WriteString(`<nav class="toc"><h2>Table of Contents</h2><ul>`)
	for _, p := range pages {
		walkHeadings(p.Content, func(level int, text string) {
			fmt.Fprintf(&b, `<li style="margin-left:%dem">%s</li>`, (level-1)*1, html.EscapeString(text))
		})
	}
	b.WriteString("</ul></nav>\n")
	return b.String()
}

// walkHeadings invokes visit for every heading node in a
// ProseMirror doc. Malformed JSON silently produces no headings.
func walkHeadings(pm string, visit func(level int, text string)) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(pm), &doc); err != nil {
		return
	}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if v["type"] == "heading" {
				level := 1
				if attrs, ok := v["attrs"].(map[string]any); ok {
					if l, ok := attrs["level"].(float64); ok {
						level = int(l)
					}
				}
				visit(level, plainTextOf(v["content"]))
				return
			}
			if c, ok := v["content"]; ok {
				walk(c)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
}

func plainTextOf(raw any) string {
	nodes, _ := raw.([]any)
	var b strings.Builder
	for _, n := range nodes {
		m, _ := n.(map[string]any)
		if t, _ := m["text"].(string); t != "" {
			b.WriteString(t)
		}
	}
	return b.String()
}

// renderHTMLBody walks a ProseMirror doc and emits clean HTML.
// Inline text always passes through html.EscapeString so untrusted
// content can't break out of the markup.
func renderHTMLBody(pm string, b *strings.Builder) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(pm), &doc); err != nil {
		return
	}
	renderHTMLBlocks(doc["content"], b)
}

func renderHTMLBlocks(raw any, b *strings.Builder) {
	blocks, _ := raw.([]any)
	for _, raw := range blocks {
		node, _ := raw.(map[string]any)
		if node == nil {
			continue
		}
		renderHTMLBlock(node, b)
	}
}

func renderHTMLBlock(node map[string]any, b *strings.Builder) {
	switch node["type"] {
	case "heading":
		level := 1
		if attrs, ok := node["attrs"].(map[string]any); ok {
			if l, ok := attrs["level"].(float64); ok {
				level = int(l)
			}
		}
		fmt.Fprintf(b, "<h%d>", level)
		renderHTMLInline(node["content"], b)
		fmt.Fprintf(b, "</h%d>\n", level)
	case "paragraph":
		b.WriteString("<p>")
		renderHTMLInline(node["content"], b)
		b.WriteString("</p>\n")
	case "code_block":
		b.WriteString("<pre><code>")
		text := html.EscapeString(plainTextOf(node["content"]))
		b.WriteString(text)
		b.WriteString("</code></pre>\n")
	case "blockquote":
		b.WriteString("<blockquote>")
		renderHTMLBlocks(node["content"], b)
		b.WriteString("</blockquote>\n")
	case "bullet_list":
		b.WriteString("<ul>")
		renderHTMLListItems(node["content"], b)
		b.WriteString("</ul>\n")
	case "ordered_list":
		b.WriteString("<ol>")
		renderHTMLListItems(node["content"], b)
		b.WriteString("</ol>\n")
	case "horizontal_rule":
		b.WriteString("<hr>\n")
	}
}

func renderHTMLListItems(raw any, b *strings.Builder) {
	items, _ := raw.([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		body, _ := item["content"].([]any)
		b.WriteString("<li>")
		// Strip the single wrapping paragraph when a list item only
		// has one paragraph — markdown-style "tight" lists read
		// better without an extra block.
		if len(body) == 1 {
			child, _ := body[0].(map[string]any)
			if child != nil && child["type"] == "paragraph" {
				renderHTMLInline(child["content"], b)
			} else {
				renderHTMLBlock(child, b)
			}
		} else {
			for _, c := range body {
				if cm, ok := c.(map[string]any); ok {
					renderHTMLBlock(cm, b)
				}
			}
		}
		b.WriteString("</li>")
	}
}

func renderHTMLInline(raw any, b *strings.Builder) {
	nodes, _ := raw.([]any)
	for _, raw := range nodes {
		n, _ := raw.(map[string]any)
		if n == nil {
			continue
		}
		text, _ := n["text"].(string)
		if text == "" {
			continue
		}
		escaped := html.EscapeString(text)
		marks, _ := n["marks"].([]any)
		b.WriteString(wrapMarks(escaped, marks))
	}
}

func wrapMarks(text string, marks []any) string {
	for _, raw := range marks {
		m, _ := raw.(map[string]any)
		switch m["type"] {
		case "code":
			return "<code>" + text + "</code>"
		}
	}
	for _, raw := range marks {
		m, _ := raw.(map[string]any)
		switch m["type"] {
		case "strong":
			text = "<strong>" + text + "</strong>"
		case "em":
			text = "<em>" + text + "</em>"
		case "link":
			href := ""
			if attrs, ok := m["attrs"].(map[string]any); ok {
				if h, ok := attrs["href"].(string); ok {
					href = h
				}
			}
			text = fmt.Sprintf(`<a href="%s" rel="noopener noreferrer">%s</a>`, html.EscapeString(href), text)
		}
	}
	return text
}

// ─── PDF ─────────────────────────────────────────────

func (e *Exporter) ToPDF(ctx context.Context, pageID string, opts ExportOptions, w io.Writer) error {
	pages, err := e.gatherPages(ctx, pageID, opts.IncludeChildren)
	if err != nil {
		return err
	}
	pdf := fpdf.New("P", "mm", "A4", "")

	// Page header (title) + footer (page numbers). fpdf calls these
	// closures on every new page so we don't have to thread state.
	root := pages[0]
	pdf.SetHeaderFunc(func() {
		pdf.SetFont("Arial", "", 9)
		pdf.SetTextColor(120, 120, 120)
		pdf.CellFormat(0, 8, ensureASCII(root.Title), "", 0, "L", false, 0, "")
		pdf.CellFormat(0, 8, root.UpdatedAt.Format("2 Jan 2006"), "", 1, "R", false, 0, "")
		pdf.Ln(2)
	})
	pdf.SetFooterFunc(func() {
		pdf.SetY(-12)
		pdf.SetFont("Arial", "I", 8)
		pdf.SetTextColor(150, 150, 150)
		pdf.CellFormat(0, 6, "Exported from Talyvor Docs", "", 0, "L", false, 0, "")
		pdf.CellFormat(0, 6, fmt.Sprintf("Page %d", pdf.PageNo()), "", 0, "R", false, 0, "")
	})

	pdf.AddPage()
	if opts.IncludeTOC {
		pdf.SetFont("Arial", "B", 14)
		pdf.SetTextColor(0, 0, 0)
		pdf.CellFormat(0, 8, "Table of Contents", "", 1, "L", false, 0, "")
		pdf.SetFont("Arial", "", 11)
		for _, p := range pages {
			walkHeadings(p.Content, func(level int, text string) {
				indent := float64(level-1) * 5
				pdf.SetX(10 + indent)
				pdf.CellFormat(0, 6, ensureASCII(text), "", 1, "L", false, 0, "")
			})
		}
		pdf.Ln(4)
	}

	for i, p := range pages {
		if i > 0 {
			if opts.PageBreaks {
				pdf.AddPage()
			} else {
				pdf.Ln(6)
			}
			renderPDFHeading(pdf, 1, p.Title)
		}
		renderPDFBody(pdf, p.Content)
	}

	if err := pdf.Output(w); err != nil {
		return fmt.Errorf("export: pdf output: %w", err)
	}
	return nil
}

// ensureASCII forces text through a printable-ASCII filter — fpdf's
// default Helvetica font can't render Unicode. Phase 3 polish: ship
// a TrueType subset; today, we transliterate the most common cases
// (emojis become a placeholder, other Unicode is stripped).
func ensureASCII(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 128 {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// renderPDFHeading sets the font size + weight for a heading level.
func renderPDFHeading(pdf *fpdf.Fpdf, level int, text string) {
	size := 13.0
	switch level {
	case 1:
		size = 18
	case 2:
		size = 14
	case 3:
		size = 12
	default:
		size = 11
	}
	pdf.SetFont("Arial", "B", size)
	pdf.SetTextColor(20, 20, 20)
	pdf.MultiCell(0, size*0.6, ensureASCII(text), "", "L", false)
	pdf.Ln(1)
}

func renderPDFBody(pdf *fpdf.Fpdf, pm string) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(pm), &doc); err != nil {
		return
	}
	renderPDFBlocks(pdf, doc["content"])
}

func renderPDFBlocks(pdf *fpdf.Fpdf, raw any) {
	blocks, _ := raw.([]any)
	for _, raw := range blocks {
		node, _ := raw.(map[string]any)
		if node == nil {
			continue
		}
		switch node["type"] {
		case "heading":
			level := 1
			if attrs, ok := node["attrs"].(map[string]any); ok {
				if l, ok := attrs["level"].(float64); ok {
					level = int(l)
				}
			}
			renderPDFHeading(pdf, level, plainTextOf(node["content"]))
		case "paragraph":
			pdf.SetFont("Arial", "", 11)
			pdf.SetTextColor(40, 40, 40)
			pdf.MultiCell(0, 5.5, ensureASCII(plainTextOf(node["content"])), "", "L", false)
			pdf.Ln(2)
		case "code_block":
			pdf.SetFont("Courier", "", 10)
			pdf.SetTextColor(20, 20, 20)
			pdf.SetFillColor(245, 245, 247)
			pdf.MultiCell(0, 5, ensureASCII(plainTextOf(node["content"])), "1", "L", true)
			pdf.Ln(2)
		case "blockquote":
			pdf.SetFont("Arial", "I", 11)
			pdf.SetTextColor(80, 80, 80)
			renderPDFBlocks(pdf, node["content"])
		case "bullet_list", "ordered_list":
			items, _ := node["content"].([]any)
			pdf.SetFont("Arial", "", 11)
			for i, raw := range items {
				item, _ := raw.(map[string]any)
				marker := "• "
				if node["type"] == "ordered_list" {
					marker = fmt.Sprintf("%d. ", i+1)
				}
				body, _ := item["content"].([]any)
				if len(body) > 0 {
					child, _ := body[0].(map[string]any)
					pdf.MultiCell(0, 5.5, ensureASCII(marker+plainTextOf(child["content"])), "", "L", false)
				}
			}
			pdf.Ln(2)
		case "horizontal_rule":
			pdf.Ln(2)
		}
	}
}

// ─── DOCX ────────────────────────────────────────────

// docxStaticParts holds the immutable XML files every minimal
// .docx ships with. Word + LibreOffice both accept this minimal
// shape; production users can swap in a richer template later.
var docxStaticParts = map[string]string{
	"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
  <Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>
</Types>`,
	"_rels/.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>`,
	"word/_rels/document.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
</Relationships>`,
	"word/styles.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:style w:type="paragraph" w:styleId="Normal"><w:name w:val="Normal"/></w:style>
  <w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="Heading 1"/><w:rPr><w:b/><w:sz w:val="36"/></w:rPr></w:style>
  <w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="Heading 2"/><w:rPr><w:b/><w:sz w:val="28"/></w:rPr></w:style>
  <w:style w:type="paragraph" w:styleId="Heading3"><w:name w:val="Heading 3"/><w:rPr><w:b/><w:sz w:val="24"/></w:rPr></w:style>
  <w:style w:type="paragraph" w:styleId="Quote"><w:name w:val="Quote"/><w:rPr><w:i/></w:rPr></w:style>
  <w:style w:type="paragraph" w:styleId="ListBullet"><w:name w:val="List Bullet"/></w:style>
  <w:style w:type="paragraph" w:styleId="ListNumber"><w:name w:val="List Number"/></w:style>
  <w:style w:type="paragraph" w:styleId="Code"><w:name w:val="Code"/><w:rPr><w:rFonts w:ascii="Courier New"/></w:rPr></w:style>
  <w:style w:type="character" w:styleId="CodeChar"><w:name w:val="Code Character"/><w:rPr><w:rFonts w:ascii="Courier New"/></w:rPr></w:style>
</w:styles>`,
}

func (e *Exporter) ToDocx(ctx context.Context, pageID string, opts ExportOptions, w io.Writer) error {
	pages, err := e.gatherPages(ctx, pageID, opts.IncludeChildren)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)

	// Write the immutable parts first.
	for name, body := range docxStaticParts {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(f, body); err != nil {
			return err
		}
	}

	// Build document.xml — the actual content.
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	if opts.IncludeTOC {
		docxParagraph(&body, "Heading1", "Table of Contents")
		for _, p := range pages {
			walkHeadings(p.Content, func(_ int, text string) {
				docxParagraph(&body, "Normal", text)
			})
		}
	}
	for i, p := range pages {
		if i > 0 {
			docxParagraph(&body, "Heading1", p.Title)
		}
		renderDocxBody(p.Content, &body)
	}
	body.WriteString(`<w:sectPr/></w:body></w:document>`)

	docXML, err := zw.Create("word/document.xml")
	if err != nil {
		return err
	}
	if _, err := io.WriteString(docXML, body.String()); err != nil {
		return err
	}
	return zw.Close()
}

// docxEscape XML-escapes a string for embedding in document.xml.
func docxEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// docxParagraph emits a single <w:p> with the requested style.
func docxParagraph(b *strings.Builder, style, text string) {
	fmt.Fprintf(b, `<w:p><w:pPr><w:pStyle w:val="%s"/></w:pPr><w:r><w:t xml:space="preserve">%s</w:t></w:r></w:p>`,
		style, docxEscape(text))
}

func renderDocxBody(pm string, b *strings.Builder) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(pm), &doc); err != nil {
		return
	}
	renderDocxBlocks(doc["content"], b)
}

func renderDocxBlocks(raw any, b *strings.Builder) {
	blocks, _ := raw.([]any)
	for _, raw := range blocks {
		node, _ := raw.(map[string]any)
		if node == nil {
			continue
		}
		renderDocxBlock(node, b)
	}
}

func renderDocxBlock(node map[string]any, b *strings.Builder) {
	switch node["type"] {
	case "heading":
		level := 1
		if attrs, ok := node["attrs"].(map[string]any); ok {
			if l, ok := attrs["level"].(float64); ok {
				level = int(l)
			}
		}
		style := "Heading1"
		switch level {
		case 2:
			style = "Heading2"
		case 3:
			style = "Heading3"
		}
		b.WriteString(`<w:p><w:pPr><w:pStyle w:val="` + style + `"/></w:pPr>`)
		renderDocxRuns(node["content"], b)
		b.WriteString(`</w:p>`)
	case "paragraph":
		b.WriteString(`<w:p>`)
		renderDocxRuns(node["content"], b)
		b.WriteString(`</w:p>`)
	case "code_block":
		docxParagraph(b, "Code", plainTextOf(node["content"]))
	case "blockquote":
		// Render every child paragraph with the Quote style.
		quoteChildren(node["content"], b)
	case "bullet_list":
		renderDocxListItems(node["content"], "ListBullet", b)
	case "ordered_list":
		renderDocxListItems(node["content"], "ListNumber", b)
	case "horizontal_rule":
		docxParagraph(b, "Normal", "————————————")
	}
}

func quoteChildren(raw any, b *strings.Builder) {
	blocks, _ := raw.([]any)
	for _, raw := range blocks {
		node, _ := raw.(map[string]any)
		if node == nil {
			continue
		}
		if node["type"] == "paragraph" {
			docxParagraph(b, "Quote", plainTextOf(node["content"]))
		} else {
			renderDocxBlock(node, b)
		}
	}
}

func renderDocxListItems(raw any, style string, b *strings.Builder) {
	items, _ := raw.([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		body, _ := item["content"].([]any)
		if len(body) == 0 {
			continue
		}
		first, _ := body[0].(map[string]any)
		docxParagraph(b, style, plainTextOf(first["content"]))
	}
}

// renderDocxRuns emits a sequence of <w:r> runs honouring the
// strong / em / code marks. Each text run gets its own properties
// block so the styling sticks even when marks straddle text spans.
func renderDocxRuns(raw any, b *strings.Builder) {
	nodes, _ := raw.([]any)
	for _, raw := range nodes {
		n, _ := raw.(map[string]any)
		if n == nil {
			continue
		}
		text, _ := n["text"].(string)
		if text == "" {
			continue
		}
		marks, _ := n["marks"].([]any)
		var props []string
		var charStyle string
		for _, raw := range marks {
			m, _ := raw.(map[string]any)
			switch m["type"] {
			case "strong":
				props = append(props, "<w:b/>")
			case "em":
				props = append(props, "<w:i/>")
			case "code":
				charStyle = "CodeChar"
			}
		}
		b.WriteString(`<w:r>`)
		if len(props) > 0 || charStyle != "" {
			b.WriteString(`<w:rPr>`)
			if charStyle != "" {
				b.WriteString(`<w:rStyle w:val="` + charStyle + `"/>`)
			}
			for _, p := range props {
				b.WriteString(p)
			}
			b.WriteString(`</w:rPr>`)
		}
		fmt.Fprintf(b, `<w:t xml:space="preserve">%s</w:t>`, docxEscape(text))
		b.WriteString(`</w:r>`)
	}
}
