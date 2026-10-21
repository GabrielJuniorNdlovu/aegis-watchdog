package store

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/moby/moby/client"
)

func startup(t *testing.T, path string, inspect func(context.Context, string) (inspection.Result, error), at time.Time) {
	t.Helper()
	lock, err := ReconcileStartup(context.Background(), path, inspect, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
func auditCount(t *testing.T, path string) int {
	t.Helper()
	var n int
	if err := readSQL(t, path).QueryRow("SELECT COUNT(*) FROM action_audit").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func TestPlannedReconciliationPreservesBudgetAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "planned.db")
	p := proposal()
	r := accept(t, path, p, testTime)
	read := func(context.Context, string) (inspection.Result, error) {
		t.Fatal("planned cancellation called Docker")
		return inspection.Result{}, nil
	}
	startup(t, path, read, testTime.Add(time.Minute))
	startup(t, path, read, testTime.Add(2*time.Minute))
	h := inspect(t, path, p, testTime.Add(2*time.Minute))
	if len(h.Reservations) != 1 || h.Reservations[0].State != "canceled" || h.Decision.Attempts != 1 || h.Decision.Reason != "cooldown" || h.Reservations[0].ID != r.ID || auditCount(t, path) != 1 {
		t.Fatal(h)
	}
}
func TestDispatchAndVerificationBecomeUncertain(t *testing.T) {
	for _, state := range []string{"dispatching", "verifying"} {
		t.Run(state, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "interrupted.db")
			p := proposal()
			r := accept(t, path, p, testTime)
			terminal(t, path, r.ID, state)
			calls := 0
			read := func(ctx context.Context, id string) (inspection.Result, error) {
				calls++
				if id != p.ContainerID {
					t.Fatal("substituted target", id)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > inspection.Timeout {
					t.Fatal("unbounded request")
				}
				result := result("healthy")
				result.Evidence.StartedAt = testTime.Add(30 * time.Second)
				return result, nil
			}
			startup(t, path, read, testTime.Add(time.Minute))
			startup(t, path, read, testTime.Add(2*time.Minute))
			h := inspect(t, path, p, testTime.Add(2*time.Minute))
			if calls != 1 || h.Reservations[0].State != "uncertain" || h.Decision.Reason != "unresolved_action" || h.Decision.Attempts != 1 || auditCount(t, path) != 1 {
				t.Fatal(calls, h)
			}
			var explanation, payload string
			if err := readSQL(t, path).QueryRow("SELECT explanation,evidence FROM action_audit").Scan(&explanation, &payload); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(explanation, "cannot prove") || !strings.Contains(payload, "healthy") {
				t.Fatal(explanation, payload)
			}
		})
	}
}
func TestUncertainAndTerminalReconciliationNoOp(t *testing.T) {
	for _, state := range []string{"uncertain", "succeeded", "failed", "canceled"} {
		t.Run(state, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "resolved.db")
			r := accept(t, path, proposal(), testTime)
			terminal(t, path, r.ID, state)
			startup(t, path, func(context.Context, string) (inspection.Result, error) {
				t.Fatal("reinspected existing outcome")
				return inspection.Result{}, nil
			}, testTime.Add(time.Minute))
			h := inspect(t, path, proposal(), testTime.Add(time.Minute))
			if h.Reservations[0].State != state || auditCount(t, path) != 0 {
				t.Fatal(h)
			}
		})
	}
}
func TestReconciliationFailureAndSafeRetryBetweenActions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retry.db")
	p := proposal()
	r := accept(t, path, p, testTime)
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Simulate multiple imported unfinished records only inside this fixture.
	if _, err := db.Exec(`DROP INDEX reservations_one_unresolved; INSERT INTO reservations(service_id,container_id,reserved_at,state,max_attempts,window_ns,cooldown_ns) SELECT service_id,container_id,reserved_at,state,max_attempts,window_ns,cooldown_ns FROM reservations;
 CREATE TRIGGER fail_second_audit BEFORE INSERT ON action_audit WHEN NEW.action_id != 1 BEGIN SELECT RAISE(ABORT,'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	lock, err := ReconcileStartup(context.Background(), path, nil, func() time.Time { return testTime.Add(time.Minute) })
	if err == nil || lock != nil {
		t.Fatal("failed startup enabled owner", lock, err)
	}
	var first, second string
	if err := db.QueryRow("SELECT state FROM reservations WHERE reservation_id=?", r.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT state FROM reservations WHERE reservation_id=?", r.ID+1).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != "canceled" || second != "planned" || auditCount(t, path) != 1 {
		t.Fatal(first, second)
	}
	if _, err := db.Exec("DROP TRIGGER fail_second_audit"); err != nil {
		t.Fatal(err)
	}
	startup(t, path, nil, testTime.Add(2*time.Minute))
	startup(t, path, nil, testTime.Add(3*time.Minute))
	if auditCount(t, path) != 2 {
		t.Fatal("retry duplicated transitions")
	}
}
func TestReconciliationAuditFailureRollsBackState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	r := accept(t, path, proposal(), testTime)
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON action_audit BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	lock, err := ReconcileStartup(context.Background(), path, nil, func() time.Time { return testTime.Add(time.Minute) })
	if err == nil || lock != nil {
		t.Fatal(lock, err)
	}
	h := inspect(t, path, proposal(), testTime.Add(time.Minute))
	if h.Reservations[0].ID != r.ID || h.Reservations[0].State != "planned" || auditCount(t, path) != 0 {
		t.Fatal(h)
	}
}
func TestReconciliationFakeDockerReadOnlyFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		category string
	}{
		{"missing", 404, `{"message":"SECRET"}`, "not_found"},
		{"denied", 403, `{"message":"SECRET"}`, "permission_denied"},
		{"unavailable", 503, `{"message":"SECRET"}`, "request_failed"},
		{"malformed", 200, `{"Id":`, "invalid_response"},
		{"replacement", 200, fmt.Sprintf(`{"Id":%q,"Name":"/reused","Config":{},"State":{"Status":"running"}}`, strings.Repeat("b", 64)), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "docker.db")
			p := proposal()
			r := accept(t, path, p, testTime)
			terminal(t, path, r.ID, "dispatching")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == "HEAD" && req.URL.Path == "/_ping" {
					w.Header().Set("API-Version", "1.52")
					return
				}
				if req.Method != "GET" || req.URL.Path != "/v1.52/containers/"+p.ContainerID+"/json" {
					t.Errorf("unexpected Docker request %s %s", req.Method, req.URL.Path)
					w.WriteHeader(500)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			docker, err := client.New(client.WithHost(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			defer docker.Close()
			read := func(ctx context.Context, id string) (inspection.Result, error) {
				return inspection.Inspect(ctx, docker, id)
			}
			startup(t, path, read, testTime.Add(time.Minute))
			var payload, explanation, reason string
			if err := readSQL(t, path).QueryRow("SELECT evidence,explanation,reason FROM action_audit").Scan(&payload, &explanation, &reason); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(payload+explanation, "SECRET") || !strings.Contains(payload, tc.category) {
				t.Fatal(payload, explanation)
			}
			if tc.name == "replacement" && reason != "target_identity_mismatch" {
				t.Fatal(reason)
			}
		})
	}
}
func TestReconciliationTimeoutAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "timeout.db")
			p := proposal()
			r := accept(t, path, p, testTime)
			terminal(t, path, r.ID, "dispatching")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == "HEAD" && req.URL.Path == "/_ping" {
					w.Header().Set("API-Version", "1.52")
					return
				}
				if req.Method != "GET" || req.URL.Path != "/v1.52/containers/"+p.ContainerID+"/json" {
					t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
					w.WriteHeader(500)
					return
				}
				if canceled {
					cancel()
				}
				<-req.Context().Done()
			}))
			defer server.Close()
			docker, err := client.New(client.WithHost(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			defer docker.Close()
			lock, err := ReconcileStartup(ctx, path, func(ctx context.Context, id string) (inspection.Result, error) {
				return inspection.Inspect(ctx, docker, id)
			}, func() time.Time { return testTime.Add(time.Minute) })
			if canceled {
				if lock != nil || !errors.Is(err, context.Canceled) {
					t.Fatal(lock, err)
				}
				if inspect(t, path, p, testTime).Reservations[0].State != "dispatching" {
					t.Fatal("cancellation changed outcome")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			lock.Close()
			var evidence string
			if err := readSQL(t, path).QueryRow("SELECT evidence FROM action_audit").Scan(&evidence); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(evidence, "timeout") {
				t.Fatal(evidence)
			}
		})
	}
}
func TestReconciliationUnavailableDocker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unavailable.db")
	p := proposal()
	r := accept(t, path, p, testTime)
	terminal(t, path, r.ID, "verifying")
	docker, err := client.New(client.WithHost("unix://" + filepath.Join(t.TempDir(), "missing.sock")))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	startup(t, path, func(ctx context.Context, id string) (inspection.Result, error) {
		return inspection.Inspect(ctx, docker, id)
	}, testTime.Add(time.Minute))
	var evidence string
	if err := readSQL(t, path).QueryRow("SELECT evidence FROM action_audit").Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(evidence, "unavailable") {
		t.Fatal(evidence)
	}
}

func TestReconciliationCannotOverwriteNewerOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conditional.db")
	p := proposal()
	r := accept(t, path, p, testTime)
	terminal(t, path, r.ID, "dispatching")
	startup(t, path, func(context.Context, string) (inspection.Result, error) {
		terminal(t, path, r.ID, "failed")
		return result("healthy"), nil
	}, testTime.Add(time.Minute))
	h := inspect(t, path, p, testTime.Add(time.Minute))
	if h.Reservations[0].State != "failed" || auditCount(t, path) != 0 {
		t.Fatal(h)
	}
}
