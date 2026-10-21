package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
)

type BudgetHistory struct {
	EvaluatedAt  time.Time
	Policy       budget.Policy
	Facts        budget.Facts
	Decision     budget.Decision
	Reservations []Reservation
}

// InspectBudget takes a consistent read-only snapshot. Inspection never updates
// clock metadata, registers a service, or migrates a database.
func InspectBudget(ctx context.Context, path, service string, p budget.Policy, now time.Time, limit int) (BudgetHistory, error) {
	now = now.UTC()
	var history BudgetHistory
	if !budget.ValidService(service) {
		return history, errors.New("invalid logical service ID")
	}
	if err := p.Validate(); err != nil {
		return history, err
	}
	if err := budget.ValidateTime(now, p); err != nil {
		return history, err
	}
	if err := ValidateLimit(limit); err != nil {
		return history, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := open(path, true)
	if err != nil {
		return history, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return history, err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return history, err
	}
	if version < 1 || version > schemaVersion {
		return history, versionError(version)
	}
	history.EvaluatedAt, history.Policy = now, p
	if version >= 4 {
		history.Facts, err = reservationFacts(ctx, tx, service, p, now)
		if err != nil {
			return BudgetHistory{}, err
		}
		history.Reservations, err = reservationHistory(ctx, tx, service, limit)
		if err != nil {
			return BudgetHistory{}, err
		}
	}
	history.Decision = budget.Evaluate(history.Facts, p, now)
	if err := tx.Commit(); err != nil {
		return BudgetHistory{}, err
	}
	return history, nil
}

func reservationHistory(ctx context.Context, tx *sql.Tx, service string, limit int) ([]Reservation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT reservation_id,service_id,container_id,reserved_at,state,max_attempts,window_ns,cooldown_ns,assessment_id,assessment_summary FROM reservations WHERE service_id=? ORDER BY reserved_at DESC,reservation_id DESC LIMIT ?`, service, limit)
	if err != nil {
		return nil, fmt.Errorf("read reservations: %w", err)
	}
	defer rows.Close()
	var history []Reservation
	for rows.Next() {
		var r Reservation
		var at string
		var ref sql.NullInt64
		var summary sql.NullString
		if err := rows.Scan(&r.ID, &r.ServiceID, &r.ContainerID, &at, &r.State, &r.Policy.MaxAttempts, &r.Policy.Window, &r.Policy.Cooldown, &ref, &summary); err != nil {
			return nil, err
		}
		r.ReservedAt, err = time.Parse(timestampLayout, at)
		if err != nil {
			return nil, errors.New("invalid reservation time")
		}
		r.AssessmentID = ref.Int64
		if summary.Valid {
			r.Assessment = &assessment.Assessment{}
			if err := json.Unmarshal([]byte(summary.String), r.Assessment); err != nil {
				return nil, errors.New("invalid reservation assessment summary")
			}
		}
		history = append(history, r)
	}
	return history, rows.Err()
}
