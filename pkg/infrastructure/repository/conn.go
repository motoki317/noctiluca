package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"

	"github.com/motoki317/noctiluca/migrations"
)

type Config struct {
	Path string `mapstructure:"path"`
}

func New(ctx context.Context, config Config) (*sql.DB, error) {
	if config.Path == "" {
		return nil, errors.New("database.path must not be empty")
	}
	path, err := filepath.Abs(config.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	// OAuth credentials must not inherit the usual world-readable file mode.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("protect database: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close database file: %w", err)
	}
	dsn := url.URL{Scheme: "file", Path: path}
	query := url.Values{}
	query.Set("modeof", path)
	query.Set("_pragma", "busy_timeout(5000)")
	// Fixed-width UTC timestamps keep lexical ordering precise to the nanosecond.
	query.Set("_timefmt", "2006-01-02T15:04:05.000000000Z07:00")
	dsn.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20")
	db, err := driver.Open(dsn.String(), fts5.Register)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("begin schema migration: %w", err)
	}
	if _, err := tx.ExecContext(ctx, migrations.Schema); err != nil {
		tx.Rollback()
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		return nil, fmt.Errorf("commit schema: %w", err)
	}
	return db, nil
}
