// Package importer ingests external knowledge bases — Notion
// markdown exports and Confluence HTML exports — and writes the
// content into Talyvor Docs as ProseMirror pages.
//
// Both importers stream from a zip reader, classify each file by
// extension, and create pages (with folder-derived parent pages
// where applicable) via the same page.Store interface the live
// app uses. Files we can't handle are counted in Skipped rather
// than erroring — a single bad asset shouldn't abort a 1000-page
// import.
package importer

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"golang.org/x/net/html"

	"github.com/talyvor/docs/internal/mcp"
	"github.com/talyvor/docs/internal/model"
)

type ImportResult struct {
	SpaceID  string   `json:"space_id"`
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors,omitempty"`
}

// pageStore is the narrow Create surface the importer relies on.
// Phase 10 keeps it interface-shaped so tests can swap in an
// in-memory recorder.
type pageStore interface {
	Create(ctx context.Context, p model.Page) (*model.Page, error)
}

// spaceStore lets the importer create a fallback space if the
// caller didn't pin one. Live callers always pass a spaceID; the
// CLI bulk-import case can leave it empty and we make one.
type spaceStore interface {
	GetByID(ctx context.Context, id string) (*model.Space, error)
	Create(ctx context.Context, sp model.Space) (*model.Space, error)
}

type ConfluenceImporter struct {
	pages  pageStore
	spaces spaceStore
}

func New(pages pageStore, spaces spaceStore) *ConfluenceImporter {
	return newImporter(pages, spaces)
}

func newImporter(pages pageStore, spaces spaceStore) *ConfluenceImporter {
	return &ConfluenceImporter{pages: pages, spaces: spaces}
}

// ImportFromNotion processes a Notion markdown export (.md inside a
// zip). Folders become parent pages; .md files become child pages
// of their folder. The first H1 in each file is used as the title;
// if missing, the filename (sans extension) is the fallback.
func (i *ConfluenceImporter) ImportFromNotion(ctx context.Context, workspaceID, spaceID string, r io.Reader) (*ImportResult, error) {
	zr, err := readZip(r)
	if err != nil {
		return nil, err
	}
	result := &ImportResult{SpaceID: spaceID}
	folders := newFolderCache(i.pages, workspaceID, spaceID)

	// Sort entries so folder pages always exist before their children.
	files := zr.File
	sort.SliceStable(files, func(a, b int) bool { return files[a].Name < files[b].Name })

	for _, f := range files {
		if f.FileInfo().IsDir() {
			continue
		}
		ext := strings.ToLower(path.Ext(f.Name))
		if ext != ".md" && ext != ".markdown" {
			result.Skipped++
			continue
		}
		body, err := readAll(f)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", f.Name, err))
			result.Skipped++
			continue
		}
		title := titleFromMarkdown(body, f.Name)
		parentID, err := folders.ensure(ctx, path.Dir(f.Name))
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", f.Name, err))
			result.Skipped++
			continue
		}
		pm := mcp.MarkdownToProseMirror(body)
		page := model.Page{
			SpaceID:     spaceID,
			WorkspaceID: workspaceID,
			Title:       title,
			Content:     pm,
			ContentText: stripMarkdown(body),
			CreatedBy:   "importer",
		}
		if parentID != "" {
			page.ParentID = &parentID
		}
		if _, err := i.pages.Create(ctx, page); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", f.Name, err))
			result.Skipped++
			continue
		}
		result.Imported++
	}
	return result, nil
}

// ImportExport processes a Confluence HTML export. Each .html /
// .htm becomes one page. Confluence's nesting metadata isn't
// reliably present in space exports — we keep this iteration flat
// and let users reorganise after import.
func (i *ConfluenceImporter) ImportExport(ctx context.Context, workspaceID, spaceID string, r io.Reader) (*ImportResult, error) {
	zr, err := readZip(r)
	if err != nil {
		return nil, err
	}
	result := &ImportResult{SpaceID: spaceID}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		ext := strings.ToLower(path.Ext(f.Name))
		if ext != ".html" && ext != ".htm" {
			result.Skipped++
			continue
		}
		body, err := readAll(f)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", f.Name, err))
			result.Skipped++
			continue
		}
		title, pm, plain, err := htmlToProseMirror(body, f.Name)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", f.Name, err))
			result.Skipped++
			continue
		}
		page := model.Page{
			SpaceID:     spaceID,
			WorkspaceID: workspaceID,
			Title:       title,
			Content:     pm,
			ContentText: plain,
			CreatedBy:   "importer",
		}
		if _, err := i.pages.Create(ctx, page); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", f.Name, err))
			result.Skipped++
			continue
		}
		result.Imported++
	}
	return result, nil
}

// ─── helpers ────────────────────────────────────────────

// folderCache turns Notion's folder-shaped export into a hierarchy
// of parent pages. The first time we see "Onboarding/Day 1.md" we
// create an "Onboarding" page; subsequent files under the same
// folder reuse the cached parent ID.
type folderCache struct {
	pages       pageStore
	workspaceID string
	spaceID     string
	known       map[string]string // path → page ID
}

func newFolderCache(pages pageStore, workspaceID, spaceID string) *folderCache {
	return &folderCache{
		pages:       pages,
		workspaceID: workspaceID,
		spaceID:     spaceID,
		known:       map[string]string{"": "", ".": ""},
	}
}

func (c *folderCache) ensure(ctx context.Context, dir string) (string, error) {
	dir = strings.TrimSpace(strings.Trim(dir, "/"))
	if dir == "" || dir == "." {
		return "", nil
	}
	if id, ok := c.known[dir]; ok {
		return id, nil
	}
	// Walk up so each ancestor exists before we create the leaf.
	parent, err := c.ensure(ctx, path.Dir(dir))
	if err != nil {
		return "", err
	}
	name := path.Base(dir)
	page := model.Page{
		SpaceID:     c.spaceID,
		WorkspaceID: c.workspaceID,
		Title:       name,
		Content:     `{"type":"doc","content":[{"type":"paragraph"}]}`,
		ContentText: "",
		CreatedBy:   "importer",
	}
	if parent != "" {
		page.ParentID = &parent
	}
	created, err := c.pages.Create(ctx, page)
	if err != nil {
		return "", err
	}
	c.known[dir] = created.ID
	return created.ID, nil
}

func readZip(r io.Reader) (*zip.Reader, error) {
	// zip.NewReader needs a ReaderAt + size, so buffer the input.
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("importer: read zip: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, fmt.Errorf("importer: parse zip: %w", err)
	}
	return zr, nil
}

func readAll(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// titleFromMarkdown returns the first H1 from the markdown body,
// stripping the leading "# " token. Falls back to the filename
// when the file has no leading heading.
func titleFromMarkdown(body, filename string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	base := path.Base(filename)
	ext := path.Ext(base)
	return strings.TrimSuffix(base, ext)
}

// stripMarkdown collapses markdown source into a plain-text
// projection for the search index. We don't need a real parser —
// the dehydrated text only needs to be reasonable.
func stripMarkdown(md string) string {
	cleaners := []string{"# ", "## ", "### ", "#### ", "##### ", "###### ", "**", "*", "`", "> ", "- ", "1. ", "2. ", "3. "}
	out := md
	for _, c := range cleaners {
		out = strings.ReplaceAll(out, c, "")
	}
	out = strings.ReplaceAll(out, "\n\n", "\n")
	return strings.TrimSpace(out)
}

// ─── HTML → ProseMirror ─────────────────────────────────

// htmlToProseMirror walks a Confluence-style HTML page and returns
// the title (from <title> or first <h1>), the encoded ProseMirror
// JSON, and a plain-text projection. Unknown tags drop through to
// their child content so the document doesn't lose data.
func htmlToProseMirror(body, filename string) (string, string, string, error) {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return "", "", "", fmt.Errorf("html parse: %w", err)
	}
	w := htmlWalker{}
	w.walk(doc)
	title := w.title
	if title == "" {
		title = w.firstH1
	}
	if title == "" {
		base := path.Base(filename)
		title = strings.TrimSuffix(base, path.Ext(base))
	}
	if len(w.blocks) == 0 {
		w.blocks = []map[string]any{{"type": "paragraph"}}
	}
	docJSON, err := json.Marshal(map[string]any{
		"type":    "doc",
		"content": w.blocks,
	})
	if err != nil {
		return "", "", "", err
	}
	return title, string(docJSON), strings.TrimSpace(w.plain.String()), nil
}

// htmlWalker collects ProseMirror blocks as it descends an HTML
// tree. We track the first <h1> separately so a missing <title>
// still gives the page a reasonable name.
type htmlWalker struct {
	title   string
	firstH1 string
	blocks  []map[string]any
	plain   strings.Builder
}

func (w *htmlWalker) walk(n *html.Node) {
	if n == nil {
		return
	}
	if n.Type == html.ElementNode {
		switch n.Data {
		case "title":
			w.title = strings.TrimSpace(textContent(n))
			return
		case "h1", "h2", "h3", "h4", "h5", "h6":
			level := int(n.Data[1] - '0')
			text := strings.TrimSpace(textContent(n))
			if level == 1 && w.firstH1 == "" {
				w.firstH1 = text
			}
			if text != "" {
				w.blocks = append(w.blocks, map[string]any{
					"type":    "heading",
					"attrs":   map[string]any{"level": level},
					"content": inlineText(n),
				})
				w.plain.WriteString(text)
				w.plain.WriteString("\n")
			}
			return
		case "p":
			inline := inlineText(n)
			if len(inline) > 0 {
				w.blocks = append(w.blocks, map[string]any{
					"type":    "paragraph",
					"content": inline,
				})
				w.plain.WriteString(strings.TrimSpace(textContent(n)))
				w.plain.WriteString("\n")
			}
			return
		case "ul", "ol":
			items := htmlListItems(n)
			if len(items) > 0 {
				listType := "bullet_list"
				if n.Data == "ol" {
					listType = "ordered_list"
				}
				w.blocks = append(w.blocks, map[string]any{
					"type":    listType,
					"content": items,
				})
			}
			return
		case "blockquote":
			inner := htmlWalker{}
			inner.walk(n)
			if len(inner.blocks) > 0 {
				w.blocks = append(w.blocks, map[string]any{
					"type":    "blockquote",
					"content": inner.blocks,
				})
				w.plain.WriteString(inner.plain.String())
			}
			return
		case "pre":
			// Treat <pre> as a code block. Confluence wraps code in
			// nested <pre><code> tags; we flatten both shapes.
			text := strings.TrimSpace(textContent(n))
			if text != "" {
				w.blocks = append(w.blocks, map[string]any{
					"type": "code_block",
					"content": []map[string]any{
						{"type": "text", "text": text},
					},
				})
				w.plain.WriteString(text)
				w.plain.WriteString("\n")
			}
			return
		case "hr":
			w.blocks = append(w.blocks, map[string]any{"type": "horizontal_rule"})
			return
		case "script", "style", "noscript", "head":
			// Skip non-content branches entirely.
			return
		}
	}
	// Recurse into children for the wrapper elements we don't
	// explicitly handle (html, body, div, span, ...).
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.walk(c)
	}
}

// inlineText flattens an inline subtree of an HTML node into the
// ProseMirror text-with-marks shape. Bold (<strong>/<b>),
// italic (<em>/<i>), and inline code (<code>) are honoured.
func inlineText(n *html.Node) []map[string]any {
	var out []map[string]any
	collectInline(n, nil, &out)
	if len(out) == 0 {
		// Even an "empty" inline run needs a non-empty text node so
		// the schema doesn't reject the doc. Caller filters empties.
	}
	return out
}

func collectInline(n *html.Node, marks []map[string]any, out *[]map[string]any) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type == html.TextNode:
			text := normaliseSpace(c.Data)
			if text == "" {
				continue
			}
			node := map[string]any{"type": "text", "text": text}
			if len(marks) > 0 {
				node["marks"] = marks
			}
			*out = append(*out, node)
		case c.Type == html.ElementNode:
			switch c.Data {
			case "strong", "b":
				collectInline(c, appendMark(marks, "strong"), out)
			case "em", "i":
				collectInline(c, appendMark(marks, "em"), out)
			case "code":
				collectInline(c, appendMark(marks, "code"), out)
			case "br":
				// Convert <br> into a literal newline inside the text
				// node so paragraphs preserve hard breaks.
				*out = append(*out, map[string]any{"type": "text", "text": "\n"})
			case "a":
				href := getAttr(c, "href")
				if href != "" {
					collectInline(c, appendMarkAttrs(marks, "link", map[string]any{"href": href}), out)
				} else {
					collectInline(c, marks, out)
				}
			default:
				collectInline(c, marks, out)
			}
		}
	}
}

func appendMark(existing []map[string]any, name string) []map[string]any {
	for _, m := range existing {
		if m["type"] == name {
			return existing
		}
	}
	next := make([]map[string]any, 0, len(existing)+1)
	next = append(next, existing...)
	next = append(next, map[string]any{"type": name})
	return next
}

func appendMarkAttrs(existing []map[string]any, name string, attrs map[string]any) []map[string]any {
	next := make([]map[string]any, 0, len(existing)+1)
	next = append(next, existing...)
	next = append(next, map[string]any{"type": name, "attrs": attrs})
	return next
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func htmlListItems(list *html.Node) []map[string]any {
	var items []map[string]any
	for c := list.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || c.Data != "li" {
			continue
		}
		inline := inlineText(c)
		body := []map[string]any{}
		if len(inline) > 0 {
			body = append(body, map[string]any{
				"type":    "paragraph",
				"content": inline,
			})
		}
		// Nested <ul>/<ol> inside an <li> attach as additional block
		// content on the same list_item.
		for child := c.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.ElementNode && (child.Data == "ul" || child.Data == "ol") {
				kids := htmlListItems(child)
				listType := "bullet_list"
				if child.Data == "ol" {
					listType = "ordered_list"
				}
				body = append(body, map[string]any{
					"type":    listType,
					"content": kids,
				})
			}
		}
		if len(body) == 0 {
			body = []map[string]any{{"type": "paragraph"}}
		}
		items = append(items, map[string]any{
			"type":    "list_item",
			"content": body,
		})
	}
	return items
}

func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// normaliseSpace collapses runs of whitespace down to a single
// space. HTML treats whitespace as collapsible, and Confluence
// exports are full of indentation noise.
func normaliseSpace(s string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			if !inSpace {
				b.WriteRune(' ')
				inSpace = true
			}
		default:
			b.WriteRune(r)
			inSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}
