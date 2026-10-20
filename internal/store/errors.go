package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
)

type CollectionError struct {
	Target      string
	CollectedAt time.Time
	Category    string
	Explanation string
}

func SaveCollectionError(ctx context.Context, path, target string, at time.Time, cause error) error {
	if !inspection.ValidSelector(target) || at.IsZero() || cause == nil {
		return errors.New("invalid collection error")
	}
	category, explanation := inspection.FailureDetails(cause)
	return write(ctx, path, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO collection_errors (target, collected_at, category, explanation) VALUES (?, ?, ?, ?)`, target, at.UTC().Format(timestampLayout), category, explanation)
		if err != nil {
			return fmt.Errorf("insert collection error: %w", err)
		}
		return nil
	})
}

// ErrorHistory filters by the exact requested target; it does not invent a Docker ID.
func ErrorHistory(ctx context.Context, path, target string, limit int) ([]CollectionError, error) {
	if !inspection.ValidSelector(target) {
		return nil, errors.New("invalid requested target")
	}
	if err := ValidateLimit(limit); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := open(path, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, fmt.Errorf("read SQLite schema version: %w", err)
	}
	if version < 1 || version > schemaVersion {
		return nil, versionError(version)
	}
	if version == 1 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT target, collected_at, category, explanation FROM collection_errors WHERE target = ? ORDER BY collected_at DESC, sequence DESC LIMIT ?`, target, limit)
	if err != nil {
		return nil, fmt.Errorf("read collection errors: %w", err)
	}
	defer rows.Close()
	var failures []CollectionError
	for rows.Next() {
		var failure CollectionError
		var at string
		if err := rows.Scan(&failure.Target, &at, &failure.Category, &failure.Explanation); err != nil {
			return nil, err
		}
		failure.CollectedAt, err = time.Parse(timestampLayout, at)
		if err != nil {
			return nil, errors.New("database contains an invalid collection timestamp")
		}
		failures = append(failures, failure)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return failures, nil
}
