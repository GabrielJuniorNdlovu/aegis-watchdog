package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestDispatchTransitionIsAtomicAndReservationStillCounts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dispatch.db")
	r := accept(t, path, proposal(), testTime)
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER fail_recovery_event BEFORE INSERT ON recovery_events BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := TransitionRecovery(ctx, path, r.ID, "planned", "dispatching", testTime.Add(time.Second), "dispatching", "dispatch record"); err == nil {
		t.Fatal("event persistence failure was ignored")
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM reservations WHERE reservation_id=?`, r.ID).Scan(&state); err != nil || state != "planned" {
		t.Fatal(state, err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_recovery_event`); err != nil {
		t.Fatal(err)
	}
	if err := TransitionRecovery(ctx, path, r.ID, "planned", "dispatching", testTime.Add(time.Second), "dispatching", "persisted before request"); err != nil {
		t.Fatal(err)
	}
	action, err := InspectAction(ctx, path, r.ID)
	if err != nil || action.Reservation.State != "dispatching" || len(action.Audit) != 1 || action.Audit[0].ToState != "dispatching" {
		t.Fatal(action, err)
	}
	budgetResult, err := InspectBudget(ctx, path, proposal().ServiceID, proposal().Policy, testTime.Add(time.Second), 20)
	if err != nil || budgetResult.Decision.Attempts != 1 {
		t.Fatal(budgetResult, err)
	}
}

func TestLockedReservationDoesNotAcquireOwnerLockAgainOrDuplicateAttempt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "locked.db")
	lock, err := ReconcileStartup(ctx, path, nil, func() time.Time { return testTime })
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	p := proposal()
	first, err := ReserveLocked(ctx, path, p, testTime)
	if err != nil || first.Reservation == nil {
		t.Fatal(first, err)
	}
	second, err := ReserveLocked(ctx, path, p, testTime.Add(time.Second))
	if err != nil || second.Reservation != nil || second.Decision.Reason != "startup_reconciliation_required" {
		t.Fatal(second, err)
	}
	if got := len(inspect(t, path, p, testTime.Add(time.Second)).Reservations); got != 1 {
		t.Fatal("duplicate reservation", got)
	}
}

func TestRecoveryAuditNotePersistsWithoutChangingActionState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gate-note.db")
	r := accept(t, path, proposal(), testTime)
	at := testTime.Add(time.Second)
	if err := RecordRecoveryNote(ctx, path, r.ID, at, "dependency_gate_revalidated", "database passed with fresh evidence"); err != nil {
		t.Fatal(err)
	}
	action, err := InspectAction(ctx, path, r.ID)
	if err != nil || action.Reservation.State != "planned" || len(action.Audit) != 1 || action.Audit[0].FromState != "planned" || action.Audit[0].ToState != "planned" || action.Audit[0].Reason != "dependency_gate_revalidated" || !action.Audit[0].RecordedAt.Equal(at) {
		t.Fatalf("gate note did not persist independently of action state: %#v %v", action, err)
	}
}
