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
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
	"github.com/moby/moby/client"
)

const (
	DefaultInterval  = 5 * time.Second
	DefaultRetention = 7 * 24 * time.Hour
	PruneInterval    = time.Minute
)

type Config struct {
	Policy    assessment.Policy
	Target    string
	DBPath    string
	Interval  time.Duration
	Retention time.Duration
}

func (c Config) Validate() error {
	if !inspection.ValidSelector(c.Target) {
		return errors.New("monitor requires an explicit container name or ID")
	}
	if strings.TrimSpace(c.DBPath) == "" {
		return errors.New("database path must not be empty")
	}
	if c.Policy != (assessment.Policy{}) {
		if err := c.Policy.Validate(); err != nil {
			return err
		}
	}
	if c.Interval <= 0 {
		return errors.New("interval must be positive")
	}
	if c.Retention <= 0 {
		return errors.New("retention must be positive")
	}
	return nil
}

func Run(ctx context.Context, docker *client.Client, config Config, stdout, stderr io.Writer) error {
	collect := func(ctx context.Context) (inspection.Result, error) {
		requestCtx, cancel := context.WithTimeout(ctx, inspection.Timeout)
		defer cancel()
		return inspection.Inspect(requestCtx, docker, config.Target)
	}
	return loop(ctx, config, collect, time.Now, waitInterval, stdout, stderr)
}

// collect, now, and wait are small seams for deterministic scheduling tests.
// Production waits only after a complete collection/write cycle, never on a ticker.
func loop(ctx context.Context, config Config, collect func(context.Context) (inspection.Result, error), now func() time.Time, wait func(context.Context, time.Duration) error, stdout, stderr io.Writer) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	lock, err := acquireLock(config.DBPath)
	if err != nil {
		return err
	}
	defer lock.Close()
	policy := config.Policy
	if policy == (assessment.Policy{}) {
		policy = assessment.DefaultPolicy()
	}
	state := assessment.NewState(now(), policy)
	var lastPrune time.Time
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
