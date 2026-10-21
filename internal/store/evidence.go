package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
)

const schemaV6 = `
CREATE TABLE evidence_captures (
 action_id INTEGER PRIMARY KEY REFERENCES reservations(reservation_id),
 status TEXT NOT NULL CHECK(status IN ('pending','complete','partial','failed')),
 started_at TEXT NOT NULL,
 ended_at TEXT NOT NULL DEFAULT '',
 input TEXT NOT NULL CHECK(length(CAST(input AS BLOB))<=16384),
 limits TEXT NOT NULL CHECK(length(CAST(limits AS BLOB))<=1024),
 bundle_name TEXT NOT NULL DEFAULT '',
 byte_count INTEGER NOT NULL DEFAULT 0 CHECK(byte_count>=0),
 digest TEXT NOT NULL DEFAULT '',
 truncated INTEGER NOT NULL DEFAULT 0,
 issues TEXT NOT NULL DEFAULT '[]',
 deleting INTEGER NOT NULL DEFAULT 0,
 deleted_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX assessments_evidence ON assessments(container_id,evaluated_at DESC,sequence DESC);
PRAGMA user_version=6;
`

// CaptureRecord is the independent evidence journal, never an action outcome.
// Input and Limits are bounded, versioned collector metadata, not Docker bodies.
type CaptureRecord struct {
	ActionID           int64
	Status             string
	StartedAt, EndedAt time.Time
	Input, Limits      string
	BundleName         string
	Bytes              int64
	Digest             string
	Truncated          bool
	Issues             []string
	Deleting           bool
	DeletedAt          time.Time
	Protected          bool
}

// EvidenceInput reads at most four bounded assessment payloads. It deliberately
// avoids InspectAction's operator audit and acknowledgment fields.
func EvidenceInput(ctx context.Context, path string, id int64) (Reservation, []assessment.Assessment, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var r Reservation
	db, err := open(path, true)
	if err != nil {
		return r, nil, false, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return r, nil, false, err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return r, nil, false, err
	}
	if version < 4 || version > schemaVersion {
		return r, nil, false, versionError(version)
	}
	var reserved string
	var payload sql.NullString
	r.ID = id
	err = tx.QueryRowContext(ctx, `SELECT substr(service_id,1,129),substr(container_id,1,65),reserved_at,max_attempts,window_ns,cooldown_ns,substr(CAST(assessment_summary AS BLOB),1,8193) FROM reservations WHERE reservation_id=?`, id).Scan(&r.ServiceID, &r.ContainerID, &reserved, &r.Policy.MaxAttempts, &r.Policy.Window, &r.Policy.Cooldown, &payload)
	if err != nil {
		return r, nil, false, fmt.Errorf("read evidence action: %w", err)
	}
	r.ReservedAt, err = time.Parse(timestampLayout, reserved)
	if err != nil {
		return r, nil, false, err
	}
	var out []assessment.Assessment
	missing := false
	add := func(s string) {
		var a assessment.Assessment
		if len(s) > 8192 || json.Unmarshal([]byte(s), &a) != nil || a.ContainerID != r.ContainerID || a.EvaluatedAt.IsZero() || a.Policy.Validate() != nil || len(a.Status) > 32 || len(a.Reason) > 128 || len(a.Explanation) > 512 {
			missing = true
			return
		}
		for _, old := range out {
			if old.EvaluatedAt.Equal(a.EvaluatedAt) {
				return
			}
		}
		out = append(out, a)
	}
	if payload.Valid {
		add(payload.String)
	} else {
		missing = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT substr(CAST(assessment AS BLOB),1,8193) FROM assessments WHERE container_id=? ORDER BY evaluated_at DESC,sequence DESC LIMIT 3`, r.ContainerID)
	if err != nil {
		return r, nil, false, err
	}
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			break
		}
		add(s)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return r, nil, false, err
	}
	if rowErr != nil {
		return r, nil, false, rowErr
	}
	if err = tx.Commit(); err != nil {
		return r, nil, false, err
	}
	return r, out, missing, nil
}

// BeginEvidence must be called under the data-directory ownership lock.
func BeginEvidence(ctx context.Context, path string, r CaptureRecord) error {
	if r.ActionID <= 0 || r.StartedAt.IsZero() || len(r.Input) > 16384 || len(r.Limits) > 1024 {
		return errors.New("invalid evidence intent")
	}
	return write(ctx, path, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO evidence_captures(action_id,status,started_at,input,limits) VALUES (?,'pending',?,?,?)`, r.ActionID, r.StartedAt.UTC().Format(timestampLayout), r.Input, r.Limits)
		return err
	})
}

// UpdateEvidence records publication, recovery, or deletion progress only.
// The caller holds ownership; reservations, budgets, and audits are untouched.
func UpdateEvidence(ctx context.Context, path string, r CaptureRecord) error {
	issues, err := json.Marshal(r.Issues)
	if err != nil {
		return err
	}
	if len(issues) > 2048 || len(r.BundleName) > 80 || len(r.Digest) > 64 || r.Bytes < 0 {
		return errors.New("invalid evidence result")
	}
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(timestampLayout)
	}
	return write(ctx, path, func(ctx context.Context, tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE evidence_captures SET status=?,ended_at=?,bundle_name=?,byte_count=?,digest=?,truncated=?,issues=?,deleting=?,deleted_at=? WHERE action_id=?`, r.Status, stamp(r.EndedAt), r.BundleName, r.Bytes, r.Digest, r.Truncated, string(issues), r.Deleting, stamp(r.DeletedAt), r.ActionID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err == nil && n != 1 {
			return errors.New("evidence intent is missing")
		}
		return err
	})
}

// EvidenceRecords is read-only, including on old databases. A zero ID selects
// at most limit active records; a positive ID includes retired metadata.
func EvidenceRecords(ctx context.Context, path string, id int64, limit int) ([]CaptureRecord, error) {
	if limit < 1 || limit > 4097 {
		return nil, errors.New("invalid evidence registry limit")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := open(path, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version < 0 || version > schemaVersion {
		return nil, versionError(version)
	}
	if version < 6 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT e.action_id,e.status,e.started_at,e.ended_at,substr(CAST(e.input AS BLOB),1,16385),substr(CAST(e.limits AS BLOB),1,1025),substr(e.bundle_name,1,81),e.byte_count,substr(e.digest,1,65),e.truncated,substr(CAST(e.issues AS BLOB),1,2049),e.deleting,e.deleted_at,(r.state IN ('planned','dispatching','verifying') OR (r.state='uncertain' AND r.acknowledged_at IS NULL)) FROM evidence_captures e JOIN reservations r ON r.reservation_id=e.action_id WHERE (e.action_id=? OR (?=0 AND e.deleted_at='')) ORDER BY e.action_id LIMIT ?`, id, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CaptureRecord
	for rows.Next() {
		var r CaptureRecord
		var start, end, deleted, issues string
		if err = rows.Scan(&r.ActionID, &r.Status, &start, &end, &r.Input, &r.Limits, &r.BundleName, &r.Bytes, &r.Digest, &r.Truncated, &issues, &r.Deleting, &deleted, &r.Protected); err != nil {
			return nil, err
		}
		if len(r.Input) > 16384 || len(r.Limits) > 1024 || len(r.BundleName) > 80 || len(r.Digest) > 64 || len(issues) > 2048 {
			return nil, errors.New("evidence registry metadata exceeds limits")
		}
		r.StartedAt, err = time.Parse(timestampLayout, start)
		if err != nil {
			return nil, err
		}
		if end != "" {
			r.EndedAt, err = time.Parse(timestampLayout, end)
			if err != nil {
				return nil, err
			}
		}
		if deleted != "" {
			r.DeletedAt, err = time.Parse(timestampLayout, deleted)
			if err != nil {
				return nil, err
			}
		}
		if err = json.Unmarshal([]byte(issues), &r.Issues); err != nil {
			return nil, errors.New("invalid evidence issues")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
