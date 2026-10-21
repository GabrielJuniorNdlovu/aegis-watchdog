package budget

import (
	"reflect"
	"testing"
	"time"
)

var at = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func TestPolicyBoundaries(t *testing.T) {
	p := DefaultPolicy()
	f := Facts{CoverageStart: at.Add(-p.Window), HighWater: at, LastReservation: at.Add(-p.Cooldown)}
	a := Evaluate(f, p, at)
	if !a.Available || !reflect.DeepEqual(a, Evaluate(f, p, at)) {
		t.Fatal(a)
	}
	f.LastReservation = f.LastReservation.Add(time.Nanosecond)
	if a := Evaluate(f, p, at); a.Reason != "cooldown" {
		t.Fatal(a)
	}
	f.LastReservation = time.Time{}
	f.Attempts = p.MaxAttempts
	if a := Evaluate(f, p, at); a.Reason != "budget_exhausted" {
		t.Fatal(a)
	}
	f.UnresolvedID = 1
	if a := Evaluate(f, p, at); a.Reason != "unresolved_action" {
		t.Fatal(a)
	}
	f.HighWater = at.Add(time.Nanosecond)
	if a := Evaluate(f, p, at); !a.ClockUnsafe {
		t.Fatal(a)
	}
	f.HighWater = at
	f.UnresolvedID = 0
	f.Attempts = 0
	p.Window += time.Nanosecond
	if a := Evaluate(f, p, at); a.Reason != "insufficient_history" {
		t.Fatal(a)
	}
}
func TestValidation(t *testing.T) {
	for _, p := range []Policy{{}, {0, time.Hour, time.Minute}, {1, 0, time.Minute}, {1, time.Hour, -1}} {
		if p.Validate() == nil {
			t.Fatal(p)
		}
	}
	for _, s := range []string{"", "/api", "a b", "a?b"} {
		if ValidService(s) {
			t.Fatal(s)
		}
	}
	if !ValidService("orders-api.v1") || ValidateTime(time.Time{}, DefaultPolicy()) == nil {
		t.Fatal("invalid validation")
	}
}
