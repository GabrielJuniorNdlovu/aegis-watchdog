// Package monitor collects one target serially, without recovery actions.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/dependency"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/evidence"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/recovery"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
	"github.com/moby/moby/client"
)

const (
	DefaultInterval  = 5 * time.Second
	DefaultRetention = 7 * 24 * time.Hour
	PruneInterval    = time.Minute
)

type Config struct {
	Policy          assessment.Policy
	Target          string
	DBPath          string
	Interval        time.Duration
	Retention       time.Duration
	Recovery        recovery.Config
	ServiceManifest *dependency.Manifest
}

func (c Config) Validate() error {
	if !inspection.ValidSelector(c.Target) {
		return errors.New("monitor requires an explicit container name or ID")
	}
	if strings.TrimSpace(c.DBPath) == "" {
		return errors.New("database path must not be empty")
	}
	if err := c.Policy.Validate(); err != nil {
		return err
	}
	if c.Interval <= 0 {
		return errors.New("interval must be positive")
	}
	if c.Retention <= 0 {
		return errors.New("retention must be positive")
	}
	if err := c.Recovery.Validate(); err != nil {
		return err
	}
	if c.ServiceManifest != nil {
		if !c.Recovery.Enabled {
			return errors.New("service dependency gates require explicitly enabled recovery")
		}
		if err := c.ServiceManifest.ValidateTarget(c.Recovery.Service, c.Target); err != nil {
			return err
		}
	}
	return nil
}

func Run(ctx context.Context, docker *client.Client, config Config, stdout, stderr io.Writer) error {
	collect := func(ctx context.Context) (inspection.Result, error) {
		requestCtx, cancel := context.WithTimeout(ctx, inspection.Timeout)
		defer cancel()
		return inspection.Inspect(requestCtx, docker, config.Target)
	}
	var recoveryClient *evidence.DockerClient
	if config.Recovery.Enabled {
		var err error
		recoveryClient, err = evidence.NewDockerClient(client.FromEnv)
		if err != nil {
			return fmt.Errorf("create bounded recovery Docker client: %w", err)
		}
		defer recoveryClient.Close()
	}
	return loopConfigured(ctx, config, collect, func(ctx context.Context, id string) (inspection.Result, error) {
		return inspection.Inspect(ctx, docker, id)
	}, recoveryClient, recoveryClient, time.Now, waitInterval, stdout, stderr)
}

// collect, now, and wait are small seams for deterministic scheduling tests.
// Production waits only after a complete collection/write cycle, never on a ticker.
func loop(ctx context.Context, config Config, collect func(context.Context) (inspection.Result, error), inspectExact func(context.Context, string) (inspection.Result, error), now func() time.Time, wait func(context.Context, time.Duration) error, stdout, stderr io.Writer) error {
	return loopRecovery(ctx, config, collect, inspectExact, nil, now, wait, stdout, stderr)
}

func loopRecovery(ctx context.Context, config Config, collect func(context.Context) (inspection.Result, error), inspectExact func(context.Context, string) (inspection.Result, error), recoveryClient *evidence.DockerClient, now func() time.Time, wait func(context.Context, time.Duration) error, stdout, stderr io.Writer) error {
	return loopConfigured(ctx, config, collect, inspectExact, recoveryClient, nil, now, wait, stdout, stderr)
}

func loopConfigured(ctx context.Context, config Config, collect func(context.Context) (inspection.Result, error), inspectExact func(context.Context, string) (inspection.Result, error), recoveryClient *evidence.DockerClient, dependencyInspector dependency.Inspector, now func() time.Time, wait func(context.Context, time.Duration) error, stdout, stderr io.Writer) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if config.Recovery.Enabled && recoveryClient == nil {
		return errors.New("recovery enabled without a recovery Docker client")
	}
	if config.ServiceManifest != nil && dependencyInspector == nil {
		return errors.New("service manifest configured without a dependency inspector")
	}
	if ctx.Err() != nil {
		return nil
	}
	lock, err := store.ReconcileStartup(ctx, config.DBPath, inspectExact, now)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer lock.Close()
	if err := evidence.ReconcileLocked(ctx, config.DBPath, now()); err != nil {
		return persistenceError(ctx, fmt.Errorf("evidence reconciliation failed: %w", err))
	}
	policy := config.Policy
	state := assessment.NewState(now(), policy)
	var lastPrune time.Time
	var verification *recovery.Verification
	for {
		if ctx.Err() != nil {
			return nil
		}
		result, collectionErr := collect(ctx)
		collectedAt := now().UTC()
		if ctx.Err() != nil {
			return nil
		}
		var gateReport *dependency.Report
		if config.ServiceManifest != nil {
			service, _ := config.ServiceManifest.Service(config.Recovery.Service)
			if len(service.Dependencies) > 0 {
				var targetObservation *dependency.Observation
				if collectionErr == nil {
					observed := dependency.FromResult(config.Recovery.Service, config.Target, result, nil, collectedAt)
					targetObservation = &observed
				}
				observations := dependency.Collect(ctx, *config.ServiceManifest, config.Recovery.Service, dependencyInspector, targetObservation, now)
				at := now().UTC()
				report := dependency.Evaluate(*config.ServiceManifest, config.Recovery.Service, observations, nil, at, policy.MaxAge)
				gateReport = &report
			}
		}
		at := now().UTC()
		category := ""
		if collectionErr != nil {
			category, _ = inspection.FailureDetails(collectionErr)
		}
		next, evaluated := assessment.Evaluate(state, assessment.Input{Observation: result, CollectedAt: collectedAt, ErrorCategory: category}, policy, at)
		if gateReport != nil {
			evaluated.DependencyGate = gateReport
			evaluated.Explanation += " " + gateReport.Summary()
		}
		if err := store.SaveMonitoringResult(ctx, config.DBPath, config.Target, result, collectionErr, evaluated); err != nil {
			return persistenceError(ctx, err)
		}
		state = next
		if verification != nil {
			message, done, verifyErr := recovery.Verify(ctx, config.DBPath, verification, result, collectionErr, at)
			if verifyErr != nil {
				return persistenceError(ctx, fmt.Errorf("persist recovery verification: %w", verifyErr))
			}
			if message != "" {
				if _, err := fmt.Fprintln(stdout, "Recovery:", message); err != nil {
					return err
				}
			}
			if done {
				verification = nil
			}
		}
		if verification == nil && config.Recovery.Enabled && collectionErr == nil {
			var message string
			capture := func(ctx context.Context, path string, id int64) (string, error) {
				captured, err := evidence.CaptureLocked(ctx, path, id, recoveryClient, evidence.DefaultLimits(), time.Now)
				if len(captured.Issues) > 0 {
					return captured.Issues[0], err
				}
				return "", err
			}
			verification, message, err = recovery.Consider(ctx, config.Recovery, config.DBPath, recoveryClient, capture, evaluated, result, at, config.ServiceManifest, gateReport, now)
			if err != nil {
				return persistenceError(ctx, fmt.Errorf("coordinate recovery: %w", err))
			}
			if message != "" {
				if _, err := fmt.Fprintln(stdout, "Recovery:", message); err != nil {
					return err
				}
			}
		}
		if collectionErr != nil {
			category, message := inspection.FailureDetails(collectionErr)
			if _, err := fmt.Fprintf(stderr, "Collection error at: %s\nTarget: %s\nCategory: %s\nExplanation: %s\n", at.Format(time.RFC3339Nano), config.Target, category, message); err != nil {
				return fmt.Errorf("write monitor error output: %w", err)
			}
		} else {
			resultText := result.Text(at)
			if gateReport != nil && !gateReport.Passed {
				resultText = strings.Replace(resultText, "Action: none — inspection only.", "Action: none — dependency gate blocked recovery.", 1)
			}
			if _, err := fmt.Fprintf(stdout, "Observed at: %s\n%s", at.Format(time.RFC3339Nano), resultText); err != nil {
				return fmt.Errorf("write monitor output: %w", err)
			}
		}
		writer := stdout
		if collectionErr != nil {
			writer = stderr
		}
		if _, err := fmt.Fprintf(writer, "Assessment: %s\nReason: %s\nExplanation: %s\n", evaluated.Status, evaluated.Reason, evaluated.Explanation); err != nil {
			return fmt.Errorf("write assessment output: %w", err)
		}
		pruneAt := now()
		if lastPrune.IsZero() || pruneAt.Sub(lastPrune) >= PruneInterval {
			if _, err := store.Prune(ctx, config.DBPath, pruneAt.Add(-config.Retention)); err != nil {
				return persistenceError(ctx, fmt.Errorf("retention pruning failed: %w", err))
			}
			if _, err := evidence.CleanupLocked(ctx, config.DBPath, pruneAt); err != nil {
				return persistenceError(ctx, fmt.Errorf("evidence retention failed: %w", err))
			}
			lastPrune = pruneAt
		}
		if err := wait(ctx, config.Interval); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

func persistenceError(ctx context.Context, err error) error {
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return nil
	}
	return fmt.Errorf("monitor stopped: cannot persist history: %w", err)
}

func waitInterval(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
