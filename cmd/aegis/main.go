package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/dependency"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/monitor"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/recovery"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
	"github.com/moby/moby/client"
)

const usage = "Usage:\n" +
	"  aegis inspect --container <name-or-id> [--save] [--db <path>]\n" +
	"  aegis history --container <full-docker-id> [--limit <1-1000>] [--db <path>]\n" +
	"  aegis errors --container <requested-name-or-id> [--limit <1-1000>] [--db <path>]\n" +
	"  aegis assessments --container <requested-name-or-id> [--limit <1-1000>] [--db <path>]\n" +
	"  aegis monitor --container <name-or-id> [--interval <duration>] [--retention <duration>] [--db <path>] [assessment options] [recovery options]\n" +
	budgetUsage + actionsUsage + welcomeUsage +
	"Assessment options: --failure-threshold 3 --recovery-threshold 2 --max-evidence-age 1m --max-check-gap 1m\n" +
	"Recovery options (all required): --enable-recovery --service <logical-id> --healthcheck-local-liveness --restart-appropriate [--service-config <json-file>]\n"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if len(args) > 0 && args[0] == "actions" {
		return actionsCommand(ctx, args[1:], time.Now().UTC(), stdout, stderr)
	}
	if len(args) > 0 && args[0] == "welcome" {
		return welcomeCommand(args[1:], stdout, stderr, os.Getenv, detectTerminal(stdout))
	}
	if len(args) > 0 && args[0] == "budget" {
		return inspectBudgetCommand(ctx, args[1:], time.Now().UTC(), stdout, stderr)
	}
	if len(args) == 0 || (args[0] != "inspect" && args[0] != "history" && args[0] != "errors" && args[0] != "monitor" && args[0] != "assessments") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, usage) }
	var selector string
	var save bool
	limit := store.DefaultLimit
	interval := monitor.DefaultInterval
	retention := monitor.DefaultRetention
	policy := assessment.DefaultPolicy()
	recoveryEnabled, localLiveness, restartAppropriate := false, false, false
	service := ""
	serviceConfigPath := ""
	budgetPolicy := budget.DefaultPolicy()
	startupGrace, stopGrace, requestTimeout, verificationDeadline := recovery.StartupGrace, recovery.GracefulStop, recovery.RequestDeadline, recovery.VerifyDeadline
	path := flags.String("db", store.DefaultPath, "SQLite file path; used by monitor, history, errors, assessments, or inspect --save")
	if args[0] == "inspect" {
		flags.BoolVar(&save, "save", false, "save the successful inspection to SQLite")
	} else if args[0] == "monitor" {
		flags.DurationVar(&interval, "interval", monitor.DefaultInterval, "wait after each completed collection (must be positive)")
		flags.DurationVar(&retention, "retention", monitor.DefaultRetention, "routine history retention (must be positive; default 168h)")
		flags.IntVar(&policy.FailureThreshold, "failure-threshold", policy.FailureThreshold, "distinct failed checks required for persistent failure")
		flags.IntVar(&policy.RecoveryThreshold, "recovery-threshold", policy.RecoveryThreshold, "distinct successful checks required after persistent failure")
		flags.DurationVar(&policy.MaxAge, "max-evidence-age", policy.MaxAge, "maximum age of usable evidence")
		flags.DurationVar(&policy.MaxGap, "max-check-gap", policy.MaxGap, "maximum gap between completed checks")
		flags.BoolVar(&recoveryEnabled, "enable-recovery", false, "enable explicitly opted-in bounded container restarts")
		flags.StringVar(&service, "service", "", "stable logical service identity used for recovery budgets")
		flags.BoolVar(&localLiveness, "healthcheck-local-liveness", false, "declare Docker health checks as local application liveness")
		flags.BoolVar(&restartAppropriate, "restart-appropriate", false, "declare that restarting is appropriate for this service")
		flags.StringVar(&serviceConfigPath, "service-config", "", "restart-only JSON service manifest with direct dependency gates")
		flags.IntVar(&budgetPolicy.MaxAttempts, "recovery-max-attempts", budgetPolicy.MaxAttempts, "maximum consumed recovery attempts in the rolling window")
		flags.DurationVar(&budgetPolicy.Window, "recovery-window", budgetPolicy.Window, "rolling recovery budget window")
		flags.DurationVar(&budgetPolicy.Cooldown, "recovery-cooldown", budgetPolicy.Cooldown, "minimum time between recovery reservations")
		flags.DurationVar(&startupGrace, "recovery-startup-grace", startupGrace, "grace after monitor startup before recovery")
		flags.DurationVar(&stopGrace, "restart-stop-grace", stopGrace, "Docker graceful stop period before SIGKILL")
		flags.DurationVar(&requestTimeout, "restart-request-timeout", requestTimeout, "overall restart request deadline")
		flags.DurationVar(&verificationDeadline, "verification-deadline", verificationDeadline, "maximum time allowed to verify sustained recovery")
	} else {
		flags.IntVar(&limit, "limit", store.DefaultLimit, "maximum observations to show (1-1000)")
	}
	seen := false
	flags.Func("container", "explicit Docker container name or ID", func(value string) error {
		if seen || !inspection.ValidSelector(value) {
			return errors.New("supply one nonempty container name or ID")
		}
		seen, selector = true, value
		return nil
	})
	if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if !seen || flags.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if (save || args[0] != "inspect") && strings.TrimSpace(*path) == "" {
		fmt.Fprintln(stderr, "aegis: database path must not be empty")
		return 2
	}
	if args[0] == "history" || args[0] == "errors" || args[0] == "assessments" {
		if args[0] == "history" && !inspection.ValidID(selector) {
			fmt.Fprintln(stderr, "aegis: history requires a full 64-character Docker ID")
			return 2
		}
		if err := store.ValidateLimit(limit); err != nil {
			fmt.Fprintln(stderr, "aegis:", err)
			return 2
		}
		if args[0] == "assessments" {
			return assessmentHistory(ctx, *path, selector, limit, time.Now().UTC(), stdout, stderr)
		}
		if args[0] == "errors" {
			return errorHistory(ctx, *path, selector, limit, stdout, stderr)
		}
		return history(ctx, *path, selector, limit, stdout, stderr)
	}
	config := monitor.Config{Policy: policy, Target: selector, DBPath: *path, Interval: interval, Retention: retention,
		Recovery: recovery.Config{Enabled: recoveryEnabled, Service: service, LocalLiveness: localLiveness, RestartAppropriate: restartAppropriate, Budget: budgetPolicy, StartupGrace: startupGrace, GracefulStop: stopGrace, RequestTimeout: requestTimeout, VerificationDeadline: verificationDeadline}}
	if serviceConfigPath != "" {
		manifest, err := dependency.Load(serviceConfigPath)
		if err != nil {
			fmt.Fprintln(stderr, "aegis:", err)
			return 2
		}
		config.ServiceManifest = &manifest
	}
	if args[0] == "monitor" {
		if err := config.Validate(); err != nil {
			fmt.Fprintln(stderr, "aegis:", err)
			return 2
		}
	}
	docker, err := client.New(client.FromEnv)
	if err != nil {
		fmt.Fprintln(stderr, "aegis: invalid Docker connection configuration; check Docker environment settings")
		return 1
	}
	defer docker.Close()
	if args[0] == "monitor" {
		if err := monitor.Run(ctx, docker, config, stdout, stderr); err != nil {
			fmt.Fprintln(stderr, "aegis:", err)
			return 1
		}
		return 0
	}
	inspectCtx, cancel := context.WithTimeout(ctx, inspection.Timeout)
	defer cancel()
	result, err := inspection.Inspect(inspectCtx, docker, selector)
	if err != nil {
		fmt.Fprintln(stderr, "aegis:", err)
		return 1
	}
	observedAt := time.Now().UTC()
	output := result.Text(observedAt)
	if save {
		if err := store.Save(ctx, *path, result, observedAt); err != nil {
			fmt.Fprintln(stderr, "aegis: inspection succeeded, but saving failed:", err)
			return 1
		}
		output += "Saved observation at: " + observedAt.Format(time.RFC3339Nano) + "\n"
	}
	if _, err := io.WriteString(stdout, output); err != nil {
		if save {
			fmt.Fprintln(stderr, "aegis: observation was saved, but cannot write inspection output")
		} else {
			fmt.Fprintln(stderr, "aegis: cannot write inspection output")
		}
		return 1
	}
	return 0
}

func history(ctx context.Context, path, containerID string, limit int, stdout, stderr io.Writer) int {
	observations, err := store.History(ctx, path, containerID, limit)
	if err != nil {
		fmt.Fprintln(stderr, "aegis: cannot read history:", err)
		return 1
	}
	var output strings.Builder
	if len(observations) == 0 {
		fmt.Fprintf(&output, "No saved observations for Docker ID %s.\n", containerID)
	} else {
		fmt.Fprintf(&output, "History for Docker ID: %s\nSaved observations (historical; not current health):\n", containerID)
		for _, observation := range observations {
			check := "unavailable"
			if !observation.CheckTime.IsZero() {
				check = observation.CheckTime.UTC().Format(time.RFC3339Nano)
			}
			fmt.Fprintf(&output, "\nObserved at: %s\nContainer: %s\nLifecycle: %s\nHealth: %s\nLast completed health check: %s\n",
				observation.ObservedAt.UTC().Format(time.RFC3339Nano), observation.Name, observation.Lifecycle, observation.Health, check)
		}
	}
	if _, err := io.WriteString(stdout, output.String()); err != nil {
		fmt.Fprintln(stderr, "aegis: cannot write history output")
		return 1
	}
	return 0
}
