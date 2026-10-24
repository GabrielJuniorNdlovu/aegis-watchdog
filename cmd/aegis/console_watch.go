package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func consoleWatch(ctx context.Context, args []string, path string, out io.Writer) error {
	flags := flag.NewFlagSet("/watch", flag.ContinueOnError)
	flags.SetOutput(out)
	db := flags.String("db", path, "read-only journal")
	interval := flags.Duration("interval", 2*time.Second, "poll interval (200ms to 1m)")
	var target string
	seen := false
	flags.Func("container", "exact requested monitoring selector", func(s string) error {
		if seen || !inspection.ValidSelector(s) {
			return fmt.Errorf("supply one explicit container selector")
		}
		target, seen = s, true
		return nil
	})
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || target == "" || strings.TrimSpace(*db) == "" || *interval < 200*time.Millisecond || *interval > time.Minute {
		return fmt.Errorf("usage: /watch --container <requested-name-or-id> [--db <path>] [--interval <200ms-1m>]")
	}
	if _, err := fmt.Fprintf(out, "Following stored assessments in %s for %s. Newest snapshot and freshness changes only; Ctrl+C returns to the prompt.\n", *db, target); err != nil {
		return err
	}
	last := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := store.AssessmentHistory(ctx, *db, target, 1)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		key := "empty"
		var snapshot strings.Builder
		if len(rows) == 0 {
			fmt.Fprintf(&snapshot, "No saved assessments for requested target %s. Waiting for a separate monitor to write history.\n", target)
		} else {
			row := rows[0]
			collected := consoleFreshness(row.CollectedAt, now, row.Policy.MaxAge)
			health := consoleFreshness(row.EvidenceEnd, now, row.Policy.MaxAge)
			identity, err := json.Marshal(row)
			if err != nil {
				return fmt.Errorf("identify stored assessment: %w", err)
			}
			key = string(identity) + "/" + collected + "/" + health
			fmt.Fprintf(&snapshot, "Historical assessment read at: %s\nEvaluated at: %s\nCollected at: %s (age at read: %s; %s)\nDocker ID: %s\nAssessment: %s\nReason: %s\nHealth evidence: %s (age at read: %s; %s; recorded max age: %s)\nExplanation: %s\n", timestamp(now), timestamp(row.EvaluatedAt), timestamp(row.CollectedAt), evidenceAge(row.CollectedAt, now), collected, available(row.ContainerID), row.Status, row.Reason, timestamp(row.EvidenceEnd), evidenceAge(row.EvidenceEnd, now), health, row.Policy.MaxAge, row.Explanation)
		}
		if key != last {
			if _, err := io.WriteString(out, snapshot.String()); err != nil {
				return err
			}
			last = key
		}
		timer := time.NewTimer(*interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func consoleFreshness(at, now time.Time, maxAge time.Duration) string {
	switch {
	case at.IsZero():
		return "unavailable"
	case at.After(now):
		return "unknown (future timestamp)"
	case maxAge <= 0:
		return "unknown (no valid age policy)"
	case now.Sub(at) > maxAge:
		return "stale"
	default:
		return "fresh at read"
	}
}
