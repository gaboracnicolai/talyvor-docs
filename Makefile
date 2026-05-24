# Talyvor Docs — top-level developer commands.

.PHONY: up down dev test vet build tidy run frontend-build frontend-typecheck

# ─── Docker-based stack ─────────────────────────────────

up:
	docker compose up -d

down:
	docker compose down

# ─── Local dev ──────────────────────────────────────────

# Run the API server + Vite dev server side-by-side. The API needs
# DOCS_DATABASE_URL pointing at a Postgres with pgvector.
dev:
	(go run ./cmd/docs &) && cd frontend && npm run dev

# ─── Go gates ───────────────────────────────────────────

build:
	go build -ldflags="-w -s" -o bin/docs ./cmd/docs

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

tidy:
	go mod tidy

# Run the API server locally. Requires Postgres + DOCS_DATABASE_URL.
run:
	go run ./cmd/docs

# ─── Frontend gates ─────────────────────────────────────

frontend-build:
	cd frontend && npm run build

frontend-typecheck:
	cd frontend && npm run typecheck
