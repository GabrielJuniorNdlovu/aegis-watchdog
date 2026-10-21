package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
)

const schemaV3 = `
ALTER TABLE observations ADD COLUMN evidence TEXT;
CREATE TABLE assessments (
 sequence INTEGER PRIMARY KEY,
 target TEXT NOT NULL,
 evaluated_at TEXT NOT NULL,
 container_id TEXT,
 assessment TEXT NOT NULL
);
CREATE INDEX assessments_history ON assessments(target,evaluated_at DESC,sequence DESC);
CREATE INDEX assessments_retention ON assessments(evaluated_at,sequence);
PRAGMA user_version=3;
`

// SaveMonitoringResult atomically commits exactly one result and its explanation.
// Assessment summaries survive independent pruning of raw observations.
func SaveMonitoringResult(ctx context.Context, path, target string, result inspection.Result, cause error, a assessment.Assessment) error {
	if !inspection.ValidSelector(target) || a.EvaluatedAt.IsZero() || a.CollectedAt.IsZero() {
		return errors.New("invalid assessment target or time")
	}
	if err := a.Policy.Validate(); err != nil {
		return err
	}
	if (cause != nil && a.ContainerID != "") || (cause == nil && a.ContainerID != result.ID) {
		return errors.New("assessment identity does not match collection result")
	}
	payload, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("encode assessment: %w", err)
	}
	return write(ctx, path, func(ctx context.Context, tx *sql.Tx) error {
		if cause != nil {
			err = insertCollectionError(ctx, tx, target, a.CollectedAt, cause)
		} else {
			err = insertObservation(ctx, tx, result, a.CollectedAt)
		}
		if err != nil {
			return err
		}
		var id any
		if a.ContainerID != "" {
			id = a.ContainerID
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO assessments (target,evaluated_at,container_id,assessment) VALUES (?,?,?,?)`, target, a.EvaluatedAt.UTC().Format(timestampLayout), id, string(payload))
		if err != nil {
			return fmt.Errorf("insert assessment: %w", err)
		}
		return nil
	})
}

// AssessmentHistory uses the exact requested selector, including unresolved errors.
func AssessmentHistory(ctx context.Context, path, target string, limit int) ([]assessment.Assessment, error) {
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
		return nil, err
	}
	if version < 1 || version > schemaVersion {
		return nil, versionError(version)
	}
	if version < 3 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT evaluated_at,container_id,assessment FROM assessments WHERE target=? ORDER BY evaluated_at DESC,sequence DESC LIMIT ?`, target, limit)
	if err != nil {
		return nil, fmt.Errorf("read assessments: %w", err)
	}
	defer rows.Close()
	var history []assessment.Assessment
	for rows.Next() {
		var at, payload string
		var id sql.NullString
		var a assessment.Assessment
		if err := rows.Scan(&at, &id, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &a); err != nil {
			return nil, errors.New("database contains an invalid assessment")
		}
		if a.EvaluatedAt.UTC().Format(timestampLayout) != at || a.ContainerID != id.String {
			return nil, errors.New("database contains inconsistent assessment identity or time")
		}
		history = append(history, a)
	}
	return history, rows.Err()
}
