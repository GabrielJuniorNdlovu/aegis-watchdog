package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/ownership"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func consoleFixture(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	id := strings.Repeat("a", 64)
	at := time.Now().UTC().Add(-2 * time.Minute)
	result := inspection.Result{ID: id, Name: "demo", Lifecycle: "running", Health: "unknown"}
	a := assessment.Assessment{ContainerID: id, CollectedAt: at, EvaluatedAt: at, Status: "unknown", Reason: "no_healthcheck", Policy: assessment.DefaultPolicy(), Explanation: "fixture \x1b]52;c;untrusted\a explanation"}
	if err := store.SaveMonitoringResult(context.Background(), path, "demo", result, nil, a); err != nil {
		t.Fatal(err)
	}
	r, err := store.Reserve(context.Background(), path, store.Proposal{ServiceID: "demo", ContainerID: id, Policy: budget.DefaultPolicy()}, at)
	if err != nil || r.Reservation == nil {
		t.Fatal(r, err)
	}
	if err := store.SaveCollectionError(context.Background(), path, "demo", at, errors.New("fixture")); err != nil {
		t.Fatal(err)
	}
	return path, id
}

func consoleTestLine(t *testing.T, line, path string) string {
	t.Helper()
	var out bytes.Buffer
	w := &consoleOutput{writer: &out, remaining: consoleOutputLimit}
	if consoleLine(context.Background(), line, path, w) {
		t.Fatal("unexpected session exit")
	}
	if w.err != nil {
		t.Fatal(w.err)
	}
	return out.String()
}

func TestConsoleQueriesUseExistingReadOnlyServices(t *testing.T) {
	dockerEnv(t, "invalid-Docker-host")
	path, id := consoleFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ line, want string }{
		{"/history --container " + id, "Container: demo"},
		{"/history --container " + strings.Repeat("b", 64), "No saved observations"},
		{"/decisions --container demo", "Assessment: unknown"},
		{"/decisions --container empty", "No saved assessments"},
		{"/errors --container demo", "Category: request_failed"},
		{"/errors --container empty", "No saved collection errors"},
		{"/actions --id 1", "Current state: planned"},
		{"/evidence 1", "Evidence capture: none recorded"},
		{"/budget --service demo --limit 1 --max-attempts 1", "Reservation: 1"},
		{"/status", "Controller status: unknown"},
	} {
		out := consoleTestLine(t, tc.line, path)
		if !strings.Contains(out, tc.want) {
			t.Fatal(tc.line, tc.want, out)
		}
		if strings.ContainsAny(out, "\x1b\a") {
			t.Fatal("unsafe terminal output", out)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read commands mutated journal", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "evidence")); !os.IsNotExist(err) {
		t.Fatal("queries created evidence", err)
	}
	alternate := filepath.Join(t.TempDir(), "absent.db")
	out := consoleTestLine(t, fmt.Sprintf("/status --db %q", alternate), path)
	if !strings.Contains(out, alternate) || !strings.Contains(out, "absent") {
		t.Fatal(out)
	}
	if _, err := os.Stat(alternate); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestConsoleRestrictsMutationsAndValidatesArguments(t *testing.T) {
	path, _ := consoleFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"/monitor --container demo --enable-recovery", "/restart demo", "/actions acknowledge --id 1 --reason checked",
		"/actions --db nowhere acknowledge --id 1 --reason checked", "/actions --id 0", "/evidence 0", "/evidence x", "/evidence", "/evidence 1 extra",
		"/history --container demo", "/history --container " + strings.Repeat("a", 64) + " --limit 1001", "/inspect", "/status extra", "/inspect demo --interval 1s",
	} {
		if out := consoleTestLine(t, line, path); !strings.Contains(out, "aegis:") && !strings.Contains(out, "Usage:") && !strings.Contains(out, "Unknown command") {
			t.Fatal(line, out)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected operation mutated journal", err)
	}
}

func TestConsoleInspectUsesBoundedReadOnlyAPIAndExplicitSave(t *testing.T) {
	calls := inspectionAPI(t, "unhealthy", 200)
	path := filepath.Join(t.TempDir(), "saved.db")
	out := consoleTestLine(t, "/inspect demo", path)
	if !strings.Contains(out, "Health: unhealthy") || calls.Load() != 1 {
		t.Fatal(out, calls.Load())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("inspect without --save wrote journal", err)
	}
	out = consoleTestLine(t, "/inspect demo --save", path)
	if !strings.Contains(out, "Saved observation at:") || calls.Load() != 2 {
		t.Fatal(out, calls.Load())
	}
	rows, err := store.History(context.Background(), path, historyID, 20)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
}

func TestConsoleInspectInterruptReturnsToPrompt(t *testing.T) {
	requested := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" && r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.52")
			return
		}
		if r.Method != "GET" || r.URL.Path != "/v1.52/containers/demo/json" {
			t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.Path)
			return
		}
		requested <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	dockerEnv(t, server.URL)
	input := &scriptedConsole{steps: []consoleStep{{line: "/inspect demo"}, {line: "/help"}, {line: "/exit"}}}
	var out, errOut bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- consoleLoop(ctx, input, "unused.db", &out, &errOut) }()
	select {
	case <-requested:
	case <-ctx.Done():
		t.Fatal("inspection did not reach API")
	}
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := self.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 || !strings.Contains(out.String(), "Command canceled; returning to prompt") || !strings.Contains(out.String(), "Console commands:") {
			t.Fatal(code, out.String(), errOut.String())
		}
	case <-ctx.Done():
		t.Fatal("interrupt did not return to prompt")
	}
}

type cancelWatchOutput struct {
	bytes.Buffer
	cancel context.CancelFunc
	marker string
}

func (w *cancelWatchOutput) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if strings.Contains(w.String(), w.marker) {
		w.cancel()
	}
	return n, err
}
func (w *cancelWatchOutput) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func TestConsoleWatchReadsUnderMonitorLockAndCancels(t *testing.T) {
	dockerEnv(t, "invalid-Docker-host")
	path, _ := consoleFixture(t)
	lock, err := ownership.Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ target, marker string }{{"demo", "Historical assessment read at:"}, {"empty", "No saved assessments"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		out := &cancelWatchOutput{cancel: cancel, marker: tc.marker}
		err := consoleWatch(ctx, []string{"--container", tc.target, "--interval", "200ms"}, path, out)
		cancel()
		if !errors.Is(err, context.Canceled) || !strings.Contains(out.String(), tc.marker) {
			t.Fatal(out.String(), err)
		}
		if tc.target == "demo" && !strings.Contains(out.String(), "stale") {
			t.Fatal("stale collection not labeled", out.String())
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("watch changed journal", err)
	}
	if another, err := ownership.Acquire(path); err == nil {
		another.Close()
		t.Fatal("watch released another process's lock")
	}
	for _, args := range [][]string{{}, {"--container", "demo", "--container", "other"}, {"--container", "demo", "--interval", "0"}, {"--container", "demo", "--interval", "1h"}, {"--container", "demo", "extra"}} {
		if err := consoleWatch(context.Background(), args, path, io.Discard); err == nil {
			t.Fatal(args)
		}
	}
}

func TestConsoleFreshness(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		at   time.Time
		age  time.Duration
		want string
	}{
		{time.Time{}, time.Minute, "unavailable"}, {now.Add(time.Second), time.Minute, "unknown (future timestamp)"}, {now, 0, "unknown (no valid age policy)"}, {now.Add(-2 * time.Minute), time.Minute, "stale"}, {now.Add(-time.Minute), time.Minute, "fresh at read"},
	} {
		if got := consoleFreshness(tc.at, now, tc.age); got != tc.want {
			t.Fatal(got, tc.want)
		}
	}
}
