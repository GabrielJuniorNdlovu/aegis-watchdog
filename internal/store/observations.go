// Package store persists only allowlisted container observation metadata.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	_ "github.com/mattn/go-sqlite3"
)

const (
	DefaultPath   = "data/aegis.db"
	DefaultLimit  = 20
	MaxLimit      = 1000
	schemaVersion = 1
	// Fixed-width UTC text sorts chronologically, including fractional seconds.
	timestampLayout = "2006-01-02T15:04:05.000000000Z"
)

const schemaV1 = `
CREATE TABLE observations (
    sequence INTEGER PRIMARY KEY,
    container_id TEXT NOT NULL CHECK(length(container_id) = 64 AND container_id NOT GLOB '*[^0-9a-f]*'),
    container_name TEXT NOT NULL,
    observed_at TEXT NOT NULL,
    lifecycle TEXT NOT NULL CHECK(lifecycle IN ('created','running','paused','restarting','removing','exited','dead')),
    health TEXT NOT NULL CHECK(health IN ('healthy','unhealthy','starting','unknown')),
    check_time TEXT
);
CREATE INDEX observations_history ON observations(container_id, observed_at DESC, sequence DESC);
PRAGMA user_version = 1;
`

// Observation contains saved facts, not a claim about current health.
type Observation struct {
	ContainerID string
	Name        string
	ObservedAt  time.Time
	Lifecycle   string
	Health      string
	CheckTime   time.Time
}

func ValidateLimit(limit int) error {
	if limit < 1 || limit > MaxLimit {
		return fmt.Errorf("limit must be between 1 and %d", MaxLimit)
	}
	return nil
}

// Save commits the migration and one observation in a single transaction. It does
// not deduplicate health-check timestamps: each requested save is an observation.
func Save(ctx context.Context, path string, result inspection.Result, observedAt time.Time) error {
	if !inspection.ValidID(result.ID) || !inspection.ValidSelector(result.Name) || observedAt.IsZero() {
		return errors.New("invalid observation identity or timestamp")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := open(path, false)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin observation save: %w", err)
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read SQLite schema version: %w", err)
	}
	if version != 0 && version != schemaVersion {
		return versionError(version)
	}
	// Set this only after checking the version, so newer databases stay untouched.
	var journal string
	if err := tx.QueryRowContext(ctx, "PRAGMA journal_mode = DELETE").Scan(&journal); err != nil {
		return fmt.Errorf("select SQLite rollback journal: %w", err)
	}
	if journal != "delete" {
		return errors.New("SQLite rollback journal is unavailable")
	}
	if version == 0 {
		if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
			return fmt.Errorf("initialize SQLite schema: %w", err)
		}
	}
	var checkTime any
	if !result.CheckTime.IsZero() {
		checkTime = result.CheckTime.UTC().Format(timestampLayout)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO observations
        (container_id, container_name, observed_at, lifecycle, health, check_time)
        VALUES (?, ?, ?, ?, ?, ?)`, result.ID, result.Name, observedAt.UTC().Format(timestampLayout), result.Lifecycle, result.Health, checkTime)
	if err != nil {
		return fmt.Errorf("insert observation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit observation: %w", err)
	}
	return nil
}

// History opens an existing file read-only and never performs migrations.
func History(ctx context.Context, path, containerID string, limit int) ([]Observation, error) {
	if !inspection.ValidID(containerID) {
		return nil, errors.New("history requires a full 64-character Docker ID")
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
	if version != schemaVersion {
		return nil, versionError(version)
	}
	rows, err := db.QueryContext(ctx, `SELECT container_id, container_name, observed_at, lifecycle, health, check_time
        FROM observations WHERE container_id = ? ORDER BY observed_at DESC, sequence DESC LIMIT ?`, containerID, limit)
	if err != nil {
		return nil, fmt.Errorf("read observation history: %w", err)
	}
	defer rows.Close()
	var observations []Observation
	for rows.Next() {
		var observation Observation
		var observed string
		var check sql.NullString
		if err := rows.Scan(&observation.ContainerID, &observation.Name, &observed, &observation.Lifecycle, &observation.Health, &check); err != nil {
			return nil, fmt.Errorf("read observation: %w", err)
		}
		observation.ObservedAt, err = time.Parse(timestampLayout, observed)
		if err != nil {
			return nil, errors.New("database contains an invalid observation timestamp")
		}
		if check.Valid {
			observation.CheckTime, err = time.Parse(timestampLayout, check.String)
			if err != nil {
				return nil, errors.New("database contains an invalid health-check timestamp")
			}
		}
		if !inspection.ValidSelector(observation.Name) {
			return nil, errors.New("database contains an invalid container name")
		}
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read observation history: %w", err)
	}
	return observations, nil
}

func versionError(version int) error {
	return fmt.Errorf("unsupported SQLite schema version %d (supported: %d); database left unchanged", version, schemaVersion)
}

func open(path string, readOnly bool) (*sql.DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path must not be empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	mode := "ro"
	if readOnly {
		if _, err := os.Stat(absolute); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, errors.New("database does not exist; run inspect --container <name-or-id> --save first, using the same --db path")
			}
			return nil, fmt.Errorf("access database: %w", err)
		}
	} else {
		mode = "rw"
		if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
		file, err := os.OpenFile(absolute, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err == nil {
			if err := file.Close(); err != nil {
				return nil, fmt.Errorf("close new database file: %w", err)
			}
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create database file: %w", err)
		}
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	query := url.Values{"mode": {mode}, "_busy_timeout": {"1000"}}
	if !readOnly {
		query.Set("_synchronous", "FULL")
		query.Set("_txlock", "immediate")
	}
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
