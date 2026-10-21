// Package assessment evaluates observed check outcomes without performing I/O.
package assessment

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
)

const NoAction = "No recovery action is authorized or executed."

type Policy struct {
	FailureThreshold  int
	RecoveryThreshold int
	MaxAge            time.Duration
	MaxGap            time.Duration
}

func DefaultPolicy() Policy { return Policy{3, 2, 30 * time.Second, 30 * time.Second} }
func (p Policy) Validate() error {
	if p.FailureThreshold <= 0 || p.RecoveryThreshold <= 0 || p.MaxAge <= 0 || p.MaxGap <= 0 {
		return errors.New("failure threshold, recovery threshold, max evidence age, and max check gap must be positive")
	}
	return nil
}

type State struct {
	Boundary    time.Time
	EvaluatedAt time.Time
	Policy      Policy
	ID          string
	StartedAt   time.Time
	LastStart   time.Time
	LastEnd     time.Time
	LastExit    int
	Seen        []inspection.Check
	Failures    int
	Successes   int
	Persistent  bool
	Status      string
}

type Input struct {
	Observation   inspection.Result
	CollectedAt   time.Time
	ErrorCategory string
}

type Assessment struct {
	EvaluatedAt     time.Time
	ContainerID     string
	StartedAt       time.Time
	Lifecycle       string
	AggregateHealth string
	Status          string
	Reason          string
	Explanation     string
	Failures        int
	Successes       int
	Persistent      bool
	EvidenceStart   time.Time
	EvidenceEnd     time.Time
	Policy          Policy
}

// NewState deliberately restores no persisted streaks or pre-start checks.
func NewState(start time.Time, policy Policy) State {
	return State{Boundary: start.UTC(), Policy: policy}
}

// Evaluate does not mutate previous state or observation slices. Callers validate policy.
func Evaluate(previous State, in Input, policy Policy, now time.Time) (State, Assessment) {
	s := previous
	r := in.Observation
	now = now.UTC()
	if s.Boundary.IsZero() || s.Policy != policy {
		s = NewState(now, policy)
	}
	a := Assessment{EvaluatedAt: now, Policy: policy}
	if in.ErrorCategory == "" {
		a.ContainerID, a.StartedAt = r.ID, r.Evidence.StartedAt
		a.Lifecycle, a.AggregateHealth = r.Lifecycle, r.Health
	}
	finish := func(status, reason, message string) (State, Assessment) {
		s.Status, s.EvaluatedAt = status, now
		a.Status, a.Reason = status, reason
		a.Failures, a.Successes, a.Persistent = s.Failures, s.Successes, s.Persistent
		a.EvidenceStart, a.EvidenceEnd = s.LastStart, s.LastEnd
		a.Explanation = message + " " + NoAction
		return s, a
	}
	unknown := func(reason, message string) (State, Assessment) {
		s.Failures, s.Successes = 0, 0
		// Preserve the persistent condition, but require checks starting after this gap.
		if now.After(s.Boundary) {
			s.Boundary = now
		}
		next, result := finish("unknown", reason, message)
		next.LastStart, next.LastEnd, next.Seen = time.Time{}, time.Time{}, nil
		return next, result
	}
	if err := policy.Validate(); err != nil {
		return unknown("invalid_policy", err.Error()+".")
	}
	if now.IsZero() || (!s.EvaluatedAt.IsZero() && now.Before(s.EvaluatedAt)) {
		return unknown("clock_regression", "Evaluation time is missing or moved backward; continuity is broken.")
	}
	if in.ErrorCategory != "" {
		return unknown("collection_error", "Docker collection failed; no application health was observed.")
	}
	if !inspection.ValidID(r.ID) || r.Evidence.StartedAt.IsZero() {
		return unknown("missing_identity", "Container or execution identity is unavailable.")
	}
	if s.ID != r.ID || !s.StartedAt.Equal(r.Evidence.StartedAt) {
		// Initial identity uses the process boundary; subsequent changes start fresh now.
		boundary := s.Boundary
		if s.ID != "" {
			boundary = now
		}
		s = NewState(boundary, policy)
		s.ID, s.StartedAt = r.ID, r.Evidence.StartedAt
	}
	if r.Lifecycle != "running" {
		return unknown("lifecycle", fmt.Sprintf("Container lifecycle is %s; application checks cannot establish availability.", r.Lifecycle))
	}
	if in.CollectedAt.IsZero() || in.CollectedAt.After(now) || r.Evidence.StartedAt.After(in.CollectedAt) {
		return unknown("invalid_timestamp", "Collection or execution timestamp is missing or in the future.")
	}
	if now.Sub(in.CollectedAt) > policy.MaxAge {
		return unknown("stale_collection", "The collected observation exceeds the maximum evidence age.")
	}
	if !r.Evidence.Configured {
		return unknown("no_healthcheck", "No Docker health check is configured.")
	}
	if len(r.Evidence.Checks) == 0 {
		return unknown("missing_checks", "Docker supplied no individual completed health checks.")
	}
	checks := slices.Clone(r.Evidence.Checks)
	slices.SortFunc(checks, func(a, b inspection.Check) int {
		if c := a.End.Compare(b.End); c != 0 {
			return c
		}
		return a.Start.Compare(b.Start)
	})
	// Validate the entire response before accepting any of its outcomes.
	for i, c := range checks {
		if c.Start.IsZero() || c.End.IsZero() || c.End.Before(c.Start) || c.ExitCode == nil || *c.ExitCode < 0 {
			return unknown("unusable_check", "An individual check has missing or unusable timestamps or exit code.")
		}
		if c.End.After(in.CollectedAt) || c.Start.After(in.CollectedAt) {
			return unknown("future_check", "A check is dated after collection; host clocks or evidence are inconsistent.")
		}
		if c.Start.Before(s.StartedAt) || c.Start.Before(s.Boundary) || !c.End.After(s.Boundary) {
			continue
		}
		if i > 0 && c.Start.Before(checks[i-1].End) && !(c.Start.Equal(checks[i-1].Start) && c.End.Equal(checks[i-1].End) && checks[i-1].ExitCode != nil && *c.ExitCode == *checks[i-1].ExitCode) {
			return unknown("overlapping_checks", "Individual check executions overlap; continuity is uncertain.")
		}
		if !s.LastEnd.IsZero() && !c.End.After(s.LastEnd) {
			known := false
			for _, old := range s.Seen {
				if c.Start.Equal(old.Start) && c.End.Equal(old.End) && old.ExitCode != nil && *c.ExitCode == *old.ExitCode {
					known = true
					break
				}
			}
			if !known {
				return unknown("out_of_order", "An older or conflicting check arrived after newer evidence; continuity is broken.")
			}
		}
	}
	newest := checks[len(checks)-1]
	if now.Sub(newest.End) > policy.MaxAge {
		return unknown("stale_check", "The latest completed check exceeds the maximum evidence age.")
	}
	processed := false
	for _, c := range checks {
		if c.Start.Before(s.StartedAt) || c.Start.Before(s.Boundary) || !c.End.After(s.Boundary) || !c.End.After(s.LastEnd) {
			continue
		}
		if !s.LastEnd.IsZero() && c.End.Sub(s.LastEnd) > policy.MaxGap {
			return unknown("check_gap", "The gap between observed checks exceeds the maximum; missing outcomes are not inferred.")
		}
		s.LastStart, s.LastEnd, s.LastExit = c.Start, c.End, *c.ExitCode
		processed = true
		if *c.ExitCode == 0 {
			s.Failures = 0
			if s.Persistent {
				s.Successes++
				if s.Successes >= policy.RecoveryThreshold {
					s.Persistent = false
					s.Status = "healthy"
				} else {
					s.Status = "recovering"
				}
			} else {
				s.Successes = 1
				s.Status = "healthy"
			}
		} else {
			s.Successes = 0
			if s.Failures < policy.FailureThreshold {
				s.Failures++
			}
			if s.Failures >= policy.FailureThreshold {
				s.Persistent = true
			}
			if s.Persistent {
				s.Status = "persistent failure"
			} else {
				s.Status = "suspect"
			}
		}
	}
	s.Seen = slices.Clone(checks)
	if !processed {
		if s.LastEnd.IsZero() || !s.LastEnd.After(s.Boundary) {
			return finish("unknown", "awaiting_fresh_check", "Waiting for a completed check that starts after the evidence boundary.")
		}
		if now.Sub(s.LastEnd) > policy.MaxGap {
			return unknown("check_gap", "No new usable check arrived within the maximum gap.")
		}
		return finish(s.Status, "duplicate_check", "No new distinct check; fresh duplicate evidence leaves counts unchanged.")
	}
	return finish(s.Status, "check_"+map[string]string{"healthy": "success", "suspect": "failure", "persistent failure": "persistent_failure", "recovering": "recovery"}[s.Status],
		fmt.Sprintf("Lifecycle %s; Docker aggregate health %s. Consecutive observed checks: %d failed, %d successful; thresholds %d and %d. Docker history is bounded; unobserved checks are not inferred.", r.Lifecycle, r.Health, s.Failures, s.Successes, policy.FailureThreshold, policy.RecoveryThreshold))
}
