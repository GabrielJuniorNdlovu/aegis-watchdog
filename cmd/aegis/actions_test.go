package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func TestActionsCLIInspectionAcknowledgmentAndLock(t *testing.T) {
	dockerEnv(t, "invalid-Docker-host")
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "actions.db")
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	p := store.Proposal{ServiceID: "orders-api", ContainerID: strings.Repeat("a", 64), Policy: budget.DefaultPolicy()}
	r, err := store.Reserve(ctx, path, p, at)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE reservations SET state='dispatching'"); err != nil {
		t.Fatal(err)
	}
	lock, err := store.ReconcileStartup(ctx, path, func(context.Context, string) (inspection.Result, error) {
		return inspection.Result{ID: p.ContainerID, Lifecycle: "running", Health: "healthy"}, nil
	}, func() time.Time { return at.Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	ack := []string{"acknowledge", "--id", "1", "--reason", "Inspected workload manually; accepting uncertainty", "--db", path}
	if code := actionsCommand(ctx, ack, at.Add(2*time.Minute), &out, &errOut); code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "another Aegis process") {
		t.Fatal(code, out.String(), errOut.String())
	}
	lock.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run(ctx, []string{"actions", "--id", "1", "--db", path}, &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatal(code, errOut.String())
	}
	for _, part := range []string{"Target Docker ID: " + p.ContainerID, "Current state: uncertain", "Action unresolved: true", "Audit: startup", "interrupted_dispatching", "still counts"} {
		if !strings.Contains(out.String(), part) {
			t.Fatal(part, out.String())
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("action inspection wrote to SQLite")
	}
	out.Reset()
	if code := actionsCommand(ctx, ack, at.Add(2*time.Minute), &out, &errOut); code != 0 || !strings.Contains(out.String(), "Uncertainty acknowledged") {
		t.Fatal(code, out.String(), errOut.String())
	}
	out.Reset()
	if code := actionsCommand(ctx, ack, at.Add(3*time.Minute), &out, &errOut); code != 0 || !strings.Contains(out.String(), "Already acknowledged") {
		t.Fatal(code, out.String(), errOut.String())
	}
	action, err := store.InspectAction(ctx, path, r.Reservation.ID)
	if err != nil || action.Unresolved || len(action.Audit) != 2 {
		t.Fatal(action, err)
	}
	if code := actionsCommand(ctx, []string{"--id", "1", "--db", path}, at, brokenOutput{}, &errOut); code != 1 {
		t.Fatal(code)
	}
}
func TestActionsCLIArguments(t *testing.T) {
	dockerEnv(t, "invalid-Docker-host")
	for _, args := range [][]string{
		{}, {"--id", "0"}, {"--id", "abc"}, {"--id", "1", "--id", "2"}, {"--id", "1", "--db", ""},
		{"acknowledge", "--id", "1"}, {"acknowledge", "--id", "1", "--reason", ""}, {"acknowledge", "--id", "1", "--reason", strings.Repeat("a", 513)},
		{"acknowledge", "--id", "1", "--reason", "reason", "--reason", "other"}, {"dispatch", "--id", "1"},
	} {
		var out, errOut bytes.Buffer
		if code := actionsCommand(context.Background(), args, time.Now(), &out, &errOut); code != 2 || out.Len() != 0 {
			t.Fatal(args, code, out.String(), errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"actions", "--help"}, &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatal(code, errOut.String())
	}
	path := filepath.Join(t.TempDir(), "missing.db")
	out.Reset()
	if code := actionsCommand(context.Background(), []string{"acknowledge", "--id", "999", "--reason", "checked", "--db", path}, time.Now(), &out, &errOut); code != 1 || out.Len() != 0 {
		t.Fatal(code, out.String(), errOut.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("unknown journal created a database")
	}
}
