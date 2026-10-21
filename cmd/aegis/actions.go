package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

const actionsUsage = "  aegis actions --id <action-id> [--db <path>]\n  aegis actions acknowledge --id <action-id> --reason <operator-explanation> [--db <path>]\n"

func actionsCommand(ctx context.Context, args []string, at time.Time, stdout, stderr io.Writer) int {
	acknowledge := len(args) > 0 && args[0] == "acknowledge"
	if acknowledge {
		args = args[1:]
	}
	flags := flag.NewFlagSet("actions", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, actionsUsage) }
	path := flags.String("db", store.DefaultPath, "SQLite action journal")
	var id int64
	var reason string
	seenID, seenReason := false, false
	flags.Func("id", "one explicit positive action ID", func(s string) error {
		parsed, err := strconv.ParseInt(s, 10, 64)
		if err != nil || parsed <= 0 || seenID {
			return fmt.Errorf("supply one positive action ID")
		}
		id, seenID = parsed, true
		return nil
	})
	if acknowledge {
		flags.Func("reason", "operator explanation (1-512 UTF-8 bytes, no control characters)", func(s string) error {
			if seenReason {
				return fmt.Errorf("supply one acknowledgment reason")
			}
			if err := store.ValidateAcknowledgmentReason(s); err != nil {
				return err
			}
			reason, seenReason = s, true
			return nil
		})
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(stdout, actionsUsage)
		return 0
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if !seenID || (acknowledge && !seenReason) || flags.NArg() != 0 || strings.TrimSpace(*path) == "" {
		fmt.Fprint(stderr, actionsUsage)
		return 2
	}
	if acknowledge {
		result, err := store.Acknowledge(ctx, *path, id, reason, at)
		if err != nil {
			fmt.Fprintln(stderr, "aegis:", err)
			return 1
		}
		message := "Uncertainty acknowledged"
		if result.AlreadyAcknowledged {
			message = "Already acknowledged; original acknowledgment retained"
		}
		if _, err := fmt.Fprintf(stdout, "%s for action %d.\nAcknowledged at: %s\nReason: %s\nThis permits future policy evaluation; it does not prove success, refund budget, reserve an action, or execute a restart.\n", message, id, timestamp(result.At), result.Reason); err != nil {
			fmt.Fprintln(stderr, "aegis: acknowledgment is recorded, but output failed")
			return 1
		}
		return 0
	}
	action, err := store.InspectAction(ctx, *path, id)
	if err != nil {
		fmt.Fprintln(stderr, "aegis: cannot inspect action:", err)
		return 1
	}
	r := action.Reservation
	var out strings.Builder
	fmt.Fprintf(&out, "Action: %d\nLogical service: %s\nOriginal intent: proposed recovery attempt (reservation only)\nTarget Docker ID: %s\nReserved at: %s\nOriginal policy: max attempts %d; window %s; cooldown %s\nCurrent state: %s\nAction unresolved: %t\n", r.ID, r.ServiceID, r.ContainerID, timestamp(r.ReservedAt), r.Policy.MaxAttempts, r.Policy.Window, r.Policy.Cooldown, r.State, action.Unresolved)
	if action.HostUnresolvedID != 0 {
		fmt.Fprintf(&out, "Host unresolved-action block: action %d\n", action.HostUnresolvedID)
	} else {
		fmt.Fprintln(&out, "Host unresolved-action block: none")
	}
	if r.AcknowledgedAt.IsZero() {
		fmt.Fprintln(&out, "Acknowledgment: none")
	} else {
		fmt.Fprintf(&out, "Acknowledged at: %s\nOperator reason: %s\nAcknowledgment accepts uncertainty; it does not prove earlier success.\n", timestamp(r.AcknowledgedAt), r.AcknowledgmentReason)
	}
	if len(action.Audit) == 0 {
		fmt.Fprintln(&out, "Reconciliation/audit evidence: unavailable (no recorded transition)")
	}
	for _, entry := range action.Audit {
		fmt.Fprintf(&out, "\nAudit: %s\nRecorded at: %s\nTransition: %s -> %s\nOutcome reason: %s\nExplanation: %s\n", entry.Kind, timestamp(entry.RecordedAt), entry.FromState, entry.ToState, entry.Reason, entry.Explanation)
		if e := entry.Evidence; e != nil {
			fmt.Fprintf(&out, "Recorded inspection evidence (historical):\nReturned Docker ID: %s\nLifecycle: %s\nDocker health: %s\nExecution started at: %s\nLast check: %s\nCollection error: %s\n", available(e.ContainerID), available(e.Lifecycle), available(e.Health), timestamp(e.StartedAt), timestamp(e.CheckTime), available(e.ErrorCategory))
		} else {
			fmt.Fprintln(&out, "Inspection evidence: none recorded for this transition")
		}
	}
	fmt.Fprintln(&out, "Budget accounting: the original reservation still counts within applicable rolling windows. No attempt is refunded. No restart is authorized or executed.")
	if _, err := io.WriteString(stdout, out.String()); err != nil {
		fmt.Fprintln(stderr, "aegis: cannot write action inspection output")
		return 1
	}
	return 0
}
