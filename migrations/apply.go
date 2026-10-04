package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// Apply applies pending migrations from source atomically. Callers must provide
// an append-only migration set with consecutive numeric filename prefixes.
func Apply(ctx context.Context, db *sql.DB, source fs.FS) error {
	files, err := fs.Glob(source, "*.sql")
	if err != nil {
		return err
	}
	versions := make([]int, len(files))
	// Validate the whole source before opening a transaction, including migrations
	// already applied to an existing database. Never infer a version from position.
	for i, filename := range files {
		prefix, name, ok := strings.Cut(filename, "_")
		if !ok || prefix == "" || strings.Trim(prefix, "0123456789") != "" || name == ".sql" {
			return fmt.Errorf("invalid migration filename %q: expected <number>_<name>.sql", filename)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return fmt.Errorf("invalid migration version in %q: %w", filename, err)
		}
		if version != i+1 {
			return fmt.Errorf("invalid migration sequence at %q: version %d, expected %d (gap, duplicate, or incorrect filename order)", filename, version, i+1)
		}
		versions[i] = version
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 0 || version > len(files) {
		return fmt.Errorf("schema version %d is unsupported by this binary (latest %d)", version, len(files))
	}
	for i := version; i < len(files); i++ {
		migration, err := fs.ReadFile(source, files[i])
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(migration)); err != nil {
			return fmt.Errorf("%s: %w", files[i], err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", versions[i])); err != nil {
			return err
		}
	}
	return tx.Commit()
}
