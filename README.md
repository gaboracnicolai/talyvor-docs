# Talyvor Docs

AI-native documentation platform — the writing surface in the Talyvor
suite (alongside Track for issues and Lens for AI cost telemetry).

Spaces hold pages, pages hold ProseMirror content, and the system
gets out of the way: real-time collaboration, slash-menu authoring,
inline AI assists, semantic + full-text search, freshness signals
driven by linked Track work, readership analytics, and a public
sharing surface for customer-facing docs.

## Running locally

```bash
cp .env.example .env
go run ./cmd/docs
```

The server listens on `:4000` by default (`DOCS_LISTEN_ADDR`). Set
`DOCS_DATABASE_URL` to a Postgres connection string with the
`vector` extension available.

The frontend lives in `frontend/`:

```bash
cd frontend
npm install
npm run dev
```

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
content_text (plain markdown) — never raw ProseMirror.

| Tool | Purpose |
|------|---------|
| `search_docs` | Full-text + ranked search across pages |
| `get_page` | Fetch a page by ID or slug, returns plain text body |
| `create_page` | Create a page from markdown |
| `update_page` | Update title or content (markdown) |
| `list_pages` | Browse pages in a space |
| `ask_docs` | Natural-language Q&A with citations |
| `get_stale_pages` | List pages flagged by the freshness engine |
| `verify_page` | Mark a page as still accurate |
| `get_page_analytics` | Readership stats for a page |
| `get_space_tree` | 2-level map of the knowledge base |

### Protocol

`POST /mcp` accepts JSON-RPC 2.0 with `initialize`, `tools/list`,
and `tools/call` methods. Protocol version `2024-11-05`.

`GET /mcp/sse` opens a Server-Sent Events stream with 30-second
keep-alive pings.

## REST API

The full REST surface lives under `/v1` — spaces, pages, blocks,
comments, search, AI, analytics, freshness, permissions, sharing.
See the handler files under `internal/*/handler.go` for the
endpoint inventory.

## Tests

```bash
go vet ./...
go test ./...
cd frontend && npm run typecheck && npm run build
```
