# BUILD_STATE — talyvor-docs

**Base:** `9bc98b2` · **Branch:** `docs-foundation-run1` · **Generated:** 2026-07-15

This file is the honest current state of the repo, written to be trusted when planning
the next run. Where something is thin, it says so. The README is a product pitch and is
**not** a reliable description of what works — this file is.

---

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
| Migration runner (bar A) | **Built.** Subcommand + `schema_migrations` + 5 fail-closed guards + boot apply |
| Boot / quick start | **Works.** `docker compose up -d` → healthy |
| Semgrep guardrail | **Runs, blocks, and covers page/space/search** |

**Test posture.** 27 packages green, `-race`, `go vet` clean. 13 real-PG tests + 9 new
migrate tests. Still true: **the majority of store tests are pgxmock-only, and pgxmock
never executes SQL** — `internal/comment/store.go` documents two real bugs (error 42702)
that ten passing mock tests hid for six weeks until the real-PG harness ran them. Packages
whose SQL has still never executed in a test: `analytics`, `approval`, `block`,
`changelog`, `customdomain`, `pagelink`, `pagelock`, `search`, `sharing`, `templatelib`.
`authz` and `gatewayauth` — the auth boundary — still have **no direct unit tests**.

---

## 3. Known-broken / NOT fixed (deliberately out of scope for Run 1)

### New findings from this run — not fixed, needs a decision

1. **`is_admin` is client-supplied** — `internal/pagelock/handler.go` decodes
   `IsAdmin bool \`json:"is_admin"\`` from the request **body** and passes it to
   `Store.Unlock`, which trusts it to bypass the "only the locker can unlock" rule. Any
   member with `AccessEdit` on a page can steal another member's lock with
   `{"is_admin": true}`. Not cross-tenant (the route gate holds), so not SEC-4 — but a
   genuine privilege bug. Contrast `memberID` on the same handler, which is correctly
   resolved from the verified identity.
2. **Comment routes gate on `{pageID}` but act on `{id}`** — `UnresolveInWorkspaces` /
   `DeleteInWorkspaces` assert only that the comment's page is *somewhere* in the caller's
   workspace set, not that it is under the `{pageID}` that was authorized. Structurally the
   same shape as the `ce8bfe3` share-revoke bug, one blast radius smaller: cross-tenant is
   blocked, cross-page within a tenant is not.
3. **`POST /v1/spaces` is not covered by any semgrep rule.** Its workspace id arrives in
   the request *body*, and `docs-no-url-param-workspace-scope` only matches
   `chi.URLParam`. The route is fixed and tested, but the guardrail would not catch this
   shape recurring elsewhere.
4. **Class-B `nosemgrep` suppressions are externally justified.** `analytics/store.go`,
   `block/store.go`, `pagelock/store.go` have no workspace concept; their cross-tenant
   safety lives entirely in `cmd/docs/main.go`'s `WithAccess` wiring, and
   `Enforcer.Require` is **pass-through on a nil receiver** — so dropping a `WithAccess`
   call silently converts each into a live cross-tenant write with the alarm already
   suppressed. Each suppression names `main.go` as the load-bearing gate. The durable fix
   is a wiring test asserting a foreign id 404s through the real chain.
5. **Route collision:** `POST /v1/spaces/{spaceID}/pages/{pageID}/view` is registered by
   both `page/handler.go` and `analytics/handler.go`; the later mount wins and one is dead.
   Both are gated identically, so nothing is exposed.

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
