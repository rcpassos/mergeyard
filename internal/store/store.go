package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"

	"github.com/rcpassos/mergeyard/migrations"
	_ "modernc.org/sqlite"
)

// Open opens runtime storage and applies pending embedded migrations atomically.
// Callers own the returned database and must close it before releasing the workspace lock.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("workspace.database_path: resolve database path: %w", err)
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	query := url.Values{"_pragma": {"foreign_keys(1)", "busy_timeout(5000)"}}
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, fmt.Errorf("workspace.database_open: open database: %w", err)
	}
	// Serialize local writes and keep connection-local settings consistent.
	db.SetMaxOpenConns(1)
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("workspace.database_migrate: migrate database: %w", err)
	}
	return db, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	files, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		return err
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
		migration, err := migrations.Files.ReadFile(files[i])
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(migration)); err != nil {
			return fmt.Errorf("%s: %w", files[i], err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
