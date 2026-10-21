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
	if version < 1 || version > schemaVersion {
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
