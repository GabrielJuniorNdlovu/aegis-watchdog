package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/dependency"
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

type revalidationDocker struct {
	app, dependency inspection.Result
	restarts        int
}

func (d *revalidationDocker) Inspect(_ context.Context, selector string) (inspection.Result, error) {
	if selector == d.app.ID {
		return d.app, nil
	}
	if selector == "database" {
		return d.dependency, nil
	}
	return inspection.Result{}, errors.New("unexpected container selector")
}
func (d *revalidationDocker) Restart(context.Context, string, time.Duration) error {
	d.restarts++
	return nil
}

func TestUnsupportedDockerRestartPolicyBlocksBeforeReservation(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	cfg := Config{Enabled: true, LocalLiveness: true, RestartAppropriate: true, Service: "orders", Budget: budget.DefaultPolicy(), StartupGrace: 0, GracefulStop: GracefulStop, RequestTimeout: RequestDeadline, VerificationDeadline: VerifyDeadline}
	docker := &untouchedDocker{}
	a := assessment.Assessment{Status: "persistent failure", Persistent: true, Boundary: now.Add(-time.Hour), EvidenceEnd: now.Add(-time.Second), Policy: assessment.DefaultPolicy()}
	r := inspection.Result{ID: strings.Repeat("c", 64), Lifecycle: "running", Health: "unhealthy", RestartPolicy: "always", CheckTime: now.Add(-time.Second), Evidence: inspection.Evidence{Configured: true, StartedAt: now.Add(-time.Hour)}}
	message, err := func() (string, error) {
		gate := &dependency.Report{ServiceID: "orders", EvaluatedAt: now, Passed: true}
		_, message, err := Consider(context.Background(), cfg, filepath.Join(t.TempDir(), "never-created.db"), docker, nil, a, r, now, nil, gate, nil)
		return message, err
	}()
	if err != nil || !strings.Contains(message, "restart policy must be no") || docker.calls != 0 {
		t.Fatal(message, err, docker.calls)
	}
}

func TestBlockedDependencyDoesNotReserveOrInspectApplication(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	cfg := Config{Enabled: true, LocalLiveness: true, RestartAppropriate: true, Service: "orders", Budget: budget.DefaultPolicy(), StartupGrace: 0, GracefulStop: GracefulStop, RequestTimeout: RequestDeadline, VerificationDeadline: VerifyDeadline}
	docker := &untouchedDocker{}
	path := filepath.Join(t.TempDir(), "blocked.db")
	a := assessment.Assessment{Status: "persistent failure", Persistent: true, Boundary: now.Add(-time.Hour), EvidenceEnd: now.Add(-time.Second), Policy: assessment.DefaultPolicy()}
	r := inspection.Result{ID: strings.Repeat("c", 64), Lifecycle: "running", Health: "unhealthy", RestartPolicy: "no", CheckTime: now.Add(-time.Second), Evidence: inspection.Evidence{Configured: true, StartedAt: now.Add(-time.Hour)}}
	gate := &dependency.Report{ServiceID: "orders", EvaluatedAt: now, Passed: false, Observations: []dependency.Observation{{ServiceID: "database", ReasonCode: "health_unhealthy", Explanation: "dependency Docker health is not healthy"}}}
	_, message, err := Consider(context.Background(), cfg, path, docker, nil, a, r, now, nil, gate, nil)
	if err != nil || !strings.Contains(message, `database= [health_unhealthy`) || docker.calls != 0 {
		t.Fatalf("dependency block was not honored before reservation: %q %v calls=%d", message, err, docker.calls)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("blocked dependency created a reservation database: %v", err)
	}
}

func TestDependencyExecutionChangeAfterEvidenceCancelsReservedAction(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "changed-dependency.db")
	id := strings.Repeat("c", 64)
	oldDependency := inspection.Result{ID: strings.Repeat("d", 64), Name: "database", Lifecycle: "running", Health: "healthy", RestartPolicy: "no", CheckTime: now.Add(-time.Second), Evidence: inspection.Evidence{Configured: true, StartedAt: now.Add(-time.Hour)}}
	app := inspection.Result{ID: id, Name: "api", Lifecycle: "running", Health: "unhealthy", RestartPolicy: "no", CheckTime: now, Evidence: inspection.Evidence{Configured: true, StartedAt: now.Add(-time.Hour)}}
	newDependency := oldDependency
	newDependency.Evidence.StartedAt = now.Add(-30 * time.Minute)
	docker := &revalidationDocker{app: app, dependency: newDependency}
	manifest := &dependency.Manifest{Services: []dependency.Service{{ID: "api", Container: "api", Dependencies: []string{"database"}}, {ID: "database", Container: "database"}}}
	initial := dependency.Evaluate(*manifest, "api", map[string]dependency.Observation{"database": dependency.FromResult("database", "database", oldDependency, nil, now)}, nil, now, time.Minute)
	if !initial.Passed {
		t.Fatal(initial)
	}
	cfg := Config{Enabled: true, LocalLiveness: true, RestartAppropriate: true, Service: "api", Budget: budget.DefaultPolicy(), StartupGrace: 0, GracefulStop: GracefulStop, RequestTimeout: RequestDeadline, VerificationDeadline: VerifyDeadline}
	a := assessment.Assessment{Status: "persistent failure", Persistent: true, Boundary: now.Add(-time.Hour), EvidenceEnd: now.Add(-2 * time.Second), Policy: assessment.DefaultPolicy()}
	calls := 0
	capture := func(context.Context, string, int64) (string, error) { calls++; return "", nil }
	_, message, err := Consider(ctx, cfg, path, docker, capture, a, app, now, manifest, &initial, func() time.Time { return now.Add(time.Second) })
	if err != nil || !strings.Contains(message, "dependency gate closed") || calls != 1 || docker.restarts != 0 {
		t.Fatalf("replaced dependency did not cancel dispatch: %q %v capture=%d restarts=%d", message, err, calls, docker.restarts)
	}
	action, err := store.InspectAction(ctx, path, 1)
	if err != nil || action.Reservation.State != "canceled" || len(action.Audit) != 1 || action.Audit[0].Reason != "dependency_gate_closed" || !strings.Contains(action.Audit[0].Explanation, "identity_changed") {
		t.Fatalf("dependency cancellation was not durably explained: %#v %v", action, err)
	}
	budgetResult, err := store.InspectBudget(ctx, path, "api", cfg.Budget, now.Add(time.Second), 20)
	if err != nil || budgetResult.Decision.Attempts != 1 {
		t.Fatalf("canceled dependency action refunded its reservation: %#v %v", budgetResult, err)
	}
}

func TestApplicationRecoveryDuringEvidenceCaptureCancelsPlannedRestart(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "application-recovered.db")
	id := strings.Repeat("c", 64)
	initial := inspection.Result{ID: id, Name: "api", Lifecycle: "running", Health: "unhealthy", RestartPolicy: "no", CheckTime: now.Add(-time.Second), Evidence: inspection.Evidence{Configured: true, StartedAt: now.Add(-time.Hour)}}
	current := initial
	current.Health, current.CheckTime = "healthy", now
	docker := &revalidationDocker{app: current}
	cfg := Config{Enabled: true, LocalLiveness: true, RestartAppropriate: true, Service: "api", Budget: budget.DefaultPolicy(), StartupGrace: 0, GracefulStop: GracefulStop, RequestTimeout: RequestDeadline, VerificationDeadline: VerifyDeadline}
	a := assessment.Assessment{Status: "persistent failure", Persistent: true, Boundary: now.Add(-time.Hour), EvidenceEnd: now.Add(-2 * time.Second), Policy: assessment.DefaultPolicy()}
	capture := func(context.Context, string, int64) (string, error) { return "", nil }
	_, _, err := Consider(ctx, cfg, path, docker, capture, a, initial, now, nil, nil, nil)
	if err != nil || docker.restarts != 0 {
		t.Fatalf("recovered application received a restart: %v restarts=%d", err, docker.restarts)
	}
	action, err := store.InspectAction(ctx, path, 1)
	if err != nil || action.Reservation.State != "canceled" || len(action.Audit) != 1 || action.Audit[0].Reason != "pre_dispatch_revalidation_failed" {
		t.Fatalf("recovered application was not durably canceled: %#v %v", action, err)
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
