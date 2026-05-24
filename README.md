# Talyvor Docs

**AI-native knowledge base — the only docs tool that shows you what your documentation costs to implement.**

Talyvor Docs is the writing surface in the Talyvor suite (alongside
Track for issues and Lens for AI cost telemetry). Spaces hold
pages, pages hold ProseMirror content, and the system gets out of
the way: real-time collaboration, slash-menu authoring, inline AI
assists, semantic + full-text search, freshness signals driven by
linked Track work, readership analytics, and a public sharing
surface for customer-facing docs.

## Why Talyvor Docs?

| Feature                  | Confluence       | Notion         | Talyvor Docs       |
| ------------------------ | ---------------- | -------------- | ------------------ |
| Real-time collaboration  | ✅               | ✅             | ✅                 |
| AI writing assistant     | ❌               | ✅ ($15/mo)    | ✅ (free via Lens) |
| Doc freshness alerts     | ❌               | ❌             | ✅                 |
| Readership analytics     | ❌               | ❌             | ✅                 |
| Semantic search          | ❌               | ❌             | ✅                 |
| Issue linking (Track)    | ✅ (Jira only)   | ❌             | ✅                 |
| AI cost per doc          | ❌               | ❌             | ✅                 |
| MCP integration          | ❌               | ❌             | ✅                 |
| Self-hosted              | ✅ (paid)        | ❌             | ✅ (free)          |

## Quick start (2 commands)

```bash
cp .env.example .env
docker compose up -d
```

- Docs UI: <http://localhost:4001>
- API: <http://localhost:4000>
- Healthcheck: <http://localhost:4000/healthz>

The Postgres service is `pgvector/pgvector:pg16` so the semantic
search index works out of the box. Schema migrations under
`migrations/` are mounted into the container's init-db hook and run
in order on first boot.

## Local development

```bash
cp .env.example .env
go run ./cmd/docs           # API on :4000
cd frontend && npm run dev  # Vite on :5173
```

The Vite dev server proxies API requests to :4000. You'll need a
Postgres reachable via `DOCS_DATABASE_URL`.

## Importing from Confluence / Notion

```bash
# Notion markdown export (zip of .md files):
curl -F file=@notion-export.zip \
     -F workspace_id=default \
     -F space_id=sp-abc \
     http://localhost:4000/v1/import/notion

# Confluence HTML export:
curl -F file=@confluence-space.zip \
     -F workspace_id=default \
     -F space_id=sp-abc \
     http://localhost:4000/v1/import/confluence
```

Both endpoints return `{imported, skipped, errors}`. The importer
counts unsupported files (images, CSS, binary attachments) under
`skipped` rather than failing the whole upload. Folder hierarchy
from a Notion export is preserved as parent pages.

## MCP Integration (Claude Code)

The MCP server lets agents read, search, create, and update
documentation directly from a coding workflow. Add to your Claude
Code MCP config:

```json
{
  "mcpServers": {
    "talyvor-docs": {
      "url": "http://localhost:4000/mcp"
    }
  }
}
```

Once configured, try:

- "Search our docs for authentication flow"
- "What does our deployment process say?"
- "Create a new page about our API rate limits"
- "Which docs are stale and need updating?"

### Tools

The server registers 10 tools. Inputs are JSON; outputs use
`content_text` (plain markdown) — never raw ProseMirror.

| Tool                 | Purpose                                            |
| -------------------- | -------------------------------------------------- |
| `search_docs`        | Full-text + ranked search across pages             |
| `get_page`           | Fetch a page by ID or slug (returns plain text)    |
| `create_page`        | Create a page from markdown                        |
| `update_page`        | Update title or content (markdown)                 |
| `list_pages`         | Browse pages in a space                            |
| `ask_docs`           | Natural-language Q&A with citations                |
| `get_stale_pages`    | List pages flagged by the freshness engine         |
| `verify_page`        | Mark a page as still accurate                      |
| `get_page_analytics` | Readership stats for a page                        |
| `get_space_tree`     | 2-level map of the knowledge base                  |

### Protocol

`POST /mcp` accepts JSON-RPC 2.0 with `initialize`, `tools/list`,
and `tools/call` methods. Protocol version `2024-11-05`.

`GET /mcp/sse` opens a Server-Sent Events stream with 30-second
keep-alive pings.

## REST API

The full REST surface lives under `/v1` — spaces, pages, blocks,
comments, search, AI, analytics, freshness, permissions, sharing,
importer. See the handler files under `internal/*/handler.go` for
the endpoint inventory.

## Architecture

| Package              | Responsibility                                              |
| -------------------- | ----------------------------------------------------------- |
| `internal/page`      | Page CRUD, versioning, content_text, stale tracking         |
| `internal/space`     | Space CRUD, slug generation                                 |
| `internal/block`     | Comments + reactions                                        |
| `internal/collab`    | WebSocket OT engine + AutoSaver                             |
| `internal/search`    | Semantic search (pgvector) + unified search handler         |
| `internal/ai`        | 8 AI features (write/transform/translate/ask/title)         |
| `internal/freshness` | "Is this still accurate?" engine + daily digest             |
| `internal/analytics` | View recording + per-page/workspace readership rollups      |
| `internal/permission`| Access control + middleware                                 |
| `internal/sharing`   | Public token sharing with bcrypt passwords + expiry         |
| `internal/mcp`       | JSON-RPC MCP server + markdown ↔ ProseMirror conversion     |
| `internal/importer`  | Notion / Confluence migration                               |
| `internal/lensintegration` | Lens HTTP client (proxies AI calls through Lens)      |
| `internal/trackintegration`| Track HTTP client + cost syncer                       |

## Tests

```bash
make vet       # go vet ./...
make test      # go test -race -count=1 ./...
docker compose config --quiet
cd frontend && npm run typecheck
cd frontend && npm run build
```

The `Makefile` also exposes `make up` / `make down` / `make dev`
for everyday workflows. CI runs the same gates plus a multi-arch
container build on push to `main`.
