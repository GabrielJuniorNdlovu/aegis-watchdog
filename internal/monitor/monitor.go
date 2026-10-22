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
	Policy       assessment.Policy
	Target       string
	DBPath       string
	Interval     time.Duration
	Retention    time.Duration
	Recovery     recovery.Config
	Dependencies []string
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
	if len(c.Dependencies) > 0 {
		return errors.New("dependency recovery gates are not implemented")
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
	return loopRecovery(ctx, config, collect, func(ctx context.Context, id string) (inspection.Result, error) {
		return inspection.Inspect(ctx, docker, id)
	}, recoveryClient, time.Now, waitInterval, stdout, stderr)
}

// collect, now, and wait are small seams for deterministic scheduling tests.
// Production waits only after a complete collection/write cycle, never on a ticker.
func loop(ctx context.Context, config Config, collect func(context.Context) (inspection.Result, error), inspectExact func(context.Context, string) (inspection.Result, error), now func() time.Time, wait func(context.Context, time.Duration) error, stdout, stderr io.Writer) error {
	return loopRecovery(ctx, config, collect, inspectExact, nil, now, wait, stdout, stderr)
}

func loopRecovery(ctx context.Context, config Config, collect func(context.Context) (inspection.Result, error), inspectExact func(context.Context, string) (inspection.Result, error), recoveryClient *evidence.DockerClient, now func() time.Time, wait func(context.Context, time.Duration) error, stdout, stderr io.Writer) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if config.Recovery.Enabled && recoveryClient == nil {
		return errors.New("recovery enabled without a recovery Docker client")
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
		at := now().UTC()
		if ctx.Err() != nil {
			return nil
		}
		category := ""
		if collectionErr != nil {
			category, _ = inspection.FailureDetails(collectionErr)
		}
		next, evaluated := assessment.Evaluate(state, assessment.Input{Observation: result, CollectedAt: at, ErrorCategory: category}, policy, at)
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
			verification, message, err = recovery.Consider(ctx, config.Recovery, config.DBPath, recoveryClient, recoveryClient, evaluated, result, at)
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
			if _, err := fmt.Fprintf(stdout, "Observed at: %s\n%s", at.Format(time.RFC3339Nano), result.Text(at)); err != nil {
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
