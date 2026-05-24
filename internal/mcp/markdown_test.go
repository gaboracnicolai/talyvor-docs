package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// firstBlock returns the first content block of a ProseMirror doc as
// a generic map. Helper for tests that want to assert on type/attrs
// without re-implementing the unmarshal dance per case.
func firstBlock(t *testing.T, pm string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(pm), &doc); err != nil {
		t.Fatalf("invalid prosemirror json: %v\n%s", err, pm)
	}
	if doc["type"] != "doc" {
		t.Fatalf("root must be doc, got %v", doc["type"])
	}
	content, _ := doc["content"].([]any)
	if len(content) == 0 {
		t.Fatal("doc has no content")
	}
	return content[0].(map[string]any)
}

func TestMarkdownToProseMirror_Heading(t *testing.T) {
	pm := MarkdownToProseMirror("# Title")
	first := firstBlock(t, pm)
	if first["type"] != "heading" {
		t.Fatalf("expected heading, got %v", first["type"])
	}
	attrs, _ := first["attrs"].(map[string]any)
	if int(attrs["level"].(float64)) != 1 {
		t.Fatalf("expected level=1, got %v", attrs["level"])
	}
	inline, _ := first["content"].([]any)
	if len(inline) == 0 {
		t.Fatal("heading text missing")
	}
	text := inline[0].(map[string]any)
	if text["text"] != "Title" {
		t.Fatalf("title wrong: %v", text["text"])
	}
}

func TestMarkdownToProseMirror_BoldItalicCode(t *testing.T) {
	pm := MarkdownToProseMirror("Hello **bold** and *italic* and `code`.")
	first := firstBlock(t, pm)
	if first["type"] != "paragraph" {
		t.Fatalf("expected paragraph, got %v", first["type"])
	}
	inline, _ := first["content"].([]any)
	// Find each marked span.
	var hasBold, hasItalic, hasCode bool
	for _, raw := range inline {
		n := raw.(map[string]any)
		marks, _ := n["marks"].([]any)
		for _, m := range marks {
			t := m.(map[string]any)["type"]
			if t == "strong" {
				hasBold = true
			}
			if t == "em" {
				hasItalic = true
			}
			if t == "code" {
				hasCode = true
			}
		}
	}
	if !hasBold || !hasItalic || !hasCode {
		t.Fatalf("inline marks missing: bold=%v italic=%v code=%v", hasBold, hasItalic, hasCode)
	}
}

func TestMarkdownToProseMirror_CodeBlock(t *testing.T) {
	pm := MarkdownToProseMirror("```go\nfmt.Println(\"hi\")\n```")
	first := firstBlock(t, pm)
	if first["type"] != "code_block" {
		t.Fatalf("expected code_block, got %v", first["type"])
	}
	inline, _ := first["content"].([]any)
	if len(inline) == 0 {
		t.Fatal("code block body missing")
	}
	if !strings.Contains(inline[0].(map[string]any)["text"].(string), "fmt.Println") {
		t.Fatalf("code body wrong: %+v", inline[0])
	}
}

func TestMarkdownToProseMirror_BulletList(t *testing.T) {
	pm := MarkdownToProseMirror("- one\n- two\n- three")
	first := firstBlock(t, pm)
	if first["type"] != "bullet_list" {
		t.Fatalf("expected bullet_list, got %v", first["type"])
	}
	items, _ := first["content"].([]any)
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %d", len(items))
	}
}

func TestMarkdownToProseMirror_OrderedList(t *testing.T) {
	pm := MarkdownToProseMirror("1. one\n2. two")
	first := firstBlock(t, pm)
	if first["type"] != "ordered_list" {
		t.Fatalf("expected ordered_list, got %v", first["type"])
	}
}

func TestMarkdownToProseMirror_NestedList(t *testing.T) {
	pm := MarkdownToProseMirror("- parent\n    - child")
	first := firstBlock(t, pm)
	items, _ := first["content"].([]any)
	if len(items) != 1 {
		t.Fatalf("want 1 top-level item, got %d", len(items))
	}
	// The nested list should appear inside the first list_item's content.
	parent := items[0].(map[string]any)
	parentBody, _ := parent["content"].([]any)
	var foundNested bool
	for _, raw := range parentBody {
		n := raw.(map[string]any)
		if n["type"] == "bullet_list" {
			foundNested = true
		}
	}
	if !foundNested {
		t.Fatalf("nested list not detected: %+v", parentBody)
	}
}

func TestMarkdownToProseMirror_Blockquote(t *testing.T) {
	pm := MarkdownToProseMirror("> quoted")
	first := firstBlock(t, pm)
	if first["type"] != "blockquote" {
		t.Fatalf("expected blockquote, got %v", first["type"])
	}
}

func TestProseMirrorToMarkdown_PlainParagraph(t *testing.T) {
	pm := `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"hello world"}]}]}`
	md := ProseMirrorToMarkdown(pm)
	if strings.TrimSpace(md) != "hello world" {
		t.Fatalf("md = %q", md)
	}
}

func TestProseMirrorToMarkdown_HeadingAndList(t *testing.T) {
	pm := `{"type":"doc","content":[
		{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Setup"}]},
		{"type":"bullet_list","content":[
			{"type":"list_item","content":[{"type":"paragraph","content":[{"type":"text","text":"first"}]}]},
			{"type":"list_item","content":[{"type":"paragraph","content":[{"type":"text","text":"second"}]}]}
		]}
	]}`
	md := ProseMirrorToMarkdown(pm)
	if !strings.Contains(md, "## Setup") {
		t.Fatalf("heading missing: %q", md)
	}
	if !strings.Contains(md, "- first") || !strings.Contains(md, "- second") {
		t.Fatalf("bullets missing: %q", md)
	}
}

func TestProseMirrorToMarkdown_BoldAndCode(t *testing.T) {
	pm := `{"type":"doc","content":[
		{"type":"paragraph","content":[
			{"type":"text","text":"hello "},
			{"type":"text","text":"world","marks":[{"type":"strong"}]},
			{"type":"text","text":" and "},
			{"type":"text","text":"code","marks":[{"type":"code"}]}
		]}
	]}`
	md := ProseMirrorToMarkdown(pm)
	if !strings.Contains(md, "**world**") {
		t.Fatalf("bold missing: %q", md)
	}
	if !strings.Contains(md, "`code`") {
		t.Fatalf("inline code missing: %q", md)
	}
}

func TestProseMirrorToMarkdown_Empty(t *testing.T) {
	if got := ProseMirrorToMarkdown(""); got != "" {
		t.Fatalf("empty in → empty out, got %q", got)
	}
	if got := ProseMirrorToMarkdown("{not json"); got != "" {
		t.Fatalf("malformed in → empty out, got %q", got)
	}
}
