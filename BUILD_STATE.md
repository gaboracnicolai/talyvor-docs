# BUILD_STATE — talyvor-docs

**Base:** `9bc98b2` (Run 1) → `f238b90` (Run 2) → `52ece7b` (Run 3) · **Branch:** `docs-hardening-d` · **Updated:** 2026-07-16

This file is the honest current state of the repo, written to be trusted when planning
the next run. Where something is thin, it says so. The README is a product pitch and is
**not** a reliable description of what works — this file is.

---

## 0. Run 3 (backend hardening) — phase narrative

**Base `52ece7b`** · branch `docs-hardening-d` · started 2026-07-16. Unattended run.
Scope: backend robustness + cost control. Go only — the frontend is deliberately untouched.

### VERIFY-FIRST recon (done before building; the roadmap's numbers were NOT assumed)

**Q1 — which endpoints reach an LLM? The roadmap said ~5. The real count is higher.**

| Surface | Reaches Lens via | Workspace verified? | Rate limited? |
|---|---|---|---|
| `POST /v1/workspaces/{wsID}/ai/write` | `ai.Engine.WriteWithAI` | ✅ `AuthorizeWorkspace` | ❌ |
| `POST /v1/workspaces/{wsID}/ai/transform` | `Summarize`/`FixGrammar`/`MakeShorter`/`MakeLonger` | ✅ | ❌ |
| `POST /v1/workspaces/{wsID}/ai/translate` | `Translate` | ✅ | ❌ |
| `POST /v1/workspaces/{wsID}/ai/ask` | `AskDocs` | ✅ | ❌ |
| `POST /v1/workspaces/{wsID}/ai/suggest-title` | `SuggestTitle` | ✅ | ❌ |
| `GET /v1/workspaces/{wsID}/search` | `search.SemanticSearch.embed` (**embeddings**) | ✅ | ❌ |
| `POST /mcp` → tool `ask_docs` | `ai.Engine.AskDocs` | ✅ (MCP chokepoint) | ❌ |
| **every page UPDATE** (no route of its own) | `page.Store.Update` → `indexer.IndexPage` → `embed` | n/a (row's own workspace) | ❌ |

So **8 LLM-reaching surfaces, not 5.** The two the roadmap missed matter:

- **Semantic search spends on every query** — `embed(ctx, "query", q)` is a Lens round-trip per search.
- **⭐ Every page save spends** (`page/store.go:471-479`). It is fire-and-forget:
  `go func(){ _ = s.indexer.IndexPage(...) }()` — detached, error discarded. The frontend
  autosaves on a **2-second debounce** and collab's AutoSaver flushes every **5 seconds**, so
  one person typing generates an embedding call every few seconds. This is the largest
  uncontrolled Lens consumer in the product, and **an HTTP-level limiter cannot see it** — by
  the time the goroutine runs, the request is over. See "deferred" below.

**Q2 — existing rate-limit middleware to extend? No. Greenfield.** No limiter anywhere in
`internal/` or `cmd/`; no limiter dependency in `go.mod` (chi ships `middleware.Throttle`,
which is a *concurrency* limiter, not a rate limiter, and is not imported). The only thing
resembling a quota is `customdomain`'s 5-rows-per-workspace cap.

**Q3 — ⭐ HOW LLM COST ACTUALLY FLOWS. This changes the framing, so state it plainly.**

Docs does **not** meter, and is **not** metered per tenant by anything it controls:

- `lensintegration.post` authenticates with `Authorization: Bearer <DOCS_LENS_API_KEY>` —
  **one service key for the entire Docs instance**, not a per-workspace credential.
- The workspace travels as `X-Talyvor-Workspace: <wsID>` — a **label**, not a credential.
  (Post-#23 that label is at least trustworthy: every call site passes a verified workspace.)
- `ai.Engine.run` gates on `IsAvailable()` — *"is Lens configured"* — and nothing else. There
  is **no balance check, no quota, no cost cap** anywhere before the call.
- `pages.ai_cost_usd` is a **report, not a gate**: it is a column populated by
  `trackintegration`'s syncer pulling spend back from Track. Nothing reads it to decide.
- Any Lens failure — including a hypothetical 402/429 from Lens — is flattened to a generic
  `AI_FAILED` 502 (`writeAIErr`). Docs cannot currently tell "Lens refused on budget" from
  "Lens is broken".

**Therefore:** from Docs's side, per-tenant LLM spend is **unbounded**. Whether it is bounded
*at all* depends on Lens enforcing the `X-Talyvor-Workspace` header — a cross-repo property
this repo cannot verify, and one worth confirming in `talyvor-lens`. Note the sharper risk:
**if Lens meters per API key rather than per workspace header, one tenant can exhaust the
whole Docs instance's Lens budget and take AI down for every other tenant** — a
noisy-neighbour outage, not just a billing surprise.

**Framing consequence:** the rate limit added here is **the only per-tenant LLM control that
exists in this repository**. It is a burst/abuse ceiling, not a billing system — it bounds
*rate*, not *cost*. If Lens does meter per workspace, this is defence-in-depth on top; if it
does not, this is the sole control and a real budget cap belongs on the roadmap. Either way
it does not replace economy metering, and BUILD_STATE should not claim it does.

**Q4 — behaviour when Postgres is down.**

- `/healthz` is a hardcoded `{"ok":true}` literal that never touches the pool
  (`cmd/docs/main.go`), so an orchestrator's probe stays green through a total outage.
- **There is no `/readyz`.**
- `internal/db` is 20 lines: `pgxpool.New` + one `Ping`, no health accessor, no retry.
- Handler behaviour on a pool error is inconsistent and was measured, not assumed — see the
  DB-outage capability below for the red-first evidence.

### What Run 3 built (all red-first, real Postgres)

**1. Per-workspace rate limiting on the LLM surfaces** — `internal/ratelimit`, a per-key
token bucket over `golang.org/x/time/rate`. Applied to **7 of the 8** surfaces recon found:
the 5 REST AI routes, the search route, and the MCP `ask_docs` tool (at the chokepoint, not
the `/mcp` endpoint — that is one JSON-RPC door for 10 tools, 9 of which never touch an LLM).

*The key is the VERIFIED workspace.* The middleware runs BEFORE the handler's own
`AuthorizeWorkspace`, so it authorizes `{wsID}` itself and keys on the returned Membership.
Keying on the raw param would have repeated #23's mistake in a new place and given an
attacker two wins: **evasion** (name any workspace for a fresh bucket, forever, by rotating
the string) and **cross-tenant DoS** (hammer `/workspaces/{victim}/ai/write` to drain the
victim's bucket and lock a tenant out of AI without being able to read a byte of their
data). Authorizing before spending a token is what makes the second impossible.
**Mutation-proven**: keying on the raw param fails the suite with *"Alice spent from
workspace B's bucket by naming it in the URL"*.

| Fork | Taken | Alternative, and why not |
|---|---|---|
| Algorithm | **Token bucket** | *Fixed window* — admits 2N across a boundary (N at the end of one window, N at the start of the next), the exact burst this exists to stop. A bucket also fits "write a bit, then idle". |
| Key | **Per-workspace** | *Per-member-per-workspace* — cost is a tenant concern; per-member multiplies the ceiling by headcount. The trade: one heavy user can spend a colleague's allowance — visible and recoverable, unlike a surprise bill. One-line change if the product wants it. |
| Store | **In-memory** | *Shared (Redis)* — needed the day HA lands. Single-replica today, and with N replicas the ceiling becomes N×, degrading toward today's *unlimited* rather than wrongly denying: the safer direction to be wrong in. |
| Limits | **AI 30/min burst 10; search 240/min burst 40** | *One limit for both* — would break Cmd+K. Sized from measured behaviour: the frontend debounces search at 300ms and `type=all` is the default, so one typist drives ~200 embeddings/min. Both env-configurable. |
| Misconfig | **Fail closed** on a non-positive rate; **default** on a malformed env value | A silent no-op limiter is the fail-open shape this repo has been burned by. But a typo in an env var must not be an outage, hence the default. Boot logs the effective limits and warns if a surface is denying. |

Buckets evict after 10 minutes idle (an unbounded map is its own robustness bug); an evicted
key returns *full*, which can only ever be more permissive, never less.

**2. Request body caps** — `internal/bodylimit`, two layers. A `Content-Length` check → 413
before the handler allocates (every ordinary client declares one, and it is the only path
that can give a clean status), plus `http.MaxBytesReader` for clients that omit or
understate it — the read simply stops, the handler's decode-error path answers 400, and the
*security* property (memory is bounded) holds. Making that case 413 too would mean touching
the error handling of every decode site; the honest trade is a slightly wrong status on a
path only a hostile or exotic client takes. 4MB default (a 4MB page is ~4M characters),
200MB for imports.

*A wiring bug a test caught and reading would not have:* chi middleware **composes**, it does
not override. `r.Use(4MB)` on `/v1` plus `r.Use(200MB)` on an importer sub-group runs the 4MB
cap **first** and rejects every legitimate Confluence/Notion export — the bigger cap never
gets a say. Hence the exempt predicate (mirroring `gatewayauth`'s convention), pinned by a
test asserting "over the normal cap but under the import cap must be ACCEPTED".

**3. DB-outage degradation** — `internal/dbhealth`. Behaviour was **measured**, not assumed
(a throwaway probe drove real requests at a pool pointed to a closed port):

> `GET /v1/workspaces/{ws}/pages/stale` → **500 in ~0ms**. `GET /v1/spaces/{s}/pages/{p}` → **500 in ~0ms**.

So Docs did **not** panic and did **not** hang — `authz.Middleware`'s `workspace_members`
lookup fails first and answers 500. Better than the roadmap feared. What was wrong was the
*shape*: a 500 reads as "this server has a bug" (non-retryable) when the truth was
"temporarily unavailable, retry" — and the status depended on whichever query failed first
(`page.Get` answers **404 on any error**, telling clients and caches the page was *deleted*).
Now: `/readyz` probes the DB (200/503), and a cached-probe middleware gives one honest,
retryable 503 on `/v1` and `/mcp`, mounted **before** authz. Note the fast failure is
specific to a *refused* connection; a blackholed network would block until pgx's connect
timeout on every request — which is where a hang would come from, and why the short-circuit
matters rather than letting each handler discover the outage itself.

*Fork — `/healthz` stays DB-free.* Liveness answers "should this process be restarted", and
restarting cannot fix a database outage: wiring the DB into `/healthz` would crash-loop every
replica during a blip, turning a recoverable incident into a self-inflicted outage. The
alternative — one DB-aware `/healthz`, which is what compose already polls — is simpler and
is the dangerous one. Readiness is the endpoint that should go false, and now does.

**4. The `{pageID}` vs `{id}` scoping bug (deferred twice) — closed.** Different class from
#23: nothing is forged. The caller authenticates honestly and the route authorizes honestly,
then the store acts on a *different* resource. `assertInWorkspaces(commentID, wsIDs)` asked
only "is this comment's page somewhere in the caller's tenant", so `{pageID}` chose the
permission check and `{id}` chose the victim. RED proved Mallory **resolved a comment on a
private page she cannot read** (200) and **replied into its thread** (201) by authorizing
against an unrelated public page. Delete survived only via its separate author check —
defence in depth, not scoping. Fixed with `assertInPage` (`AND c.page_id = $2`): the id being
acted on must belong to the resource that was authorized.

**Also fixed, surfaced by a test:** `ai.Engine.IsAvailable` is now nil-receiver safe. `mcp`
holds the engine behind an **interface**, so a nil `*ai.Engine` assigned into it yields a
**non-nil interface** — `if s.deps.ai != nil` read as a nil-guard and was not one, and
`ask_docs` dereferenced a nil receiver. Production never passed nil so it was latent, but a
panicking handler is exactly what this run exists to prevent.

### Security posture — verified not regressed

`internal/gatewayauth`, `internal/authz`, `internal/permission` and `.semgrep/` (including
#23's `ActorOrEmpty` ban) are **byte-untouched** vs `52ece7b`. `frontend/` and
`internal/collab` are untouched per the fence. The only `sec_*` change is two new test files
plus `page/sec4_scoping_test.go`, updated because the comment store's signature got stricter
— with a new assert that the right workspace and the WRONG page is now a not-found. Rate
limit keys come from the verified context, mirroring #23's discipline.

### Deferred (with reasons)

- **⭐ The 8th LLM surface: the async page-save indexer.** `page/store.go` fires
  `go func(){ _ = s.indexer.IndexPage(...) }()` on every Update — detached, error discarded.
  With the frontend's 2s autosave debounce and collab's 5s flush, one person typing spends an
  embedding call every few seconds. **It is the largest uncontrolled Lens consumer in the
  product** and an HTTP limiter structurally cannot reach it: the request is over before the
  goroutine runs. Three options, none free, all needing a product call rather than a blind
  pick: (a) limit at the Lens *client* seam, which silently drops embeddings and degrades
  search quality invisibly; (b) debounce/coalesce per page, which needs a scheduler; (c) rate
  limit the page-save route, which throttles *editing* to control an *embedding* cost. Left
  for a run that can decide it.
- **`internal/importer`'s `maxUploadBytes` still caps nothing** — it is passed to
  `ParseMultipartForm`, whose argument is `maxMemory`, not a limit. The route is now bounded
  from the outside by `bodylimit`, so the exposure is closed; the misleading constant is the
  importer's own to fix.
- **Whether Lens meters the `X-Talyvor-Workspace` header** is a cross-repo question worth
  confirming in `talyvor-lens` — it decides whether this limiter is defence-in-depth or the
  sole control (§0 Q3).

## 0b. Run 4 (frontend URL router) — phase narrative

**Base `7583d2a`** · branch `docs-frontend-router` · started 2026-07-16. **SUPERVISED**
(human at the keyboard). Frontend only — the Go backend must stay byte-identical.

### VERIFY-FIRST recon (the real state on 7583d2a)

**There is no router. Navigation is a `useState` discriminated union.** `App.tsx` holds a
`Route` union (`home | space | page | analytics | ...`) in component state and swaps views
by calling `setRoute`. The only real URL handling is `/s/:token` (the public share viewer),
matched by a regex against `window.location.pathname`. Consequences, all live today: you
cannot deep-link to a page or space, the browser Back/Forward buttons do nothing, and a
refresh always lands on Home. `App.tsx:18-21` says so itself — *"Phase 2 doesn't ship a
URL-bound router … mirrors what a TanStack Router migration will look like in Phase 3."*

**The stack was PREPARED for a router; the router was just never built:**

- `nginx.conf` already has SPA fallback — `try_files $uri $uri/ /index.html`, and its comment
  names `/spaces/:id, /s/:token`. So deep-link *refresh* will work in production without any
  infra change, and Vite's dev server does the same automatically.
- `PageView`'s `pushRecentPage` already stamps `url: /spaces/${space.id}/pages/${page.id}` —
  **the URL scheme is already implied by the code.**
- `useSpace(id)` (a single-space fetch) already exists in `hooks/useSpaces.ts`, and
  `workspaceID()` is a global localStorage helper — so a URL carrying only IDs can resolve
  the full objects from queries. The route components don't need to thread `Space` objects
  through the URL.

**Stack (recon, not assumed):** React 18 + Vite 6 + TypeScript (strict, `noUnusedLocals`/
`noUnusedParameters` on), Zustand + TanStack Query. **No routing library. No test framework
at all** (no vitest/jest/testing-library/playwright). 9.8k LOC.

**Error + auth contract (decides the guard):** `api/client.ts`'s `apiRequest` throws
`APIError` with a numeric `.status` (404/403) on non-2xx, falls back to IndexedDB on network
failure (or throws `APIError(status 0, code OFFLINE)`), and authenticates with
`Authorization: Bearer <docs_api_key>` from localStorage. **The router will not touch any of
this** — it renders query results and never sets, derives, or assumes auth. That is the
tenancy-story guarantee the run requires.

### Forks

- **Router library: `react-router-dom` v7 (7.18.1), not TanStack Router.** The code comment
  anticipated TanStack Router, but the instruction is to use the *standard* React-SPA router
  and take the conservative-reversible choice. react-router-dom is the de-facto standard,
  interoperates cleanly with the existing TanStack Query (they are independent concerns), and
  is the lower-risk migration. TanStack Router's selling points (typed params + loaders) don't
  outweigh the churn/risk here. Reversible: the route table is small and isolated.
- **Adapter wrappers over a full rewrite.** The leaf components (Sidebar, Home, SpaceView,
  StalePages, …) currently take `onOpenX` callbacks. Rather than rewrite each to use router
  hooks, thin route-level wrappers own `useParams()`/`useNavigate()` and pass navigate-backed
  callbacks down — the existing components keep their interfaces. Minimal churn, maximal
  reversibility; documented so a later pass can push hooks into the leaves if desired.

### The testing boundary (the honesty line this run turns on)

**Tested-logic (genuinely correct headless — vitest, added this run):**
- Pure path builders (`paths.*`): navigation correctness = building the right URL.
- The security guard `resourceState(query)`: maps a resource fetch to a view state, and
  **collapses 403 and 404 to the same `notfound`** so the router creates no existence oracle.
  Red-first.
- Via `createMemoryRouter` + jsdom (react-router's own testing tool): route→component
  mapping, and that a 403-erroring and a 404-erroring resource render the *identical*
  not-found UI. In-memory history (`router.navigate(-1)`) is real logic and is tested.

**Click-to-verify (NOT honestly testable headless — reported, with manual steps):**
- Real browser Back/Forward buttons + address-bar sync (jsdom history is not the real thing).
- Refresh-on-a-deep-URL landing on the right view (needs the dev server's SPA fallback + a
  real reload).
- End-to-end: a genuine cross-tenant page URL showing not-found against the live Go backend.

A test that SKIPS to look green would be the green-by-absence failure this session already
cured once; it will not be written here.

### What Run 4 built

`react-router-dom` v7 replaces the `useState` route machine. App.tsx is now
`createBrowserRouter(routes)`; `src/router/` holds the table, a chrome `Layout` with an
`<Outlet/>`, pure path builders, and the resource guard. Pages and spaces have real,
shareable, refreshable URLs; the public `/s/:token` viewer is a top-level route with no
chrome. Leaf components are unchanged — route wrappers translate URL↔their existing
callbacks (the reversible adapter fork).

**Route guard approach (the security-adjacent part).** The frontend holds no authorization
knowledge and invents none: a resource route fetches its target through the existing query
hooks and `resourceState(query)` maps the result to a view. The one rule layered on top:
**403 and 404 collapse to one indistinguishable `notfound`**, with generic copy ("doesn't
exist, or isn't available to you") — so a caller rotating ids in the URL cannot tell a real
resource they can't see from one that isn't there. This mirrors the server, which already
404s cross-tenant and never leaks existence. The Bearer token is never read, set, or assumed
by the router. `offline` (status 0) and `error` (5xx) are distinct because neither is an
existence signal; anything unclassifiable fails safe to `notfound`, never to `ready`.

### The testing boundary — tested-logic vs click-to-verify

**Genuinely tested headless (vitest, 30 tests / 6 files, 0 skipped, gated in CI):**

- `paths.*` builders — navigation correctness is building the right URL (`paths.test.ts`).
- `resourceState` — the guard decision, incl. **403 === 404** (`guard.test.ts`).
- The **real** route table via `matchRoutes` — every URL maps to its intended route, params
  extract, `/s/:token` sits outside the chrome, unknown in-app URLs hit the in-chrome
  catch-all (`routes.test.tsx`).
- Render-level no-oracle — a 403-erroring page route renders **byte-identical DOM** to a 404
  and never the page; NotFound copy contains none of forbidden/denied/permission/403
  (`guard-render.test.tsx`).
- In-memory history over the real table — forward, Back, Forward, and landing directly on a
  deep URL (`history.test.tsx`).
- Real-Layout mount smoke — the actual chrome renders under the router with no backend,
  without throwing (`layout-smoke.test.tsx`).

**Click-to-verify — NOT honestly assertable headless (jsdom's history/URL bar aren't the
real thing). Manual steps for a reviewer with the app running:**

1. **Deep-link + refresh.** `cd frontend && npm run build && npm run preview`; open
   `http://localhost:<port>/spaces/<id>/pages/<id>` directly (or open any page, then hit
   browser Reload). *Expect:* the app lands on that page, not Home. (Server side is already
   confirmed — `preview` returns the SPA `index.html` with HTTP 200 for a deep URL, and
   nginx.conf has the same `try_files` fallback for prod.)
2. **Back/Forward buttons + address bar.** Navigate Home → a space → a page via the sidebar.
   *Expect:* the address bar updates at each step; the browser Back button returns to the
   space then Home; Forward re-advances. (The history *stack* is tested in memory; the
   physical buttons + URL bar are the browser-chrome part.)
3. **Guard against the live backend.** Open a page URL whose id belongs to another
   workspace. *Expect:* the generic "Not found" — identical to a made-up id — never a page
   and never an "access denied".

### ⚠️ The wall I stopped at (reported, not worked around)

Steps 1–2 above are verifiable against `npm run preview` with **no backend** (routing is
client-side; data shows loading/empty). **Step 3 — the guard end-to-end against real
cross-tenant data — is blocked by a pre-existing wall this run does not own:** the SPA
authenticates with `Authorization: Bearer <docs_api_key>` from localStorage, but the Go
backend expects the edge gateway to inject `X-Gateway-Auth` + `X-User-Email`; `vite dev`
proxies straight to `:4000`, so requests 401 and no real data loads without the gateway in
front. That gateway is a separate repo/infra concern, out of a frontend-only run. So the
guard's *logic* and *render* are proven headless (403≡404, byte-identical DOM), but a live
cross-tenant click-through needs either the gateway wired locally or a seeded dev auth path —
a decision for the reviewer, flagged rather than hacked around.

### Deferred / not done (scope)

- The leaf components still use callback props (the adapter fork); pushing router hooks into
  them is a later, optional cleanup.
- No `<Link>`-ification of every in-app navigation — the sidebar/home already navigate via
  the wrappers; converting remaining `onClick` handlers to `<a href>` for middle-click/copy
  is incremental polish, not required for addressability.

## 0c. Run 5 (stale-service-worker navigation hazard) — phase narrative

**Base `90ccd84`** · branch `docs-sw-safety` · started 2026-07-16. **SUPERVISED**, frontend-only.
Fixes the hazard the #25 router browser-test surfaced: a stale SW hijacked client-side router
navigation (URL jumped to `/domains`, page reset to `about:blank`) until manually unregistered.

### VERIFY-FIRST — the actual SW setup on 90ccd84

- **Hand-written** `frontend/public/sw.js` (no vite-plugin-pwa, no workbox). Vite copies
  `public/*` to `dist/` verbatim → served at `/sw.js`. Registered fire-and-forget in
  `main.tsx` (`import.meta.env.PROD` only; no update handling anywhere in the app).
- Two caches: `talyvor-static-v1` (cache-first for the shell + `/assets/*`), `talyvor-api-v1`
  (network-first for cacheable `/v1` GETs). Writes never cached (IndexedDB write-queue does
  offline replay). `install` → `skipWaiting()`; `activate` → delete caches whose names differ
  from the two constants, then `clients.claim()`.
- **Offline is a real feature, not incidental** — OfflineIndicator, OfflineSettings, offlinedb
  (IndexedDB), sync.ts write-queue, client.ts offline fallback. The fix MUST preserve it.

### ROOT CAUSE (named before any fix)

The SW applies **cache-FIRST to the mutable app shell** — `/` and `/index.html`
(`sw.js` fetch handler: `if (pathname === "/" || "/index.html" || startsWith("/assets/")) →
cacheFirst`). `cacheFirst` is `hit = cache.match(req); if (hit) return hit;` — it **never
revalidates**. Three facts turn that into a deploy hazard:

1. `index.html` is **not content-hashed** (always `/`); it is the file that pulls in the
   hashed JS/CSS bundles. Caching it cache-first pins the whole app version.
2. The cache name is hardcoded `…-v1` and **never bumps**; `activate` only deletes caches whose
   names differ from the current constants, so with a static name **nothing is ever purged**.
3. `public/sw.js` is **byte-static across deploys** (nobody edits it) → the browser installs no
   new SW → the old SW + old cache keep serving.

**Net effect after a new deploy:** a returning user whose SW already cached `/` is served the
**stale `index.html` forever** (cache-first, no revalidation), which references the **old
hashed bundle** (also cache-first cached) — so they run a **stale app version** indefinitely,
mismatched against the freshly-deployed server (new router routes, new asset hashes, new API
shapes). That stale-shell-vs-fresh-everything mismatch is what manifested as the erratic
client-side routing (`/domains` self-navigation, `about:blank`) in the #25 browser test, and
cleared only when the SW was unregistered.

**Crucially, the activation lifecycle (`skipWaiting` + `clientsClaim`) is NOT the bug** — it is
the correct prompt-handover behaviour. The bug is the **caching STRATEGY on the mutable shell**
plus the **unversioned cache**. (This is why a cargo-culted "add skipWaiting" fix would miss it
— it is already there.)

### FIX → MECHANISM mapping (root-cause driven)

| Mechanism | Fix |
|---|---|
| cache-first serves a stale shell forever | **Shell → network-FIRST** (fresh when online; cache fallback only offline). THE primary fix. `/assets/*` STAY cache-first (immutable, hashed — correct). `/v1/*` stays network-first. Offline preserved. |
| unversioned cache never purged | **Bump `v1`→`v2`** so the fixed SW's `activate` purges the poisoned `talyvor-static-v1` once, healing existing stale installs. |
| an already-open tab keeps the old bundle after the new SW takes over | **App-side loop-safe update→reload** (`src/sw/register.ts`): reload ONCE when a NEW SW controls a PREVIOUSLY-controlled page — guarded against the classic `skipWaiting` reload loop and against reloading on first-ever install. |
| (not the bug) skipWaiting/clientsClaim, `/assets` cache-first, `/v1` network-first, offline fallback | **UNCHANGED.** |

Because the shell is now network-first, future deploys **self-propagate even without editing
`sw.js`** (the shell revalidates on every online load) — the hazard cannot recur.

### Test plan (untested-UI layer — same honesty rules as #25)

- **Unit-tested, in CI** (the #25 run wired `npm test` into CI's frontend job): (a) the REAL
  `public/sw.js` loaded into a mocked ServiceWorker scope — assert a navigate to `/` is served
  network-first (fresh over stale-cache), `/assets/*` cache-first, `/v1/search` skipped, and
  `activate` purges the old cache version; (b) the pure update-decision in `register.ts`
  (reload only on a real update, never first install, never twice → no loop). Red-first.
- **Browser-demonstrated (Playwright):** build v1 → load → SW installs; build v2 (visible
  change) → reload → new version takes over, deep-link + sidebar nav still work, no
  `/domains` hijack, no reload loop.
- **Fork:** keep `sw.js` hand-written in `public/` (ships as-is; no new build pipeline or
  module-SW browser caveats) and test the REAL file via a scope harness — over
  building the SW from a shared TS module. Lower-risk, reversible, zero drift (the test
  exercises the shipped bytes).

### What Run 5 built + how it was verified

**The fix (`frontend/public/sw.js` + `frontend/src/sw/register.ts`):**
- **App shell (`/`, `/index.html`) → network-first** (was cache-first). Fresh index.html when
  online → current bundle; cached shell is the OFFLINE fallback only. `/assets/*` stay
  cache-first (immutable). This is the root-cause fix: the shell no longer pins a stale app
  version, so a deploy is immediately visible to returning users.
- **Cache `v1`→`v2`** so the fixed SW's `activate` purges the poisoned `talyvor-static-v1`
  once (heals existing stale installs). Because the shell revalidates every online load,
  future deploys self-propagate without another bump.
- **`skipWaiting` + `clientsClaim` kept** — the correct prompt handover, never the bug.
- **App-side loop-safe update→reload** (`registerServiceWorker`): reload the open tab once
  when a NEW worker takes over an ALREADY-controlled page; never on first install, never
  twice. `main.tsx` calls it instead of the fire-and-forget block.
- **Offline preserved** — network-first falls back to the cached shell so the SPA mounts
  offline; the IndexedDB write-queue / OfflineIndicator path is untouched.

**Unit-tested (vitest, in CI's frontend job — 15 new tests, 0 skipped, red-first):**
- `src/sw/sw.test.ts` loads the **real** `public/sw.js` into a mocked ServiceWorker scope
  (no drift — the shipped bytes) and pins the behaviour by the exact discriminator (does the
  handler consult the network when the shell is cached?): shell **network-first** (fetches
  fresh over a stale cached `/`), offline **falls back to cache**, `/assets/*` **cache-first**,
  deep routes **pass through**, real-time **skipped**, `activate` **purges old versions**. 4 of
  these failed on the cache-first shell and pass on the fix.
- `src/sw/register.test.ts` pins the reload decision (only on a real update, never first
  install, never twice → no loop) and the registration wiring.

**Browser-demonstrated (Playwright Chromium, prod `vite preview`, objective signal = the
bundle hash `index.html` references):**
- Loaded v1 → the fixed SW installed and **controlled** the page; cache name was
  `talyvor-api-v2` (version bump live). Bundle: `index-DJVsh331.js`.
- **Deployed v2** (a visible change → new bundle hash `index-nVnWAC8H.js`), reloaded. The SW
  served the **fresh v2 shell** — `index.html` referenced the **new** hash and the v2 change
  was visible — **not** the stale v1 shell. This is the exact inverse of the original bug,
  end-to-end in a real browser.
- **Routing intact, no hijack:** deep-link to `/spaces/space-x/pages/page-y` mounted the page
  route (breadcrumb "Space / Page"), `location.pathname` was the deep URL, **not** `/domains`;
  a sidebar click navigated client-side to `/templates` (a `window` marker survived → real
  pushState, no reload). SW controlling throughout; bundle stable (no reload loop).

**Left as unit-tested-not-browser-demonstrated (honest boundary):** the loop-safe auto-reload
FIRING once in a real browser requires a `sw.js`-CHANGED deploy (byte-different SW → new
install → `controllerchange`). The browser demo above was an app-only deploy (sw.js
byte-identical), so no new SW installed and the auto-reload path didn't fire — correctly, and
the page stayed stable. The reload path's loop-safety is covered by `register.test.ts`
(reload-once, never-first-install); demonstrating it in a live browser is the one piece that
would need a second SW-changed deploy, and SW-install timing under Playwright was the flaky
surface in the #25 run — so it is reported here rather than flaked or faked.

**Fork (BUILD_STATE §0c):** keep `sw.js` hand-written in `public/` and test the REAL file via
a scope harness, over building the SW from a shared TS module. Lower-risk, reversible, zero
drift — the test exercises the deployed bytes.

## 0d. Run 6 (page-save indexer throttle) — phase narrative

**Base `1b576c8`** · branch `docs-pagesave-indexer-throttle` · 2026-07-16. Backend-only.
Throttles the largest uncontrolled Lens consumer: the async page-save embed indexer.

### VERIFY-FIRST — the exact path

- **Goroutine spawn site:** `internal/page/store.go:475-479`, inside `Store.Update` (only
  `Update` indexes — `Create` does not): `go func(){ _ = s.indexer.IndexPage(ctx, pageID,
  workspaceID, text) }()`. One goroutine per save, no coalescing, no concurrency bound, no
  rate limit; the HTTP rate limiter can't see this internal path.
- **The seam:** `searchIndexer.IndexPage(ctx, pageID, workspaceID, text)` (page/store.go:52).
- **The Lens call:** `SemanticSearch.IndexPage` → `embed()` (`internal/search/semantic.go`),
  which POSTs to Lens with `Authorization: Bearer` + `X-Talyvor-Feature: docs-search`.
- **⭐ Workspace-label finding:** the embed call does **NOT** currently send
  `X-Talyvor-Workspace` — `IndexPage(ctx, pageID, _ /*workspaceID*/, text)` and `embed(ctx,
  _, text)` **ignore the workspaceID**. So there was no label here to "not break"; the label
  SEAM is the `workspaceID` argument threaded to `IndexPage`, which the throttle preserves.
  Adding the workspace header to `embed()` would be a metering/Lens-coordination change —
  out of scope for a throttling run; flagged, not done.
- `IndexAllPages` (a bulk backfill that also calls `IndexPage`) is **unwired** (no caller) —
  not a live path, out of scope.

### THE BUILD — `internal/pageindex.Throttle`

Implements the `IndexPage(ctx, pageID, workspaceID, text)` signature, so `WithIndexer`
accepts it; delegates the real embed to `SemanticSearch`, passing `workspaceID` through
unchanged. `main.go` wraps the indexer; the store's per-save goroutine now just ENQUEUES
(returns immediately) and the throttle owns concurrency/rate/coalescing. `page/store.go` is
**byte-untouched** (only the `main.go` wiring swapped the indexer).

Four properties, each tested with `-race`, in `internal/pageindex` (Go tests → they run in
CI's `test` job — the Go equivalent of "the vitest suite that gates"; no `t.Skip`, no build
tags, no DB needed → 0 skips):

1. **COALESCING (latest-wins).** RED→GREEN, strict (build-fail, then a naive embed-immediately
   impl fails "exactly 1"): 10 rapid saves to one page → **1** embed carrying the **final**
   content. `TestCoalescing`.
2. **NEVER-DROP-FINAL.** A save arriving during an in-flight embed is re-embedded afterwards.
   Proven under coalescing (`TestNeverDrop_SaveDuringInflight`) AND under worker-pool
   backpressure (`TestNeverDrop_UnderPoolBackpressure`: single worker blocked, a newer save
   for the in-flight page + fresh pages queued behind it → every page's FINAL content still
   embeds; the in-flight page embeds exactly twice, v1 then v2). A dropped embed = stale
   search = a silent data-visibility bug, so this is the load-bearing invariant.
3. **BOUNDED POOL.** N workers → concurrency never exceeds pool size; a 20-page burst against
   a pool of 3 stays ≤3 concurrent and all 20 embed. **Mutation-proven**: unbounding the pool
   (goroutine-per-item) fails the assertion.
4. **RATE CAP.** Token bucket (`golang.org/x/time/rate`) paces the total embed call rate —
   consumer-side, independent of how Lens meters. 6 embeds at 20/s span ≥ ~200ms.
   **Mutation-proven**: disabling the limiter → 6 embeds in ~5ms → the pacing assertion fails.

**STALENESS** is the coalescing window and the config knob for max save→searchable delay: a
page's first save schedules its embed one window out; saves within update the pending content
without extending the deadline (no starvation), so the final state embeds within ~staleness
under normal load (`TestStaleness`).

### Config (env-overridable; malformed → default)

| Env | Default | Meaning |
|---|---|---|
| `DOCS_INDEX_WORKERS` | 4 | concurrent embed workers |
| `DOCS_INDEX_RATE_PER_MIN` | 300 | embed call-rate ceiling; **<=0 = unlimited** (still pool-bounded) |
| `DOCS_INDEX_RATE_BURST` | 10 | token-bucket burst |
| `DOCS_INDEX_STALENESS_SEC` | 5 | coalescing window / max save→searchable delay under normal load |

**Sizing:** with coalescing, one editor's autosaves collapse to ~1 embed per staleness window
(~12/min at 5s), so 300/min allows ~25 concurrent active editors before pacing — generous,
still a ceiling. **Fork:** unlike the AI/search HTTP limiters (which fail CLOSED for security),
a non-positive index rate means UNLIMITED and degrades to the pool bound — this is a
throughput throttle, not a security gate, so a misconfig must not wall off all indexing.

### Forks & guards

- **Fork — wrap at the seam, leave `page/store.go` untouched.** The store's per-save goroutine
  now enqueues into the throttle (O(1), returns instantly). A transient enqueue-goroutine per
  save remains, but it does no Lens work and exits immediately; the *embed* concurrency (the
  resource concern) is pool-bounded. Most conservative/reversible (revert = swap the indexer
  back). Removing the store's `go func` is a trivial follow-up, not required.
- **Fork — rate cap in the throttle**, which is the per-save "internal goroutine path" the
  brief names. `IndexAllPages` (unwired bulk) is not covered; capping it would touch
  `internal/search` and is a documented follow-up.
- Guards (by diff vs `1b576c8`): only `cmd/docs/main.go`, `internal/config`, and
  `internal/pageindex` changed. **Byte-untouched:** the money path (no ledger/supply/mint/
  balance tokens added — throttling, not minting), `internal/search` + `internal/lensintegration`
  (the embed/Lens seam, `X-Talyvor-Feature` label preserved), `internal/page/store.go`, the
  SW fix, the URL router, the route guards, `.semgrep`, and `migrations`.

### Deferred (with reasons)

- **The embed call sends no `X-Talyvor-Workspace` header** (workspaceID ignored in
  `embed()`). Adding it would let Lens meter Docs embeds per workspace — but that's a
  metering/Lens-coordination change, out of a throttling run. The throttle preserves the
  workspaceID argument so the seam is ready. Flagged for a watched money/metering run.
- `IndexAllPages` (unwired bulk backfill) is not throttled; wire + cap it if/when it gets a
  route.

## 0e. Run 7 (per-workspace Lens JWT metering) — phase narrative

**Base `57a1f96`** · branch `docs-perworkspace-jwt-metering` · started 2026-07-16. Watched run.
Scope: the credential seam only. Stop sending the shared global key on the Lens DATA path;
send a PER-WORKSPACE JWT whose claim = the request's workspace. The Lens side is already
merged + proven (`872f676`): a per-workspace JWT gets its own rate-limit bucket AND
attributes spend to its real workspace; a forged workspace claim is rejected. This is the
Docs half — it closes the "label ≠ credential" gap Run 6 §0d and Run 3 §0 both flagged.

### VERIFY-FIRST recon (read, names exact, changed nothing)

**Mint contract** (proven Lens-side): `POST {LensURL}/v1/auth/token`, `Authorization: Bearer
<admin key>`, body `{"workspace_id","ttl_hours"}` → `201 {"token","expires_at"}`. Referenced
nowhere in Docs before this run. **Docs never parses JWTs** (gatewayauth trusts the edge
gateway to validate them) and go.mod has no JWT lib — so the token is an OPAQUE bearer string
on the Docs side; the provider carries it and tracks its expiry, nothing more.

**Every Lens DATA-path call and where the credential injects** (all bore the global key):

| Call | Function (file:line) | URL | Bearer today | Workspace today | wsID in scope? |
|---|---|---|---|---|---|
| AI completions (Anthropic) | `lensintegration.Client.post` via `Complete*` (`client.go:104-111`) | `/v1/proxy/anthropic/v1/messages` | `Bearer <global key>` (`:109`) | `X-Talyvor-Workspace` header (`:111`) | **yes** (threaded `ai.Engine.run`→`CompleteWithFeature`) |
| AI completions (OpenAI) | same `post` via `CompleteOpenAI` (`client.go:92`) | `/v1/proxy/openai/v1/chat/completions` | same | same | **yes** |
| Embed — index | `SemanticSearch.embed` via `IndexPage` (`semantic.go:239-245`, called `:117`) | `/v1/proxy/openai/v1/embeddings` | `Bearer <global key>` (`:244`) | **none** — `IndexPage(_/*ws*/)` and `embed(_)` both drop it | **yes** at the `IndexPage` seam (throttle threads `e.ws`), currently DROPPED |
| Embed — search | `SemanticSearch.embed` via `Search` (`semantic.go:239-245`, called `:155`) | same embeddings path | `Bearer <global key>` | **none** | **yes** (`Search`'s `workspaceID` param), not passed to `embed` |

The recon's finding, restated: because Lens meters per **key** (the JWT), the
`X-Talyvor-Workspace` header the completions path sends is a LABEL Lens ignores — so all three
paths collapse to the global key's bucket. The fix therefore must convert **all three**, not
just the two embed paths, or completions metering stays broken (and the guard "global key must
never be the data-path bearer after this" would be violated on the completions path).

### Property 1 — per-workspace JWT provider (`internal/lenscreds`) — DONE

`Provider.TokenFor(ctx, workspaceID)`: returns a cached per-workspace bearer, minting via the
contract above with the admin key when absent or within `skew` of expiry. Per-workspace cache
with per-entry locking — concurrent callers for the same cold workspace coalesce onto ONE
mint (safe under the bounded worker pool); different workspaces mint concurrently (map lock
never held across the HTTP call). Admin key used ONLY to mint; on mint failure returns an
error, NEVER the admin key. RED→GREEN + mutation-proven: disabling the cache-hit path makes
cache/refresh/isolation/coalesce mint 2×/2×/3/20× — restored → 1×. `-race` clean.

### Property 2 — wire the embed + search data path to the provider — DONE

`SemanticSearch.embed(ctx, workspaceID, text)` now takes the workspace (was `_`), fetches a
per-workspace bearer from the provider, and sends `Authorization: Bearer <JWT>` — never the
global key. `IndexPage` stops dropping its workspaceID; `Search` passes its own. Dead `apiKey`
field removed; `WithLensCreds`→`WithLensURL` (the data-path credential is now the JWT, not a
key). main.go constructs one shared `lenscreds.Provider` and wires it into `semSearch`.

**Stated fail policy (per the brief; implemented + proven, not silently chosen):**
- **Async index = best-effort.** Mint failure ⇒ `IndexPage` logs and returns nil, writes
  nothing, sends NO data-path request. The page re-indexes on its next save (the pageindex
  throttle re-enqueues on the next Update) — the async "retry" rides the normal save loop; the
  throttle's never-drop machinery is untouched.
- **Sync search = fail-closed.** Mint failure ⇒ `Search` returns `ErrTokenUnavailable`; the
  handler errors the search (500). It does NOT degrade to empty and NEVER falls back to the
  global key. Tradeoff (stated): a Lens-auth outage errors search entirely rather than serving
  full-text — the brief's chosen policy ("error the search").

RED→GREEN: embed carried `Bearer GLOBAL-ADMIN-KEY` → now carries a per-workspace JWT decoding
to the request's workspace. Mutation-proven: flipping the search branch to degrade-instead-of-
fail-closed breaks both fail-closed tests (handler 200 + full-text; Search nil error).

### Property 3 — the decisive two-tenant proof — DONE

`TestTwoTenants_DecisiveEndToEndIsolation`: one `SemanticSearch` (one shared provider) drives
wsA index → wsA search → wsB index → wsB search against a fake Lens that mints per-workspace
JWTs and decodes the bearer's `workspace_id` claim on every data-path call. Proves: the four
data-path calls decode to `[wsA, wsA, wsB, wsB]` (each attributed to the workspace that made
it, no cross-tenant leak); the mint endpoint was called with the admin key, exactly once per
workspace (the tenant's search reused the token its index minted — cache held end-to-end); and
the raw global key never appeared on a data-path call. Mutation-proven decisive: reintroducing
the original global-key bug (`Bearer <global>` on embed) fails this test and both Property-2
tests (claims decode to `""`). With the merged Lens-side proof (per-ws JWT → isolated
rate-limit bucket + isolated COGS), the chain is complete.

### Deferred / forks — see §2/§3 (filled in as the run completes)

## 1. What this run changed

A security-first foundation run, in strict order.

### CI is now a real gate (was green-by-absence)

`testutil.New(t)` **skips** when `DOCS_TEST_DATABASE_URL` is unset, and `ci.yaml` had no
Postgres service and never set that variable. So the entire real-schema suite — every
SEC-4 cross-tenant test — silently skipped on every PR while CI reported green. Measured
before the change: **9 real-PG tests skipped, every package `ok`**.

- `test` job now runs a `pgvector/pgvector:pg16` service and sets `DOCS_TEST_DATABASE_URL`.
  Result: **13 real-PG tests run, 0 skipped**.
- A dedicated **"assert real-PG tests actually ran"** step fails the build if the skip
  count is ever non-zero. Skips do not fail a build on their own, so without this step a
  regression to green-by-absence would be invisible again. Mutation-proven: with the DB,
  13 pass / 0 skip; with the DB removed, 13 skip and the step goes red.
- `semgrep` job added (`semgrep/semgrep:1.165.0`, `--config .semgrep/ --error`). The rules
  had existed since the SEC-4 work and had **never been run by anything**.

### Four cross-tenant routes closed (red-first, real-PG)

Every *by-id* route already scoped correctly to `authz.WorkspaceIDs(ctx)`. These four
took the workspace id from caller-controlled input and passed it straight to the store:

| Route | Was | Now |
|---|---|---|
| `GET /v1/workspaces/{wsID}/pages/search` | 200 + another workspace's page bodies | 403 |
| `GET /v1/workspaces/{wsID}/pages/stale` | 200 + another workspace's pages | 403 |
| `GET /v1/workspaces/{wsID}/spaces` | 200 + another workspace's spaces | 403 |
| `POST /v1/spaces` | **201 — planted an owned space in any workspace** | 403 |

`POST /v1/spaces` was the worst: `workspace_id` **and** `created_by` came from the request
body, and `permission/store.go`'s `resolveAccess` treats a space's creator as its admin —
so it was plant-and-own on any workspace. `created_by` is now taken from the
gateway-verified identity (`AuthorizeWorkspace(...).MemberID`) and the body value is
ignored.

Each fix was driven by a RED real-PG test that first demonstrated the leak with real data,
including the case of a caller with **zero memberships** (`authz.Middleware` proceeds with
an empty membership set rather than 401ing, so these routes had to deny explicitly). Tests:
`internal/page/sec4_workspace_routes_test.go`, `internal/space/sec4_workspace_routes_test.go`.
Every one asserts the caller's **own** workspace still works, so a blanket-deny cannot pass.

### The semgrep rule that structurally could not see the hole

`docs-no-url-param-workspace-scope` had a `paths.include` list equal to exactly the 7
packages SEC-4's secondary sweep touched — so it could not see `page`, `space`, or
`search`, which is where the holes were. A guardrail scoped to "the code we already fixed"
cannot catch the next miss. Now includes `page` + `space` + `search`. Proven both
directions: **fires on the pre-fix code at 9bc98b2 (3 findings), passes on the fixed code
(0)**, and mutation-proven to catch a newly-introduced unguarded route.

### It boots

`config.Load()` fail-closes on `GATEWAY_AUTH_SECRET` (<16 chars → `os.Exit(1)`), but that
variable was absent from `docker-compose.yaml`, `.env.example`, and the `Dockerfile`. The
README's two-command quick start was an infinite crash loop. **The fail-closed check was
not weakened** — the deployment surface was fixed to match it. Verified: `docker compose
up -d` → server listening, `/healthz` 200, `RestartCount=0`.

### Privilege escalation closed: `is_admin` was client-supplied — in TWO places (red-first)

**Instance 1 — `pagelock.Unlock` (lock theft).** It read `is_admin` out of the **request
body** and the store trusted it to bypass "only the locker or an admin can unlock". Two
failures in one:

- **Any Edit-tier member could steal another member's lock** with `{"is_admin": true}`.
- **A real admin who sent no claim was denied** — the body flag was the *only* way to be
  admin, so the override was available to anyone who lied and unavailable to anyone who
  didn't. The RED test proved both halves.

Fixed at the source: `permission.RequireAccess` already computed the caller's true
`AccessLevel` from the gateway-verified identity and **threw it away**. It now carries it
on the request context (`permission.LevelFromContext` / `IsAdminFromContext`), and
`Unlock` reads admin status from there. The `is_admin` field is **gone from the request
struct** — the body is ignored in both directions (an admin sending `is_admin:false` is
still an admin). Fails closed: an unguarded mount yields no level in context → not admin.

Both legitimate paths are covered by tests: the actual locker unlocks, and a real admin
(space creator → `AccessAdmin` on its pages, per `resolveAccess`) unlocks someone else's
lock with no body claim.

**Instance 2 — `page.Update` (edit *through* another member's lock).** Found by the
adversarial sweep below, same field, one layer deeper, and arguably worse: you don't steal
the lock, you just write through it. `page.Handler.Update` does
`json.NewDecoder(r.Body).Decode(&updates)` into a `map[string]any` — **the map is the
request body**. It carefully overwrites `updates["updated_by"]` with the verified id (SEC-4
did that) and never touched `updates["is_admin"]`, which `Store.Update` hands to
`guard.CanEdit`, which returns `true` outright for an admin. So
`PATCH {"is_admin":true,"title":"..."}` wrote through another member's lock.

`page/store.go`'s comment asserted the invariant that was being violated — *"admin-bypass
is communicated via `updates["is_admin"]` (handler-injected, never trusted from request
bodies)"*. No handler injected it. The `delete(updates, "is_admin")` a few lines below was
the tell: it exists **because the key does arrive**, and it made the attack silent by
keeping the flag out of the SQL. The handler now assigns
`updates["is_admin"] = permission.IsAdminFromContext(...)` unconditionally, which both
overwrites any body claim and restores the override for real admins. The comment is now
true. RED proved both halves, exactly as for instance 1.

Note: `frontend/src/hooks/usePageLock.ts` still sends `is_admin` as a **UI-chosen
boolean**. It is now ignored by the server. The frontend may still render an override
affordance that correctly 403s for non-admins — cosmetic, not a security issue.

### Parity bar A: the migration runner

Was: no `migrate` subcommand, no `schema_migrations`, schema applied only by Postgres's
`docker-entrypoint-initdb.d` — which runs **only on first boot of an empty volume**, so an
existing deployment had no upgrade path at all and nothing recorded which version a
database was at.

Now: `internal/migrate` + a `docs migrate` subcommand, applied on boot (fail-closed).
Guards, all fail-closed: **ordering** (`NNNN_name.sql`, duplicate versions rejected — the
collision class that has bitten the sibling repos), **checksum** (an edited applied
migration is a hard error), **completeness** (a recorded version with no file = database
ahead of code), **atomicity** (migration + its record commit in one transaction, so a
failed migration is never recorded as applied), **concurrency** (a session advisory lock
serialises concurrent replica boots).

The `initdb.d` mount is **removed** — the runner is now the single source of schema truth.
Because the migrations are `IF NOT EXISTS` idempotent, the runner **adopts** an existing
initdb.d-provisioned database without error and records its versions (covered by
`TestApply_AdoptsInitdbProvisionedDatabase`). Proven from zero in the real compose stack:
fresh volume → `migrations applied count=14 versions=0001..0014` → `schema_migrations` = 14
= the number of `.sql` files → restart → `migrations up to date`, still 14 rows.

---

## 2. Post-run state

| Area | State |
|---|---|
| CI as a gate | **Real.** Postgres service, 13 real-PG tests run, anti-regression assert, semgrep blocking |
| SEC-4 cross-tenant (by-id) | **Closed**, and now actually tested on every PR |
| SEC-4 cross-tenant (workspace routes) | **Closed this run** — page search/stale, space list/create |
| Client-supplied authority | **CLOSED (Run 2).** Root + all 8 instances + a residual sweep; `ActorOrEmpty`/`WorkspaceOrEmpty` now banned by semgrep. One documented residual: `collab` (not an authority hole) |
| Migration runner (bar A) | **Built.** Subcommand + `schema_migrations` + 5 fail-closed guards + boot apply |
| Boot / quick start | **Works.** `docker compose up -d` → healthy |
| LLM cost control | **Rate-limited per verified workspace** on 7 of 8 surfaces (Run 3). Bounds RATE, not cost — Docs still has no budget cap, and the async page-save indexer is uncovered. See §0 |
| Request body caps | **Capped** — 4MB `/v1`+`/mcp`, 200MB imports (Run 3). Was: unbounded everywhere |
| DB-outage behaviour | **Clean 503 + `/readyz`** (Run 3). Was: a 500 from authz's lookup, no readiness signal at all |
| Resource scoping ({pageID} vs {id}) | **Closed** (Run 3) — the last known scoping gap |
| Semgrep guardrail | **Runs, blocks.** Covers page/space/search (URL-param shape) **and the body-supplied shape** — 4 new rules, mutation-proven, catching 7/7 of the packages Run 2 fixed |

**Test posture.** 30 packages green, `-race`, `go vet` clean. **29 real-PG security /
integration tests** (Run 1 inherited 9, all of them skipping) + 9 migrate tests. Packages
with real-PG coverage: `analytics`, `approval`, `changelog`, `collab`, `comment`, `database`, `dbhealth`, `mcp`, `membership`, `migrate`, `page`, `pagelink`, `pagelock`, `permission`, `ratelimit`, `space`, `trackintegration`.

Still true: **most store tests are pgxmock-only, and pgxmock never executes SQL** —
`internal/comment/store.go` documents two real bugs (error 42702) that ten passing mock
tests hid for six weeks until the real-PG harness ran them. Packages whose SQL has still
never executed in a test: `block`, `customdomain`, `search`, `sharing`, `templatelib`. `authz` and `gatewayauth` — the auth
boundary — still have **no direct unit tests**.

---

## 3. Known-broken / NOT fixed

### ✅ The client-supplied-authority class — CLOSED (Run 2)

Run 1's sweep found a systemic root plus 8 call-site instances. Run 2 closed all of them,
root first, each red-first against real Postgres.

**The root (was §3.8).** `authz.ActorOrEmpty` → `SingleMemberID` returned `""` for ANY
caller whose membership count `!= 1` — not just zero-membership callers, but every
**multi-workspace member**. Two opposite failures from one line:

- *Functional:* `permission.RequireAccess` evaluated them as `memberID ""`, so
  `resolveAccess` skipped the creator rule and no `subject_type='member'` grant could
  match. Their real grants evaporated; they collapsed to the `everyone`/public default.
- *Security:* because the actor was `""`, every `memberFromReq(r, in.MemberID)` fallback
  went live and the **request body named the actor**. Proven: a two-workspace member
  unlocked another member's lock with `{"member_id":"<locker>"}`, and could not use the
  feature honestly (an empty body was rejected outright — forge or nothing).

Fixed by resolving the actor **per-resource-workspace**. `resourceContext` now carries the
owning `WorkspaceID`; `RequireAccess` resolves the caller via
`authz.MemberIDForWorkspace(ctx, res.WorkspaceID)` — correct for any membership count —
and stashes both, plus the level, on the request context:

| accessor | what it returns |
|---|---|
| `permission.ActorFromContext(ctx)` | caller's member id **in the resource's workspace** |
| `permission.WorkspaceFromContext(ctx)` | the workspace **owning** the gated resource |
| `permission.IsAdminFromContext(ctx)` | caller's real level on that resource (Run 1) |

All fail closed on an unguarded mount. `authz.AuthorizeWorkspace(ctx, ws)` covers
workspace-level routes (use the returned `Membership.MemberID`), `authz.AuthorizedMember`
the MCP chokepoint. **`ActorOrEmpty`/`WorkspaceOrEmpty` are now banned by semgrep** — the
migration is complete, so any new use is a regression.

**The root fix alone closed §3.8 and, via `ActorFromContext`, the `memberFromReq`
fallbacks in `pagelock` and `comment` (its blast radius). It closed none of §3.1–3.7 on
its own** — none of those routed their client value through `RequireAccess`; each needed
its own fix.

| Was | Now |
|---|---|
| **§3.1 `page.Create`** — body `workspace_id` planted a row in another tenant (surfacing in the **victim's** search/stale, falsely attributed) | Workspace **derived** from the parent space, `created_by` from the verified actor. The store no longer demands a client-supplied tenant, so an honest client sends none. |
| **§3.2 MCP `update_page`** — `updated_by` arg impersonated the lock holder → edit **through** their lock | Actor from `authz.AuthorizedMember` (which the chokepoint already stashed and every tool ignored — it had **zero callers**) |
| **§3.3 `changelog.Create`** — **inverted** fallback preferred the client's `created_by` outright | Verified actor, unconditionally; workspace derived from the page. Same fix applied to `Generate`. |
| **§3.4 `pagelink.Create`** — body `workspace_id` + `created_by`, no authz import | Both derived from the parent page |
| **§3.5 MCP `verify_page` / `create_page`** — `verified_by` / `created_by` from args (the default was the literal `"agent"`) | Verified actor; the three identity props are gone from the tool schema |
| **§3.6 `analytics.RecordView`** — body `viewer_id` fed `COUNT(DISTINCT viewer_id)`, forging *who read a page* | Viewer + workspace derived. **Route collision resolved empirically**: a real-PG test proved `analytics` served the path and `page.RecordView` was the *shadowed* safe handler — the duplicate registration is removed, so the live handler is the safe one and there is one owner. |
| **§3.7 `approval.Pending`** — `?reviewer_id=victim` read another member's queue; route **ungated**; `{wsID}` ignored | `{wsID}` authorized, reviewer is always the caller's member id in it, results scoped to it |

**Residual sweep beyond the enumerated 8.** The same root had left `WorkspaceOrEmpty`
fail-open no-ops and `ActorOrEmpty` attribution gaps in `sharing`, `templatelib`,
`permission`, `customdomain`, `database`, and `approval.Request`/`Decide` — including one
that attributed a multi-workspace member's approval request to the literal string
`"user"`. All migrated.

**The guardrail (§3.3 of Run 1's list) is closed too.** `.semgrep/body-supplied-authority.yml`
adds four rules for the body-supplied shape the URL-param rule was blind to: (A) a
body-decoded struct reaching a store with no approved resolver consulted, (B) an authority
field read off the body and never assigned, (C) the inverted fallback, (D) a ban on the
ambiguous helpers. Proven both directions and mutation-proven against freshly-injected
instances. **Against the real pre-fix code at `f238b90` they catch 7 of the 7 packages this
run fixed.** Honest limits are documented in the rule file: they are shape rules, not
dataflow — they do not follow a decoded body across a function boundary, and semgrep
patterns are not flow-ordered, so rule B can only flag a field the handler never assigns.
Rule D is the exact one; the others are approximations over it.

### Still open from this class — one, deliberate

**`collab.ServeWS` is the last `ActorOrEmpty` caller** (carries a documented `nosemgrep`).
**Not an authority hole**: the `?member_id=` query param is ignored and there is no body
fallback, so nothing client-supplied can name the actor. The effect is the root's
*functional* residue — a multi-workspace member opens a session with an empty member id, so
their presence is unlabelled and `CanEdit` will not recognise them as the holder of their
own lock. The WS route is mounted directly with no resource middleware, so there is no
resolved actor in context; fixing it needs `PageScoper` to also yield the page's workspace
so the actor can come from `authz.MemberIDForWorkspace`. Small, but it is an interface
change to a package whose rewrite is already queued — folded into that work rather than
done blind here.

### Still open — deliberate deferrals

1. **The async page-save indexer — the 8th LLM surface, uncontrolled.** See §0
   "Deferred". Largest uncontrolled Lens consumer; needs a product decision, not a blind
   pick. **Top candidate for the next run.**
2. **`collab.ServeWS`'s empty actor for multi-workspace members** — not an authority hole
   (no body fallback, the `?member_id=` param is ignored); folded into the queued collab
   work because it needs a `PageScoper` interface change.
3. **Class-B `nosemgrep` suppressions are externally justified.** `analytics/store.go`,
   `block/store.go`, `pagelock/store.go` have no workspace concept; their cross-tenant
   safety lives entirely in `cmd/docs/main.go`'s `WithAccess` wiring, and
   `Enforcer.Require` is **pass-through on a nil receiver** — so dropping a `WithAccess`
   call silently converts each into a live cross-tenant write with the alarm already
   suppressed. Each suppression names `main.go` as the load-bearing gate. The durable fix
   is a wiring test asserting a foreign id 404s through the real chain. (Note this now cuts
   deeper than cross-tenancy: `permission.ActorFromContext` / `WorkspaceFromContext` are
   populated by `RequireAccess`, so a dropped `WithAccess` also empties the actor — which
   fails closed at every call site added this run, by construction.)

Closed since this file was last written: the `member_id`/`author_id` body fallback and its
systemic root (Run 2); the `POST /v1/spaces` guardrail gap (rule A in
`.semgrep/body-supplied-authority.yml`, verified against `9bc98b2`); the
`POST /v1/spaces/{s}/pages/{p}/view` route collision; and — **Run 3** — the
`{pageID}`-vs-`{id}` comment scoping bug, unbounded LLM spend on 7 of 8 surfaces, unbounded
request bodies, the missing readiness signal, and a latent nil-receiver panic in
`ai.Engine.IsAvailable` reachable through `mcp`'s interface-held engine.

### Carried from recon, still true

- **Collab persistence is last-write-wins.** The OT transform is real, but the stored
  document is the client's snapshot (`ot.go`: "Servers ship without a ProseMirror runtime,
  so we can't replay ops"). Also: `ot.go`'s `Leave` deletes page state on last disconnect
  **with no flush**, so edits inside the 5s autosave window are lost — single replica, no
  restart needed. Explicitly out of scope; it needs a server-side ProseMirror model.
- **Cannot run more than one replica.** OT state is in-process; `trackSyncer`,
  `freshEngine` and `saver` are uncoordinated `go` statements — no leader election. The
  migration runner *is* now replica-safe (advisory lock).
- **WebSocket `SetReadLimit` is still absent** — collab frames are unbounded. (REST/MCP body
  caps and LLM rate limits landed in Run 3; the AI endpoints are throttled but there is still
  no *cost* cap — a rate ceiling is not a budget.)
- `/healthz` never touches the DB **by design** (liveness; see §0) and `/readyz` now probes
  it. `page/handler.go`'s `Get` **still returns 404 on any error** — the DB-outage case is now
  short-circuited to 503 upstream by `dbhealth`, but the misclassification remains for
  transient per-query errors and should be fixed at the handler.
- **`DOCS_LOG_LEVEL` is read into a struct field and never used** (`main.go` hardcodes the
  slog default before config loads). **`DOCS_SHARE_SECRET` and `DOCS_BASE_URL` in
  `.env.example` are dead config** — nothing reads them; share tokens are unsigned random
  hex, so the "signing secret" the file describes does not exist.
- **`team` permission grants are a silent no-op.** **The freshness digest is log-only** and
  its "warning" tier is unreachable. **`changelog` auto-grouping is broken** (`nil` context
  → the Track lookup always errors). **`trackintegration`'s routes have no authz** and its
  syncer zeroes `ai_cost_usd` on a Track outage.
- **The `blocks` table has no readers** — real CRUD API over a table nothing renders. Don't
  build on it.
- **Frontend has no router** (`App.tsx`: "Phase 2 doesn't ship a URL-bound router") — a
  wiki with no page URLs. Zero frontend tests, no eslint.
- **Docs cannot serve an authenticated request without an edge gateway**, which is not in
  this repo. Browser WebSockets cannot set `X-Gateway-Auth`, so collab is undemoable
  locally. `/v1/public/s/{token}` is the only unauthenticated route. `/metrics` is
  unauthenticated and its `space_id` label leaks tenant ids.

---

## 4. Working on this repo

```bash
# real-PG tests (they SKIP without this — that is not a pass)
docker run -d --name docs-pg -e POSTGRES_PASSWORD=postgres -p 5432:5432 pgvector/pgvector:pg16
export DOCS_TEST_DATABASE_URL="postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"

go vet ./... && go test -timeout 300s -race -count=1 ./...
semgrep --config .semgrep/ --error          # the tenancy class-guard CI runs
cd frontend && npm ci && npm run typecheck && npm run build

cp .env.example .env && docker compose up -d   # boots; /healthz → {"ok":true}
docs migrate                                   # apply schema standalone
```

**Adding a migration:** drop `00NN_name.sql` in `migrations/`. It is embedded, applied on
boot in `NNNN` order, and recorded in `schema_migrations`. Never edit an applied migration
— the checksum guard will fail the boot, by design. Docs numbering is independent of the
sibling repos; current high-water is **0014**.
