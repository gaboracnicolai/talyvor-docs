# BUILD_STATE — talyvor-docs

**Base:** `9bc98b2` (Run 1) → `f238b90` (Run 2) · **Branch:** `docs-authority-class` · **Updated:** 2026-07-16

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
| Semgrep guardrail | **Runs, blocks.** Covers page/space/search (URL-param shape) **and the body-supplied shape** — 4 new rules, mutation-proven, catching 7/7 of the packages Run 2 fixed |

**Test posture.** 27 packages green, `-race`, `go vet` clean. **26 real-PG security /
integration tests** (Run 1 inherited 9, all of them skipping) + 9 migrate tests. Packages
with real-PG coverage: `analytics`, `approval`, `changelog`, `collab`, `comment`, `database`, `mcp`, `membership`, `migrate`, `page`, `pagelink`, `pagelock`, `permission`, `space`, `trackintegration`.

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

1. **Comment routes gate on `{pageID}` but act on `{id}`** — `UnresolveInWorkspaces` /
   `DeleteInWorkspaces` assert only that the comment's page is *somewhere* in the caller's
   workspace set, not that it is under the `{pageID}` the route authorized. Structurally
   the same shape as the `ce8bfe3` share-revoke bug, one blast radius smaller: cross-tenant
   is blocked, cross-page within a tenant is not. Deferred by decision — smaller blast
   radius, and it is a resource-scoping bug rather than a client-authority one, so it did
   not belong in the authority run. **This is the top candidate for the next run.**
2. **`collab.ServeWS`'s empty actor for multi-workspace members** — see the section above.
   Not an authority hole; folded into the queued collab work because it needs a
   `PageScoper` interface change.
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
systemic root (Run 2 — see above); the `POST /v1/spaces` guardrail gap (rule A in
`.semgrep/body-supplied-authority.yml` catches that exact shape — verified against
`9bc98b2`); and the `POST /v1/spaces/{s}/pages/{p}/view` route collision (resolved: the
duplicate registration is gone and the live handler is the safe one).

### Carried from recon, still true

- **Collab persistence is last-write-wins.** The OT transform is real, but the stored
  document is the client's snapshot (`ot.go`: "Servers ship without a ProseMirror runtime,
  so we can't replay ops"). Also: `ot.go`'s `Leave` deletes page state on last disconnect
  **with no flush**, so edits inside the 5s autosave window are lost — single replica, no
  restart needed. Explicitly out of scope; it needs a server-side ProseMirror model.
- **Cannot run more than one replica.** OT state is in-process; `trackSyncer`,
  `freshEngine` and `saver` are uncoordinated `go` statements — no leader election. The
  migration runner *is* now replica-safe (advisory lock).
- **No rate limiting, no request body size caps** (`MaxBytesReader` appears nowhere), no
  WebSocket `SetReadLimit`. The AI endpoints proxy to Lens with no throttle or cost cap.
- **`/healthz` never touches the DB** and there is no `/readyz`. `page/handler.go`'s `Get`
  returns **404 on any error including a DB outage**.
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
