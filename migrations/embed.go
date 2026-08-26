// Package migrations exposes the docs SQL schema as an embedded filesystem.
//
// ⚠ THIS EMBED IS THE PRODUCTION PATH, AND THE COMMENT THAT SAID OTHERWISE WAS
// THE EXACT BELIEF docker-compose.yaml WAS CHANGED TO KILL. It used to read
// "Production applies these same files via Postgres's docker-entrypoint-initdb.d
// (see docker-compose.yaml) — this embed is a test-only accessor". Measured:
// cmd/docs/main.go:103 and :162 call migrate.Apply(ctx, pool, migrations.FS) on
// boot, and docker-compose.yaml:79-84 records the ./migrations:/docker-entrypoint-
// initdb.d mount as deliberately DELETED, because that hook runs only on the first
// boot of an empty data directory and so "could never upgrade an existing one —
// adding a 00NN_*.sql to a running deployment did nothing, silently".
//
// The stale comment told the next person that a file added here is test-only, which
// is the same wrong conclusion the deleted mount used to produce, reached through a
// doc instead of a volume. internal/testutil applies the same bytes to a per-test
// database; that is the SECOND consumer, not the only one.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
