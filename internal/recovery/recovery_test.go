package recovery

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func TestRecoveryRequiresBothExplicitOptIns(t *testing.T) {
	base := Config{Budget: budget.DefaultPolicy(), StartupGrace: StartupGrace, GracefulStop: GracefulStop, RequestTimeout: RequestDeadline, VerificationDeadline: VerifyDeadline}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	base.LocalLiveness = true
	if err := base.Validate(); err == nil {
		t.Fatal("partial permission accepted")
	}
	base.LocalLiveness = false
	base.Enabled, base.Service, base.RestartAppropriate = true, "orders", true
	if err := base.Validate(); err == nil {
		t.Fatal("recovery without liveness declaration accepted")
	}
	base.LocalLiveness = true
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
}

type untouchedDocker struct{ calls int }

func (d *untouchedDocker) Inspect(context.Context, string) (inspection.Result, error) {
	d.calls++
	return inspection.Result{}, nil
}
func (d *untouchedDocker) Restart(context.Context, string, time.Duration) error {
	d.calls++
	return nil
}

func TestUnsupportedDockerRestartPolicyBlocksBeforeReservation(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	cfg := Config{Enabled: true, LocalLiveness: true, RestartAppropriate: true, Service: "orders", Budget: budget.DefaultPolicy(), StartupGrace: 0, GracefulStop: GracefulStop, RequestTimeout: RequestDeadline, VerificationDeadline: VerifyDeadline}
	docker := &untouchedDocker{}
	a := assessment.Assessment{Status: "persistent failure", Persistent: true, Boundary: now.Add(-time.Hour), EvidenceEnd: now.Add(-time.Second), Policy: assessment.DefaultPolicy()}
	r := inspection.Result{ID: strings.Repeat("c", 64), Lifecycle: "running", Health: "unhealthy", RestartPolicy: "always", CheckTime: now.Add(-time.Second), Evidence: inspection.Evidence{Configured: true, StartedAt: now.Add(-time.Hour)}}
	message, err := func() (string, error) {
		_, message, err := Consider(context.Background(), cfg, filepath.Join(t.TempDir(), "never-created.db"), docker, nil, a, r, now)
		return message, err
	}()
	if err != nil || !strings.Contains(message, "restart policy must be no") || docker.calls != 0 {
		t.Fatal(message, err, docker.calls)
	}
}

func TestVerifyUsesDistinctFreshChecksAndPersistsTerminalResult(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recovery.db")
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	policy := assessment.DefaultPolicy()
	id := strings.Repeat("a", 64)
	reserved, err := store.Reserve(ctx, path, store.Proposal{ServiceID: "orders", ContainerID: id, Policy: budget.DefaultPolicy()}, at)
	if err != nil || reserved.Reservation == nil {
		t.Fatal(reserved, err)
	}
	if err := store.TransitionRecovery(ctx, path, reserved.Reservation.ID, "planned", "dispatching", at, "dispatching", "durable before request"); err != nil {
		t.Fatal(err)
	}
	session := store.RecoverySession{ContainerID: id, OldExecution: at.Format(time.RFC3339Nano), NewExecution: at.Add(time.Second).Format(time.RFC3339Nano), StartedAt: at.Add(time.Second), Deadline: at.Add(time.Minute), GraceUntil: at.Add(2 * time.Second)}
	if err := store.RecordRecoverySession(ctx, path, reserved.Reservation.ID, session); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionRecovery(ctx, path, reserved.Reservation.ID, "dispatching", "verifying", at.Add(time.Second), "verification_started", "verifying"); err != nil {
		t.Fatal(err)
	}
	verification := &Verification{ActionID: reserved.Reservation.ID, Container: id, NewExecution: at.Add(time.Second).Format(time.RFC3339Nano), StartedAt: at.Add(time.Second), GraceUntil: at.Add(time.Second), Deadline: at.Add(time.Minute), Policy: policy}
	zero := 0
	result := inspection.Result{ID: id, Lifecycle: "running", Health: "healthy", CheckTime: at.Add(4 * time.Second), Evidence: inspection.Evidence{Configured: true, StartedAt: at.Add(time.Second), Checks: []inspection.Check{{Start: at.Add(3 * time.Second), End: at.Add(4 * time.Second), ExitCode: &zero}}}}
	if _, done, err := Verify(ctx, path, verification, result, nil, at.Add(5*time.Second)); err != nil || done {
		t.Fatal(done, err)
	}
	if _, done, err := Verify(ctx, path, verification, result, nil, at.Add(6*time.Second)); err != nil || done {
		t.Fatal("duplicate check advanced verification", done, err)
	}
	result.Evidence.Checks = append(result.Evidence.Checks, inspection.Check{Start: at.Add(7 * time.Second), End: at.Add(8 * time.Second), ExitCode: &zero})
	result.CheckTime = at.Add(8 * time.Second)
	message, done, err := Verify(ctx, path, verification, result, nil, at.Add(9*time.Second))
	if err != nil || !done || !strings.Contains(message, "Two distinct fresh") {
		t.Fatal(message, done, err)
	}
	action, err := store.InspectAction(ctx, path, reserved.Reservation.ID)
	if err != nil || action.Reservation.State != "succeeded" || len(action.Audit) != 3 || action.Verification == nil || !action.Verification.Deadline.Equal(session.Deadline) {
		t.Fatal(action, err)
	}
	if budgetResult, err := store.InspectBudget(ctx, path, "orders", budget.DefaultPolicy(), at.Add(10*time.Second), 20); err != nil || budgetResult.Decision.Attempts != 1 {
		t.Fatal(budgetResult, err)
	}
}

func TestVerifyBreaksStreakAndDoesNotClaimSuccessAtDeadline(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "deadline.db")
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	id := strings.Repeat("b", 64)
	r, err := store.Reserve(ctx, path, store.Proposal{ServiceID: "payments", ContainerID: id, Policy: budget.DefaultPolicy()}, at)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.TransitionRecovery(ctx, path, r.Reservation.ID, "planned", "dispatching", at, "dispatching", "")
	_ = store.TransitionRecovery(ctx, path, r.Reservation.ID, "dispatching", "verifying", at.Add(time.Second), "verification_started", "")
	v := &Verification{ActionID: r.Reservation.ID, Container: id, NewExecution: at.Add(time.Second).Format(time.RFC3339Nano), StartedAt: at.Add(time.Second), GraceUntil: at.Add(time.Second), Deadline: at.Add(10 * time.Second), Policy: assessment.DefaultPolicy()}
	failed := 1
	running := inspection.Result{ID: id, Lifecycle: "running", Health: "healthy", CheckTime: at.Add(3 * time.Second), Evidence: inspection.Evidence{Configured: true, StartedAt: at.Add(time.Second), Checks: []inspection.Check{{Start: at.Add(2 * time.Second), End: at.Add(3 * time.Second), ExitCode: ptr(0)}}}}
	_, _, _ = Verify(ctx, path, v, running, nil, at.Add(4*time.Second))
	running.Evidence.Checks = append(running.Evidence.Checks, inspection.Check{Start: at.Add(4 * time.Second), End: at.Add(5 * time.Second), ExitCode: &failed})
	running.CheckTime = at.Add(5 * time.Second)
	_, _, _ = Verify(ctx, path, v, running, nil, at.Add(6*time.Second))
	if v.Successes != 0 {
		t.Fatal("failed check did not reset streak", v.Successes)
	}
	message, done, err := Verify(ctx, path, v, running, nil, v.Deadline)
	if err != nil || !done || !strings.Contains(message, "could not be established") {
		t.Fatal(message, done, err)
	}
	action, err := store.InspectAction(ctx, path, r.Reservation.ID)
	if err != nil || action.Reservation.State != "uncertain" {
		t.Fatal(action, err)
	}
}

func ptr(n int) *int { return &n }
