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

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
	"github.com/ergochat/readline"
)

const consoleUsage = "  aegis console [--db <path>]\n"

const consoleHelp = `Console commands:
  /help                 Show this help.
  /exit, /quit          Leave the console.

Input supports quotes and backslash escapes; no shell expansion is performed.
Use arrow keys to edit and recall commands. History lasts only for this session.
Ctrl+C clears input or cancels a command. Ctrl+D at an empty prompt exits.
`

type consoleInput interface {
	ReadLine() (string, error)
	Close() error
}

func consoleCommand(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) (code int) {
	flags := flag.NewFlagSet("console", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, "Usage:\n"+consoleUsage) }
	path := flags.String("db", store.DefaultPath, "default journal for console queries")
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(stdout, "Usage:\n"+consoleUsage)
		return 0
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*path) == "" {
		flags.Usage()
		return 2
	}
	if !detectTerminal(stdin).isTTY || !detectTerminal(stdout).isTTY {
		fmt.Fprintln(stderr, "aegis: console requires interactive stdin and stdout; use one-shot commands such as aegis inspect or aegis history (see aegis --help)")
		return 2
	}
	if os.Getenv("TERM") == "dumb" || os.Getenv("TERM") == "" {
		fmt.Fprintln(stderr, "aegis: console requires an ANSI-capable terminal for input editing; use the one-shot commands (see aegis --help)")
		return 2
	}
	editor, err := readline.NewFromConfig(&readline.Config{
		Prompt: "aegis> ", Stdin: stdin, Stdout: stdout, Stderr: stderr,
		HistoryLimit: 100, HistoryFile: "", InterruptPrompt: "^C", EOFPrompt: "\n",
	})
	if err != nil {
		fmt.Fprintln(stderr, "aegis: cannot initialize console input")
		return 1
	}
	defer func() {
		if err := editor.Close(); err != nil {
			fmt.Fprintln(stderr, "aegis: cannot restore console terminal")
			code = 1
		}
	}()
	if welcomeCommand(nil, stdout, stderr, os.Getenv, detectTerminal(stdout)) != 0 {
		return 1
	}
	if _, err := fmt.Fprintln(stdout, "Interactive Aegis console. Type /help for commands; /exit leaves."); err != nil {
		return 1
	}
	return consoleLoop(ctx, editor, *path, stdout, stderr)
}

func consoleLoop(ctx context.Context, input consoleInput, path string, stdout, stderr io.Writer) int {
	// Keep external SIGINT from terminating an idle raw terminal. Readline handles
	// keyboard Ctrl+C; each foreground operation has its own signal context.
	idleInterrupts := make(chan os.Signal, 1)
	signal.Notify(idleInterrupts, os.Interrupt)
	defer signal.Stop(idleInterrupts)
	stopClose := context.AfterFunc(ctx, func() { _ = input.Close() })
	defer stopClose()
	for ctx.Err() == nil {
		line, err := input.ReadLine()
		if errors.Is(err, readline.ErrInterrupt) {
			continue
		}
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			return 0
		}
		if err != nil {
			fmt.Fprintln(stderr, "aegis: console input failed")
			return 1
		}
		output := &consoleOutput{writer: stdout, remaining: consoleOutputLimit}
		commandCtx, cancel := signal.NotifyContext(ctx, os.Interrupt)
		exit := consoleLine(commandCtx, line, path, output)
		interrupted := commandCtx.Err() != nil
		cancel()
		if output.err != nil && !errors.Is(output.err, errConsoleOutputLimit) {
			return 1
		}
		if interrupted && ctx.Err() == nil {
			if _, err := fmt.Fprintln(stdout, "Command canceled; returning to prompt."); err != nil {
				return 1
			}
		}
		if exit {
			return 0
		}
	}
	return 0
}

func consoleLine(ctx context.Context, line, path string, out io.Writer) bool {
	args, err := parseConsoleLine(line)
	if err != nil {
		fmt.Fprintln(out, "aegis:", err)
		return false
	}
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "/help":
		if len(args) != 1 {
			fmt.Fprintln(out, "Usage: /help")
			return false
		}
		fmt.Fprint(out, consoleHelp)
	case "/exit", "/quit":
		if len(args) != 1 {
			fmt.Fprintln(out, "Usage: /exit or /quit")
			return false
		}
		return true
	default:
		fmt.Fprintf(out, "Unknown command %q. Type /help for available commands.\n", args[0])
	}
	return false
}
