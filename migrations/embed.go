// Package migrations exposes the docs SQL schema as an embedded filesystem so
// tests (internal/testutil) can apply it to a fresh database without a relative
// path dependency. Production applies these same files via Postgres's
// docker-entrypoint-initdb.d (see docker-compose.yaml) — this embed is a
// test-only accessor over the identical bytes, not a second schema copy.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
