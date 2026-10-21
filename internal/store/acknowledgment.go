package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/ownership"
)

const MaxAcknowledgmentReason = 512

func ValidateAcknowledgmentReason(reason string) error {
	if strings.TrimSpace(reason) == "" || len(reason) > MaxAcknowledgmentReason || !utf8.ValidString(reason) {
		return errors.New("acknowledgment requires a nonempty reason of at most 512 UTF-8 bytes")
	}
	for _, r := range reason {
		if unicode.IsControl(r) {
			return errors.New("acknowledgment reason must not contain control characters")
		}
	}
	return nil
}

type Acknowledgment struct {
	AlreadyAcknowledged bool
	At                  time.Time
	Reason              string
}

func Acknowledge(ctx context.Context, path string, id int64, reason string, at time.Time) (Acknowledgment, error) {
	var out Acknowledgment
	if id <= 0 {
		return out, errors.New("supply one positive action ID")
	}
	if err := ValidateAcknowledgmentReason(reason); err != nil {
		return out, err
	}
	at = at.UTC()
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return out, errors.New("invalid acknowledgment time")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	lock, err := ownership.Acquire(path)
	if err != nil {
		return out, err
	}
	defer lock.Close()
	if _, err := os.Stat(path); err != nil {
		return out, fmt.Errorf("access action journal: %w", err)
	}
	err = write(ctx, path, func(ctx context.Context, tx *sql.Tx) error {
		var state string
		var previousAt, previousReason sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT state,acknowledged_at,acknowledgment_reason FROM reservations WHERE reservation_id=?`, id).Scan(&state, &previousAt, &previousReason); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("action %d does not exist", id)
			}
			return err
		}
		if state != "uncertain" {
			return fmt.Errorf("action %d is %s; only uncertain actions can be acknowledged", id, state)
		}
		if previousAt.Valid {
			parsed, err := time.Parse(timestampLayout, previousAt.String)
			if err != nil {
				return errors.New("invalid saved acknowledgment time")
			}
			out = Acknowledgment{AlreadyAcknowledged: true, At: parsed, Reason: previousReason.String}
			return nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE reservations SET acknowledged_at=?,acknowledgment_reason=? WHERE reservation_id=? AND state='uncertain' AND acknowledged_at IS NULL`, at.Format(timestampLayout), reason, id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("action changed before acknowledgment; inspect it again")
		}
		explanation := "Operator acknowledgment: " + reason + ". This records acceptance of uncertainty, not proof of success. The reserved attempt is not refunded; future policy checks still apply."
		if err := insertActionAudit(ctx, tx, id, ActionAudit{Kind: "acknowledgment", RecordedAt: at, FromState: "uncertain", ToState: "uncertain", Reason: "operator_acknowledged", Explanation: explanation}); err != nil {
			return err
		}
		if err := advanceActionClock(ctx, tx, at); err != nil {
			return err
		}
		out = Acknowledgment{At: at, Reason: reason}
		return nil
	})
	if err != nil {
		return Acknowledgment{}, fmt.Errorf("cannot acknowledge action: %w", err)
	}
	return out, nil
}
