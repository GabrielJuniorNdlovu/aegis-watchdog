package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Action struct {
	Reservation      Reservation
	Audit            []ActionAudit
	Unresolved       bool
	HostUnresolvedID int64
}

func InspectAction(ctx context.Context, path string, id int64) (Action, error) {
	var out Action
	if id <= 0 {
		return out, errors.New("supply one positive action ID")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := open(path, true)
	if err != nil {
		return out, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return out, err
	}
	if version < 1 || version > schemaVersion {
		return out, versionError(version)
	}
	if version < 4 {
		return out, fmt.Errorf("action %d does not exist", id)
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+reservationColumns(version)+` FROM reservations WHERE reservation_id=?`, id)
	if err != nil {
		return out, err
	}
	records, err := readReservationRows(rows)
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(records) == 0 {
		return out, fmt.Errorf("action %d does not exist", id)
	}
	out.Reservation = records[0]
	err = tx.QueryRowContext(ctx, `SELECT reservation_id FROM reservations WHERE `+unresolvedWhere(version >= 5)+` LIMIT 1`).Scan(&out.HostUnresolvedID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Action{}, err
	}
	r := out.Reservation
	out.Unresolved = r.State == "planned" || r.State == "dispatching" || r.State == "verifying" || (r.State == "uncertain" && r.AcknowledgedAt.IsZero())
	if version >= 5 {
		rows, err := tx.QueryContext(ctx, `SELECT kind,recorded_at,from_state,to_state,reason,explanation,evidence FROM action_audit WHERE action_id=? ORDER BY sequence`, id)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var a ActionAudit
			var at string
			var evidence sql.NullString
			if err := rows.Scan(&a.Kind, &at, &a.FromState, &a.ToState, &a.Reason, &a.Explanation, &evidence); err != nil {
				rows.Close()
				return Action{}, err
			}
			a.RecordedAt, err = time.Parse(timestampLayout, at)
			if err != nil {
				rows.Close()
				return Action{}, errors.New("invalid action audit time")
			}
			if evidence.Valid {
				a.Evidence = &ActionEvidence{}
				if err := json.Unmarshal([]byte(evidence.String), a.Evidence); err != nil {
					rows.Close()
					return Action{}, errors.New("invalid action evidence")
				}
			}
			out.Audit = append(out.Audit, a)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return Action{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Action{}, err
	}
	return out, nil
}
