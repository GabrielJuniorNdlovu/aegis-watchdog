// Package recovery coordinates a single explicitly opted-in restart.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/evidence"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
	"github.com/containerd/errdefs"
)

const (
	RequestDeadline = 30 * time.Second
	GracefulStop    = 10 * time.Second
	VerifyDeadline  = 2 * time.Minute
	StartupGrace    = 20 * time.Second
)

type Docker interface {
	Inspect(context.Context, string) (inspection.Result, error)
	Restart(context.Context, string, time.Duration) error
}

type Config struct {
	Enabled, LocalLiveness, RestartAppropriate                       bool
	Service                                                          string
	Budget                                                           budget.Policy
	StartupGrace, GracefulStop, RequestTimeout, VerificationDeadline time.Duration
}

func (c Config) Validate() error {
	if !c.Enabled {
		if c.LocalLiveness || c.RestartAppropriate || c.Service != "" {
			return errors.New("recovery permission flags require --enable-recovery")
		}
		return nil
	}
	if !budget.ValidService(c.Service) || !c.LocalLiveness || !c.RestartAppropriate {
		return errors.New("recovery requires --service, --healthcheck-local-liveness, and --restart-appropriate")
	}
	if err := c.Budget.Validate(); err != nil {
		return err
	}
	if c.StartupGrace < 0 || c.StartupGrace > 10*time.Minute || c.GracefulStop <= 0 || c.GracefulStop > time.Minute || c.RequestTimeout <= c.GracefulStop || c.RequestTimeout > time.Minute || c.VerificationDeadline <= 0 || c.VerificationDeadline > 10*time.Minute {
		return errors.New("recovery grace, restart, and verification deadlines are outside their supported bounds")
	}
	return nil
}

type Verification struct {
	ActionID                                   int64
	Container                                  string
	Policy                                     assessment.Policy
	OldExecution, NewExecution                 string
	StartedAt, Deadline, GraceUntil, LastCheck time.Time
	StartedMono                                time.Time
	DeadlineAfter, GraceAfter                  time.Duration
	Successes                                  int
}

// Consider runs only after an assessment and observation have been durably
// saved. It reserves once, captures evidence, then revalidates before dispatch.
func Consider(ctx context.Context, cfg Config, path string, docker Docker, capture *evidence.DockerClient, a assessment.Assessment, result inspection.Result, now time.Time) (*Verification, string, error) {
	if !cfg.Enabled || !a.Persistent || a.Status != "persistent failure" || a.CollectionErrorCategory != "" || a.Reason == "duplicate_check" {
		return nil, "", nil
	}
	if result.Health != "unhealthy" || !result.Evidence.Configured || result.CheckTime.IsZero() || now.Sub(result.CheckTime) > a.Policy.MaxAge {
		return nil, "recovery blocked: no fresh configured failing Docker health check", nil
	}
	if now.Sub(a.Boundary) < cfg.StartupGrace {
		return nil, "recovery blocked: startup grace has not elapsed", nil
	}
	if result.Lifecycle != "running" {
		return nil, "recovery blocked: container is not running", nil
	}
	if result.RestartPolicy != "" && result.RestartPolicy != "no" {
		return nil, "recovery blocked: Docker restart policy must be no", nil
	}
	if result.RestartPolicy == "" {
		return nil, "recovery blocked: Docker restart policy is unavailable", nil
	}
	reserved, err := store.ReserveLocked(ctx, path, store.Proposal{ServiceID: cfg.Service, ContainerID: result.ID, Policy: cfg.Budget}, now)
	if err != nil {
		return nil, "", err
	}
	if reserved.Reservation == nil {
		return nil, "recovery blocked: " + reserved.Decision.Reason + ": " + reserved.Decision.Explanation, nil
	}
	reservation := reserved.Reservation
	if captured, captureErr := evidence.CaptureLocked(ctx, path, reservation.ID, capture, evidence.DefaultLimits(), time.Now); captureErr != nil {
		issue := "capture_failed"
		if len(captured.Issues) > 0 {
			issue = captured.Issues[0]
		}
		if e := store.TransitionRecovery(context.Background(), path, reservation.ID, "planned", "canceled", time.Now(), "evidence_failed", "Bounded diagnostic evidence was not durably published ("+issue+"); no restart was sent and the reserved attempt remains consumed."); e != nil {
			return nil, "", fmt.Errorf("capture failed (%v), and action cancellation could not be persisted: %w", captureErr, e)
		}
		return nil, "recovery canceled: evidence capture was not durable (" + issue + ")", nil
	}
	validatedAt := time.Now().UTC()
	checkCtx, cancel := context.WithTimeout(ctx, inspection.Timeout)
	current, inspectErr := docker.Inspect(checkCtx, result.ID)
	cancel()
	if inspectErr != nil || current.ID != result.ID || !current.Evidence.StartedAt.Equal(result.Evidence.StartedAt) || current.Lifecycle != "running" || current.Health != "unhealthy" || current.RestartPolicy != "no" || current.CheckTime.IsZero() || validatedAt.Sub(current.CheckTime) > a.Policy.MaxAge || current.CheckTime.Before(a.EvidenceEnd) {
		if err = store.TransitionRecovery(context.Background(), path, reservation.ID, "planned", "canceled", time.Now(), "pre_dispatch_revalidation_failed", "The exact target or fresh unhealthy evidence changed before dispatch; no restart was sent and the reservation remains consumed."); err != nil {
			return nil, "", fmt.Errorf("cannot persist recovery cancellation: %w", err)
		}
		return nil, "recovery canceled: target or eligibility changed before dispatch", nil
	}
	if err = store.TransitionRecovery(ctx, path, reservation.ID, "planned", "dispatching", validatedAt, "dispatching", "The attempt is durably reserved; one bounded restart request will be sent to this exact running container ID."); err != nil {
		return nil, "", err
	}
	requestCtx, requestCancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	restartErr := docker.Restart(requestCtx, result.ID, cfg.GracefulStop)
	requestCancel()
	if restartErr != nil {
		to, reason, explanation := "uncertain", "restart_outcome_ambiguous", "Docker did not provide a confirmed restart response. The request is never retried automatically and this reservation blocks future actions."
		category, _ := inspection.FailureDetails(restartErr)
		explanation = "Docker restart request ended with " + category + " and no confirmed response. It is never retried automatically; this reservation blocks future actions."
		if errdefs.IsNotFound(restartErr) || errdefs.IsInvalidArgument(restartErr) || errdefs.IsPermissionDenied(restartErr) || errdefs.IsUnauthorized(restartErr) || errdefs.IsConflict(restartErr) {
			to, reason, explanation = "failed", "restart_rejected", "Docker definitively rejected the restart request; the reserved attempt remains consumed."
		}
		if err = store.TransitionRecovery(context.Background(), path, reservation.ID, "dispatching", to, time.Now(), reason, explanation); err != nil {
			return nil, "", fmt.Errorf("restart outcome ambiguous and uncertainty could not be persisted: %w", err)
		}
		if to == "failed" {
			return nil, "restart request rejected; inspect the action journal", nil
		}
		return nil, "recovery outcome uncertain; inspect the action journal", nil
	}
	verifiedMono := time.Now()
	verifiedAt := verifiedMono.UTC()
	postCtx, postCancel := context.WithTimeout(context.Background(), inspection.Timeout)
	post, postErr := docker.Inspect(postCtx, result.ID)
	postCancel()
	if postErr != nil || post.ID != result.ID || post.Evidence.StartedAt.IsZero() || post.Evidence.StartedAt.Equal(result.Evidence.StartedAt) {
		if err = store.TransitionRecovery(context.Background(), path, reservation.ID, "dispatching", "uncertain", time.Now(), "new_execution_unconfirmed", "Docker accepted the restart request, but a new execution identity could not be established. No application recovery is claimed."); err != nil {
			return nil, "", fmt.Errorf("cannot persist uncertain restart result: %w", err)
		}
		return nil, "recovery outcome uncertain; inspect the action journal", nil
	}
	verification := Verification{ActionID: reservation.ID, Container: result.ID, Policy: a.Policy, OldExecution: result.Evidence.StartedAt.Format(time.RFC3339Nano), NewExecution: post.Evidence.StartedAt.Format(time.RFC3339Nano), StartedAt: verifiedAt, Deadline: verifiedAt.Add(cfg.VerificationDeadline), GraceUntil: verifiedAt.Add(cfg.StartupGrace), StartedMono: verifiedMono, DeadlineAfter: cfg.VerificationDeadline, GraceAfter: cfg.StartupGrace}
	if err = store.RecordRecoverySession(context.Background(), path, reservation.ID, store.RecoverySession{ContainerID: verification.Container, OldExecution: verification.OldExecution, NewExecution: verification.NewExecution, StartedAt: verification.StartedAt, Deadline: verification.Deadline, GraceUntil: verification.GraceUntil}); err != nil {
		return nil, "", fmt.Errorf("restart accepted but verification identity could not be persisted: %w", err)
	}
	if err = store.TransitionRecovery(context.Background(), path, reservation.ID, "dispatching", "verifying", verifiedAt, "verification_started", fmt.Sprintf("Docker accepted one restart request. Sustained health verification runs until %s; Docker acceptance alone is not application recovery.", verification.Deadline.Format(time.RFC3339))); err != nil {
		return nil, "", fmt.Errorf("restart accepted but verification state could not be persisted: %w", err)
	}
	return &verification, "restart accepted; verifying distinct health checks", nil
}

// Verify consumes each completed health-check identity once; any fault or gap
// breaks the success streak. The monitor reconciles unfinished state on restart.
func Verify(ctx context.Context, path string, v *Verification, r inspection.Result, collectionErr error, now time.Time) (string, bool, error) {
	if v == nil {
		return "", false, nil
	}
	finish := func(to, reason, explanation string) (string, bool, error) {
		if err := store.TransitionRecovery(ctx, path, v.ActionID, "verifying", to, now, reason, explanation); err != nil {
			return "", false, err
		}
		return explanation, true, nil
	}
	deadlineReached := !now.Before(v.Deadline)
	graceElapsed := !now.Before(v.GraceUntil)
	if !v.StartedMono.IsZero() {
		elapsed := time.Since(v.StartedMono)
		deadlineReached = elapsed >= v.DeadlineAfter
		graceElapsed = elapsed >= v.GraceAfter
	}
	if deadlineReached {
		if collectionErr == nil && r.ID == v.Container && r.Evidence.StartedAt.Format(time.RFC3339Nano) == v.NewExecution && r.Health == "unhealthy" && !r.CheckTime.IsZero() && now.Sub(r.CheckTime) <= v.Policy.MaxAge {
			return finish("failed", "verification_deadline_unhealthy", "Fresh Docker health evidence remained unhealthy at the verification deadline.")
		}
		return finish("uncertain", "verification_deadline_unknown", "Sustained recovery could not be established before the deadline.")
	} else if collectionErr != nil {
		v.Successes = 0
	} else if r.ID != v.Container || r.Evidence.StartedAt.Format(time.RFC3339Nano) != v.NewExecution {
		return finish("uncertain", "execution_changed", "Container identity or execution changed again during verification; the restart outcome is uncertain.")
	} else if r.Health != "healthy" || !r.Evidence.Configured || r.Lifecycle != "running" || r.CheckTime.IsZero() || now.Sub(r.CheckTime) > v.Policy.MaxAge {
		v.Successes = 0
	} else if graceElapsed {
		for _, c := range r.Evidence.Checks {
			if c.End.After(v.LastCheck) && c.Start.After(v.GraceUntil) && c.Start.After(v.StartedAt) && now.Sub(c.End) <= v.Policy.MaxAge {
				if !v.LastCheck.IsZero() && c.End.Sub(v.LastCheck) > v.Policy.MaxGap {
					v.Successes = 0
				}
				v.LastCheck = c.End
				if c.ExitCode != nil && *c.ExitCode == 0 {
					v.Successes++
				} else {
					v.Successes = 0
				}
			}
		}
		if v.Successes >= 2 {
			return finish("succeeded", "sustained_health_verified", "Two distinct fresh successful Docker health checks for the new execution established sustained recovery.")
		}
	}
	return "", false, nil
}
