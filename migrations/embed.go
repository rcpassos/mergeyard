// Package migrations contains the ordered, embedded runtime schema migrations.
package migrations

import "embed"

// Files contains SQL migrations named <number>_<name>.sql. Apply validates that
// filename order contains consecutive numeric versions starting at 1; use a
// consistent prefix width (e.g. 001, 002) so lexical order matches version order.
// Applied files must not be changed or renumbered; append new migrations only.
//
//go:embed *.sql
var Files embed.FS
