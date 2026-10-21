package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
)

func TestBudgetReadOnlyLegacyAndUnsupportedSchema(t *testing.T) {
	for _, version := range []int{1, 2, 3, 6} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			db, err := open(path, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(schemaV1); err != nil {
				t.Fatal(err)
			}
			if version >= 2 {
				if _, err := db.Exec(schemaV2); err != nil {
					t.Fatal(err)
				}
			}
			if version >= 3 {
				if _, err := db.Exec(schemaV3); err != nil {
					t.Fatal(err)
				}
			}
			if version == 6 {
				if _, err := db.Exec("PRAGMA user_version=6"); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			h, err := InspectBudget(context.Background(), path, "api", budget.DefaultPolicy(), testTime, 20)
			if version == 6 {
				if err == nil {
					t.Fatal("newer schema accepted")
				}
			} else if err != nil || h.Decision.Reason != "insufficient_history" || len(h.Reservations) != 0 {
				t.Fatal(h, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("inspection changed legacy database")
			}
		})
	}
}
func TestBudgetHistoryOrderingLimitAndIndependentCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordering.db")
	p := proposal()
	r := accept(t, path, p, testTime)
	terminal(t, path, r.ID, "failed")
	r = accept(t, path, p, testTime.Add(p.Policy.Cooldown))
	terminal(t, path, r.ID, "canceled")
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO reservations(service_id,container_id,reserved_at,state,max_attempts,window_ns,cooldown_ns) SELECT service_id,container_id,reserved_at,'succeeded',max_attempts,window_ns,cooldown_ns FROM reservations WHERE reservation_id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	h, err := InspectBudget(context.Background(), path, p.ServiceID, p.Policy, r.ReservedAt, 1)
	if err != nil || len(h.Reservations) != 1 || h.Reservations[0].ID <= r.ID || h.Decision.Attempts != 3 {
		t.Fatal(h, err)
	}
	// Inspection opens and closes its connection; this second call reopens history.
	h = inspect(t, path, p, r.ReservedAt)
	if len(h.Reservations) != 3 || h.Reservations[0].ID <= h.Reservations[1].ID || !h.Reservations[2].ReservedAt.Equal(testTime) {
		t.Fatal(h)
	}
	readonly, err := open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	if _, err := readonly.Exec("DELETE FROM reservations"); err == nil {
		t.Fatal("read-only connection allowed journal mutation")
	}
}
func TestEveryReservationStateConsumesBudget(t *testing.T) {
	for _, state := range []string{"planned", "dispatching", "verifying", "uncertain", "failed", "succeeded", "canceled"} {
		t.Run(state, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			p := proposal()
			p.Policy.MaxAttempts = 1
			r := accept(t, path, p, testTime)
			db, err := open(path, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE reservations SET state=? WHERE reservation_id=?", state, r.ID); err != nil {
				t.Fatal(err)
			}
			db.Close()
			h := inspect(t, path, p, testTime.Add(p.Policy.Cooldown))
			if h.Decision.Attempts != 1 || h.Decision.Available {
				t.Fatal(h)
			}
		})
	}
}
