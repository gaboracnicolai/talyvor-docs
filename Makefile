# Talyvor Docs — top-level developer commands.

.PHONY: build test vet tidy run

build:
	go build ./...

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

tidy:
	go mod tidy

# Run the API server locally. Requires Postgres + DOCS_DATABASE_URL.
run:
	go run ./cmd/docs
