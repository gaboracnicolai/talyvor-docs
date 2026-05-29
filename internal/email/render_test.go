package email

import (
	"strings"
	"testing"
)

func sampleData() RenderData {
	return RenderData{
		AppName:        "Talyvor Docs",
		Heading:        "Something happened",
		Title:          "Q3 Launch Plan",
		Lines:          []string{"Detail line one.", "Detail line two."},
		CTALabel:       "View page",
		CTAURL:         "https://docs.example.com/spaces/s1/pages/p1",
		PreferencesURL: "https://docs.example.com/preferences",
		Items: []DigestItem{
			{Title: "Old Spec", Detail: "not updated in 90 days", URL: "https://docs.example.com/pages/old"},
		},
	}
}

func TestRenderer_RendersEveryEventWithoutError(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	for _, ev := range docsEventTemplates {
		html, text, err := r.Render(ev, sampleData())
		if err != nil {
			t.Fatalf("Render(%q): %v", ev, err)
		}
		if strings.TrimSpace(html) == "" || strings.TrimSpace(text) == "" {
			t.Fatalf("Render(%q): empty body", ev)
		}
		if !strings.Contains(html, "https://docs.example.com/preferences") {
			t.Errorf("Render(%q): HTML missing preferences link", ev)
		}
	}
}

func TestRenderer_DigestListsItems(t *testing.T) {
	r, _ := NewRenderer()
	html, text, err := r.Render(EventPageStaleDigest, sampleData())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(html, "Old Spec") || !strings.Contains(html, "https://docs.example.com/pages/old") {
		t.Error("digest HTML should list the stale page + its link")
	}
	if !strings.Contains(text, "Old Spec") {
		t.Error("digest text should list the stale page")
	}
}

func TestRenderer_UnknownEventErrors(t *testing.T) {
	r, _ := NewRenderer()
	if _, _, err := r.Render("does.not.exist", sampleData()); err == nil {
		t.Fatal("unknown event should error")
	}
}

func TestRenderer_EscapesUserSuppliedContent(t *testing.T) {
	r, _ := NewRenderer()
	d := sampleData()
	d.Title = `<script>alert(1)</script>`
	html, _, err := r.Render(EventPageMentioned, d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Error("user-supplied Title must be HTML-escaped")
	}
}
