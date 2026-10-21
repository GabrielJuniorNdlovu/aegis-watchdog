package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func assessmentHistory(ctx context.Context, path, target string, limit int, now time.Time, stdout, stderr io.Writer) int {
	rows, err := store.AssessmentHistory(ctx, path, target, limit)
	if err != nil {
		fmt.Fprintln(stderr, "aegis: cannot read assessments:", err)
		return 1
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Assessment history for requested target: %s\nHistorical assessments; not current health. Read at: %s\n", target, now.Format(time.RFC3339Nano))
	if len(rows) == 0 {
		fmt.Fprintln(&out, "No saved assessments.")
	}
	for _, a := range rows {
		id := a.ContainerID
		if id == "" {
			id = "unavailable (collection failed)"
		}
		fmt.Fprintf(&out, "\nEvaluated at: %s\nCollected at: %s\nDocker ID: %s\nExecution started at: %s\nEvidence boundary: %s\nLifecycle: %s\nDocker aggregate health: %s\nAssessment: %s\nReason: %s\nConsecutive observed failures: %d\nConsecutive observed successes: %d\nPersistent failure active: %t\nLast evidence start: %s\nLast evidence end: %s\nEvidence age at evaluation: %s\nEvidence age now: %s\n",
			timestamp(a.EvaluatedAt), timestamp(a.CollectedAt), id, timestamp(a.StartedAt), timestamp(a.Boundary), available(a.Lifecycle), available(a.AggregateHealth), a.Status, a.Reason, a.Failures, a.Successes, a.Persistent, timestamp(a.EvidenceStart), timestamp(a.EvidenceEnd), evidenceAge(a.EvidenceEnd, a.EvaluatedAt), evidenceAge(a.EvidenceEnd, now))
		if a.EvidenceExitCode != nil {
			fmt.Fprintf(&out, "Last evidence exit code: %d\n", *a.EvidenceExitCode)
		}
		if a.CollectionErrorCategory != "" {
			fmt.Fprintf(&out, "Collection error category: %s\n", a.CollectionErrorCategory)
		}
		fmt.Fprintf(&out, "Policy: failure threshold %d; recovery threshold %d; max evidence age %s; max check gap %s\nExplanation: %s\n", a.Policy.FailureThreshold, a.Policy.RecoveryThreshold, a.Policy.MaxAge, a.Policy.MaxGap, a.Explanation)
	}
	if _, err := io.WriteString(stdout, out.String()); err != nil {
		fmt.Fprintln(stderr, "aegis: cannot write assessment history output")
		return 1
	}
	return 0
}
func timestamp(at time.Time) string {
	if at.IsZero() {
		return "unavailable"
	}
	return at.UTC().Format(time.RFC3339Nano)
}
func available(s string) string {
	if s == "" {
		return "unavailable"
	}
	return s
}
func evidenceAge(at, now time.Time) string {
	if at.IsZero() {
		return "unavailable"
	}
	if at.After(now) {
		return "unknown (evidence timestamp is in the future)"
	}
	return now.Sub(at).String()
}
