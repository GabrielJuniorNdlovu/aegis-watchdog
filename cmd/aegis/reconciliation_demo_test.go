package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

// Child processes run the real CLI entry point, including signal handling.
func TestActionCLIProcessHelper(t *testing.T) {
	if os.Getenv("AEGIS_TEST_ACTION_CLI") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"aegis"}, os.Args[i+1:]...)
			main()
			return
		}
	}
	t.Fatal("missing child CLI arguments")
}

func TestSimulatedActionAcknowledgmentDemo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "simulated-actions.db")
	p := store.Proposal{ServiceID: "simulated-service", ContainerID: strings.Repeat("a", 64), Policy: budget.Policy{MaxAttempts: 1, Window: time.Hour, Cooldown: 5 * time.Minute}}
	reservation, err := store.Reserve(ctx, path, p, time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE reservations SET state='verifying' WHERE reservation_id=?", reservation.Reservation.ID); err != nil {
		t.Fatal(err)
	}
	t.Log("SIMULATED: fixture marked verifying; no Docker restart was ever dispatched")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" && r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.52")
			return
		}
		if r.Method != "GET" || r.URL.Path != "/v1.52/containers/"+p.ContainerID+"/json" {
			t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Id":%q,"Name":"/simulated","Config":{"Healthcheck":{"Test":["CMD","true"]}},"State":{"Status":"running","Health":{"Status":"healthy","Log":[]}}}`, p.ContainerID)
	}))
	defer server.Close()
	dockerEnv(t, server.URL)
	child := func(args ...string) *exec.Cmd {
		argv := append([]string{"-test.run=^TestActionCLIProcessHelper$", "--"}, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], argv...)
		cmd.Env = append(os.Environ(), "AEGIS_TEST_ACTION_CLI=1")
		return cmd
	}
	monitor := child("monitor", "--container", p.ContainerID, "--interval", "1h", "--db", path)
	stdout, err := monitor.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var monitorErr bytes.Buffer
	monitor.Stderr = &monitorErr
	if err := monitor.Start(); err != nil {
		t.Fatal(err)
	}
	running := true
	defer func() {
		if running {
			monitor.Process.Kill()
			monitor.Wait()
		}
	}()
	ready := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(stdout)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				ready <- err
				return
			}
			if strings.HasPrefix(line, "Assessment:") {
				ready <- nil
				io.Copy(io.Discard, reader)
				return
			}
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal("monitor did not start", err)
		}
	case <-ctx.Done():
		t.Fatal("monitor startup timed out")
	}
	h, err := store.InspectBudget(ctx, path, p.ServiceID, p.Policy, time.Now().UTC(), 20)
	if err != nil || h.Decision.Reason != "unresolved_action" || h.Decision.Attempts != 1 {
		t.Fatal(h, err)
	}
	t.Log("PASS: startup records uncertainty; monitoring continues; reservations remain blocked")
	ackArgs := []string{"actions", "acknowledge", "--id", "1", "--reason", "Reviewed simulated workload; accepting the uncertain outcome", "--db", path}
	output, err := child(ackArgs...).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "another Aegis process") {
		t.Fatalf("acknowledgment bypassed running daemon: %v %s", err, output)
	}
	t.Log("PASS: operator acknowledgment refuses while the daemon owns the lock")
	if err := monitor.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = monitor.Wait()
	running = false
	if err != nil {
		t.Fatalf("monitor stop: %v %s", err, monitorErr.String())
	}
	output, err = child(ackArgs...).CombinedOutput()
	if err != nil || !strings.Contains(string(output), "Uncertainty acknowledged") {
		t.Fatalf("acknowledgment after shutdown: %v %s", err, output)
	}
	t.Log("PASS: with daemon stopped, explicit acknowledgment is recorded")
	output, err = child("actions", "--id", "1", "--db", path).CombinedOutput()
	if err != nil || !strings.Contains(string(output), "Audit: startup") || !strings.Contains(string(output), "Audit: acknowledgment") || !strings.Contains(string(output), "Current state: uncertain") || !strings.Contains(string(output), "Action unresolved: false") {
		t.Fatalf("audit inspection: %v %s", err, output)
	}
	output, err = child("budget", "--service", p.ServiceID, "--max-attempts", "1", "--db", path).CombinedOutput()
	if err != nil || !strings.Contains(string(output), "Reason: budget_exhausted") || !strings.Contains(string(output), "Attempts within (now - window, now]: 1") {
		t.Fatalf("budget inspection: %v %s", err, output)
	}
	t.Log("PASS: original uncertainty and audit remain; the attempt still consumes budget")
	output, err = child(ackArgs...).CombinedOutput()
	if err != nil || !strings.Contains(string(output), "Already acknowledged") {
		t.Fatalf("repeated acknowledgment: %v %s", err, output)
	}
	t.Log("PASS: repeated acknowledgment preserves the original audit; temporary resources clean up automatically")
}
