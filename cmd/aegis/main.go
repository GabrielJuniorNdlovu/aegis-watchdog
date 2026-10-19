package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/moby/moby/client"
)

const usage = "Usage: aegis inspect --container <name-or-id>\n"

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
	if len(args) == 0 || args[0] != "inspect" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, usage) }
	var selector string
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
	docker, err := client.New(client.FromEnv)
	if err != nil {
		fmt.Fprintln(stderr, "aegis: invalid Docker connection configuration; check Docker environment settings")
		return 1
	}
	defer docker.Close()
	ctx, cancel := context.WithTimeout(ctx, inspection.Timeout)
	defer cancel()
	result, err := inspection.Inspect(ctx, docker, selector)
	if err != nil {
		fmt.Fprintln(stderr, "aegis:", err)
		return 1
	}
	if _, err := io.WriteString(stdout, result.Text(time.Now())); err != nil {
		fmt.Fprintln(stderr, "aegis: cannot write inspection output")
		return 1
	}
	return 0
}
