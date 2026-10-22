package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/ownership"
)

const schemaV5 = `
ALTER TABLE reservations ADD COLUMN acknowledged_at TEXT;
ALTER TABLE reservations ADD COLUMN acknowledgment_reason TEXT;
DROP INDEX reservations_one_unresolved;
CREATE UNIQUE INDEX reservations_one_unresolved ON reservations((1))
 WHERE state IN ('planned','dispatching','verifying') OR (state='uncertain' AND acknowledged_at IS NULL);
CREATE TABLE action_audit (
 sequence INTEGER PRIMARY KEY,
 action_id INTEGER NOT NULL REFERENCES reservations(reservation_id),
 kind TEXT NOT NULL CHECK(kind IN ('startup','acknowledgment')),
 recorded_at TEXT NOT NULL,
 from_state TEXT NOT NULL,
 to_state TEXT NOT NULL,
 reason TEXT NOT NULL,
 explanation TEXT NOT NULL,
 evidence TEXT,
 UNIQUE(action_id,kind)
);
PRAGMA user_version=5;
`

const schemaV7 = `
CREATE TABLE recovery_events (
 sequence INTEGER PRIMARY KEY,
 action_id INTEGER NOT NULL REFERENCES reservations(reservation_id),
 recorded_at TEXT NOT NULL,
 from_state TEXT NOT NULL,
 to_state TEXT NOT NULL,
 reason TEXT NOT NULL,
 explanation TEXT NOT NULL
);
CREATE INDEX recovery_events_action ON recovery_events(action_id, sequence);
CREATE TABLE recovery_sessions (
 action_id INTEGER PRIMARY KEY REFERENCES reservations(reservation_id),
 container_id TEXT NOT NULL,
 old_execution TEXT NOT NULL,
 new_execution TEXT NOT NULL,
 started_at TEXT NOT NULL,
 deadline TEXT NOT NULL,
 grace_until TEXT NOT NULL
);
PRAGMA user_version=7;
`

type RecoverySession struct {
	ContainerID, OldExecution, NewExecution string
	StartedAt, Deadline, GraceUntil         time.Time
}

func RecordRecoverySession(ctx context.Context, path string, id int64, s RecoverySession) error {
	if id <= 0 || !inspection.ValidID(s.ContainerID) || s.OldExecution == "" || s.NewExecution == "" || s.StartedAt.IsZero() || !s.Deadline.After(s.StartedAt) || s.GraceUntil.Before(s.StartedAt) {
		return errors.New("invalid recovery verification session")
	}
	return write(ctx, path, func(ctx context.Context, tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM reservations WHERE reservation_id=?`, id).Scan(&state); err != nil {
			return err
		}
		if state != "dispatching" {
			return errors.New("recovery action is not dispatching")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO recovery_sessions(action_id,container_id,old_execution,new_execution,started_at,deadline,grace_until) VALUES (?,?,?,?,?,?,?)`, id, s.ContainerID, s.OldExecution, s.NewExecution, s.StartedAt.UTC().Format(timestampLayout), s.Deadline.UTC().Format(timestampLayout), s.GraceUntil.UTC().Format(timestampLayout))
		return err
	})
}

// TransitionRecovery durably records a lifecycle transition before its caller
// proceeds to the next external operation.
func TransitionRecovery(ctx context.Context, path string, id int64, from, to string, at time.Time, reason, explanation string) error {
	allowed := map[string]map[string]bool{
		"planned":     {"dispatching": true, "canceled": true},
		"dispatching": {"verifying": true, "failed": true, "uncertain": true},
		"verifying":   {"succeeded": true, "failed": true, "uncertain": true},
	}
	if id <= 0 || !allowed[from][to] || at.IsZero() || len(reason) > 128 || len(explanation) > 8192 {
		return errors.New("invalid recovery action transition")
	}
	return write(ctx, path, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE reservations SET state=? WHERE reservation_id=? AND state=?`, to, id, from)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("recovery action state changed; inspect the action journal")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO recovery_events(action_id,recorded_at,from_state,to_state,reason,explanation) VALUES (?,?,?,?,?,?)`, id, at.UTC().Format(timestampLayout), from, to, reason, explanation)
		return err
	})
}

func RecordRecoveryNote(ctx context.Context, path string, id int64, at time.Time, reason, explanation string) error {
	if id <= 0 || at.IsZero() || len(reason) == 0 || len(reason) > 128 || len(explanation) > 8192 {
		return errors.New("invalid recovery audit note")
	}
	return write(ctx, path, func(ctx context.Context, tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM reservations WHERE reservation_id=?`, id).Scan(&state); err != nil {
			return err
		}
		if state != "planned" {
			return errors.New("recovery action is no longer planned")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO recovery_events(action_id,recorded_at,from_state,to_state,reason,explanation) VALUES (?,?,?,?,?,?)`, id, at.UTC().Format(timestampLayout), state, state, reason, explanation)
		return err
	})
}

type ActionEvidence struct {
	ContainerID   string
	Lifecycle     string
	Health        string
	StartedAt     time.Time
	CheckTime     time.Time
	ErrorCategory string
}
type ActionAudit struct {
	Kind                                    string
	RecordedAt                              time.Time
	FromState, ToState, Reason, Explanation string
	Evidence                                *ActionEvidence
}

// ReconcileStartup returns the acquired owner lock only after all transitions
// persist. The caller holds it until shutdown. No executor is enabled here.
func ReconcileStartup(ctx context.Context, path string, inspect func(context.Context, string) (inspection.Result, error), now func() time.Time) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock, err := ownership.Acquire(path)
	if err != nil {
		return nil, err
	}
	if err := reconcile(ctx, path, inspect, now); err != nil {
		lock.Close()
		return nil, fmt.Errorf("startup reconciliation failed: %w", err)
	}
	return lock, nil
}

func reconcile(ctx context.Context, path string, inspect func(context.Context, string) (inspection.Result, error), now func() time.Time) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	db, err := open(path, true)
	if err != nil {
		return err
	}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return err
	}
	if version < 0 || version > schemaVersion {
		db.Close()
		return versionError(version)
	}
	if version < 4 {
		db.Close()
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT reservation_id,container_id,state FROM reservations WHERE state IN ('planned','dispatching','verifying') ORDER BY reservation_id`)
	if err != nil {
		db.Close()
		return err
	}
	type pending struct {
		id            int64
		target, state string
	}
	var actions []pending
	for rows.Next() {
		var a pending
		if err := rows.Scan(&a.id, &a.target, &a.state); err != nil {
			rows.Close()
			db.Close()
			return err
		}
		actions = append(actions, a)
	}
	err = rows.Err()
	rows.Close()
	db.Close()
	if err != nil {
		return err
	}
	for _, a := range actions {
		if err := ctx.Err(); err != nil {
			return err
		}
		next, reason, explanation := "canceled", "stopped_before_dispatch", "Controller stopped before dispatch. Planned means no Docker mutation was dispatched; the attempt remains consumed."
		var evidence *ActionEvidence
		if a.state != "planned" {
			next = "uncertain"
			reason = "interrupted_" + a.state
			evidence = &ActionEvidence{}
			if inspect == nil {
				evidence.ErrorCategory = "unavailable"
				explanation = "No Docker inspector is available; the outcome of the recorded action remains unknown."
			} else {
				requestCtx, cancel := context.WithTimeout(ctx, inspection.Timeout)
				result, err := inspect(requestCtx, a.target)
				cancel()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err != nil {
					category, message := inspection.FailureDetails(err)
					evidence.ErrorCategory = category
					explanation = "Exact recorded target inspection failed: " + message + ". No replacement was inspected."
				} else {
					evidence.ContainerID, evidence.Lifecycle, evidence.Health = result.ID, result.Lifecycle, result.Health
					evidence.StartedAt, evidence.CheckTime = result.Evidence.StartedAt, result.CheckTime
					if result.ID != a.target {
						reason = "target_identity_mismatch"
						explanation = "Docker returned a different container ID. Its state is not evidence of the recorded target's action outcome."
					} else {
						explanation = "Only the exact recorded container was inspected. Current health and execution start cannot prove that Aegis caused a restart or achieved sustained recovery. Interrupted " + a.state + " remains uncertain."
					}
				}
			}
			explanation += " No mutation was resent. Operator acknowledgment is required."
		}
		at := now().UTC()
		if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
			return errors.New("invalid reconciliation time")
		}
		if err := write(ctx, path, func(ctx context.Context, tx *sql.Tx) error {
			result, err := tx.ExecContext(ctx, `UPDATE reservations SET state=? WHERE reservation_id=? AND state=? AND container_id=? AND acknowledged_at IS NULL`, next, a.id, a.state, a.target)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				return nil
			}
			if err := insertActionAudit(ctx, tx, a.id, ActionAudit{Kind: "startup", RecordedAt: at, FromState: a.state, ToState: next, Reason: reason, Explanation: explanation, Evidence: evidence}); err != nil {
				return err
			}
			return advanceActionClock(ctx, tx, at)
		}); err != nil {
			return err
		}
	}
	return nil
}

func insertActionAudit(ctx context.Context, tx *sql.Tx, id int64, a ActionAudit) error {
	var evidence any
	if a.Evidence != nil {
		encoded, err := json.Marshal(a.Evidence)
		if err != nil {
			return err
		}
		evidence = string(encoded)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO action_audit(action_id,kind,recorded_at,from_state,to_state,reason,explanation,evidence) VALUES (?,?,?,?,?,?,?,?)`, id, a.Kind, a.RecordedAt.Format(timestampLayout), a.FromState, a.ToState, a.Reason, a.Explanation, evidence)
	return err
}
func advanceActionClock(ctx context.Context, tx *sql.Tx, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO reservation_clock(singleton,high_water) VALUES (1,?) ON CONFLICT(singleton) DO UPDATE SET high_water=MAX(high_water,excluded.high_water)`, at.UTC().Format(timestampLayout))
	return err
}
