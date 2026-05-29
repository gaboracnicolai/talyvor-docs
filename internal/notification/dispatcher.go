package notification

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/talyvor/docs/internal/email"
	"github.com/talyvor/docs/internal/model"
)

var errNotFound = errors.New("notification: not found")

// PageRef is the minimal page data the dispatcher needs for subjects + links.
type PageRef struct {
	ID          string
	SpaceID     string
	WorkspaceID string
	Title       string
	CreatedBy   string
}

// directory loads page metadata and resolves @mentions. Backed by the DB in
// production; faked in tests.
type directory interface {
	PageByID(ctx context.Context, id string) (*PageRef, error)
	ResolveMentions(ctx context.Context, workspaceID string, handles []string) ([]string, error)
}

// recipientResolver turns member IDs into email addresses (directory-backed;
// members without a row are simply absent).
type recipientResolver interface {
	EmailsByIDs(ctx context.Context, ids []string) (map[string]Recipient, error)
}

// prefChecker filters recipients by their per-event opt-out preference.
type prefChecker interface {
	EnabledMembers(ctx context.Context, eventType string, memberIDs []string) ([]string, error)
}

// enqueuer hands a rendered message to async delivery (*email.Queue).
type enqueuer interface {
	Enqueue(email.Message) bool
}

// Dispatcher turns Docs events into emails. Every method is best-effort: it
// resolves recipients, excludes the actor, honours preferences, skips members
// with no resolvable address, renders, and enqueues — logging and swallowing
// all errors so a notification can never block or fail the request/job.
type Dispatcher struct {
	dir        directory
	recipients recipientResolver
	prefs      prefChecker
	queue      enqueuer
	renderer   *email.Renderer
	baseURL    string
	appName    string
	logger     *slog.Logger
}

func newDispatcher(dir directory, recipients recipientResolver, prefs prefChecker, queue enqueuer, renderer *email.Renderer, baseURL, appName string, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{
		dir: dir, recipients: recipients, prefs: prefs, queue: queue, renderer: renderer,
		baseURL: strings.TrimRight(baseURL, "/"), appName: appName, logger: logger,
	}
}

// ApprovalRequested emails each reviewer (excluding the requester) that their
// review is requested on a page.
func (d *Dispatcher) ApprovalRequested(ctx context.Context, pageID, workspaceID, requestedBy string, reviewers []string, message string) {
	ref := d.pageRefOrStub(ctx, pageID, workspaceID)
	lines := []string{}
	if message != "" {
		lines = append(lines, "“"+message+"”")
	}
	d.fanout(ctx, email.EventPageApprovalRequested, reviewers, requestedBy,
		fmt.Sprintf("Review requested: %s", ref.Title),
		email.RenderData{
			Heading:  fmt.Sprintf("Review requested: %s", ref.Title),
			Title:    ref.Title,
			Lines:    lines,
			CTALabel: "Review page", CTAURL: d.pageURL(ref),
		})
}

// PageMentioned parses @mentions from text (a page body or comment), resolves
// them, and emails everyone mentioned except the actor.
func (d *Dispatcher) PageMentioned(ctx context.Context, pageID, text, actorID string) {
	handles := parseMentions(text)
	if len(handles) == 0 {
		return
	}
	ref := d.pageRefOrStub(ctx, pageID, "")
	ids, err := d.dir.ResolveMentions(ctx, ref.WorkspaceID, handles)
	if err != nil {
		d.logger.Warn("email: resolve mentions failed", slog.String("err", err.Error()))
		return
	}
	if len(ids) == 0 {
		return
	}
	d.fanout(ctx, email.EventPageMentioned, ids, actorID,
		fmt.Sprintf("You were mentioned on %s", ref.Title),
		email.RenderData{
			Heading:  fmt.Sprintf("You were mentioned on %s", ref.Title),
			Title:    ref.Title,
			CTALabel: "View page", CTAURL: d.pageURL(ref),
		})
}

// StaleDigest emails each page owner a single digest of THEIR stale pages.
// Driven by the freshness scheduler; there is no actor to exclude.
func (d *Dispatcher) StaleDigest(ctx context.Context, workspaceID string, stale []model.Page) {
	byOwner := map[string][]model.Page{}
	var owners []string
	for _, p := range stale {
		if p.CreatedBy == "" {
			continue
		}
		if _, seen := byOwner[p.CreatedBy]; !seen {
			owners = append(owners, p.CreatedBy)
		}
		byOwner[p.CreatedBy] = append(byOwner[p.CreatedBy], p)
	}
	for _, owner := range owners {
		recips := d.deliverable(ctx, email.EventPageStaleDigest, []string{owner}, "")
		if len(recips) == 0 {
			continue
		}
		pages := byOwner[owner]
		items := make([]email.DigestItem, 0, len(pages))
		for _, p := range pages {
			items = append(items, email.DigestItem{
				Title:  p.Title,
				Detail: fmt.Sprintf("TTL %d days", p.StaleAfterDays),
				URL:    d.pageURL(PageRef{ID: p.ID, SpaceID: p.SpaceID}),
			})
		}
		data := email.RenderData{
			Heading: fmt.Sprintf("%d page(s) may be out of date", len(pages)),
			Items:   items,
			AppName: d.appName,
		}
		data.PreferencesURL = d.preferencesURL()
		html, text, err := d.renderer.Render(email.EventPageStaleDigest, data)
		if err != nil {
			d.logger.Warn("email: render digest failed", slog.String("err", err.Error()))
			continue
		}
		d.queue.Enqueue(email.Message{
			To:       []string{recips[0].Email},
			Subject:  fmt.Sprintf("%d Docs page(s) may be out of date", len(pages)),
			HTMLBody: html, TextBody: text,
		})
	}
}

// fanout: dedupe + exclude actor → preference filter → resolve addresses
// (skip absent) → render once → enqueue one message per recipient.
func (d *Dispatcher) fanout(ctx context.Context, event string, recipientIDs []string, actorID, subject string, data email.RenderData) {
	recips := d.deliverable(ctx, event, recipientIDs, actorID)
	if len(recips) == 0 {
		return
	}
	data.AppName = d.appName
	data.PreferencesURL = d.preferencesURL()
	html, text, err := d.renderer.Render(event, data)
	if err != nil {
		d.logger.Warn("email: render failed", slog.String("event", event), slog.String("err", err.Error()))
		return
	}
	for _, r := range recips {
		d.queue.Enqueue(email.Message{To: []string{r.Email}, Subject: subject, HTMLBody: html, TextBody: text})
	}
}

// deliverable resolves the final recipient list for an event: dedupe, drop the
// actor, apply opt-out preferences, and resolve to addresses (members without a
// directory row are dropped).
func (d *Dispatcher) deliverable(ctx context.Context, event string, ids []string, actorID string) []Recipient {
	ids = dedupeExclude(ids, actorID)
	if len(ids) == 0 {
		return nil
	}
	enabled, err := d.prefs.EnabledMembers(ctx, event, ids)
	if err != nil {
		d.logger.Warn("email: preference filter failed", slog.String("event", event), slog.String("err", err.Error()))
		return nil
	}
	if len(enabled) == 0 {
		return nil
	}
	emails, err := d.recipients.EmailsByIDs(ctx, enabled)
	if err != nil {
		d.logger.Warn("email: resolve addresses failed", slog.String("err", err.Error()))
		return nil
	}
	out := make([]Recipient, 0, len(enabled))
	for _, id := range enabled {
		if r, ok := emails[id]; ok && r.Email != "" {
			out = append(out, r)
		}
	}
	return out
}

// pageRefOrStub loads the page; on failure returns a stub so the email can
// still be addressed (best-effort) with a generic title.
func (d *Dispatcher) pageRefOrStub(ctx context.Context, pageID, workspaceID string) PageRef {
	if ref, err := d.dir.PageByID(ctx, pageID); err == nil && ref != nil {
		return *ref
	}
	return PageRef{ID: pageID, WorkspaceID: workspaceID, Title: "a page"}
}

func (d *Dispatcher) pageURL(ref PageRef) string {
	if ref.SpaceID != "" {
		return fmt.Sprintf("%s/spaces/%s/pages/%s", d.baseURL, ref.SpaceID, ref.ID)
	}
	return fmt.Sprintf("%s/pages/%s", d.baseURL, ref.ID)
}

func (d *Dispatcher) preferencesURL() string { return d.baseURL + "/settings/notifications" }

func dedupeExclude(ids []string, exclude string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if id == "" || id == exclude || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
