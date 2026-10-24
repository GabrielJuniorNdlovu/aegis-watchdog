package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func consoleQuery(ctx context.Context, args []string, path string, out io.Writer) {
	name := args[0]
	rest := args[1:]
	switch name {
	case "/inspect", "/history", "/decisions", "/errors", "/actions", "/budget":
		command := strings.TrimPrefix(name, "/")
		if command == "decisions" {
			command = "assessments"
		}
		if command == "actions" && len(rest) > 0 && rest[0] == "acknowledge" {
			fmt.Fprintln(out, "aegis: acknowledgment is not available in the console; use aegis actions acknowledge with an exact ID and reason after stopping the daemon")
			return
		}
		if command == "inspect" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			rest = append([]string{"--container", rest[0]}, rest[1:]...)
		}
		// Only this allowlist reaches the shared CLI handlers. The console cannot
		// route input into monitor, recovery configuration, or acknowledgment.
		if run(ctx, append([]string{command, "--db", path}, rest...), out, out) == 2 {
			fmt.Fprintln(out, "aegis: invalid arguments; type /help for console syntax")
		}
	case "/evidence":
		consoleEvidence(ctx, rest, path, out)
	case "/status":
		consoleStatus(rest, path, out)
	case "/watch":
		if err := consoleWatch(ctx, rest, path, out); err != nil && ctx.Err() == nil {
			fmt.Fprintln(out, "aegis: watch stopped:", err)
		}
	default:
		fmt.Fprintf(out, "Unknown command %q. Type /help for available commands.\n", name)
	}
}

func consoleEvidence(ctx context.Context, args []string, path string, out io.Writer) {
	if len(args) == 0 {
		fmt.Fprintln(out, "Usage: /evidence <positive-action-id> [--db <path>]")
		return
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintln(out, "aegis: supply one positive action ID")
		return
	}
	flags := flag.NewFlagSet("/evidence", flag.ContinueOnError)
	flags.SetOutput(out)
	db := flags.String("db", path, "journal containing the action")
	if flags.Parse(args[1:]) != nil {
		return
	}
	if flags.NArg() != 0 || strings.TrimSpace(*db) == "" {
		fmt.Fprintln(out, "Usage: /evidence <positive-action-id> [--db <path>]")
		return
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Evidence for action %d (historical metadata; log content is not displayed):\n", id)
	if err := appendEvidenceInspection(ctx, *db, id, &text); err != nil {
		fmt.Fprintln(out, "aegis: cannot inspect evidence:", err)
		return
	}
	io.WriteString(out, text.String())
}

func consoleStatus(args []string, path string, out io.Writer) {
	flags := flag.NewFlagSet("/status", flag.ContinueOnError)
	flags.SetOutput(out)
	db := flags.String("db", path, "journal to locate")
	if flags.Parse(args) != nil {
		return
	}
	if flags.NArg() != 0 || strings.TrimSpace(*db) == "" {
		fmt.Fprintln(out, "Usage: /status [--db <path>]")
		return
	}
	absolute, err := filepath.Abs(*db)
	if err != nil {
		fmt.Fprintln(out, "aegis: cannot resolve journal path:", err)
		return
	}
	state := "present (existence only; contents not inspected)"
	info, err := os.Stat(absolute)
	switch {
	case os.IsNotExist(err):
		state = "absent; queries do not create a journal"
	case err != nil:
		state = "unavailable: " + err.Error()
	case !info.Mode().IsRegular():
		state = "not a regular file"
	}
	fmt.Fprintf(out, "Console: interactive command session\nJournal: %s\nJournal file: %s\nController status: unknown; no daemon connection is established.\nDocker connectivity: not probed; use /inspect <container>.\nMonitoring and recovery are not started by the console.\n", absolute, state)
}
