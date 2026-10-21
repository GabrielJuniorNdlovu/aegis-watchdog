// Package budget evaluates reservation limits using explicit facts and time.
package budget

import (
	"errors"
	"regexp"
	"time"
)

type Policy struct {
	MaxAttempts      int
	Window, Cooldown time.Duration
}

func DefaultPolicy() Policy { return Policy{3, time.Hour, 5 * time.Minute} }
func (p Policy) Validate() error {
	if p.MaxAttempts <= 0 || p.Window <= 0 || p.Cooldown <= 0 {
		return errors.New("maximum attempts, budget window, and cooldown must be positive")
	}
	return nil
}

var servicePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func ValidService(id string) bool { return servicePattern.MatchString(id) }
func ValidateTime(now time.Time, p Policy) error {
	if now.IsZero() || now.Year() < 1 || now.Year() > 9999 || now.Add(-p.Window).Year() < 1 || now.Add(p.Cooldown).Year() > 9999 {
		return errors.New("evaluation time or policy boundary is outside the supported UTC range")
	}
	return nil
}

type Facts struct {
	CoverageStart         time.Time
	HighWater             time.Time
	LastReservation       time.Time
	LatestHostReservation time.Time
	Attempts              int
	UnresolvedID          int64
	UnresolvedService     string
}
type Decision struct {
	Available           bool
	Reason, Explanation string
	Attempts            int
	CooldownUntil       time.Time
	ClockUnsafe         bool
}

// Evaluate treats all journal states as consumed attempts. Equality ends cooldown;
// attempts at the left window boundary are excluded by the caller's SQL query.
func Evaluate(f Facts, p Policy, now time.Time) Decision {
	d := Decision{Attempts: f.Attempts}
	block := func(reason, message string) Decision { d.Reason, d.Explanation = reason, message; return d }
	if p.Validate() != nil || ValidateTime(now, p) != nil {
		return block("invalid_input", "Policy or evaluation time is invalid.")
	}
	if !f.LastReservation.IsZero() {
		d.CooldownUntil = f.LastReservation.Add(p.Cooldown)
	}
	d.ClockUnsafe = now.Before(f.HighWater) || now.Before(f.LatestHostReservation) || now.Before(f.CoverageStart)
	if d.ClockUnsafe {
		return block("clock_unsafe", "Time is earlier than durable timing evidence; wait until the recorded high-water time is reached.")
	}
	if f.UnresolvedID != 0 {
		return block("unresolved_action", "An unresolved reservation blocks new reservations across this host journal; it does not expire automatically.")
	}
	if f.CoverageStart.IsZero() || now.Add(-p.Window).Before(f.CoverageStart) {
		return block("insufficient_history", "The requested window extends before known journal coverage; missing history is not assumed empty.")
	}
	if f.Attempts >= p.MaxAttempts {
		return block("budget_exhausted", "The rolling reservation budget is exhausted; committed attempts are never refunded.")
	}
	if !d.CooldownUntil.IsZero() && now.Before(d.CooldownUntil) {
		return block("cooldown", "The cooldown since the last reservation has not ended.")
	}
	d.Available = true
	d.Reason = "budget_available"
	d.Explanation = "Budget available. Other recovery prerequisites are not evaluated; no restart is authorized or executed."
	return d
}
