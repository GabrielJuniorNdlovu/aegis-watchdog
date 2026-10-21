package store

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/ownership"
)

func proposal() Proposal {
	return Proposal{ServiceID: "orders-api", ContainerID: strings.Repeat("a", 64), Policy: budget.DefaultPolicy()}
}
func accept(t *testing.T, path string, p Proposal, at time.Time) Reservation {
	t.Helper()
	out, err := Reserve(context.Background(), path, p, at)
	if err != nil || out.Reservation == nil || !out.Decision.Available {
		t.Fatalf("reservation=%+v err=%v", out, err)
	}
	return *out.Reservation
}

// Terminal records are fixtures only. No production lifecycle API exists yet.
func terminal(t *testing.T, path string, id int64, state string) {
	t.Helper()
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE reservations SET state=? WHERE reservation_id=?`, state, id); err != nil {
		t.Fatal(err)
	}
}
func inspect(t *testing.T, path string, p Proposal, at time.Time) BudgetHistory {
	t.Helper()
	h, err := InspectBudget(context.Background(), path, p.ServiceID, p.Policy, at, 20)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func TestReservationFirstPersistenceAndHostBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.db")
	p := proposal()
	r := accept(t, path, p, testTime)
	if r.ID <= 0 || r.State != "planned" || r.ServiceID != p.ServiceID || r.ContainerID != p.ContainerID {
		t.Fatal(r)
	}
	h := inspect(t, path, p, testTime)
	if len(h.Reservations) != 1 || h.Decision.Attempts != 1 || h.Decision.Reason != "unresolved_action" {
		t.Fatal(h)
	}
	// A different service is blocked even after the old budget and cooldown expire.
	p.ServiceID = "unrelated"
	out, err := Reserve(context.Background(), path, p, testTime.Add(24*time.Hour))
	if err != nil || out.Reservation != nil || out.Decision.Reason != "unresolved_action" {
		t.Fatal(out, err)
	}
	h = inspect(t, path, p, testTime.Add(24*time.Hour))
	if h.Facts.UnresolvedService != "orders-api" {
		t.Fatal(h)
	}
}
func TestCooldownBoundaryAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cooldown.db")
	p := proposal()
	r := accept(t, path, p, testTime)
	terminal(t, path, r.ID, "failed")
	p.ContainerID = strings.Repeat("b", 64)
	out, err := Reserve(context.Background(), path, p, testTime.Add(p.Policy.Cooldown-time.Nanosecond))
	if err != nil || out.Decision.Reason != "cooldown" {
		t.Fatal(out, err)
	}
	h := inspect(t, path, p, testTime.Add(p.Policy.Cooldown))
	if h.Decision.Attempts != 1 || !h.Decision.Available {
		t.Fatal(h)
	}
	r = accept(t, path, p, testTime.Add(p.Policy.Cooldown))
	if r.ContainerID != p.ContainerID {
		t.Fatal(r)
	}
	h = inspect(t, path, p, r.ReservedAt)
	if h.Decision.Attempts != 2 || len(h.Reservations) != 2 {
		t.Fatal(h)
	}
}
func TestBudgetExhaustionWindowAndAllStatesCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "window.db")
	p := proposal()
	p.Policy.MaxAttempts = 2
	r := accept(t, path, p, testTime)
	terminal(t, path, r.ID, "canceled")
	r = accept(t, path, p, testTime.Add(p.Policy.Cooldown))
	terminal(t, path, r.ID, "succeeded")
	out, err := Reserve(context.Background(), path, p, testTime.Add(2*p.Policy.Cooldown))
	if err != nil || out.Decision.Reason != "budget_exhausted" {
		t.Fatal(out, err)
	}
	h := inspect(t, path, p, testTime.Add(p.Policy.Window-time.Nanosecond))
	if h.Decision.Attempts != 2 {
		t.Fatal(h)
	}
	h = inspect(t, path, p, testTime.Add(p.Policy.Window))
	if h.Decision.Attempts != 1 || !h.Decision.Available {
		t.Fatal(h)
	}
	accept(t, path, p, testTime.Add(p.Policy.Window))
}
func TestPolicyChangeCoverageAndClockAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clock.db")
	p := proposal()
	p.Policy.MaxAttempts = 1
	r := accept(t, path, p, testTime)
	terminal(t, path, r.ID, "failed")
	p.Policy.MaxAttempts = 2
	h := inspect(t, path, p, testTime.Add(p.Policy.Cooldown))
	if !h.Decision.Available || h.Decision.Attempts != 1 {
		t.Fatal(h)
	}
	p.Policy.Window = 2 * time.Hour
	out, err := Reserve(context.Background(), path, p, testTime.Add(time.Minute))
	if err != nil || out.Decision.Reason != "insufficient_history" {
		t.Fatal(out, err)
	}
	// The blocked call's durable clock also survives closing and reopening.
	h = inspect(t, path, p, testTime)
	if !h.Decision.ClockUnsafe {
		t.Fatal(h)
	}
	out, err = Reserve(context.Background(), path, p, testTime)
	if err != nil || out.Decision.Reason != "clock_unsafe" {
		t.Fatal(out, err)
	}
	h = inspect(t, path, p, testTime.Add(time.Minute))
	if h.Decision.ClockUnsafe {
		t.Fatal("clock did not recover at equality", h)
	}
	// The larger window becomes supported when its start reaches known coverage.
	h = inspect(t, path, p, testTime.Add(time.Hour))
	if !h.Decision.Available {
		t.Fatal(h)
	}
}
func TestFutureReservationNeverReplenishesBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	p := proposal()
	r := accept(t, path, p, testTime.Add(time.Hour))
	terminal(t, path, r.ID, "succeeded")
	out, err := Reserve(context.Background(), path, p, testTime)
	if err != nil || out.Decision.Reason != "clock_unsafe" {
		t.Fatal(out, err)
	}
}
func TestConcurrentSQLiteReservations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	p := proposal()
	// Each call to reserve opens its own SQLite connection. Deliberately bypass
	// the outer owner lock to prove transaction and constraint correctness.
	const n = 8
	start := make(chan struct{})
	out := make(chan ReservationResult, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := reserve(context.Background(), path, p, testTime)
			out <- r
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	close(errs)
	accepted := 0
	for r := range out {
		if r.Reservation != nil {
			accepted++
		} else if r.Decision.Reason != "unresolved_action" {
			t.Fatal(r)
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if accepted != 1 {
		t.Fatal("accepted", accepted)
	}
	h := inspect(t, path, p, testTime)
	if len(h.Reservations) != 1 || h.Decision.Attempts != 1 {
		t.Fatal(h)
	}
}
func TestReservationFailureRollsBackEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	p := proposal()
	ctx := context.Background()
	if err := Save(ctx, path, result("healthy"), testTime); err != nil {
		t.Fatal(err)
	}
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_reservation BEFORE INSERT ON reservations BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	out, err := Reserve(ctx, path, p, testTime)
	if err == nil || out.Reservation != nil || out.Decision.Available {
		t.Fatal(out, err)
	}
	for _, table := range []string{"services", "reservations", "reservation_clock"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatal("partial write", table, n, err)
		}
	}
	if _, err := db.Exec(`DROP TRIGGER reject_reservation`); err != nil {
		t.Fatal(err)
	}
	accept(t, path, p, testTime)
}
func TestReservationValidationAndOwnership(t *testing.T) {
	p := proposal()
	for _, modify := range []func(*Proposal){func(p *Proposal) { p.ServiceID = "" }, func(p *Proposal) { p.ContainerID = "prefix" }, func(p *Proposal) { p.Policy.MaxAttempts = 0 }, func(p *Proposal) { p.AssessmentID = -1 }} {
		invalid := p
		modify(&invalid)
		if _, err := Reserve(context.Background(), filepath.Join(t.TempDir(), "x.db"), invalid, testTime); err == nil {
			t.Fatal(invalid)
		}
	}
	path := filepath.Join(t.TempDir(), "owned.db")
	lock, err := ownership.Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := Reserve(context.Background(), path, p, testTime); err == nil {
		t.Fatal("bypassed active daemon ownership")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Reserve(ctx, path, p, testTime); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestAssessmentReferenceSurvivesRoutinePruning(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "evidence.db")
	r, a := evaluated(testTime, nil)
	if err := SaveMonitoringResult(ctx, path, "app", r, nil, a); err != nil {
		t.Fatal(err)
	}
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id int64
	if err := db.QueryRow("SELECT sequence FROM assessments").Scan(&id); err != nil {
		t.Fatal(err)
	}
	p := proposal()
	p.AssessmentID = id
	reserved := accept(t, path, p, testTime)
	if reserved.Assessment == nil || reserved.AssessmentID != id {
		t.Fatal(reserved)
	}
	if _, err := Prune(ctx, path, testTime.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	h := inspect(t, path, p, testTime.Add(24*time.Hour))
	if len(h.Reservations) != 1 || h.Reservations[0].Assessment == nil || h.Reservations[0].State != "planned" {
		t.Fatal(h)
	}
	if h.Facts.UnresolvedID != reserved.ID {
		t.Fatal("expired unresolved reservation", h)
	}
}
func TestAssessmentMismatchLeavesNoReservation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mismatch.db")
	r, a := evaluated(testTime, nil)
	if err := SaveMonitoringResult(ctx, path, "app", r, nil, a); err != nil {
		t.Fatal(err)
	}
	p := proposal()
	p.ContainerID = strings.Repeat("b", 64)
	p.AssessmentID = 1
	if _, err := Reserve(ctx, path, p, testTime); err == nil {
		t.Fatal("assessment mismatched target")
	}
	h := inspect(t, path, p, testTime)
	if len(h.Reservations) != 0 {
		t.Fatal(h)
	}
}

func TestRetentionPreservesAllReservationAccounting(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "keep.db")
	p := proposal()
	r := accept(t, path, p, testTime)
	terminal(t, path, r.ID, "failed")
	r = accept(t, path, p, testTime.Add(p.Policy.Cooldown))
	terminal(t, path, r.ID, "canceled")
	// Even a cutoff newer than the active budget window cannot prune its journal.
	if _, err := Prune(ctx, path, testTime.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	h := inspect(t, path, p, testTime.Add(10*time.Minute))
	if len(h.Reservations) != 2 || h.Decision.Attempts != 2 || h.Facts.HighWater.IsZero() || h.Facts.CoverageStart.IsZero() {
		t.Fatal(h)
	}
	if _, err := Prune(ctx, path, testTime.Add(365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	h = inspect(t, path, p, testTime.Add(365*24*time.Hour))
	if len(h.Reservations) != 2 {
		t.Fatal("journal was pruned", h)
	}
}

func TestBudgetRestartClockHelper(t *testing.T) {
	path := os.Getenv("AEGIS_TEST_BUDGET_RESTART_PATH")
	if path == "" {
		return
	}
	out, err := Reserve(context.Background(), path, proposal(), testTime)
	if err != nil || out.Decision.Reason != "clock_unsafe" || out.Reservation != nil {
		t.Fatal(out, err)
	}
}
func TestBudgetClockSurvivesFreshProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	p := proposal()
	r := accept(t, path, p, testTime)
	terminal(t, path, r.ID, "failed")
	p.Policy.Window *= 2
	out, err := Reserve(context.Background(), path, p, testTime.Add(time.Minute))
	if err != nil || out.Decision.Reason != "insufficient_history" {
		t.Fatal(out, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBudgetRestartClockHelper$")
	cmd.Env = append(os.Environ(), "AEGIS_TEST_BUDGET_RESTART_PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh process forgot clock evidence: %v %s", err, output)
	}
	h := inspect(t, path, proposal(), testTime.Add(time.Minute))
	if h.Decision.Attempts != 1 {
		t.Fatal("blocked process consumed budget", h)
	}
}
