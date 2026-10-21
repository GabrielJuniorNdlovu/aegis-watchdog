package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
)

func uncertain(t *testing.T, path string, p Proposal) Reservation {
	t.Helper()
	r := accept(t, path, p, testTime)
	terminal(t, path, r.ID, "dispatching")
	startup(t, path, func(context.Context, string) (inspection.Result, error) { return result("healthy"), nil }, testTime.Add(time.Minute))
	return r
}
func TestAcknowledgmentPreservesUncertaintyHistoryAndBudget(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ack.db")
	p := proposal()
	r := uncertain(t, path, p)
	a, err := Acknowledge(ctx, path, r.ID, "Checked workload manually; accepting the unresolved outcome", testTime.Add(2*time.Minute))
	if err != nil || a.AlreadyAcknowledged {
		t.Fatal(a, err)
	}
	action, err := InspectAction(ctx, path, r.ID)
	if err != nil || action.Unresolved || action.Reservation.State != "uncertain" || len(action.Audit) != 2 || action.Reservation.AcknowledgedAt.IsZero() || action.HostUnresolvedID != 0 {
		t.Fatal(action, err)
	}
	h := inspect(t, path, p, testTime.Add(2*time.Minute))
	if h.Decision.Attempts != 1 || h.Decision.Reason != "cooldown" {
		t.Fatal(h)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := Acknowledge(ctx, path, r.ID, "different reason must not overwrite original", testTime.Add(3*time.Minute))
	if err != nil || !repeated.AlreadyAcknowledged || repeated.Reason != a.Reason || !repeated.At.Equal(a.At) {
		t.Fatal(repeated, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("repeat acknowledgment modified history")
	}
	startup(t, path, nil, testTime.Add(4*time.Minute))
	if auditCount(t, path) != 2 {
		t.Fatal("startup duplicated resolved uncertainty")
	}
	p.Policy.MaxAttempts = 1
	blocked, err := Reserve(ctx, path, p, testTime.Add(p.Policy.Cooldown))
	if err != nil || blocked.Decision.Reason != "budget_exhausted" {
		t.Fatal(blocked, err)
	}
	p.Policy.MaxAttempts = 2
	accept(t, path, p, testTime.Add(p.Policy.Cooldown))
	if _, err := Prune(ctx, path, testTime.Add(365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	action, err = InspectAction(ctx, path, r.ID)
	if err != nil || len(action.Audit) != 2 || action.Reservation.AcknowledgmentReason != a.Reason {
		t.Fatal("pruning lost audit", action, err)
	}
}
func TestAcknowledgmentValidationAndWrongStates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reject.db")
	r := accept(t, path, proposal(), testTime)
	for _, reason := range []string{"", "  ", strings.Repeat("x", 513), "line\nbreak"} {
		if _, err := Acknowledge(ctx, path, r.ID, reason, testTime); err == nil {
			t.Fatal(reason)
		}
	}
	for _, id := range []int64{0, -1, 999, r.ID} {
		if _, err := Acknowledge(ctx, path, id, "operator reason", testTime); err == nil {
			t.Fatal(id)
		}
	}
	if auditCount(t, path) != 0 || inspect(t, path, proposal(), testTime).Reservations[0].State != "planned" {
		t.Fatal("invalid acknowledgment changed state")
	}
	for _, state := range []string{"dispatching", "verifying", "succeeded", "failed", "canceled"} {
		terminal(t, path, r.ID, state)
		if _, err := Acknowledge(ctx, path, r.ID, "operator reason", testTime); err == nil {
			t.Fatal(state)
		}
	}
}
func TestAcknowledgmentOwnerLockAndAtomicFailure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lock.db")
	r := uncertain(t, path, proposal())
	lock, err := ReconcileStartup(ctx, path, nil, func() time.Time { return testTime.Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acknowledge(ctx, path, r.ID, "operator reason", testTime.Add(2*time.Minute)); err == nil {
		t.Fatal("acknowledged while daemon owned lock")
	}
	lock.Close()
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_ack BEFORE INSERT ON action_audit WHEN NEW.kind='acknowledgment' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := Acknowledge(ctx, path, r.ID, "operator reason", testTime.Add(2*time.Minute)); err == nil {
		t.Fatal("ignored audit failure")
	}
	a, err := InspectAction(ctx, path, r.ID)
	if err != nil || !a.Unresolved || len(a.Audit) != 1 || !a.Reservation.AcknowledgedAt.IsZero() {
		t.Fatal(a, err)
	}
}

func TestActionVersionFourReadAndMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schemaV1 + schemaV2 + schemaV3 + schemaV4); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO services VALUES ('legacy',?); INSERT INTO reservations(service_id,container_id,reserved_at,state,max_attempts,window_ns,cooldown_ns) VALUES ('legacy',?,?,'planned',3,?,?)`, testTime.Add(-time.Hour).Format(timestampLayout), proposal().ContainerID, testTime.Format(timestampLayout), int64(time.Hour), int64(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := InspectAction(context.Background(), path, 1)
	if err != nil || !a.Unresolved || len(a.Audit) != 0 {
		t.Fatal(a, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read migrated old action")
	}
	startup(t, path, nil, testTime.Add(time.Minute))
	a, err = InspectAction(context.Background(), path, 1)
	if err != nil || a.Reservation.State != "canceled" || len(a.Audit) != 1 || !a.Reservation.ReservedAt.Equal(testTime) {
		t.Fatal(a, err)
	}
}
