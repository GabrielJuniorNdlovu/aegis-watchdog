package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

const budgetUsage = "  aegis budget --service <logical-service-id> [--max-attempts 3] [--budget-window 1h] [--cooldown 5m] [--limit <1-1000>] [--db <path>]\n"

func inspectBudgetCommand(ctx context.Context, args []string, now time.Time, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("budget", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, budgetUsage) }
	p := budget.DefaultPolicy()
	var service string
	seen := false
	path := flags.String("db", store.DefaultPath, "host SQLite journal path")
	limit := flags.Int("limit", store.DefaultLimit, "maximum reservations to show (1-1000)")
	flags.IntVar(&p.MaxAttempts, "max-attempts", p.MaxAttempts, "positive maximum attempts in the rolling window")
	flags.DurationVar(&p.Window, "budget-window", p.Window, "positive rolling budget window")
	flags.DurationVar(&p.Cooldown, "cooldown", p.Cooldown, "positive minimum time between reservations")
	flags.Func("service", "explicit stable logical service ID", func(s string) error {
		if seen || !budget.ValidService(s) {
			return errors.New("supply one valid logical service ID")
		}
		seen, service = true, s
		return nil
	})
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(stdout, budgetUsage)
		return 0
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if !seen || flags.NArg() != 0 {
		fmt.Fprint(stderr, budgetUsage)
		return 2
	}
	if strings.TrimSpace(*path) == "" {
		fmt.Fprintln(stderr, "aegis: database path must not be empty")
		return 2
	}
	for _, err := range []error{p.Validate(), budget.ValidateTime(now, p), store.ValidateLimit(*limit)} {
		if err != nil {
			fmt.Fprintln(stderr, "aegis:", err)
			return 2
		}
	}
	h, err := store.InspectBudget(ctx, *path, service, p, now, *limit)
	if err != nil {
		fmt.Fprintln(stderr, "aegis: cannot inspect budget:", err)
		return 1
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Logical service: %s\nEvaluated at: %s\nEvaluation policy: max attempts %d; window %s; cooldown %s\nAttempts within (now - window, now]: %d\nKnown history coverage starts: %s\nClock high-water: %s\nLatest host reservation: %s\nClock unsafe: %t\nCooldown ends: %s\n", service, timestamp(h.EvaluatedAt), p.MaxAttempts, p.Window, p.Cooldown, h.Decision.Attempts, timestamp(h.Facts.CoverageStart), timestamp(h.Facts.HighWater), timestamp(h.Facts.LatestHostReservation), h.Decision.ClockUnsafe, timestamp(h.Decision.CooldownUntil))
	if h.Facts.UnresolvedID != 0 {
		fmt.Fprintf(&out, "Host unresolved reservation: %d (service %s)\n", h.Facts.UnresolvedID, h.Facts.UnresolvedService)
	} else {
		fmt.Fprintln(&out, "Host unresolved reservation: none")
	}
	status := "budget blocked"
	if h.Decision.Available {
		status = "budget available"
	}
	fmt.Fprintf(&out, "Budget status: %s\nReason: %s\nExplanation: %s\n", status, h.Decision.Reason, h.Decision.Explanation)
	if len(h.Reservations) == 0 {
		fmt.Fprintln(&out, "No reservations for this logical service.")
	}
	for _, r := range h.Reservations {
		fmt.Fprintf(&out, "\nReservation: %d\nReserved at: %s\nState: %s\nTarget Docker ID: %s\nReserved policy: max attempts %d; window %s; cooldown %s\n", r.ID, timestamp(r.ReservedAt), r.State, r.ContainerID, r.Policy.MaxAttempts, r.Policy.Window, r.Policy.Cooldown)
		if !r.AcknowledgedAt.IsZero() {
			fmt.Fprintf(&out, "Uncertainty acknowledged at: %s\nOperator reason: %s\n", timestamp(r.AcknowledgedAt), r.AcknowledgmentReason)
		}
		if r.Assessment != nil {
			fmt.Fprintf(&out, "Assessment reference: %d (retained snapshot)\nAssessment evaluated at: %s\nAssessment: %s\nAssessment reason: %s\n", r.AssessmentID, timestamp(r.Assessment.EvaluatedAt), r.Assessment.Status, r.Assessment.Reason)
		}
	}
	fmt.Fprintln(&out, "No restart is authorized or executed. Startup cancels interrupted planned actions; uncertainty requires explicit acknowledgment without a budget refund.")
	if _, err := io.WriteString(stdout, out.String()); err != nil {
		fmt.Fprintln(stderr, "aegis: cannot write budget inspection output")
		return 1
	}
	return 0
}
