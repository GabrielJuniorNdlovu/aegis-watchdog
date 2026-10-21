package assessment

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
)

var epoch = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func check(n int, exit int) inspection.Check {
	return inspection.Check{Start: epoch.Add(time.Duration(n) * time.Second), End: epoch.Add(time.Duration(n)*time.Second + time.Millisecond), ExitCode: &exit}
}
func input(checks ...inspection.Check) Input {
	return Input{Observation: inspection.Result{ID: strings.Repeat("a", 64), Lifecycle: "running", Health: "starting", Evidence: inspection.Evidence{StartedAt: epoch.Add(-time.Hour), Configured: true, Checks: checks}}, CollectedAt: epoch.Add(20 * time.Second)}
}
func TestSequences(t *testing.T) {
	tests := []struct {
		name  string
		exits []int
		want  []string
	}{
		{"transient", []int{1, 0}, []string{"suspect", "healthy"}},
		{"threshold and recovery", []int{1, 1, 1, 0, 0}, []string{"suspect", "suspect", "persistent failure", "recovering", "healthy"}},
		{"interrupted failures", []int{1, 1, 0, 1}, []string{"suspect", "suspect", "healthy", "suspect"}},
		{"recovery failure", []int{1, 1, 1, 0, 1, 0, 0}, []string{"suspect", "suspect", "persistent failure", "recovering", "persistent failure", "recovering", "healthy"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := DefaultPolicy()
			s := NewState(epoch, p)
			for i, exit := range tt.exits {
				in := input(check(i+1, exit))
				before := s
				next, a := Evaluate(s, in, p, in.CollectedAt)
				again, b := Evaluate(before, in, p, in.CollectedAt)
				if !reflect.DeepEqual(next, again) || !reflect.DeepEqual(a, b) {
					t.Fatal("not deterministic")
				}
				if a.Status != tt.want[i] || !strings.Contains(a.Explanation, NoAction) {
					t.Fatalf("step %d: %+v", i, a)
				}
				s = next
			}
		})
	}
}
func TestDuplicatesOverlapAndOrdering(t *testing.T) {
	p := DefaultPolicy()
	s := NewState(epoch, p)
	in := input(check(1, 1), check(2, 1))
	s, a := Evaluate(s, in, p, in.CollectedAt)
	if a.Failures != 2 {
		t.Fatal(a)
	}
	s, a = Evaluate(s, in, p, in.CollectedAt)
	if a.Reason != "duplicate_check" || a.Failures != 2 {
		t.Fatal(a)
	}
	in = input(check(2, 1), check(3, 1))
	s, a = Evaluate(s, in, p, in.CollectedAt)
	if a.Status != "persistent failure" || a.Failures != 3 {
		t.Fatal(a)
	}
	in = input(check(1, 1), check(3, 1))
	_, a = Evaluate(s, in, p, in.CollectedAt)
	if a.Reason != "out_of_order" {
		t.Fatal(a)
	}
}
func TestGapsPreservePersistentCondition(t *testing.T) {
	p := DefaultPolicy()
	s := NewState(epoch, p)
	in := input(check(1, 1), check(2, 1), check(3, 1))
	s, _ = Evaluate(s, in, p, in.CollectedAt)
	in.ErrorCategory = "unavailable"
	s, a := Evaluate(s, in, p, in.CollectedAt)
	if a.Status != "unknown" || !s.Persistent || a.Failures != 0 || a.ContainerID != "" {
		t.Fatal(a)
	}
	for i, want := range []string{"recovering", "healthy"} {
		in = input(check(21+i, 0))
		in.CollectedAt = epoch.Add(time.Duration(22+i) * time.Second)
		s, a = Evaluate(s, in, p, in.CollectedAt)
		if a.Status != want {
			t.Fatal(a)
		}
	}
}
func TestUnknownEvidence(t *testing.T) {
	tests := []struct {
		name, reason string
		change       func(*Input)
	}{
		{"error", "collection_error", func(i *Input) { i.ErrorCategory = "not_found" }},
		{"missing execution", "missing_identity", func(i *Input) { i.Observation.Evidence.StartedAt = time.Time{} }},
		{"stale collection", "stale_collection", func(i *Input) { i.CollectedAt = epoch.Add(-time.Minute) }},
		{"stale check", "stale_check", func(i *Input) { i.Observation.Evidence.Checks = []inspection.Check{check(-40, 1)} }},
		{"no health check", "no_healthcheck", func(i *Input) { i.Observation.Evidence.Configured = false }},
		{"empty history", "missing_checks", func(i *Input) { i.Observation.Evidence.Checks = nil }},
		{"missing end", "unusable_check", func(i *Input) { i.Observation.Evidence.Checks[0].End = time.Time{} }},
		{"missing code", "unusable_check", func(i *Input) { i.Observation.Evidence.Checks[0].ExitCode = nil }},
		{"negative code", "unusable_check", func(i *Input) { n := -1; i.Observation.Evidence.Checks[0].ExitCode = &n }},
		{"future", "future_check", func(i *Input) { i.Observation.Evidence.Checks = []inspection.Check{check(21, 1)} }},
		{"paused", "lifecycle", func(i *Input) { i.Observation.Lifecycle = "paused" }},
		{"stopped", "lifecycle", func(i *Input) { i.Observation.Lifecycle = "exited" }},
		{"restarting", "lifecycle", func(i *Input) { i.Observation.Lifecycle = "restarting" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := DefaultPolicy()
			in := input(check(1, 1))
			tt.change(&in)
			_, a := Evaluate(NewState(epoch, p), in, p, epoch.Add(20*time.Second))
			if a.Status != "unknown" || a.Reason != tt.reason {
				t.Fatalf("%+v", a)
			}
		})
	}
}
func TestBoundariesAndReset(t *testing.T) {
	p := DefaultPolicy()
	p.MaxAge = 10 * time.Second
	p.MaxGap = 10 * time.Second
	s := NewState(epoch, p)
	in := input(check(1, 1))
	in.CollectedAt = in.Observation.Evidence.Checks[0].End.Add(p.MaxAge)
	s, a := Evaluate(s, in, p, in.CollectedAt)
	if a.Status != "suspect" {
		t.Fatal(a)
	}
	// A duplicate is usable at the exact age/gap boundary, stale one nanosecond later.
	_, a = Evaluate(s, in, p, in.CollectedAt)
	if a.Reason != "duplicate_check" {
		t.Fatal(a)
	}
	in.CollectedAt = in.CollectedAt.Add(time.Nanosecond)
	_, a = Evaluate(s, in, p, in.CollectedAt)
	if a.Reason != "stale_check" {
		t.Fatal(a)
	}
	// Gap is inclusive; a larger interval invalidates continuity.
	s = NewState(epoch, p)
	in = input(check(1, 1), check(11, 1))
	in.CollectedAt = epoch.Add(12 * time.Second)
	s, a = Evaluate(s, in, p, in.CollectedAt)
	if a.Failures != 2 {
		t.Fatal(a)
	}
	in = input(check(22, 1))
	in.CollectedAt = epoch.Add(23 * time.Second)
	_, a = Evaluate(s, in, p, in.CollectedAt)
	if a.Reason != "check_gap" {
		t.Fatal(a)
	}
	for _, kind := range []string{"replacement", "restart", "policy", "startup"} {
		t.Run(kind, func(t *testing.T) {
			p := DefaultPolicy()
			in := input(check(1, 1), check(2, 1), check(3, 1))
			s, _ := Evaluate(NewState(epoch, p), in, p, in.CollectedAt)
			switch kind {
			case "replacement":
				in.Observation.ID = strings.Repeat("b", 64)
			case "restart":
				in.Observation.Evidence.StartedAt = epoch.Add(10 * time.Second)
			case "policy":
				p.FailureThreshold++
			case "startup":
				s = NewState(in.CollectedAt, p)
			}
			s, a := Evaluate(s, in, p, in.CollectedAt)
			if a.Status != "unknown" || s.Persistent || s.Failures != 0 {
				t.Fatal(a)
			}
		})
	}
}
func TestPolicyValidation(t *testing.T) {
	for _, p := range []Policy{{}, {0, 2, time.Second, time.Second}, {3, -1, time.Second, time.Second}, {3, 2, 0, time.Second}, {3, 2, time.Second, -1}} {
		if p.Validate() == nil {
			t.Fatal(p)
		}
	}
	if DefaultPolicy().Validate() != nil {
		t.Fatal("default invalid")
	}
}
