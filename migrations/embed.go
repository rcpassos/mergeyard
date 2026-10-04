// Package migrations contains the ordered, embedded runtime schema migrations.
package migrations

import "embed"

// Files contains SQL migrations ordered by their numeric filename prefix.
// Applied files must not be changed; schema changes get a new migration.
//
//go:embed *.sql
var Files embed.FS
