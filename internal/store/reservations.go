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
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/ownership"
)

type Proposal struct {
	ServiceID    string
	ContainerID  string
	Policy       budget.Policy
	AssessmentID int64 // Zero means no linked assessment; positive IDs are checked and copied.
}

func (p Proposal) validate(now time.Time) error {
	if !budget.ValidService(p.ServiceID) || !inspection.ValidID(p.ContainerID) || p.AssessmentID < 0 {
		return errors.New("reservation requires an explicit logical service, full Docker ID, and valid assessment reference")
	}
	if err := p.Policy.Validate(); err != nil {
		return err
	}
	return budget.ValidateTime(now, p.Policy)
}

type Reservation struct {
	ID                     int64
	ServiceID, ContainerID string
	ReservedAt             time.Time
	State                  string
	Policy                 budget.Policy
	AssessmentID           int64
	Assessment             *assessment.Assessment
}
type ReservationResult struct {
	Decision    budget.Decision
	Reservation *Reservation
}

// Reserve records intent only. No caller in the live monitor invokes it.
// The directory lock coordinates owners; SQLite also serializes transactions.
func Reserve(ctx context.Context, path string, p Proposal, now time.Time) (ReservationResult, error) {
	now = now.UTC()
	if err := p.validate(now); err != nil {
		return ReservationResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ReservationResult{}, err
	}
	lock, err := ownership.Acquire(path)
	if err != nil {
		return ReservationResult{}, err
	}
	defer lock.Close()
	return reserve(ctx, path, p, now.UTC())
}

// reserve is the database operation under the owner lock. Separate SQLite
// connections still cannot both pass the checks, even without that outer lock.
func reserve(ctx context.Context, path string, p Proposal, now time.Time) (ReservationResult, error) {
	var outcome ReservationResult
	err := write(ctx, path, func(ctx context.Context, tx *sql.Tx) error {
		facts, err := reservationFacts(ctx, tx, p.ServiceID, p.Policy, now)
		if err != nil {
			return err
		}
		newService := facts.CoverageStart.IsZero()
		if newService {
			facts.CoverageStart = now.Add(-p.Policy.Window)
		}
		outcome.Decision = budget.Evaluate(facts, p.Policy, now)
		high := now
		for _, at := range []time.Time{facts.HighWater, facts.LatestHostReservation} {
			if at.After(high) {
				high = at
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO reservation_clock(singleton,high_water) VALUES (1,?) ON CONFLICT(singleton) DO UPDATE SET high_water=excluded.high_water`, high.Format(timestampLayout)); err != nil {
			return fmt.Errorf("persist reservation clock: %w", err)
		}
		if !outcome.Decision.Available {
			return nil
		}
		var summary any
		var evidence *assessment.Assessment
		var reference any
		if p.AssessmentID != 0 {
			var id, payload string
			if err := tx.QueryRowContext(ctx, `SELECT container_id,assessment FROM assessments WHERE sequence=?`, p.AssessmentID).Scan(&id, &payload); err != nil {
				return fmt.Errorf("read proposed assessment: %w", err)
			}
			evidence = &assessment.Assessment{}
			if err := json.Unmarshal([]byte(payload), evidence); err != nil {
				return errors.New("proposed assessment is invalid")
			}
			if id != p.ContainerID || evidence.ContainerID != p.ContainerID || evidence.EvaluatedAt.IsZero() {
				return errors.New("proposed assessment does not identify the exact target container")
			}
			encoded, err := json.Marshal(evidence)
			if err != nil {
				return fmt.Errorf("encode assessment summary: %w", err)
			}
			summary, reference = string(encoded), p.AssessmentID
		}
		if newService {
			if _, err := tx.ExecContext(ctx, `INSERT INTO services(service_id,coverage_start) VALUES (?,?)`, p.ServiceID, facts.CoverageStart.Format(timestampLayout)); err != nil {
				return fmt.Errorf("register logical service: %w", err)
			}
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO reservations(service_id,container_id,reserved_at,state,max_attempts,window_ns,cooldown_ns,assessment_id,assessment_summary) VALUES (?,?,?,'planned',?,?,?,?,?)`, p.ServiceID, p.ContainerID, now.Format(timestampLayout), p.Policy.MaxAttempts, int64(p.Policy.Window), int64(p.Policy.Cooldown), reference, summary)
		if err != nil {
			return fmt.Errorf("persist reservation: %w", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return err
		}
		outcome.Reservation = &Reservation{ID: id, ServiceID: p.ServiceID, ContainerID: p.ContainerID, ReservedAt: now, State: "planned", Policy: p.Policy, AssessmentID: p.AssessmentID, Assessment: evidence}
		return nil
	})
	if err != nil {
		return ReservationResult{}, fmt.Errorf("cannot evaluate or persist reservation: %w", err)
	}
	return outcome, nil
}

func reservationFacts(ctx context.Context, q *sql.Tx, service string, p budget.Policy, now time.Time) (budget.Facts, error) {
	var f budget.Facts
	readTime := func(query string, args ...any) (time.Time, error) {
		var s sql.NullString
		err := q.QueryRowContext(ctx, query, args...).Scan(&s)
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, nil
		}
		if err != nil {
			return time.Time{}, err
		}
		if !s.Valid {
			return time.Time{}, nil
		}
		at, err := time.Parse(timestampLayout, s.String)
		if err != nil || at.IsZero() {
			return time.Time{}, errors.New("reservation journal contains an invalid timestamp")
		}
		return at, nil
	}
	var err error
	if f.CoverageStart, err = readTime(`SELECT coverage_start FROM services WHERE service_id=?`, service); err != nil {
		return f, err
	}
	if f.HighWater, err = readTime(`SELECT high_water FROM reservation_clock WHERE singleton=1`); err != nil {
		return f, err
	}
	if f.LatestHostReservation, err = readTime(`SELECT MAX(reserved_at) FROM reservations`); err != nil {
		return f, err
	}
	if f.LastReservation, err = readTime(`SELECT MAX(reserved_at) FROM reservations WHERE service_id=?`, service); err != nil {
		return f, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM reservations WHERE service_id=? AND reserved_at>? AND reserved_at<=?`, service, now.Add(-p.Window).UTC().Format(timestampLayout), now.UTC().Format(timestampLayout)).Scan(&f.Attempts); err != nil {
		return f, err
	}
	err = q.QueryRowContext(ctx, `SELECT reservation_id,service_id FROM reservations WHERE state IN ('planned','dispatching','verifying','uncertain') LIMIT 1`).Scan(&f.UnresolvedID, &f.UnresolvedService)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return f, err
	}
	return f, nil
}
