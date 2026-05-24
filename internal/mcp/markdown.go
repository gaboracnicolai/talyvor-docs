// Package mcp owns the JSON-RPC MCP server agents talk to. This
// file is the Markdown ↔ ProseMirror bridge: tool callers send
// markdown (the cheapest format LLMs can produce reliably) and Docs
// stores ProseMirror JSON. We translate at the boundary so the
// rest of the codebase keeps its single canonical content shape.
package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// MarkdownToProseMirror converts a markdown string into a
// ProseMirror JSON document. The supported subset matches the Docs
// editor schema — anything richer (tables, images, footnotes) is
// downgraded to its plain-text fallback rather than failing the
// conversion, so unknown markdown never blocks a save.
func MarkdownToProseMirror(md string) string {
	if strings.TrimSpace(md) == "" {
		return emptyDoc()
	}
	source := []byte(md)
	root := goldmark.DefaultParser().Parse(text.NewReader(source))
	doc := map[string]any{
		"type":    "doc",
		"content": walkBlocks(root, source),
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return emptyDoc()
	}
	return string(out)
}

func emptyDoc() string {
	return `{"type":"doc","content":[{"type":"paragraph"}]}`
}

// walkBlocks walks the children of an AST container and returns
// ProseMirror block nodes. Each goldmark block type maps to one
// ProseMirror node (headings, paragraphs, code blocks, lists,
// blockquotes); unsupported types fall back to a paragraph with
// the rendered text content.
func walkBlocks(parent ast.Node, source []byte) []map[string]any {
	var out []map[string]any
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		if block := convertBlock(n, source); block != nil {
			out = append(out, block)
		}
	}
	return out
}

func convertBlock(n ast.Node, source []byte) map[string]any {
	switch v := n.(type) {
	case *ast.Heading:
		return map[string]any{
			"type":    "heading",
			"attrs":   map[string]any{"level": v.Level},
			"content": walkInline(v, source),
		}
	case *ast.Paragraph:
		// Skip empty paragraphs (goldmark sometimes emits these for
		// trailing blank lines).
		inline := walkInline(v, source)
		if len(inline) == 0 {
			return nil
		}
		return map[string]any{
			"type":    "paragraph",
			"content": inline,
		}
	case *ast.FencedCodeBlock:
		body := readLines(v, source)
		node := map[string]any{
			"type": "code_block",
			"content": []map[string]any{
				{"type": "text", "text": body},
			},
		}
		if lang := string(v.Language(source)); lang != "" {
			node["attrs"] = map[string]any{"language": lang}
		}
		return node
	case *ast.CodeBlock:
		// Indented code block — no language.
		return map[string]any{
			"type": "code_block",
			"content": []map[string]any{
				{"type": "text", "text": readLines(v, source)},
			},
		}
	case *ast.Blockquote:
		return map[string]any{
			"type":    "blockquote",
			"content": walkBlocks(v, source),
		}
	case *ast.List:
		listType := "bullet_list"
		if v.IsOrdered() {
			listType = "ordered_list"
		}
		return map[string]any{
			"type":    listType,
			"content": walkListItems(v, source),
		}
	case *ast.ThematicBreak:
		return map[string]any{"type": "horizontal_rule"}
	}
	// Fallback: dump the node's text into a paragraph so nothing is
	// silently dropped.
	plain := readText(n, source)
	if plain == "" {
		return nil
	}
	return map[string]any{
		"type": "paragraph",
		"content": []map[string]any{
			{"type": "text", "text": plain},
		},
	}
}

// walkListItems handles the goldmark List → ProseMirror list_item
// translation. A list item's first child is usually a TextBlock
// (NOT a Paragraph) — we wrap its inline content into an explicit
// paragraph so the result satisfies the editor schema. Nested lists
// land alongside the paragraph as additional block content.
func walkListItems(list *ast.List, source []byte) []map[string]any {
	var items []map[string]any
	for n := list.FirstChild(); n != nil; n = n.NextSibling() {
		li, ok := n.(*ast.ListItem)
		if !ok {
			continue
		}
		body := walkListItemBody(li, source)
		items = append(items, map[string]any{
			"type":    "list_item",
			"content": body,
		})
	}
	return items
}

// walkListItemBody assembles the block content for one list_item.
// TextBlocks become paragraphs; nested Lists pass through to their
// list_item form.
func walkListItemBody(li *ast.ListItem, source []byte) []map[string]any {
	var body []map[string]any
	for child := li.FirstChild(); child != nil; child = child.NextSibling() {
		switch v := child.(type) {
		case *ast.TextBlock:
			inline := walkInline(v, source)
			if len(inline) > 0 {
				body = append(body, map[string]any{
					"type":    "paragraph",
					"content": inline,
				})
			}
		case *ast.Paragraph:
			inline := walkInline(v, source)
			if len(inline) > 0 {
				body = append(body, map[string]any{
					"type":    "paragraph",
					"content": inline,
				})
			}
		case *ast.List:
			body = append(body, convertBlock(v, source))
		default:
			if block := convertBlock(v, source); block != nil {
				body = append(body, block)
			}
		}
	}
	if len(body) == 0 {
		body = []map[string]any{{"type": "paragraph"}}
	}
	return body
}

// walkInline converts an AST container's inline children into
// ProseMirror inline nodes (always type=text with optional marks).
// Adjacent text fragments under the same mark set are kept separate
// — the editor merges them at runtime and we don't gain anything
// from coalescing here.
func walkInline(parent ast.Node, source []byte) []map[string]any {
	var out []map[string]any
	collect(parent, source, nil, &out)
	return out
}

// collect recurses through inline nodes, accumulating active marks
// as it descends. Each leaf Text node is emitted with the current
// mark set.
func collect(parent ast.Node, source []byte, marks []map[string]any, out *[]map[string]any) {
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		switch v := n.(type) {
		case *ast.Text:
			text := string(v.Segment.Value(source))
			if text == "" {
				continue
			}
			node := map[string]any{"type": "text", "text": text}
			if len(marks) > 0 {
				node["marks"] = marks
			}
			*out = append(*out, node)
		case *ast.Emphasis:
			mark := "em"
			if v.Level == 2 {
				mark = "strong"
			}
			collect(v, source, addMark(marks, mark), out)
		case *ast.CodeSpan:
			body := readText(v, source)
			node := map[string]any{
				"type":  "text",
				"text":  body,
				"marks": addMark(marks, "code"),
			}
			*out = append(*out, node)
		case *ast.AutoLink:
			label := string(v.Label(source))
			node := map[string]any{
				"type":  "text",
				"text":  label,
				"marks": addMarkAttrs(marks, "link", map[string]any{"href": label}),
			}
			*out = append(*out, node)
		case *ast.Link:
			href := string(v.Destination)
			body := readText(v, source)
			if body == "" {
				body = href
			}
			node := map[string]any{
				"type":  "text",
				"text":  body,
				"marks": addMarkAttrs(marks, "link", map[string]any{"href": href}),
			}
			*out = append(*out, node)
		default:
			// Recurse — unknown inline wrappers shouldn't drop their
			// children.
			collect(n, source, marks, out)
		}
	}
}

func addMark(existing []map[string]any, name string) []map[string]any {
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

func addMarkAttrs(existing []map[string]any, name string, attrs map[string]any) []map[string]any {
	next := make([]map[string]any, 0, len(existing)+1)
	next = append(next, existing...)
	next = append(next, map[string]any{"type": name, "attrs": attrs})
	return next
}

// readLines concatenates the text of an AST node's contained lines
// (used for code blocks).
func readLines(n ast.Node, source []byte) string {
	var b strings.Builder
	for i := 0; i < n.Lines().Len(); i++ {
		line := n.Lines().At(i)
		b.Write(line.Value(source))
	}
	return strings.TrimRight(b.String(), "\n")
}

// readText flattens an inline subtree to its plain text. Used by
// the unknown-block fallback and inline code spans.
func readText(n ast.Node, source []byte) string {
	var b strings.Builder
	collectText(n, source, &b)
	return b.String()
}

func collectText(n ast.Node, source []byte, b *strings.Builder) {
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		switch v := child.(type) {
		case *ast.Text:
			b.Write(v.Segment.Value(source))
		default:
			collectText(child, source, b)
		}
	}
}

// ─── ProseMirror → Markdown ─────────────────────────────

// ProseMirrorToMarkdown renders a ProseMirror JSON document back to
// markdown for MCP responses. The tool surface returns this as
// content_text — agents process text, not editor JSON.
//
// Malformed input returns an empty string so callers can always
// safely use the result.
func ProseMirrorToMarkdown(pm string) string {
	if strings.TrimSpace(pm) == "" {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(pm), &doc); err != nil {
		return ""
	}
	var b strings.Builder
	renderBlocks(doc["content"], 0, &b)
	return strings.TrimRight(b.String(), "\n")
}

func renderBlocks(raw any, indent int, b *strings.Builder) {
	blocks, _ := raw.([]any)
	for i, raw := range blocks {
		block, _ := raw.(map[string]any)
		if block == nil {
			continue
		}
		renderBlock(block, indent, b)
		if i < len(blocks)-1 {
			b.WriteString("\n")
		}
	}
}

func renderBlock(b map[string]any, indent int, out *strings.Builder) {
	switch b["type"] {
	case "heading":
		level := 1
		if attrs, ok := b["attrs"].(map[string]any); ok {
			if l, ok := attrs["level"].(float64); ok {
				level = int(l)
			}
		}
		out.WriteString(strings.Repeat("#", level))
		out.WriteString(" ")
		renderInline(b["content"], out)
		out.WriteString("\n")
	case "paragraph":
		renderInline(b["content"], out)
		out.WriteString("\n")
	case "code_block":
		lang := ""
		if attrs, ok := b["attrs"].(map[string]any); ok {
			if l, ok := attrs["language"].(string); ok {
				lang = l
			}
		}
		out.WriteString("```")
		out.WriteString(lang)
		out.WriteString("\n")
		renderInline(b["content"], out)
		out.WriteString("\n```\n")
	case "blockquote":
		// Render children, prefix each line with "> ".
		var inner strings.Builder
		renderBlocks(b["content"], 0, &inner)
		for _, line := range strings.Split(strings.TrimRight(inner.String(), "\n"), "\n") {
			out.WriteString("> ")
			out.WriteString(line)
			out.WriteString("\n")
		}
	case "bullet_list":
		renderList(b, indent, "- ", out)
	case "ordered_list":
		renderList(b, indent, "1. ", out)
	case "horizontal_rule":
		out.WriteString("---\n")
	}
}

func renderList(b map[string]any, indent int, marker string, out *strings.Builder) {
	items, _ := b["content"].([]any)
	prefix := strings.Repeat("  ", indent)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		body, _ := item["content"].([]any)
		// First paragraph emits on the marker line; subsequent blocks
		// (including nested lists) line up under it.
		for i, rawChild := range body {
			child, _ := rawChild.(map[string]any)
			if child == nil {
				continue
			}
			switch child["type"] {
			case "paragraph":
				if i == 0 {
					out.WriteString(prefix)
					out.WriteString(marker)
					renderInline(child["content"], out)
					out.WriteString("\n")
				} else {
					out.WriteString(prefix)
					out.WriteString("  ")
					renderInline(child["content"], out)
					out.WriteString("\n")
				}
			case "bullet_list", "ordered_list":
				renderBlock(child, indent+1, out)
			default:
				// Pass nested blocks through their normal rendering.
				renderBlock(child, indent+1, out)
			}
		}
	}
}

func renderInline(raw any, out *strings.Builder) {
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
		out.WriteString(applyMarks(text, marks))
	}
}

// applyMarks wraps the text in the requested markdown decoration.
// Order matters for nested marks: code wins over emphasis since
// markdown code spans don't honour inner emphasis tokens.
func applyMarks(text string, marks []any) string {
	for _, raw := range marks {
		m, _ := raw.(map[string]any)
		switch m["type"] {
		case "code":
			return "`" + text + "`"
		}
	}
	for _, raw := range marks {
		m, _ := raw.(map[string]any)
		switch m["type"] {
		case "strong":
			text = "**" + text + "**"
		case "em":
			text = "*" + text + "*"
		case "link":
			if attrs, ok := m["attrs"].(map[string]any); ok {
				if href, ok := attrs["href"].(string); ok {
					text = fmt.Sprintf("[%s](%s)", text, href)
				}
			}
		}
	}
	return text
}
