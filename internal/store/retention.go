package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const PruneBatch = 500

// Prune removes at most 500 rows from each routine-history table, strictly before
// the cutoff. Reservations, coverage, and clock metadata are never pruned.
func Prune(ctx context.Context, path string, cutoff time.Time) (int64, error) {
	if cutoff.IsZero() {
		return 0, errors.New("retention cutoff must not be zero")
	}
	var deleted int64
	err := write(ctx, path, func(ctx context.Context, tx *sql.Tx) error {
		for _, query := range []string{
			`DELETE FROM observations WHERE sequence IN (SELECT sequence FROM observations WHERE observed_at < ? ORDER BY observed_at,sequence LIMIT ?)`,
			`DELETE FROM assessments WHERE sequence IN (SELECT sequence FROM assessments WHERE evaluated_at < ? ORDER BY evaluated_at,sequence LIMIT ?)`,
			`DELETE FROM collection_errors WHERE sequence IN (SELECT sequence FROM collection_errors WHERE collected_at < ? ORDER BY collected_at,sequence LIMIT ?)`,
		} {
			result, err := tx.ExecContext(ctx, query, cutoff.UTC().Format(timestampLayout), PruneBatch)
			if err != nil {
				return fmt.Errorf("prune routine history: %w", err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			deleted += count
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
