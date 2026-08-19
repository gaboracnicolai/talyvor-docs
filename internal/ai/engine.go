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
	"log/slog"
	"strings"

	"github.com/talyvor/docs/internal/lensintegration"
)

// Models. Cheap haiku for transforms; sonnet for the Q&A surface,
// which needs to reason over multiple page contexts.
//
// ⚠⚠ modelFast WAS "claude-haiku-4-6" AND THERE IS NO HAIKU 4.6 AT ANY VERSION. Seven of this
// service's nine AI operations — write, summarize, grammar, shorter, longer, translate,
// suggest-title — named a model that does not exist, so every one of them was a 404
// not_found_error from Anthropic in any deployment with a real Lens behind it. Only Ask
// (modelSmart) and the embedding path named a real model.
//
// ⚠ LENS DOES NOT CATCH THIS FOR US, AND ITS OWN SOURCE SAYS WHY. talyvor-lens removed the same
// literal from its COST-ROUTING CATALOG after it "404'd a live request"
// (internal/catalog/verified_models_test.go, pinning a GET /v1/models capture;
// internal/catalog/seed.go; internal/router/router.go's cheap tier now names claude-haiku-4-5,
// "the real cheapest Anthropic model"). That guard covers models LENS selects. A model id in the
// REQUEST BODY skips it: the proxy prices an unknown model on a derived fallback, alerts, and
// passes it upstream — so the provider is what answers.
//
// claude-haiku-4-5 is the id Lens's router replaced the identical phantom with, and it is in the
// pinned capture. modelexistence_test.go is the census that keeps every model literal in this
// repository answerable to that capture.
const (
	modelFast   = "claude-haiku-4-5"
	modelSmart  = "claude-sonnet-4-6"
	defaultLang = "English"
)

// ErrUnavailable surfaces to callers when Lens isn't configured.
// Handlers translate this into a 503 with a friendly user message
// rather than a raw error string.
var ErrUnavailable = errors.New("ai: lens unavailable")

// AISpendBinder records that a Lens request belonged to a page. Optional: nil ⇒ the engine
// behaves exactly as before and no attribution is recorded, which is what a bare test mount and
// any deployment without the page store get.
type AISpendBinder interface {
	BindAISpend(ctx context.Context, requestID, pageID, workspaceID, operation string) error
}

type Engine struct {
	lensClient *lensintegration.Client
	binder     AISpendBinder
}

func New(lensClient *lensintegration.Client) *Engine {
	return &Engine{lensClient: lensClient}
}

// WithSpendBinder wires page-scoped cost attribution. Without it every operation still runs; it
// simply records nothing, so the feature degrades to today's behaviour rather than failing.
func (e *Engine) WithSpendBinder(b AISpendBinder) *Engine { e.binder = b; return e }

// IsAvailable reports whether the engine can fulfil AI requests.
// Lens being misconfigured (empty URL/key) is the only thing that
// makes us unavailable in steady state.
// IsAvailable reports whether AI features can run. NIL-RECEIVER SAFE, deliberately.
//
// The mcp.Server holds its engine behind an `aiDeps` INTERFACE. Assigning a nil
// *ai.Engine into an interface field yields a NON-nil interface (it carries a type with a
// nil value), so mcp's `if s.deps.ai != nil` guard reads as a nil-check and is not one:
// the call proceeds on a nil receiver and dereferences e.lensClient. Making the receiver
// check itself part of the availability contract fixes it for every caller at once, rather
// than asking each of them to remember a Go footgun.
//
// Production never passed a nil engine (main.go always constructs one), so this was latent
// — but a panicking handler is exactly what the hardening run exists to prevent, and
// middleware.Recoverer turning it into a 500 is not a defence worth relying on.
func (e *Engine) IsAvailable() bool {
	return e != nil && e.lensClient != nil && e.lensClient.IsConfigured()
}

// run is the shared call site. Every feature method delegates here so
// the model + feature tag policy stays in one place.
// run performs the completion and, when this operation belongs to ONE page, binds Lens's request
// id to it so the cost can be attributed later.
//
// ⚠ pageID IS DELIBERATELY ALLOWED TO BE EMPTY, and two operations always pass it empty:
// docs-ai-ask (answers across many pages by construction) and docs-search (workspace-wide). Their
// cost stays visible in Lens under its operation feature and is attributed to no page, because
// attributing a multi-page answer to one page would be a fabrication. SuggestTitle also passes
// empty when the page does not exist yet — a title suggested before the first save has nothing to
// attach to.
//
// The binding NEVER fails the completion. The user asked for AI text, not for bookkeeping; a
// ledger write that failed must not cost them the answer they already paid for.
func (e *Engine) run(ctx context.Context, workspaceID, system, user, model, feature, pageID string) (string, error) {
	if !e.IsAvailable() {
		return "", ErrUnavailable
	}
	out, reqID, err := e.lensClient.CompleteWithRequestID(ctx, workspaceID, user, system, model, feature)
	if err != nil {
		return "", err
	}
	if e.binder != nil && pageID != "" && reqID != "" {
		if bErr := e.binder.BindAISpend(ctx, reqID, pageID, workspaceID, feature); bErr != nil {
			slog.Warn("ai: page spend binding failed — this operation's cost will not be attributed",
				slog.String("workspace_id", workspaceID),
				slog.String("page_id", pageID),
				slog.String("operation", feature),
				slog.String("err", bErr.Error()))
		}
	}
	return strings.TrimSpace(out), nil
}

// ─── Feature 1: Write with AI ─────────────────────────────

const writeSystem = `You are a technical documentation assistant. Write clear, concise documentation. Return ONLY the text to insert, no explanations.`

func (e *Engine) WriteWithAI(ctx context.Context, workspaceID, prompt, docContext string, pageID string) (string, error) {
	user := fmt.Sprintf("Context:\n%s\n\nWrite: %s", docContext, prompt)
	return e.run(ctx, workspaceID, writeSystem, user, modelFast, "docs-ai-write", pageID)
}

// ─── Feature 2: Summarize ─────────────────────────────────

const summarizeSystem = `Summarize the following documentation into 2-3 clear bullet points. Return ONLY the bullets, no intro text.`

func (e *Engine) Summarize(ctx context.Context, workspaceID, content string, pageID string) (string, error) {
	return e.run(ctx, workspaceID, summarizeSystem, content, modelFast, "docs-ai-summarize", pageID)
}

// ─── Feature 3: Fix grammar ───────────────────────────────

const grammarSystem = `Fix grammar and spelling in the following text. Return ONLY the corrected text, no explanations. Preserve the original meaning and tone.`

func (e *Engine) FixGrammar(ctx context.Context, workspaceID, text string, pageID string) (string, error) {
	return e.run(ctx, workspaceID, grammarSystem, text, modelFast, "docs-ai-grammar", pageID)
}

// ─── Feature 4: Make shorter ──────────────────────────────

const shorterSystem = `Shorten the following text while preserving all key information. Return ONLY the shortened text.`

func (e *Engine) MakeShorter(ctx context.Context, workspaceID, text string, pageID string) (string, error) {
	return e.run(ctx, workspaceID, shorterSystem, text, modelFast, "docs-ai-shorter", pageID)
}

// ─── Feature 5: Make longer ───────────────────────────────

const longerSystem = `Expand the following text with more detail and examples. Return ONLY the expanded text.`

func (e *Engine) MakeLonger(ctx context.Context, workspaceID, text string, pageID string) (string, error) {
	return e.run(ctx, workspaceID, longerSystem, text, modelFast, "docs-ai-longer", pageID)
}

// ─── Feature 6: Translate ─────────────────────────────────

func (e *Engine) Translate(ctx context.Context, workspaceID, text, targetLanguage, pageID string) (string, error) {
	if strings.TrimSpace(targetLanguage) == "" {
		targetLanguage = defaultLang
	}
	system := fmt.Sprintf("Translate the following text to %s. Return ONLY the translation.", targetLanguage)
	return e.run(ctx, workspaceID, system, text, modelFast, "docs-ai-translate", pageID)
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
	// NO PAGE ID: an answer drawn from several pages belongs to none of them. See run().
	return e.run(ctx, workspaceID, askSystem, b.String(), modelSmart, "docs-ai-ask", "")
}

// ─── Feature 8: Suggest title ─────────────────────────────

const titleSystem = `Suggest a concise, descriptive title for this documentation page. Return ONLY the title, no quotes.`

func (e *Engine) SuggestTitle(ctx context.Context, workspaceID, content, pageID string) (string, error) {
	out, err := e.run(ctx, workspaceID, titleSystem, content, modelFast, "docs-ai-title", pageID)
	if err != nil {
		return "", err
	}
	// Strip stray wrapping quotes — the model frequently ignores the
	// "no quotes" instruction.
	out = strings.Trim(out, " \t\n\"'")
	return out, nil
}
