// Command docs is the Talyvor Docs API server.
//
// HTTP server on :4000 (configurable via DOCS_LISTEN_ADDR) serving
// spaces / pages / blocks / comments / search. SIGTERM triggers a
// graceful drain before closing the DB pool.
//
// Subcommands:
//
//	docs            serve (default)
//	docs serve      serve, explicitly
//	docs migrate    apply pending schema migrations, then exit
//
// The server also applies pending migrations on boot (fail-closed), so a plain
// `docs` is self-sufficient. `docs migrate` exists for deployments that run schema
// changes as a separate step (a k8s initContainer / migration job) ahead of rollout.
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
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/block"
	"github.com/talyvor/docs/internal/bodylimit"
	"github.com/talyvor/docs/internal/changelog"
	"github.com/talyvor/docs/internal/collab"
	"github.com/talyvor/docs/internal/comment"
	"github.com/talyvor/docs/internal/config"
	"github.com/talyvor/docs/internal/customdomain"
	"github.com/talyvor/docs/internal/database"
	"github.com/talyvor/docs/internal/db"
	"github.com/talyvor/docs/internal/dbhealth"
	"github.com/talyvor/docs/internal/export"
	"github.com/talyvor/docs/internal/freshness"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/importer"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/mcp"
	"github.com/talyvor/docs/internal/membership"
	"github.com/talyvor/docs/internal/metrics"
	"github.com/talyvor/docs/internal/migrate"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelink"
	"github.com/talyvor/docs/internal/pagelock"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/ratelimit"
	"github.com/talyvor/docs/internal/search"
	"github.com/talyvor/docs/internal/sharing"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/templatelib"
	"github.com/talyvor/docs/internal/trackintegration"
	"github.com/talyvor/docs/migrations"
)

// subcommand returns the requested subcommand, defaulting to "serve" so a bare
// `docs` (and every existing Dockerfile ENTRYPOINT) keeps working unchanged.
func subcommand() string {
	if len(os.Args) < 2 {
		return "serve"
	}
	return os.Args[1]
}

// runMigrate applies pending migrations and exits. It reads DOCS_DATABASE_URL
// directly rather than going through config.Load(): a migration job needs a database
// and nothing else, and should not be made to carry GATEWAY_AUTH_SECRET (which
// config.Load correctly requires of the SERVER, whose every /v1 identity depends on
// it). Exits non-zero on any failure — a migration step that fails quietly is worse
// than one that fails loudly.
func runMigrate() {
	dsn := os.Getenv("DOCS_DATABASE_URL")
	if dsn == "" {
		slog.Error("migrate: DOCS_DATABASE_URL is required")
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := db.New(ctx, dsn)
	if err != nil {
		slog.Error("migrate: db init failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
	defer pool.Close()

	applied, err := migrate.Apply(ctx, pool, migrations.FS)
	if err != nil {
		slog.Error("migrate: failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
	if len(applied) == 0 {
		slog.Info("migrate: already up to date")
		return
	}
	slog.Info("migrate: applied",
		slog.Int("count", len(applied)),
		slog.String("versions", strings.Join(applied, ",")))
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	switch sub := subcommand(); sub {
	case "migrate":
		runMigrate()
		return
	case "serve":
		// fall through to the server below
	default:
		slog.Error("unknown subcommand", slog.String("subcommand", sub),
			slog.String("usage", "docs [serve|migrate]"))
		os.Exit(2)
	}

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

	// Apply pending migrations before serving a single request. FAIL-CLOSED: a server
	// running against a schema it cannot verify is how you get silent data corruption,
	// so a migration error is a boot failure, not a warning. Concurrent replicas
	// serialise on the runner's advisory lock, and a re-run is a no-op.
	appliedVersions, err := migrate.Apply(ctx, pool, migrations.FS)
	if err != nil {
		slog.Error("migrations failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
	if len(appliedVersions) > 0 {
		slog.Info("migrations applied",
			slog.Int("count", len(appliedVersions)),
			slog.String("versions", strings.Join(appliedVersions, ",")))
	} else {
		slog.Info("migrations up to date")
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
	trackClient := trackintegration.New(cfg.TrackURL, cfg.TrackAPIKey).
		WithMemberSyncSecret(cfg.TrackMemberSyncSecret) // A0b PR-2: dedicated member-sync bearer
	trackHandler := trackintegration.NewHandler(trackClient)
	linkHandler := pagelink.NewHandler(linkStore)
	// A0b PR-2: the syncer also full-pulls each workspace's roster into workspace_members.
	membershipStore := membership.NewStore(pool)
	trackSyncer := trackintegration.NewSyncer(trackClient, pageStore, linkStore, cfg.DefaultWorkspaceID).
		WithMemberSync(trackClient, membershipStore)
	go trackSyncer.Start(ctx, 15*time.Minute)

	// Lens integration. Every AI call routes through here; an empty
	// DOCS_LENS_URL/API key flips IsAvailable() off and the handler
	// returns 503 with a friendly message instead of erroring.
	aiEngine := ai.New(lensClient)
	// Per-workspace LLM rate limiting — the ONLY per-tenant LLM control in this repo (Docs
	// calls Lens on one service key with no balance/quota check anywhere; see BUILD_STATE
	// §0 Q3). Bounds RATE, not cost. Separate limiters because the two surfaces have very
	// different call shapes: AI completions are human-clicked and expensive; semantic search
	// is 300ms-debounced by the client and cheap per call.
	aiLimiter := ratelimit.New(cfg.AIRatePerMin, cfg.AIRateBurst)
	searchLimiter := ratelimit.New(cfg.SearchRatePerMin, cfg.SearchRateBurst)
	slog.Info("llm rate limits",
		slog.Float64("ai_per_min", cfg.AIRatePerMin), slog.Int("ai_burst", cfg.AIRateBurst),
		slog.Float64("search_per_min", cfg.SearchRatePerMin), slog.Int("search_burst", cfg.SearchRateBurst))
	if !aiLimiter.Enabled() || !searchLimiter.Enabled() {
		// Fail-closed limiters deny everything; that is safer than silently unlimited, but an
		// operator should learn it from a boot log rather than a wall of 429s.
		slog.Warn("an LLM rate limiter is configured non-positive and will DENY all calls on that surface",
			slog.Bool("ai_enabled", aiLimiter.Enabled()), slog.Bool("search_enabled", searchLimiter.Enabled()))
	}

	aiHandler := ai.NewHandler(aiEngine, pageStore).WithRateLimit(aiLimiter)

	// Unified search handler. Semantic-side falls back to empty when
	// Lens is unconfigured; full-text always works.
	searchHandler := search.NewHandler(pageStore, semSearch).WithRateLimit(searchLimiter)

	// Freshness engine + 9am-UTC stale-doc digest. Engine reads
	// pages + linked-issue closure to surface "this spec needs a
	// look" badges. The daily digest is currently log-only; future
	// phases will ship Slack / email.
	freshEngine := freshness.New(pageStore, linkStore, trackClient)
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
	mcpServer := mcp.New(pageStore, spaceStore, analyticsStore, aiEngine, freshEngine, "0.1.0").
		WithRateLimit(aiLimiter) // ask_docs reaches Lens; an agent loop calls it far faster than a human clicks

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

	// A3: within-workspace resource access control. The resolvers scope their metadata lookups to the
	// caller's VERIFIED workspaces (GetByIDInWorkspaces) — so a foreign resource resolves to not-found
	// (RequireAccess → 404, composing with the SEC-4 L2 layer), and an in-workspace resource is then
	// gated on the member's grant/role by resolveAccess (403 if under-privileged).
	spaceLooker := func(ctx context.Context, id string) (permission.SpaceMeta, error) {
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.SpaceMeta{}, err
		}
		// WorkspaceID is load-bearing: RequireAccess resolves the caller's member id in
		// THIS workspace (authz.MemberIDForWorkspace) to evaluate access. Omitting it
		// fails closed (no actor → 403), never open.
		return permission.SpaceMeta{WorkspaceID: sp.WorkspaceID, Private: sp.Private, CreatedBy: sp.CreatedBy}, nil
	}
	pageLooker := func(ctx context.Context, id string) (permission.PageMeta, error) {
		pg, err := pageStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, pg.SpaceID, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		return permission.PageMeta{
			WorkspaceID: pg.WorkspaceID,
			SpaceID:     pg.SpaceID, SpaceCreatedBy: sp.CreatedBy, SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
		}, nil
	}
	spaceEnf := permission.NewEnforcer(permStore, permission.SpaceResolverFromParam("spaceID", spaceLooker))
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	// /blocks/{blockID} routes resolve the owning page from the block id (blocks carry page_id).
	blockPageLooker := func(ctx context.Context, blockID string) (string, permission.PageMeta, error) {
		var pageID string
		if err := pool.QueryRow(ctx, `SELECT page_id FROM blocks WHERE id=$1`, blockID).Scan(&pageID); err != nil {
			return "", permission.PageMeta{}, err
		}
		md, err := pageLooker(ctx, pageID)
		if err != nil {
			return "", permission.PageMeta{}, err
		}
		return pageID, md, nil
	}
	blockEnf := permission.NewEnforcer(permStore, permission.PageResolverFromBlock("blockID", blockPageLooker, permStore))

	spaceHandler.WithAccess(spaceEnf)
	pageHandler.WithAccess(pageEnf, spaceEnf)
	commentHandler.WithAccess(pageEnf)
	shareHandler.WithAccess(pageEnf)
	blockHandler.WithAccess(pageEnf, blockEnf)
	lockHandler.WithAccess(pageEnf)
	linkHandler.WithAccess(pageEnf)
	analyticsHandler.WithAccess(pageEnf)
	dbHandler.WithAccess(pageEnf)
	changelogHandler.WithAccess(pageEnf)
	freshHandler.WithAccess(pageEnf)
	exportHandler.WithAccess(pageEnf)
	approvalHandler.WithAccess(pageEnf)
	permHandler.WithAccess(spaceEnf, pageEnf)

	// Collaborative editing engine. The engine is WebSocket-agnostic;
	// the handler layer below upgrades the HTTP request and shuttles
	// frames through the engine's per-client send channels.
	otEngine := collab.NewOTEngine()
	// SEC-4: WithPageScope binds every collab session to the caller's workspace membership.
	collabHandler := collab.NewHandler(otEngine).WithGuard(lockStore).WithPageScope(pageStore)
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

	// LIVENESS: deliberately DB-free. Restarting a process cannot fix a database outage, so
	// making this probe DB-aware would crash-loop every replica during a blip — turning a
	// recoverable incident into a self-inflicted one. "Is this process alive" is all it
	// answers, and that is the correct question for a liveness probe.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	// READINESS: probes the database. This is the signal an orchestrator should use to pull a
	// replica out of the load balancer — before this there was none, so a replica that could
	// not serve a single request stayed in rotation indefinitely while /healthz cheerfully
	// answered {"ok":true}.
	dbChecker := dbhealth.New(pool, 5*time.Second)
	r.Get("/readyz", dbChecker.ReadyHandler())

	r.Handle("/metrics", metrics.Handler())

	// MCP (SEC-4 model b, mirroring Track): behind the SAME gatewayauth + authz chain as /v1.
	// A tool call reaches dispatch only with a valid transit proof + verified identity; the
	// authz chokepoint in callTool then authorizes the acted-on workspace (a JSON-RPC arg, or
	// resolved from the touched object) against the caller's memberships. Agents reach this
	// through the gateway carrying the user's identity — no longer a public no-auth surface.
	r.Group(func(r chi.Router) {
		mcpExempt := func(string) bool { return false }
		r.Use(gatewayauth.Middleware(cfg.GatewayAuthSecret, mcpExempt))
		r.Use(authz.Middleware(authz.NewPGResolver(pool), mcpExempt))
		// JSON-RPC bodies are small; no exemption — nothing under /mcp uploads a file.
		r.Use(bodylimit.Middleware(cfg.MaxBodyBytes, nil))
		r.Use(dbChecker.Middleware()) // clean 503 while PG is unreachable, not a 500

		r.Post("/mcp", mcpServer.HandleRPC)
		r.Get("/mcp/sse", mcpServer.HandleSSE)
	})

	r.Route("/v1", func(r chi.Router) {
		// Bound every /v1 request body. A cap this far above a legitimate payload is a memory
		// guard, not a business rule — before it, PATCH /pages/{id} decoded into a
		// map[string]any and would read a multi-GB body into RAM, write it to Postgres, walk
		// it, and fan it to the semantic indexer, with no container memory limit behind it.
		//
		// The importer's ZIP routes are EXEMPT here and re-capped at their own, larger limit
		// where they mount below. chi middleware composes rather than overrides, so without
		// this exemption the 4MB cap would run first and reject every real Confluence/Notion
		// export regardless of the import cap — see internal/bodylimit's wiring tests.
		r.Use(bodylimit.Middleware(cfg.MaxBodyBytes, func(p string) bool {
			return strings.HasPrefix(p, "/v1/import/")
		}))
		// Degrade cleanly while Postgres is unreachable. Measured pre-fix behaviour was a 500
		// from authz's membership lookup — fast, but the wrong answer: a 500 reads as "this
		// server is broken" (non-retryable) when the truth is "temporarily unavailable, retry".
		// Placed BEFORE authz so the outage answer is one honest 503 rather than whichever
		// query happens to fail first (page.Get, for one, answers 404 on ANY error — telling
		// clients and caches the page was deleted).
		r.Use(dbChecker.Middleware())
		// SEC-4 Layer 1: transit-proof + membership on EVERY /v1 route except the public
		// share viewer (/v1/public/*, which authenticates by its own share token).
		// gatewayauth 401s a request lacking a valid x-gateway-auth BEFORE any identity
		// header is read; authz then resolves the verified x-user-email to workspace
		// memberships (from workspace_members) and puts them in context. Handlers scope every
		// by-id query to that membership set — never to a client-supplied header.
		v1Exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(cfg.GatewayAuthSecret, v1Exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(pool), v1Exempt))

		// Collab WS is now INSIDE the boundary: the gateway proof + membership run on the
		// upgrade request before ServeWS opens a session (chi's Timeout is disabled for
		// hijacked connections, so long-lived sockets are unaffected).
		r.Get("/collab/{pageID}/ws", collabHandler.ServeWS)

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
		// Importer takes Confluence/Notion ZIP exports — far larger than any JSON body, so it
		// gets its own cap (internal/importer already calls 200MB the largest reasonable
		// space export). This re-wraps the body for these two routes only; the /v1 cap above
		// would reject a legitimate import.
		r.Group(func(r chi.Router) {
			r.Use(bodylimit.Middleware(cfg.MaxImportBodyBytes, nil))
			importerHandler.Mount(r)
		})
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
