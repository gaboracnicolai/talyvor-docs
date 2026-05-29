// Command docs is the Talyvor Docs API server.
//
// HTTP server on :4000 (configurable via DOCS_LISTEN_ADDR) serving
// spaces / pages / blocks / comments / search. SIGTERM triggers a
// graceful drain before closing the DB pool.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/talyvor/docs/internal/ai"
	"github.com/talyvor/docs/internal/analytics"
	"github.com/talyvor/docs/internal/approval"
	"github.com/talyvor/docs/internal/block"
	"github.com/talyvor/docs/internal/changelog"
	"github.com/talyvor/docs/internal/collab"
	"github.com/talyvor/docs/internal/comment"
	"github.com/talyvor/docs/internal/config"
	"github.com/talyvor/docs/internal/customdomain"
	"github.com/talyvor/docs/internal/database"
	"github.com/talyvor/docs/internal/db"
	"github.com/talyvor/docs/internal/email"
	"github.com/talyvor/docs/internal/export"
	"github.com/talyvor/docs/internal/freshness"
	"github.com/talyvor/docs/internal/importer"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/mcp"
	"github.com/talyvor/docs/internal/metrics"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/notification"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelink"
	"github.com/talyvor/docs/internal/pagelock"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/search"
	"github.com/talyvor/docs/internal/sharing"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/templatelib"
	"github.com/talyvor/docs/internal/trackintegration"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", slog.String("err", err.Error()))
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("db init failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
	defer pool.Close()

	// Email notifications (stale-page digest, approval-requested, mention).
	// Strictly opt-in via EMAIL_ENABLED; when disabled, emailDispatcher stays
	// nil and no hooks are wired, so Docs behaves exactly as before. Delivery
	// is async + best-effort and never blocks or fails a request/job.
	var (
		emailQueue      *email.Queue
		emailDispatcher *notification.Dispatcher
	)
	if emailCfg := email.LoadConfig(); emailCfg.Enabled {
		renderer, rerr := email.NewRenderer()
		if rerr != nil {
			slog.Error("email disabled: renderer init failed", slog.String("err", rerr.Error()))
		} else {
			emailQueue = email.NewQueue(email.NewSender(emailCfg, logger), email.QueueOptions{Workers: 4}, logger)
			emailQueue.Start(ctx)
			emailDispatcher = notification.NewDispatcher(
				pool, notification.NewRecipientStore(pool), notification.NewPreferenceStore(pool),
				emailQueue, renderer, cfg.AppBaseURL, "Talyvor Docs", logger,
			)
			slog.Info("email notifications enabled", slog.String("from", emailCfg.From))
		}
	}

	spaceStore := space.NewStore(pool)
	linkStore := pagelink.NewStore(pool)
	// Phase 6 wires the semantic-search indexer to fire after every
	// page save. Both linker and indexer are best-effort and run
	// without blocking the save itself (indexer detaches into a
	// goroutine inside the store).
	lensClient := lensintegration.New(cfg.LensURL, cfg.LensAPIKey)
	semSearch := search.New(lensClient, pool).WithLensCreds(cfg.LensURL, cfg.LensAPIKey)
	pageStore := page.NewStore(pool).
		WithLinker(linkStore).
		WithIndexer(semSearch)
	blockStore := block.NewStore(pool)

	spaceHandler := space.NewHandler(spaceStore)
	pageHandler := page.NewHandler(pageStore, pool)
	blockHandler := block.NewHandler(blockStore)

	// Track integration. Empty trackURL / API key gracefully
	// no-op every endpoint and skip the cost syncer.
	trackClient := trackintegration.New(cfg.TrackURL, cfg.TrackAPIKey)
	trackHandler := trackintegration.NewHandler(trackClient)
	linkHandler := pagelink.NewHandler(linkStore)
	trackSyncer := trackintegration.NewSyncer(trackClient, pageStore, linkStore, cfg.DefaultWorkspaceID)
	go trackSyncer.Start(ctx, 15*time.Minute)

	// Lens integration. Every AI call routes through here; an empty
	// DOCS_LENS_URL/API key flips IsAvailable() off and the handler
	// returns 503 with a friendly message instead of erroring.
	aiEngine := ai.New(lensClient)
	aiHandler := ai.NewHandler(aiEngine, pageStore)

	// Unified search handler. Semantic-side falls back to empty when
	// Lens is unconfigured; full-text always works.
	searchHandler := search.NewHandler(pageStore, semSearch)

	// Freshness engine + 9am-UTC stale-doc digest. Engine reads
	// pages + linked-issue closure to surface "this spec needs a
	// look" badges. The daily digest is currently log-only; future
	// phases will ship Slack / email.
	freshEngine := freshness.New(pageStore, linkStore, trackClient)
	if emailDispatcher != nil {
		freshEngine.WithDigester(emailDispatcher) // daily digest now emails owners
	}
	freshHandler := freshness.NewHandler(freshEngine)
	freshEngine.Start(ctx, cfg.DefaultWorkspaceID)

	// Analytics store + handler. View recording is best-effort —
	// failures are logged on the client side rather than retried.
	analyticsStore := analytics.NewStore(pool)
	analyticsHandler := analytics.NewHandler(analyticsStore)

	// Approval workflow — owns the pages.doc_status column.
	approvalStore := approval.NewStore(pool)
	approvalHandler := approval.NewHandler(approvalStore)

	// Threaded comments — own package as of the resolution-tracking
	// rework. The page handler no longer owns comment routes.
	commentStore := comment.NewStore(pool)
	commentHandler := comment.NewHandler(commentStore)

	// Wire email hooks only when a dispatcher exists. Guarded so we never pass
	// a typed-nil into a handler's interface field (which would pass the
	// nil-check and then panic on call).
	if emailDispatcher != nil {
		approvalHandler.WithEmailer(emailDispatcher)
		commentHandler.WithEmailer(emailDispatcher)
		pageHandler.WithEmailer(emailDispatcher)
	}

	// Changelog — specialised page type with auto-grouping from
	// Track issues + RSS feed.
	changelogStore := changelog.NewStore(pool, trackClient)
	changelogHandler := changelog.NewHandler(changelogStore)

	// Custom domains. The DomainRouter middleware fronts the whole
	// HTTP stack — see the wrapping below — so verified hostnames
	// serve a read-only public space view instead of the admin UI.
	domainStore := customdomain.NewStore(pool)
	domainHandler := customdomain.NewHandler(domainStore, &publicPageAdapter{pages: pageStore})

	// Page locks — soft locks that survive restarts. The CanEdit
	// rule composes locks + approval state, so the lock store reads
	// pages.doc_status too. Wired into page.Store as the edit guard
	// so REST writes honour the lock; the collab handler picks it
	// up further down (after its own NewHandler call).
	lockStore := pagelock.NewStore(pool)
	lockHandler := pagelock.NewHandler(lockStore)
	pageStore = pageStore.WithGuard(lockStore)

	// Export (markdown / HTML / PDF / DOCX) — buffered through a
	// 50MB-capped writer in the handler.
	exporter := export.New(pageStore, spaceStore)
	exportHandler := export.NewHandler(exporter)

	// Template library — 20 built-in templates + workspace-owned
	// custom templates. UseTemplate creates a new page via the
	// shared pageStore, so the entire workflow stays in one
	// transaction-equivalent path.
	tmplStore := templatelib.NewStore(pool, pageStore)
	tmplHandler := templatelib.NewHandler(tmplStore)

	// Inline-database blocks. Independent store; rows + views live
	// in dedicated tables (see migrations/0007). Mounted under /v1.
	dbStore := database.NewStore(pool)
	dbHandler := database.NewHandler(dbStore)

	// Importer (Notion markdown / Confluence HTML) — multipart
	// upload surface so users can migrate off legacy wikis.
	importerSvc := importer.New(pageStore, spaceStore)
	importerHandler := importer.NewHandler(importerSvc)

	// MCP server. Agents (Claude Code, Cursor, etc.) connect to the
	// public /mcp endpoints and call the 10 documented tools. The
	// server keeps zero state of its own — it composes the existing
	// stores via narrow interfaces.
	mcpServer := mcp.New(pageStore, spaceStore, analyticsStore, aiEngine, freshEngine, "0.1.0")

	// Permissions + public sharing.
	permStore := permission.NewStore(pool)
	permHandler := permission.NewHandler(permStore)
	shareStore := sharing.NewStore(pool)
	shareHandler := sharing.NewHandler(shareStore, func(ctx context.Context, pageID string) (*sharing.PublicPage, error) {
		p, err := pageStore.GetByID(ctx, pageID)
		if err != nil || p == nil {
			return nil, err
		}
		return &sharing.PublicPage{
			ID:          p.ID,
			Title:       p.Title,
			Icon:        p.Icon,
			Content:     p.Content,
			ContentText: p.ContentText,
			UpdatedAt:   p.UpdatedAt.Format(time.RFC3339),
		}, nil
	})

	// Collaborative editing engine. The engine is WebSocket-agnostic;
	// the handler layer below upgrades the HTTP request and shuttles
	// frames through the engine's per-client send channels.
	otEngine := collab.NewOTEngine()
	collabHandler := collab.NewHandler(otEngine).WithGuard(lockStore)
	saver := collab.NewAutoSaver(otEngine,
		func(ctx context.Context, pageID, content string) error {
			_, err := pageStore.Update(ctx, pageID, map[string]any{"content": content})
			return err
		})
	go saver.Start(ctx)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(metricsMiddleware)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	r.Handle("/metrics", metrics.Handler())

	// Collab WS lives at the same /v1 prefix as the REST API so a
	// reverse-proxy doesn't need a special rule. The chi middleware
	// Timeout above does NOT apply because chi disables it for
	// hijacked connections.
	r.Get("/v1/collab/{pageID}/ws", collabHandler.ServeWS)

	// MCP server is a public surface (no auth) — agent clients
	// connect over JSON-RPC and SSE. Live on the top-level router
	// so the /v1 group's middleware doesn't intercept it.
	r.Post("/mcp", mcpServer.HandleRPC)
	r.Get("/mcp/sse", mcpServer.HandleSSE)

	r.Route("/v1", func(r chi.Router) {
		spaceHandler.Mount(r)
		pageHandler.Mount(r)
		blockHandler.Mount(r)
		trackHandler.Mount(r)
		linkHandler.Mount(r)
		aiHandler.Mount(r)
		searchHandler.Mount(r)
		freshHandler.Mount(r)
		analyticsHandler.Mount(r)
		permHandler.Mount(r)
		shareHandler.Mount(r)
		shareHandler.MountPublic(r)
		importerHandler.Mount(r)
		dbHandler.Mount(r)
		tmplHandler.Mount(r)
		exportHandler.Mount(r)
		approvalHandler.Mount(r)
		lockHandler.Mount(r)
		commentHandler.Mount(r)
		changelogHandler.Mount(r)
		domainHandler.Mount(r)
	})

	// DomainRouter wraps the chi tree. Requests on a verified
	// custom-domain Host header are routed to the public read-only
	// renderer; everything else passes through to `r` unchanged.
	rootHandler := customdomain.DomainRouter(
		domainStore,
		domainHandler.PublicHandler(),
		r,
		listenHostname(cfg.ListenAddr),
	)
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           rootHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("docs server listening", slog.String("addr", cfg.ListenAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-stop:
		slog.Info("shutdown signal received")
	case err := <-serverErr:
		slog.Error("server error", slog.String("err", err.Error()))
		os.Exit(1)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", slog.String("err", err.Error()))
	}

	// Drain queued emails so an in-flight notification isn't lost on a clean
	// shutdown. Bounded so a stuck SMTP server can't hang exit.
	if emailQueue != nil {
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
		emailQueue.Shutdown(drainCtx)
		drainCancel()
	}
}

// publicPageAdapter exposes the narrow page-lookup surface the
// custom-domain public renderer needs. The full page.Store has a
// PageFilter-based List; the adapter materialises a per-space slice
// from it without leaking the filter type into customdomain.
type publicPageAdapter struct{ pages *page.Store }

func (a *publicPageAdapter) GetByID(ctx context.Context, id string) (*model.Page, error) {
	return a.pages.GetByID(ctx, id)
}
func (a *publicPageAdapter) GetBySlug(ctx context.Context, spaceID, slug string) (*model.Page, error) {
	return a.pages.GetBySlug(ctx, spaceID, slug)
}
func (a *publicPageAdapter) ListBySpace(ctx context.Context, spaceID string) ([]model.Page, error) {
	return a.pages.List(ctx, page.PageFilter{SpaceID: spaceID, Limit: 500})
}

// listenHostname extracts the bind hostname from a listen address.
// "0.0.0.0:4000" → "0.0.0.0", ":4000" → "" (matches isLocalHost
// in the customdomain middleware via the empty-host shortcut).
func listenHostname(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i]
	}
	return addr
}

// metricsMiddleware records request count + latency. Cardinality is
// bounded to chi's RoutePattern() helper — never raw URL paths,
// which would explode the time-series count.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		path := chi.RouteContext(r.Context()).RoutePattern()
		if path == "" {
			path = "unknown"
		}
		metrics.APIRequests.WithLabelValues(r.Method, path, strconv.Itoa(ww.Status())).Inc()
		metrics.APILatency.WithLabelValues(r.Method, path).Observe(time.Since(start).Seconds())
	})
}
