package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func TestBudgetCLIReadOnlyWithoutDocker(t *testing.T) {
	dockerEnv(t, "invalid-Docker-host")
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "budget.db")
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	p := store.Proposal{ServiceID: "orders-api", ContainerID: strings.Repeat("a", 64), Policy: budget.DefaultPolicy()}
	r, err := store.Reserve(ctx, path, p, at)
	if err != nil || r.Reservation == nil {
		t.Fatal(r, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	args := []string{"--service", "orders-api", "--db", path}
	if code := inspectBudgetCommand(ctx, args, at, &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatal(code, errOut.String())
	}
	for _, part := range []string{"Logical service: orders-api", "Attempts within (now - window, now]: 1", "State: planned", "Target Docker ID: " + p.ContainerID, "Reason: unresolved_action", "Clock unsafe: false", "Reserved policy:", "No restart is authorized or executed."} {
		if !strings.Contains(out.String(), part) {
			t.Fatal("missing", part, out.String())
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("inspection mutated database")
	}
	out.Reset()
	if code := inspectBudgetCommand(ctx, args, at.Add(-time.Nanosecond), &out, &errOut); code != 0 || !strings.Contains(out.String(), "Clock unsafe: true") {
		t.Fatal(code, out.String(), errOut.String())
	}
	out.Reset()
	if code := run(ctx, append([]string{"budget"}, args...), &out, &errOut); code != 0 {
		t.Fatal("dispatch contacted Docker", code, errOut.String())
	}
	if code := inspectBudgetCommand(ctx, args, at, brokenOutput{}, &errOut); code != 1 {
		t.Fatal(code)
	}
}
func TestBudgetCLIArguments(t *testing.T) {
	dockerEnv(t, "invalid-Docker-host")
	for _, args := range [][]string{
		{}, {"--service", ""}, {"--service", "/name"}, {"--service", "api", "--service", "other"}, {"--service", "api", "extra"},
		{"--service", "api", "--max-attempts", "0"}, {"--service", "api", "--budget-window", "0s"}, {"--service", "api", "--cooldown", "-1s"},
		{"--service", "api", "--budget-window", "wrong"}, {"--service", "api", "--limit", "0"}, {"--service", "api", "--db", ""},
		{"--service", "api", "--cancel", "1"}, {"--service", "api", "--reserve"},
	} {
		var out, errOut bytes.Buffer
		if code := inspectBudgetCommand(context.Background(), args, time.Now(), &out, &errOut); code != 2 || out.Len() != 0 || errOut.Len() == 0 {
			t.Fatal(args, code, out.String(), errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"budget", "--help"}, &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatal(code, errOut.String())
	}
	out.Reset()
	path := filepath.Join(t.TempDir(), "missing.db")
	if code := run(context.Background(), []string{"budget", "--service", "api", "--db", path}, &out, &errOut); code != 1 || out.Len() != 0 {
		t.Fatal(code, out.String(), errOut.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read created missing database")
	}
}
