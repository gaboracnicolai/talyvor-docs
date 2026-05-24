// Package ai is the in-process AI engine that sits in front of the
// Lens client. Each feature method is one wrapped Complete() with a
// purpose-built system prompt + feature tag. The engine does not own
// any state — it's a thin orchestration layer so handlers and the
// future Q&A panel can share the same prompts.
package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/talyvor/docs/internal/lensintegration"
)

// Models. Cheap haiku for transforms; sonnet for the Q&A surface,
// which needs to reason over multiple page contexts.
const (
	modelFast   = "claude-haiku-4-6"
	modelSmart  = "claude-sonnet-4-6"
	defaultLang = "English"
)

// ErrUnavailable surfaces to callers when Lens isn't configured.
// Handlers translate this into a 503 with a friendly user message
// rather than a raw error string.
var ErrUnavailable = errors.New("ai: lens unavailable")

type Engine struct {
	lensClient *lensintegration.Client
}

func New(lensClient *lensintegration.Client) *Engine {
	return &Engine{lensClient: lensClient}
}

// IsAvailable reports whether the engine can fulfil AI requests.
// Lens being misconfigured (empty URL/key) is the only thing that
// makes us unavailable in steady state.
func (e *Engine) IsAvailable() bool {
	return e.lensClient != nil && e.lensClient.IsConfigured()
}

// run is the shared call site. Every feature method delegates here so
// the model + feature tag policy stays in one place.
func (e *Engine) run(ctx context.Context, workspaceID, system, user, model, feature string) (string, error) {
	if !e.IsAvailable() {
		return "", ErrUnavailable
	}
	out, err := e.lensClient.CompleteWithFeature(ctx, workspaceID, user, system, model, feature)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ─── Feature 1: Write with AI ─────────────────────────────

const writeSystem = `You are a technical documentation assistant. Write clear, concise documentation. Return ONLY the text to insert, no explanations.`

func (e *Engine) WriteWithAI(ctx context.Context, workspaceID, prompt, docContext string) (string, error) {
	user := fmt.Sprintf("Context:\n%s\n\nWrite: %s", docContext, prompt)
	return e.run(ctx, workspaceID, writeSystem, user, modelFast, "docs-ai-write")
}

// ─── Feature 2: Summarize ─────────────────────────────────

const summarizeSystem = `Summarize the following documentation into 2-3 clear bullet points. Return ONLY the bullets, no intro text.`

func (e *Engine) Summarize(ctx context.Context, workspaceID, content string) (string, error) {
	return e.run(ctx, workspaceID, summarizeSystem, content, modelFast, "docs-ai-summarize")
}

// ─── Feature 3: Fix grammar ───────────────────────────────

const grammarSystem = `Fix grammar and spelling in the following text. Return ONLY the corrected text, no explanations. Preserve the original meaning and tone.`

func (e *Engine) FixGrammar(ctx context.Context, workspaceID, text string) (string, error) {
	return e.run(ctx, workspaceID, grammarSystem, text, modelFast, "docs-ai-grammar")
}

// ─── Feature 4: Make shorter ──────────────────────────────

const shorterSystem = `Shorten the following text while preserving all key information. Return ONLY the shortened text.`

func (e *Engine) MakeShorter(ctx context.Context, workspaceID, text string) (string, error) {
	return e.run(ctx, workspaceID, shorterSystem, text, modelFast, "docs-ai-shorter")
}

// ─── Feature 5: Make longer ───────────────────────────────

const longerSystem = `Expand the following text with more detail and examples. Return ONLY the expanded text.`

func (e *Engine) MakeLonger(ctx context.Context, workspaceID, text string) (string, error) {
	return e.run(ctx, workspaceID, longerSystem, text, modelFast, "docs-ai-longer")
}

// ─── Feature 6: Translate ─────────────────────────────────

func (e *Engine) Translate(ctx context.Context, workspaceID, text, targetLanguage string) (string, error) {
	if strings.TrimSpace(targetLanguage) == "" {
		targetLanguage = defaultLang
	}
	system := fmt.Sprintf("Translate the following text to %s. Return ONLY the translation.", targetLanguage)
	return e.run(ctx, workspaceID, system, text, modelFast, "docs-ai-translate")
}

// ─── Feature 7: Q&A over docs ─────────────────────────────

// PageContext is the lean projection a single page contributes to the
// Q&A prompt. We don't ship full ProseMirror JSON to the model — just
// title + plain-text excerpt + URL for citation.
type PageContext struct {
	Title   string
	Content string
	URL     string
}

const askSystem = `You are a helpful assistant answering questions about internal documentation. Use ONLY the provided documentation to answer. If the answer isn't in the docs, say so clearly.`

func (e *Engine) AskDocs(ctx context.Context, workspaceID, question string, relevantPages []PageContext) (string, error) {
	var b strings.Builder
	b.WriteString("Question: ")
	b.WriteString(question)
	b.WriteString("\n\nDocumentation:\n")
	for i, p := range relevantPages {
		fmt.Fprintf(&b, "\n--- Page %d: %s ---\n%s\n", i+1, p.Title, p.Content)
		if p.URL != "" {
			fmt.Fprintf(&b, "Source: %s\n", p.URL)
		}
	}
	return e.run(ctx, workspaceID, askSystem, b.String(), modelSmart, "docs-ai-ask")
}

// ─── Feature 8: Suggest title ─────────────────────────────

const titleSystem = `Suggest a concise, descriptive title for this documentation page. Return ONLY the title, no quotes.`

func (e *Engine) SuggestTitle(ctx context.Context, workspaceID, content string) (string, error) {
	out, err := e.run(ctx, workspaceID, titleSystem, content, modelFast, "docs-ai-title")
	if err != nil {
		return "", err
	}
	// Strip stray wrapping quotes — the model frequently ignores the
	// "no quotes" instruction.
	out = strings.Trim(out, " \t\n\"'")
	return out, nil
}
