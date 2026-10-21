// Package evidence captures bounded diagnostics without changing Docker state.
package evidence

import (
	"context"
	"errors"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/moby/moby/client"
)

const MetadataReserve = 64 * 1024
const MaxEntries = 4096
const CleanupBatch = 32

type Limits struct {
	Deadline    time.Duration
	Lookback    time.Duration
	LogBytes    int64
	BundleBytes int64
	TotalBytes  int64
	Retention   time.Duration
}

func DefaultLimits() Limits {
	return Limits{10 * time.Second, 5 * time.Minute, 64 * 1024, 512 * 1024, 16 * 1024 * 1024, 7 * 24 * time.Hour}
}
func (p Limits) Validate() error {
	if p.Deadline <= 0 || p.Deadline > time.Minute || p.Lookback <= 0 || p.Lookback > 24*time.Hour || p.LogBytes < 1 || p.LogBytes > 1024*1024 || p.BundleBytes < 6*p.LogBytes+MetadataReserve || p.BundleBytes > 8*1024*1024 || p.TotalBytes < p.BundleBytes || p.TotalBytes > 1024*1024*1024 || p.Retention <= 0 {
		return errors.New("invalid evidence limits: positive bounded durations and sufficient JSON/temporary-file allowance are required")
	}
	return nil
}

type Assessment struct {
	EvaluatedAt time.Time
	Status      string
	Reason      string
	Explanation string
	Policy      assessment.Policy
	EvidenceEnd time.Time
}
type Input struct {
	ActionID           int64
	ServiceID          string
	ContainerID        string
	ReservedAt         time.Time
	Policy             budget.Policy
	Assessments        []Assessment
	MissingAssessments bool
}
type Container struct {
	ID        string
	Lifecycle string
	Health    string
	StartedAt time.Time
	CheckTime time.Time
}
type Bundle struct {
	SchemaVersion int `json:"schema_version"`
	Input         Input
	StartedAt     time.Time
	EndedAt       time.Time
	Limits        Limits
	Status        string
	Before        *Container
	After         *Container
	Logs          string
	Truncated     bool
	Issues        []string
}

func (b *Bundle) issue(code string) {
	for _, s := range b.Issues {
		if s == code {
			return
		}
	}
	if len(b.Issues) < 24 {
		b.Issues = append(b.Issues, code)
	}
}
func snapshot(r inspection.Result) *Container {
	return &Container{ID: r.ID, Lifecycle: r.Lifecycle, Health: r.Health, StartedAt: r.Evidence.StartedAt, CheckTime: r.CheckTime}
}

// Collect uses only exact-ID inspect/log requests. No filesystem writes occur here.
func Collect(ctx context.Context, docker *DockerClient, input Input, p Limits, now func() time.Time) (Bundle, error) {
	if err := p.Validate(); err != nil {
		return Bundle{}, err
	}
	if input.ActionID <= 0 || !budget.ValidService(input.ServiceID) || !inspection.ValidID(input.ContainerID) || len(input.Assessments) > 4 {
		return Bundle{}, errors.New("invalid evidence input")
	}
	if err := input.Policy.Validate(); err != nil {
		return Bundle{}, err
	}
	for _, a := range input.Assessments {
		if len(a.Status) > 32 || len(a.Reason) > 128 || len(a.Explanation) > 512 {
			return Bundle{}, errors.New("assessment metadata exceeds evidence allowlist limits")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, p.Deadline)
	defer cancel()
	b := Bundle{SchemaVersion: 1, Input: input, Limits: p, StartedAt: now().UTC(), Status: "complete"}
	if input.MissingAssessments || len(input.Assessments) == 0 {
		b.issue("persisted_assessments_missing_or_omitted")
	}
	before, err := inspection.Inspect(ctx, docker.sdk, input.ContainerID)
	if err != nil {
		category, _ := inspection.FailureDetails(err)
		b.issue("initial_inspection_" + category)
		b.issue("logs_missing")
	} else if before.ID != input.ContainerID {
		b.issue("initial_target_identity_mismatch")
		b.issue("logs_missing")
	} else {
		b.Before = snapshot(before)
		stream, err := docker.sdk.ContainerLogs(ctx, input.ContainerID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Since: b.StartedAt.Add(-p.Lookback).Format(time.RFC3339Nano), Until: b.StartedAt.Format(time.RFC3339Nano), Tail: "200"})
		if err != nil {
			b.issue("logs_collection_failed")
		} else {
			text, truncated, readErr := readLogs(stream, before.TTY, p.LogBytes)
			closeErr := stream.Close()
			b.Logs, b.Truncated = text, truncated
			if truncated {
				b.issue("logs_truncated_or_line_omitted")
			}
			if readErr != nil || closeErr != nil {
				b.issue("logs_stream_incomplete_or_malformed")
			}
		}
		after, err := inspection.Inspect(ctx, docker.sdk, input.ContainerID)
		if err != nil {
			category, _ := inspection.FailureDetails(err)
			b.issue("final_inspection_" + category)
		} else if after.ID != input.ContainerID {
			b.issue("final_target_identity_mismatch")
		} else {
			b.After = snapshot(after)
			if !before.Evidence.StartedAt.Equal(after.Evidence.StartedAt) {
				b.issue("container_execution_changed_during_capture")
			}
			if before.Evidence.StartedAt.IsZero() || after.Evidence.StartedAt.IsZero() {
				b.issue("container_execution_identity_unavailable")
			}
		}
	}
	if ctx.Err() != nil {
		b.issue("collection_canceled_or_deadline")
	}
	b.EndedAt = now().UTC()
	if b.EndedAt.Before(b.StartedAt) {
		b.issue("capture_clock_moved_backward")
	}
	if len(b.Issues) > 0 {
		b.Status = "partial"
	}
	return b, nil
}
