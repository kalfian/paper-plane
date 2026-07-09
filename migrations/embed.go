// Package migrations embeds the SQL migration files. It lives alongside the
// .sql files because go:embed patterns cannot traverse parent directories, so
// the embedding declaration must sit in the migrations directory itself.
package migrations

import "embed"

// FS holds all migration SQL files, applied in lexical filename order.
//
//go:embed *.sql
var FS embed.FS
