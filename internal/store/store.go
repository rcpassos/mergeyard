package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/migrations"
	_ "modernc.org/sqlite"
)

// Open opens runtime storage and applies pending embedded migrations atomically.
// Callers own the returned database and must close it before releasing the workspace lock.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, &fault.Error{Code: "workspace.database_path", Path: path, Err: fmt.Errorf("resolve database path: %w", err)}
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	query := url.Values{"_pragma": {"foreign_keys(1)", "busy_timeout(5000)"}}
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, &fault.Error{Code: "workspace.database_open", Path: absolute, Err: fmt.Errorf("open database: %w", err)}
	}
	// Serialize local writes and keep connection-local settings consistent.
	db.SetMaxOpenConns(1)
	if err := migrations.Apply(ctx, db, migrations.Files); err != nil {
		db.Close()
		return nil, &fault.Error{Code: "workspace.database_migrate", Path: absolute, Err: fmt.Errorf("migrate database: %w", err)}
	}
	return db, nil
}
